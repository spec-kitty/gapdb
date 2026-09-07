package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/admin"
	"github.com/spec-kitty/gapdb/internal/engine"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/persist"
	"github.com/spec-kitty/gapdb/internal/protocol"
)

func (server *Server) dispatch(request protocol.Request) protocol.Response {
	status := server.runtime.Status().Status
	response := protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: true, RequestID: request.RequestID, DatabaseID: status.DatabaseID, Operation: request.Operation}
	ctx := context.Background()
	var cancel context.CancelFunc
	if server.read > 0 {
		ctx, cancel = context.WithTimeout(ctx, server.read)
		defer cancel()
	}
	var result any
	var err error
	switch arguments := request.Arguments.(type) {
	case protocol.GetArguments:
		var record gapdb.Record
		record, err = server.runtime.State().Get(arguments.Key)
		result = protocol.GetResult{Record: record}
	case protocol.GetManyArguments:
		var records gapdb.ReadManyResult
		records, err = server.runtime.State().ReadMany(arguments.Keys)
		result = records
	case protocol.PutArguments:
		if request.Operation == protocol.OperationPut {
			value, callErr := server.runtime.State().Put(ctx, arguments.Key, arguments.Value, arguments.ExpiresAt, arguments.Ack)
			err, result = callErr, value.MutationResult
		} else {
			value, callErr := server.runtime.State().PutIfAbsent(ctx, arguments.Key, arguments.Value, arguments.ExpiresAt, arguments.Ack)
			err, result = callErr, value.MutationResult
		}
	case protocol.CompareAndSwapArguments:
		value, callErr := server.runtime.State().CompareAndSwap(ctx, arguments.Key, arguments.ExpectedRevision, arguments.Value, arguments.ExpiresAt, arguments.Ack)
		err, result = callErr, value.MutationResult
	case protocol.DeleteIfRevisionArguments:
		value, callErr := server.runtime.State().DeleteIfRevision(ctx, arguments.Key, arguments.ExpectedRevision, arguments.Ack)
		err, result = callErr, value.MutationResult
	case protocol.BatchArguments:
		value, callErr := server.runtime.State().AtomicBatch(ctx, gapdb.Batch{Ack: arguments.Ack, Assertions: arguments.Assertions, Mutations: arguments.Mutations})
		err, result = callErr, value.MutationResult
	case protocol.ScanArguments:
		result, err = server.runtime.State().ScanPrefix(arguments.Prefix, arguments.Limit, arguments.Cursor)
	case protocol.EmptyArguments:
		result, err = server.dispatchInspection(request.Operation)
	case protocol.VerifyArguments:
		result, err = server.verifyOnline(ctx, defaultString(arguments.Mode, "sampled"))
	case protocol.SnapshotArguments:
		status = server.runtime.Status().Status
		requestID := server.correlation(request.RequestID)
		var snapshot engine.SnapshotResult
		snapshot, err = server.runtime.Snapshot(ctx, admin.SnapshotRequest{ExpectedDatabaseID: arguments.ExpectedDatabaseID, ExpectedManifestGeneration: server.manifest.Load(), ExpectedRevision: arguments.ExpectedRevision, RequestID: requestID, EventID: "snapshot-" + requestID})
		if err == nil {
			server.manifest.Add(1)
			identity, readErr := persist.ReadIdentity(faultfs.NewOS(nil), server.directory)
			if readErr != nil {
				err = server.appliedAdminResultError(readErr)
			} else if manifest, readErr := persist.ReadManifest(faultfs.NewOS(nil), server.directory, identity.DatabaseID, server.manifest.Load()); readErr != nil {
				err = server.appliedAdminResultError(readErr)
			} else {
				result = gapdb.SnapshotResult{Revision: snapshot.Revision, Filename: manifest.SnapshotFile, Checksum: manifest.SnapshotSHA256, NewWALStart: manifest.WALStartRevision, Duration: snapshot.Duration}
			}
		}
		err = server.augmentAdminError(err, arguments.ExpectedDatabaseID, arguments.ExpectedRevision, status)
	case protocol.CompactArguments:
		status = server.runtime.Status().Status
		requestID := server.correlation(request.RequestID)
		var compact admin.CompactionView
		compact, err = server.runtime.Compact(admin.CompactionRequest{ExpectedDatabaseID: arguments.ExpectedDatabaseID, ExpectedManifestGeneration: server.manifest.Load(), ExpectedRevision: status.CurrentRevision, ThroughRevision: arguments.ThroughRevision, RequestID: requestID, EventID: "compact-" + requestID})
		result = gapdb.CompactionResult{BeforeRevision: compact.BeforeRevision, AfterRevision: compact.AfterRevision, Removed: compact.Removed, RemovedCount: compact.RemovedCount, SkippedCount: compact.SkippedCount, PathsTruncated: compact.PathsTruncated, DirectorySynced: err == nil}
		var unsafe *gapdb.Error
		if errors.As(err, &unsafe) && unsafe.Code == gapdb.CodeCompactionNotSafe {
			clone := unsafe.Clone()
			clone.Reason = ""
			clone.ThroughRevision = revisionPointer(arguments.ThroughRevision)
			clone.ActiveSnapshotRevision = revisionPointer(status.SnapshotRevision)
			err = clone
		}
		err = server.augmentAdminError(err, arguments.ExpectedDatabaseID, status.CurrentRevision, status)
	case protocol.BackupArguments:
		status = server.runtime.Status().Status
		requestID := server.correlation(request.RequestID)
		var backup persist.BackupMetadata
		backup, err = server.runtime.Backup(admin.BackupRequest{ExpectedDatabaseID: arguments.ExpectedDatabaseID, ExpectedManifestGeneration: server.manifest.Load(), ExpectedDurableRevision: arguments.ExpectedRevision, Destination: arguments.Destination, BackupID: "backup-" + requestID, RequestID: requestID, EventID: "backup-" + requestID})
		if err == nil {
			files := make([]gapdb.BackupFile, len(backup.Files))
			var bytes int64
			var manifestChecksum string
			for index, file := range backup.Files {
				files[index] = gapdb.BackupFile{Name: file.Name, Size: file.Size, SHA256: file.SHA256}
				bytes += file.Size
				if file.Name == persist.ManifestFilename {
					manifestChecksum = file.SHA256
				}
			}
			result = gapdb.BackupResult{DatabaseID: backup.SourceDatabaseID, Revision: backup.DurableRevision, ManifestChecksum: manifestChecksum, Files: files, ByteCount: bytes, Verified: true}
		}
		err = server.augmentAdminError(err, arguments.ExpectedDatabaseID, arguments.ExpectedRevision, status)
	case protocol.OfflineInspectArguments, protocol.OfflineVerifyArguments, protocol.OfflineRecoverProposeArguments, protocol.OfflineRecoverApplyArguments:
		err = invalidRequest("operation", "offline operations are not available on the owner socket")
	default:
		err = invalidRequest("operation", "operation is not implemented by the owner")
	}
	if err != nil {
		response.OK = false
		response.Error = structuredError(err, request.Operation, server.runtime.Status().Status)
		return response
	}
	response.Result = result
	return response
}

func (server *Server) handleRecoverySnapshot(conn net.Conn, request protocol.Request) error {
	arguments, ok := request.Arguments.(protocol.RecoverySnapshotArguments)
	if !ok {
		return server.writeFailure(conn, request.Operation, request.RequestID, invalidRequest("arguments", "recovery snapshot request is invalid"))
	}
	result, err := server.runtime.State().ReadRecoverySnapshot(gapdb.RecoverySnapshotRequest{
		Prefix: arguments.Prefix, ExpectedRevision: arguments.ExpectedRevision,
		MaxRecords: arguments.MaxRecords, MaxBytes: arguments.MaxBytes,
	})
	if err != nil {
		return server.writeFailure(conn, request.Operation, request.RequestID, err)
	}
	result.RequestID = request.RequestID
	payload, err := gapdb.EncodeRecoverySnapshot(result, gapdb.RecoverySnapshotRequest{
		Prefix: arguments.Prefix, ExpectedRevision: arguments.ExpectedRevision,
		MaxRecords: arguments.MaxRecords, MaxBytes: arguments.MaxBytes,
	})
	if err != nil {
		return server.writeFailure(conn, request.Operation, request.RequestID, err)
	}
	if server.write > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(server.write))
	}
	if err := faultfs.Checkpoint(server.fs, faultfs.PointResponsePublish, faultfs.Before); err != nil {
		return err
	}
	if err := protocol.WriteRecoveryFrame(conn, payload, arguments.MaxBytes); err != nil {
		return err
	}
	return faultfs.Checkpoint(server.fs, faultfs.PointResponsePublish, faultfs.After)
}

func (server *Server) appliedAdminResultError(err error) error {
	server.runtime.State().MarkAdminDegraded()
	var structured *gapdb.Error
	if errors.As(err, &structured) {
		clone := structured.Clone()
		clone.OperationApplied = true
		return clone
	}
	return err
}

func (server *Server) dispatchInspection(operation protocol.Operation) (any, error) {
	view := server.runtime.Status()
	switch operation {
	case protocol.OperationStatus:
		status := view.Status
		return gapdb.StatusResult{Lifecycle: status.Lifecycle, DatabaseID: status.DatabaseID, CurrentRevision: status.CurrentRevision, DurableThroughRevision: status.DurableThroughRevision, ReservedRevisionEnd: status.ReservedRevisionEnd, SnapshotRevision: status.SnapshotRevision, ActiveWALStart: status.ActiveWALStart, EarliestWatchRevision: status.EarliestWatchRevision, RecordCount: view.RecordCount, WatchCount: view.WatchCount, QueueDepth: view.QueueDepth, ActiveClients: len(server.clients), Limits: status.Limits, OwnerPID: os.Getpid(), OwnerStartedAt: server.startedAt.Format(time.RFC3339Nano)}, nil
	case protocol.OperationHealth:
		return gapdb.HealthResult{Lifecycle: view.Status.Lifecycle, Healthy: view.Healthy, FailingSubsystems: boundedFailures(view), SafeActions: healthActions(view)}, nil
	case protocol.OperationStats:
		return gapdb.StatsResult{SchemaVersion: 1, LiveRecords: view.RecordCount, Commits: uint64(view.Status.CurrentRevision), Watchers: view.WatchCount, Clients: len(server.clients), QueueDepth: view.QueueDepth}, nil
	case protocol.OperationDescribeConfig:
		effective := view.EffectiveConfig
		return gapdb.ConfigResult{Effective: gapdb.ConfigValues{Limits: effective.Limits, MutationQueueCapacity: effective.MutationQueueCapacity, WALBufferBytes: effective.WALBufferBytes, AuditMaxLineBytes: effective.AuditMaxLineBytes, AuditMaxFileBytes: effective.AuditMaxFileBytes, AuditKeepGenerations: effective.AuditKeepGenerations, MaxAdminPaths: effective.MaxAdminPaths}, Source: "flag", SafeCeilings: hardCeilings(), RestartRequired: false}, nil
	default:
		return nil, invalidRequest("operation", "operation requires non-empty arguments")
	}
}

func (server *Server) verifyOnline(ctx context.Context, mode string) (any, error) {
	if mode == "full" {
		if err := server.runtime.State().DurableAdmin(ctx, func(cut engine.DurableAdminCut) error { return nil }); err != nil {
			return nil, err
		}
	}
	status := server.runtime.Status().Status
	fsys := faultfs.NewOS(nil)
	identity, err := persist.ReadIdentity(fsys, server.directory)
	if err != nil {
		return nil, err
	}
	manifest, err := persist.ReadManifest(fsys, server.directory, identity.DatabaseID, server.manifest.Load())
	if err != nil {
		return nil, err
	}
	if _, err := (persist.SnapshotFileStore{Limits: server.limits}).Load(fsys, server.directory, manifest); err != nil {
		return nil, err
	}
	walPath := filepath.Join(server.directory, manifest.WALFile)
	wal, err := os.Open(walPath)
	if err != nil {
		return nil, permissionError(walPath, "verify_open", err)
	}
	defer wal.Close()
	header := make([]byte, persist.WALHeaderSize)
	if _, err := io.ReadFull(wal, header); err != nil {
		return nil, permissionError(walPath, "verify_read", err)
	}
	if _, err := persist.DecodeWALHeader(header, identity.DatabaseID); err != nil {
		return nil, err
	}
	if status.DatabaseID != identity.DatabaseID.String() || status.SnapshotRevision != manifest.SnapshotRevision || status.ActiveWALStart != manifest.WALStartRevision {
		return nil, &gapdb.Error{Code: gapdb.CodeStorageDegraded, Message: "Live authority does not match durable authority.", Retry: gapdb.RetryAfterRestart, FailedStage: "online_verify_authority", CurrentRevision: revisionPointer(status.CurrentRevision), DurableThroughRevision: revisionPointer(status.DurableThroughRevision), SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionRestartAfterRecovery, gapdb.ActionAbort}}
	}
	return gapdb.VerifyResult{Mode: mode, Verified: true, DatabaseID: status.DatabaseID, ManifestGeneration: manifest.Generation, CurrentRevision: status.CurrentRevision, SnapshotRevision: manifest.SnapshotRevision, ActiveWALStart: manifest.WALStartRevision, CheckedFiles: []string{persist.IdentityFilename, persist.ManifestFilename, manifest.SnapshotFile, manifest.WALFile}}, nil
}

func hardCeilings() gapdb.Limits {
	return gapdb.Limits{MaxKeyBytes: gapdb.HardMaxKeyBytes, MaxValueBytes: gapdb.HardMaxValueBytes, MaxFrameBytes: gapdb.HardMaxFrameBytes, MaxBatchBytes: gapdb.HardMaxBatchBytes, MaxBatchOperations: gapdb.HardMaxBatchOperations, MaxScanRecords: gapdb.HardMaxScanRecords, MaxScanBytes: gapdb.HardMaxScanBytes, WatchBufferEvents: gapdb.HardMaxWatchBufferEvents, MaxWatchClients: gapdb.HardMaxWatchClients, MaxConcurrentClients: gapdb.HardMaxConcurrentClients, MaxHistoryEvents: gapdb.HardMaxHistoryEvents, MaxHistoryBytes: gapdb.HardMaxHistoryBytes}
}

func (server *Server) handleWatch(conn net.Conn, request protocol.Request) {
	arguments := request.Arguments.(protocol.WatchArguments)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription, err := server.runtime.State().Watch(ctx, arguments.Prefix, arguments.AfterRevision)
	if err != nil {
		_ = server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: false, RequestID: request.RequestID, DatabaseID: server.runtime.Status().Status.DatabaseID, Operation: request.Operation, Stream: protocol.StreamEnded, EndReason: gapdb.WatchEndedByError, Error: structuredError(err, request.Operation, server.runtime.Status().Status)})
		return
	}
	defer subscription.Close()
	databaseID := server.runtime.Status().Status.DatabaseID
	if err := server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: true, RequestID: request.RequestID, DatabaseID: databaseID, Operation: request.Operation, Stream: protocol.StreamStarted, RegistrationRevision: subscription.RegistrationRevision}); err != nil {
		return
	}
	lastWritten := arguments.AfterRevision
	for {
		select {
		case event, ok := <-subscription.Events:
			if !ok {
				subscription.Events = nil
				continue
			}
			if err := server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: true, RequestID: request.RequestID, DatabaseID: databaseID, Operation: request.Operation, Stream: protocol.StreamEvent, Event: &event}); err != nil {
				return
			}
			lastWritten = event.Revision
		case termination, ok := <-subscription.Ended:
			if !ok {
				return
			}
			if termination.Error != nil {
				termination.Error = termination.Error.Clone()
				termination.Error.LastDeliveredRevision = revisionPointer(lastWritten)
				_ = server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: false, RequestID: request.RequestID, DatabaseID: databaseID, Operation: request.Operation, Stream: protocol.StreamEnded, EndReason: gapdb.WatchEndedByError, Error: termination.Error})
			} else {
				_ = server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: true, RequestID: request.RequestID, DatabaseID: databaseID, Operation: request.Operation, Stream: protocol.StreamEnded, EndReason: termination.Reason})
			}
			return
		case <-server.shutdown:
			_ = server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: true, RequestID: request.RequestID, DatabaseID: databaseID, Operation: request.Operation, Stream: protocol.StreamEnded, EndReason: gapdb.WatchEndedByShutdown})
			return
		}
	}
}

func structuredError(err error, operation protocol.Operation, status gapdb.Status) *gapdb.Error {
	var structured *gapdb.Error
	if errors.As(err, &structured) {
		clone := structured.Clone()
		for _, definition := range gapdb.ErrorDefinitions() {
			if definition.Code == clone.Code {
				clone.SafeActions = append([]gapdb.SafeAction(nil), definition.SafeActions...)
				break
			}
		}
		return clone
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &gapdb.Error{Code: gapdb.CodeDeadlineExceeded, Message: "The request deadline elapsed.", Retry: gapdb.RetryAfterReconcile, Operation: string(operation), Deadline: time.Now().UTC().Format(time.RFC3339Nano), SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionReconcile, gapdb.ActionAbort}, Cause: err}
	}
	correlation := fmt.Sprintf("server-%d", time.Now().UnixNano())
	return &gapdb.Error{Code: gapdb.CodeInternal, Message: "The server failed safely.", Retry: gapdb.RetryAfterOperator, CorrelationID: correlation, Lifecycle: status.Lifecycle, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionAbort}, Cause: err}
}

func (server *Server) augmentAdminError(err error, expectedID string, expectedRevision gapdb.Revision, status gapdb.Status) error {
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeAdminPreconditionFailed {
		return err
	}
	clone := structured.Clone()
	clone.Reason = ""
	if expectedID != status.DatabaseID {
		clone.ExpectedDatabaseID, clone.ActualDatabaseID = expectedID, status.DatabaseID
	} else {
		clone.ExpectedRevision, clone.ActualRevision = revisionPointer(expectedRevision), revisionPointer(status.CurrentRevision)
	}
	return clone
}

func (server *Server) correlation(requestID string) string {
	if requestID != "" {
		return requestID
	}
	return fmt.Sprintf("server-%d", server.requests.Add(1))
}

func revisionPointer(value gapdb.Revision) *gapdb.Revision { return &value }
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func boundedFailures(view admin.RunningView) []string {
	if view.Healthy {
		return []string{}
	}
	return []string{view.DegradedReason}
}
func healthActions(view admin.RunningView) []gapdb.SafeAction {
	if view.Healthy {
		return []gapdb.SafeAction{}
	}
	return []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionAbort}
}
