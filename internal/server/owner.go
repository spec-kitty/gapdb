package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/admin"
	"github.com/spec-kitty/gapdb/internal/engine"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/owner"
	"github.com/spec-kitty/gapdb/internal/persist"
)

const (
	defaultRevisionReservation = 1 << 20
	defaultWALBufferBytes      = 4096
)

type openedRuntime struct {
	runtime            *owner.Runtime
	manifestGeneration uint64
	wal                *persist.WAL
}

func openRuntime(config Config) (openedRuntime, error) {
	info, statErr := os.Stat(config.Directory)
	if os.IsNotExist(statErr) {
		if err := os.MkdirAll(config.Directory, 0o700); err != nil {
			return openedRuntime{}, permissionError(config.Directory, "mkdir", err)
		}
		if err := os.Chmod(config.Directory, 0o700); err != nil {
			return openedRuntime{}, permissionError(config.Directory, "chmod", err)
		}
	} else if statErr != nil {
		return openedRuntime{}, permissionError(config.Directory, "stat", statErr)
	} else if !info.IsDir() {
		return openedRuntime{}, permissionError(config.Directory, "open", fmt.Errorf("database path is not a directory"))
	}
	fsys := config.FS
	if fsys == nil {
		fsys = faultfs.NewOS(nil)
	}
	identityPath := filepath.Join(config.Directory, persist.IdentityFilename)
	if _, err := os.Stat(identityPath); err == nil {
		opened, recoverErr := recoverRuntime(config, fsys)
		return validateOpenedDirectory(config, opened, recoverErr)
	} else if !os.IsNotExist(err) {
		return openedRuntime{}, permissionError(identityPath, "stat", err)
	}
	opened, initializeErr := initializeRuntime(config, fsys)
	return validateOpenedDirectory(config, opened, initializeErr)
}

func validateOpenedDirectory(config Config, opened openedRuntime, err error) (openedRuntime, error) {
	if err != nil {
		return openedRuntime{}, err
	}
	if config.BeforeLockModeCheck != nil {
		config.BeforeLockModeCheck()
	}
	if err := secureHeldLock(opened.runtime, config.Directory); err != nil {
		_ = opened.runtime.Close(context.Background())
		_ = opened.wal.Close()
		return openedRuntime{}, err
	}
	return opened, nil
}

func secureHeldLock(runtime *owner.Runtime, directory string) error {
	path := filepath.Join(directory, "LOCK")
	if err := runtime.SecureOwnerLock(); err != nil {
		return permissionError(path, "lock_identity", err)
	}
	return nil
}

func initializeRuntime(config Config, fsys faultfs.FS) (openedRuntime, error) {
	ownership, err := persist.AcquireOwner(fsys, config.Directory)
	if err != nil {
		return openedRuntime{}, err
	}
	success := false
	defer func() {
		if !success {
			_ = ownership.Close()
		}
	}()
	// Recheck after acquiring ownership so two fresh starters cannot both
	// initialize authority.
	if _, err := os.Stat(filepath.Join(config.Directory, persist.IdentityFilename)); err == nil {
		_ = ownership.Close()
		return recoverRuntime(config, fsys)
	} else if !os.IsNotExist(err) {
		return openedRuntime{}, permissionError(config.Directory, "stat", err)
	}
	entries, err := os.ReadDir(config.Directory)
	if err != nil {
		return openedRuntime{}, permissionError(config.Directory, "read_directory", err)
	}
	staleSocketName := ""
	if filepath.Clean(filepath.Dir(config.SocketPath)) == config.Directory {
		staleSocketName = filepath.Base(config.SocketPath)
	}
	for _, entry := range entries {
		if entry.Name() == "LOCK" || entry.Name() == staleSocketName {
			continue
		}
		proposal := false
		return openedRuntime{}, &gapdb.Error{Code: gapdb.CodeRecoveryRequired, Message: "Storage artifacts exist without database identity authority.", Retry: gapdb.RetryAfterOperator, DetectedCorruption: "identity_missing_with_existing_artifacts", ProposalAvailable: &proposal, SafeActions: []gapdb.SafeAction{gapdb.ActionRecoverPropose, gapdb.ActionRestoreBackup, gapdb.ActionAbort}}
	}
	identity, created, err := persist.LoadOrCreateIdentity(fsys, config.Directory, rand.Reader)
	if err != nil || !created {
		return openedRuntime{}, err
	}
	identity, allocation, err := persist.ReserveRevisions(fsys, config.Directory, identity, 0, defaultRevisionReservation, rand.Reader)
	if err != nil {
		return openedRuntime{}, err
	}
	snapshotName := "snapshot-00000000000000000000.gdb"
	snapshot, digest, err := persist.EncodeSnapshot(identity.DatabaseID, persist.SnapshotState{}, time.Unix(0, 0).UTC(), config.Options.Limits)
	if err != nil {
		return openedRuntime{}, err
	}
	if err := createSyncedFile(filepath.Join(config.Directory, snapshotName), snapshot); err != nil {
		return openedRuntime{}, err
	}
	walName := "wal-00000000000000000001.gdb"
	wal, err := persist.CreateWAL(fsys, filepath.Join(config.Directory, walName), persist.WALHeader{DatabaseID: identity.DatabaseID, FirstRevision: 1, Generation: 1}, defaultWALBufferBytes, config.Options.Limits)
	if err != nil {
		return openedRuntime{}, err
	}
	manifest := persist.Manifest{DatabaseID: identity.DatabaseID, Generation: 1, SnapshotFile: snapshotName, SnapshotRevision: 0, SnapshotSHA256: fmt.Sprintf("%x", digest[:]), WALFile: walName, WALStartRevision: 1}
	if err := persist.InstallManifest(fsys, config.Directory, manifest, 0, rand.Reader); err != nil {
		_ = wal.Close()
		return openedRuntime{}, err
	}
	allocator, reserve := runtimeAllocator(fsys, config.Directory, identity, 0, allocation)
	runtime, err := owner.Open(owner.OpenConfig{
		Engine:    engine.Config{Limits: config.Options.Limits, QueueCapacity: config.QueueCapacity, Log: wal, Allocator: allocator, DatabaseID: identity.DatabaseID, ReservedRevisionEnd: identity.ReservedRevisionEnd, ActiveWALStart: 1, Faults: fsys},
		Admin:     admin.ControllerConfig{FS: fsys, Directory: config.Directory, Manifest: manifest, Limits: config.Options.Limits, WALBufferSize: defaultWALBufferBytes, ToolVersion: config.ToolVersion},
		Ownership: ownership,
	})
	if err != nil {
		_ = wal.Close()
		return openedRuntime{}, err
	}
	_ = reserve
	success = true
	return openedRuntime{runtime: runtime, manifestGeneration: manifest.Generation, wal: wal}, nil
}

func recoverRuntime(config Config, fsys faultfs.FS) (openedRuntime, error) {
	recovered, err := persist.RecoverDatabase(fsys, config.Directory, persist.RecoveryOptions{
		Limits: config.Options.Limits, ReservationSize: defaultRevisionReservation,
		SnapshotLoader: persist.SnapshotFileStore{Limits: config.Options.Limits},
		AuditTail: func(tail persist.TailTruncation) error {
			identity, readErr := persist.ReadIdentity(fsys, config.Directory)
			if readErr != nil {
				return readErr
			}
			return admin.AppendAudit(fsys, config.Directory, admin.AuditEntry{SchemaVersion: 1, EventID: fmt.Sprintf("startup-tail-%d", tail.OldSize), Timestamp: time.Now().UTC(), DatabaseID: identity.DatabaseID, Operation: "startup_tail_truncate", RequestID: "startup", BeforeRevision: tail.LastCompleteRevision, AfterRevision: tail.LastCompleteRevision, Outcome: "applied", Paths: []string{tail.File}, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify}}, admin.AuditOptions{})
		},
	})
	if err != nil {
		return openedRuntime{}, err
	}
	success := false
	defer func() {
		if !success {
			_ = recovered.Owner.Close()
		}
	}()
	records := replayRecords(recovered.Snapshot.Records, recovered.PostSnapshotCommits)
	wal, err := persist.OpenWALForAppend(fsys, filepath.Join(config.Directory, recovered.Manifest.WALFile), persist.WALHeader{DatabaseID: recovered.Identity.DatabaseID, FirstRevision: recovered.Manifest.WALStartRevision, Generation: recovered.Manifest.Generation}, recovered.CurrentRevision, recovered.DurableThroughRevision, defaultWALBufferBytes, config.Options.Limits)
	if err != nil {
		return openedRuntime{}, err
	}
	allocator, _ := runtimeAllocator(fsys, config.Directory, recovered.Identity, recovered.CurrentRevision, recovered.Allocation)
	runtime, err := owner.Open(owner.OpenConfig{
		Engine:    engine.Config{Limits: config.Options.Limits, QueueCapacity: config.QueueCapacity, Log: wal, Allocator: allocator, Records: records, CurrentRevision: recovered.CurrentRevision, DurableThroughRevision: recovered.DurableThroughRevision, DatabaseID: recovered.Identity.DatabaseID, SnapshotRevision: recovered.Manifest.SnapshotRevision, RecoveredCommits: recovered.PostSnapshotCommits, ReservedRevisionEnd: recovered.Identity.ReservedRevisionEnd, ActiveWALStart: recovered.Manifest.WALStartRevision, Faults: fsys},
		Admin:     admin.ControllerConfig{FS: fsys, Directory: config.Directory, Manifest: recovered.Manifest, Limits: config.Options.Limits, WALBufferSize: defaultWALBufferBytes, ToolVersion: config.ToolVersion},
		Ownership: recovered.Owner,
	})
	if err != nil {
		_ = wal.Close()
		return openedRuntime{}, err
	}
	success = true
	return openedRuntime{runtime: runtime, manifestGeneration: recovered.Manifest.Generation, wal: wal}, nil
}

func runtimeAllocator(fsys faultfs.FS, directory string, identity persist.Identity, current gapdb.Revision, allocation persist.RevisionRange) (*engine.RangeAllocator, engine.ReserveFunc) {
	var mu sync.Mutex
	reserve := func(after gapdb.Revision) (persist.RevisionRange, error) {
		mu.Lock()
		defer mu.Unlock()
		updated, next, err := persist.ReserveRevisions(fsys, directory, identity, after, defaultRevisionReservation, rand.Reader)
		if err == nil {
			identity = updated
		}
		return next, err
	}
	allocator, _ := engine.NewRangeAllocator(current, allocation, reserve)
	return allocator, reserve
}

func replayRecords(snapshot []gapdb.Record, commits []persist.CommitFrame) []gapdb.Record {
	records := make(map[string]gapdb.Record, len(snapshot))
	for _, record := range snapshot {
		records[record.Key] = record.Clone()
	}
	for _, commit := range commits {
		for _, effect := range commit.Effects {
			switch effect.Kind {
			case gapdb.ChangePut:
				records[effect.Key] = gapdb.NewRecord(effect.Key, effect.Value, commit.Revision, effect.ExpiresAt)
			case gapdb.ChangeDelete, gapdb.ChangeExpire:
				delete(records, effect.Key)
			}
		}
	}
	result := make([]gapdb.Record, 0, len(records))
	for _, record := range records {
		result = append(result, record)
	}
	return result
}

func createSyncedFile(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(value); err == nil {
		err = file.Sync()
	}
	return errorsJoin(err, file.Close())
}

func errorsJoin(left, right error) error {
	if left != nil {
		return left
	}
	return right
}

func permissionError(path, operation string, cause error) error {
	return &gapdb.Error{Code: gapdb.CodePermissionDenied, Message: "The owner cannot access the configured path.", Retry: gapdb.RetryAfterOperator, Path: filepath.Clean(path), Operation: operation, SafeActions: []gapdb.SafeAction{gapdb.ActionFixPermissions, gapdb.ActionAbort}, Cause: cause}
}
