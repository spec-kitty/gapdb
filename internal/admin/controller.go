package admin

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/engine"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/persist"
)

const defaultMaxAdminPaths = 64

type ControllerConfig struct {
	FS            faultfs.FS
	Directory     string
	State         *engine.DatabaseState
	Manifest      persist.Manifest
	Limits        gapdb.Limits
	WALBufferSize int
	Audit         AuditOptions
	Now           func() time.Time
	Random        io.Reader
	ToolVersion   string
	MaxAdminPaths int
}
type Controller struct {
	mu            sync.Mutex
	fs            faultfs.FS
	directory     string
	state         *engine.DatabaseState
	manifest      persist.Manifest
	limits        gapdb.Limits
	walBufferSize int
	audit         AuditOptions
	now           func() time.Time
	random        io.Reader
	toolVersion   string
	maxAdminPaths int
}

func NewController(config ControllerConfig) (*Controller, error) {
	if config.FS == nil || config.State == nil || config.Directory == "" || config.Manifest.DatabaseID.IsZero() {
		return nil, adminPreconditionError("controller storage, state, directory, and manifest are required")
	}
	if config.Limits.MaxKeyBytes <= 0 {
		config.Limits = gapdb.DefaultOptions().Limits
	}
	if config.WALBufferSize <= 0 {
		config.WALBufferSize = 4096
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.ToolVersion == "" {
		return nil, adminPreconditionError("tool version is required")
	}
	if config.MaxAdminPaths <= 0 {
		config.MaxAdminPaths = defaultMaxAdminPaths
	}
	return &Controller{fs: config.FS, directory: config.Directory, state: config.State, manifest: config.Manifest, limits: config.Limits, walBufferSize: config.WALBufferSize, audit: config.Audit, now: config.Now, random: config.Random, toolVersion: config.ToolVersion, maxAdminPaths: config.MaxAdminPaths}, nil
}
func (controller *Controller) Directory() string { return controller.directory }
func (controller *Controller) Status() RunningView {
	view := Status(controller.state)
	controller.mu.Lock()
	manifest := controller.manifest
	controller.mu.Unlock()
	view.Status.SnapshotRevision = manifest.SnapshotRevision
	view.Status.ActiveWALStart = manifest.WALStartRevision
	view.EffectiveConfig.WALBufferBytes = controller.walBufferSize
	view.EffectiveConfig.AuditMaxLineBytes = normalizedAudit(controller.audit).MaxLineBytes
	view.EffectiveConfig.AuditMaxFileBytes = normalizedAudit(controller.audit).MaxFileBytes
	view.EffectiveConfig.AuditKeepGenerations = normalizedAudit(controller.audit).KeepGenerations
	view.EffectiveConfig.MaxAdminPaths = controller.maxAdminPaths
	return view
}

type SnapshotRequest struct {
	ExpectedDatabaseID         string
	ExpectedManifestGeneration uint64
	ExpectedRevision           gapdb.Revision
	RequestID                  string
	EventID                    string
}

func (controller *Controller) Snapshot(ctx context.Context, request SnapshotRequest) (engine.SnapshotResult, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := validateCorrelation(request.EventID, request.RequestID); err != nil {
		return engine.SnapshotResult{}, err
	}
	if err := controller.check(request.ExpectedDatabaseID, request.ExpectedManifestGeneration, request.ExpectedRevision); err != nil {
		return engine.SnapshotResult{}, err
	}
	var installed persist.SnapshotInstallResult
	result, err := controller.state.Snapshot(ctx, engine.SnapshotInstallerFunc(func(cut engine.SnapshotCut) (engine.CommitLog, error) {
		if cut.Revision != request.ExpectedRevision {
			return nil, adminPreconditionError("snapshot revision changed")
		}
		next, installErr := persist.InstallSnapshotGeneration(persist.SnapshotInstallOptions{FS: controller.fs, Directory: controller.directory, Current: controller.manifest, State: persist.SnapshotState{Revision: cut.Revision, Records: cut.Records}, AsOf: cut.AsOf, Limits: controller.limits, Random: controller.random, WALBufferSize: controller.walBufferSize, Barrier: func() error { return nil }})
		if installErr != nil {
			if operationWasApplied(installErr) {
				installed = next
				if next.Manifest.Generation != 0 {
					controller.manifest = next.Manifest
				}
				controller.state.MarkAdminDegraded()
				return nil, controller.auditFailure("snapshot", request.EventID, request.RequestID, request.ExpectedRevision, cut.Revision, nil, installErr)
			}
			return nil, installErr
		}
		installed = next
		controller.manifest = next.Manifest
		auditErr := controller.auditApplied("snapshot", request.EventID, request.RequestID, request.ExpectedRevision, cut.Revision, []string{next.Manifest.SnapshotFile, next.Manifest.WALFile})
		if auditErr != nil {
			_ = next.WAL.Close()
			return nil, auditErr
		}
		return next.WAL, nil
	}))
	if err != nil && installed.Manifest.Generation != 0 {
		return engine.SnapshotResult{Revision: request.ExpectedRevision, RecordCount: result.RecordCount, Duration: result.Duration}, err
	}
	return result, err
}

type CompactionRequest struct {
	ExpectedDatabaseID         string
	ExpectedManifestGeneration uint64
	ExpectedRevision           gapdb.Revision
	ThroughRevision            gapdb.Revision
	RequestID                  string
	EventID                    string
}
type CompactionView struct {
	BeforeRevision gapdb.Revision `json:"before_revision"`
	AfterRevision  gapdb.Revision `json:"after_revision"`
	Removed        []string       `json:"removed"`
	RemovedCount   int            `json:"removed_count"`
	SkippedCount   int            `json:"skipped_count"`
	PathsTruncated bool           `json:"paths_truncated"`
}

func (controller *Controller) Compact(request CompactionRequest) (CompactionView, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := validateCorrelation(request.EventID, request.RequestID); err != nil {
		return CompactionView{}, err
	}
	if err := controller.check(request.ExpectedDatabaseID, request.ExpectedManifestGeneration, request.ExpectedRevision); err != nil {
		return CompactionView{}, err
	}
	id, _ := persist.ParseDatabaseID(request.ExpectedDatabaseID)
	plan, err := persist.PlanCompaction(controller.fs, controller.directory, controller.manifest, id, request.ThroughRevision)
	if err != nil {
		return CompactionView{}, err
	}
	result, err := persist.RunCompaction(controller.fs, controller.directory, controller.manifest, plan)
	view := CompactionView{BeforeRevision: result.BeforeRevision, AfterRevision: result.AfterRevision, RemovedCount: len(result.Removed), SkippedCount: len(result.Skipped)}
	view.Removed, view.PathsTruncated = boundedPaths(result.Removed, controller.maxAdminPaths)
	if err != nil {
		if operationWasApplied(err) {
			controller.state.MarkAdminDegraded()
			return view, controller.auditFailure("compact", request.EventID, request.RequestID, result.BeforeRevision, result.AfterRevision, view.Removed, err)
		}
		return view, err
	}
	if auditErr := controller.auditApplied("compact", request.EventID, request.RequestID, result.BeforeRevision, result.AfterRevision, view.Removed); auditErr != nil {
		controller.state.MarkAdminDegraded()
		return view, auditErr
	}
	return view, nil
}

type BackupRequest struct {
	ExpectedDatabaseID         string
	ExpectedManifestGeneration uint64
	ExpectedDurableRevision    gapdb.Revision
	Destination                string
	BackupID                   string
	RequestID                  string
	EventID                    string
}

func (controller *Controller) Backup(request BackupRequest) (persist.BackupMetadata, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := validateCorrelation(request.EventID, request.RequestID); err != nil {
		return persist.BackupMetadata{}, err
	}
	if err := controller.check(request.ExpectedDatabaseID, request.ExpectedManifestGeneration, request.ExpectedDurableRevision); err != nil {
		return persist.BackupMetadata{}, err
	}
	var metadata persist.BackupMetadata
	err := controller.state.DurableAdmin(context.Background(), func(cut engine.DurableAdminCut) error {
		if cut.DatabaseID != controller.manifest.DatabaseID || cut.Revision != request.ExpectedDurableRevision {
			return adminPreconditionError("backup authority changed before durable selection")
		}
		var createErr error
		metadata, createErr = persist.CreateBackup(persist.BackupOptions{FS: controller.fs, SourceDirectory: controller.directory, Destination: request.Destination, ExpectedDatabaseID: controller.manifest.DatabaseID, ExpectedDurableRevision: cut.Revision, CreatedAt: controller.now().UTC(), BackupID: request.BackupID, ToolVersion: controller.toolVersion, Limits: controller.limits, Barrier: func() error { return nil }})
		return createErr
	})
	if err != nil {
		if operationWasApplied(err) {
			controller.state.MarkAdminDegraded()
			return metadata, controller.auditFailure("backup", request.EventID, request.RequestID, request.ExpectedDurableRevision, request.ExpectedDurableRevision, []string{filepath.Base(request.Destination)}, err)
		}
		return metadata, err
	}
	if auditErr := controller.auditApplied("backup", request.EventID, request.RequestID, request.ExpectedDurableRevision, request.ExpectedDurableRevision, []string{filepath.Base(request.Destination)}); auditErr != nil {
		controller.state.MarkAdminDegraded()
		return metadata, auditErr
	}
	return metadata, nil
}

type RestoreRequest struct {
	ExpectedDatabaseID         string
	ExpectedManifestGeneration uint64
	ExpectedRevision           gapdb.Revision
	BackupDirectory            string
	Destination                string
	RequestID                  string
	EventID                    string
}

func (controller *Controller) Restore(request RestoreRequest) (persist.BackupMetadata, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := validateCorrelation(request.EventID, request.RequestID); err != nil {
		return persist.BackupMetadata{}, err
	}
	if err := controller.check(request.ExpectedDatabaseID, request.ExpectedManifestGeneration, request.ExpectedRevision); err != nil {
		return persist.BackupMetadata{}, err
	}
	verified, verifyErr := persist.VerifyBackup(controller.fs, request.BackupDirectory, controller.limits)
	if verifyErr != nil {
		return persist.BackupMetadata{}, verifyErr
	}
	if verified.SourceDatabaseID != request.ExpectedDatabaseID {
		return persist.BackupMetadata{}, adminPreconditionError("restored backup database ID does not match")
	}
	metadata, err := persist.RestoreBackup(controller.fs, request.BackupDirectory, request.Destination, controller.limits)
	if err != nil {
		if operationWasApplied(err) {
			controller.state.MarkAdminDegraded()
			return metadata, controller.auditFailure("restore", request.EventID, request.RequestID, request.ExpectedRevision, request.ExpectedRevision, []string{filepath.Base(request.BackupDirectory), filepath.Base(request.Destination)}, err)
		}
		return metadata, err
	}
	if metadata.SourceDatabaseID != request.ExpectedDatabaseID {
		return metadata, adminPreconditionError("restored backup database ID does not match")
	}
	if auditErr := controller.auditApplied("restore", request.EventID, request.RequestID, controller.state.CurrentRevision(), controller.state.CurrentRevision(), []string{filepath.Base(request.BackupDirectory), filepath.Base(request.Destination)}); auditErr != nil {
		controller.state.MarkAdminDegraded()
		return metadata, auditErr
	}
	return metadata, nil
}

func (controller *Controller) ApplyRecovery(proposal RecoveryProposal, options ApplyOptions) (ApplyResult, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	options.Audit = controller.audit
	if options.Timestamp.IsZero() {
		options.Timestamp = controller.now().UTC()
	}
	if options.RequestID == "" {
		options.RequestID = proposal.ID
	}
	result, err := ApplyRecovery(controller.fs, controller.directory, proposal, options)
	if err != nil && operationWasApplied(err) {
		controller.state.MarkAdminDegraded()
		paths := []string{proposal.File}
		return result, controller.auditFailure("recover_apply_quarantine", proposal.ID, options.RequestID, controller.state.CurrentRevision(), controller.state.CurrentRevision(), paths, err)
	}
	return result, err
}
func (controller *Controller) InspectOffline() (Inspection, error) {
	return InspectOffline(controller.fs, controller.directory, controller.limits)
}
func (controller *Controller) VerifyOffline() (Inspection, error) {
	return VerifyOffline(controller.fs, controller.directory, controller.limits)
}

func (controller *Controller) check(databaseID string, generation uint64, revision gapdb.Revision) error {
	status := controller.state.AdminStatus()
	if databaseID != controller.manifest.DatabaseID.String() || generation != controller.manifest.Generation || revision != status.CurrentRevision {
		return adminPreconditionError("database ID, manifest generation, or revision changed")
	}
	return nil
}
func (controller *Controller) auditApplied(operation, eventID, requestID string, before, after gapdb.Revision, paths []string) error {
	return AppendAuditAfterApply(controller.fs, controller.directory, AuditEntry{SchemaVersion: 1, EventID: eventID, Timestamp: controller.now().UTC(), DatabaseID: controller.manifest.DatabaseID, Operation: operation, RequestID: requestID, BeforeRevision: before, AfterRevision: after, Outcome: "applied", Paths: paths, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify}}, controller.audit)
}

func (controller *Controller) auditFailure(operation, eventID, requestID string, before, after gapdb.Revision, paths []string, original error) error {
	var structured *gapdb.Error
	_ = errors.As(original, &structured)
	code := gapdb.ErrorCode("")
	if structured != nil {
		code = structured.Code
	}
	auditErr := AppendAuditAfterApply(controller.fs, controller.directory, AuditEntry{SchemaVersion: 1, EventID: eventID, Timestamp: controller.now().UTC(), DatabaseID: controller.manifest.DatabaseID, Operation: operation, RequestID: requestID, BeforeRevision: before, AfterRevision: after, Outcome: "error", ErrorCode: code, Paths: paths, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify}}, controller.audit)
	if auditErr == nil {
		return original
	}
	var auditStructured *gapdb.Error
	if errors.As(auditErr, &auditStructured) {
		combined := auditStructured.Clone()
		combined.Reason = "original applied failure: " + string(code)
		combined.Cause = errors.Join(original, auditStructured.Cause)
		return combined
	}
	return auditErr
}

func operationWasApplied(err error) bool {
	var structured *gapdb.Error
	return errors.As(err, &structured) && structured.OperationApplied
}
func validateCorrelation(eventID, requestID string) error {
	if eventID == "" || requestID == "" {
		return &gapdb.Error{Code: gapdb.CodeInvalidRequest, Message: "Administrative correlation IDs are required.", Retry: gapdb.RetryNever, Field: "event_id", SafeActions: []gapdb.SafeAction{gapdb.ActionFixRequest, gapdb.ActionAbort}}
	}
	return nil
}
func normalizedAudit(options AuditOptions) AuditOptions {
	if options.MaxLineBytes <= 0 {
		options.MaxLineBytes = 64 << 10
	}
	if options.MaxFileBytes < int64(options.MaxLineBytes) {
		options.MaxFileBytes = 4 << 20
	}
	if options.KeepGenerations < 1 {
		options.KeepGenerations = 1
	}
	return options
}
func boundedPaths(paths []string, maximum int) ([]string, bool) {
	if len(paths) <= maximum {
		return append([]string(nil), paths...), false
	}
	return append([]string(nil), paths[:maximum]...), true
}
