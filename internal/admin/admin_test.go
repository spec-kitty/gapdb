package admin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/engine"
	"gapdb/internal/faultfs"
	"gapdb/internal/persist"
)

func TestAuditCanonicalCRCAndPostApplyFailure(t *testing.T) {
	dir := t.TempDir()
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	entry := AuditEntry{SchemaVersion: 1, EventID: "event-1", Timestamp: time.Unix(123, 0).UTC(), DatabaseID: id, Operation: "compact", RequestID: "request-1", Outcome: "applied", Paths: []string{"wal-00000000000000000001.gdb"}, SafeActions: []gapdb.SafeAction{gapdb.ActionVerify}}
	if err := AppendAudit(faultfs.NewOS(nil), dir, entry, AuditOptions{MaxLineBytes: 4096, MaxFileBytes: 8192, KeepGenerations: 2}); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(filepath.Join(dir, AuditFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(bytes.NewReader(value), 4096); err != nil {
		t.Fatal(err)
	}
	injector := faultfs.NewInjector(9, faultfs.Rule{Point: faultfs.PointAuditSync, Phase: faultfs.Before, Occurrence: 1, Seed: 9, Err: os.ErrPermission})
	err = AppendAuditAfterApply(faultfs.NewOS(injector), dir, entry, AuditOptions{MaxLineBytes: 4096, MaxFileBytes: 8192, KeepGenerations: 2})
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeAuditFailedAfterApply || !structured.OperationApplied {
		t.Fatalf("post apply = %#v", err)
	}
	afterDir := t.TempDir()
	afterInjector := faultfs.NewInjector(10, faultfs.Rule{Point: faultfs.PointAuditSync, Phase: faultfs.After, Occurrence: 1, Seed: 10, Err: os.ErrPermission})
	err = AppendAuditAfterApply(faultfs.NewOS(afterInjector), afterDir, entry, AuditOptions{MaxLineBytes: 4096, MaxFileBytes: 8192, KeepGenerations: 2})
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeAuditFailedAfterApply || !structured.OperationApplied {
		t.Fatalf("post-sync after fault=%#v", err)
	}
	afterValue, readErr := os.ReadFile(filepath.Join(afterDir, AuditFilename))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if verifyErr := VerifyAudit(bytes.NewReader(afterValue), 4096); verifyErr != nil {
		t.Fatalf("durably attempted audit malformed: %v", verifyErr)
	}
}

func TestOfflineInspectTakesOwnerLockBeforeAuthorityReads(t *testing.T) {
	dir := t.TempDir()
	owner, err := persist.AcquireOwner(faultfs.NewOS(nil), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	recorder := &faultfs.Recorder{}
	_, err = InspectOffline(faultfs.NewOS(recorder), dir, gapdb.DefaultOptions().Limits)
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeOwnerExists}) {
		t.Fatalf("inspect = %v", err)
	}
	events := recorder.Events()
	for _, event := range events {
		if event.Point == faultfs.PointOpen {
			t.Fatalf("authority read after lock conflict: %+v", events)
		}
	}
}

func TestRecoveryProposalIsStableAndStaleApplyDoesNotRename(t *testing.T) {
	report := Inspection{DatabaseID: "00112233445566778899aabbccddeeff", ManifestGeneration: 2, CurrentRevision: 4, FindingCode: gapdb.CodeCorruptWAL, FindingFile: "wal-00000000000000000003.gdb", EvidenceSHA256: "abcd", EvidenceSize: 4, EvidenceDevice: 1, EvidenceInode: 1}
	first, err := ProposeQuarantine(report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProposeQuarantine(report)
	if err != nil || first.ID != second.ID {
		t.Fatalf("proposal unstable: %+v %+v %v", first, second, err)
	}
	dir := t.TempDir()
	quarantine := filepath.Join(filepath.Dir(dir), "quarantine")
	recorder := &faultfs.Recorder{}
	_, err = ApplyRecovery(faultfs.NewOS(recorder), dir, first, ApplyOptions{ExpectedDatabaseID: report.DatabaseID, ExpectedManifestGeneration: 3, QuarantineDirectory: quarantine})
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed}) {
		t.Fatalf("stale = %v", err)
	}
	for _, event := range recorder.Events() {
		if event.Point == faultfs.PointBackupRename {
			t.Fatalf("stale proposal renamed: %+v", recorder.Events())
		}
	}
}

func TestRecoveryProposalRequiresCanonicalRecoverAuthority(t *testing.T) {
	base := Inspection{DatabaseID: "00112233445566778899aabbccddeeff", ManifestGeneration: 2, FindingFile: "snapshot-00000000000000000001.gdb", EvidenceSHA256: "abcd", EvidenceSize: 4, EvidenceDevice: 1, EvidenceInode: 1}
	for _, code := range []gapdb.ErrorCode{gapdb.CodeUnknownFormat, gapdb.CodeDatabaseIDMismatch, gapdb.CodeIOError, gapdb.CodeCorruptIdentity, gapdb.CodeRevisionRangeExhausted} {
		report := base
		report.FindingCode = code
		if proposal, err := ProposeQuarantine(report); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRecoveryActionMismatch}) || proposal.ID != "" {
			t.Fatalf("%s proposal = %+v, %v", code, proposal, err)
		}
	}
}

func TestApplyRecoveryReinspectsCurrentFindingAuthority(t *testing.T) {
	dir, id, walName := makeCorruptDatabase(t)
	report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
	proposal, err := ProposeQuarantine(report)
	if err != nil {
		t.Fatal(err)
	}
	proposal.AllowedActions = []gapdb.SafeAction{gapdb.ActionUpgradeGapdb, gapdb.ActionAbort}
	proposal, _ = proposalFromFields(proposal)
	before, err := os.ReadFile(filepath.Join(dir, walName))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyRecovery(faultfs.NewOS(nil), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: report.ManifestGeneration, QuarantineDirectory: filepath.Join(filepath.Dir(dir), "quarantine-authority")})
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRecoveryActionMismatch}) {
		t.Fatalf("apply unauthorized actions = %v", err)
	}
	after, readErr := os.ReadFile(filepath.Join(dir, walName))
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("authority rejection mutated WAL: %v", readErr)
	}
}

func TestInspectProposeAndApplyQuarantinesExactRevalidatedEvidence(t *testing.T) {
	dir, id, walName := makeCorruptDatabase(t)
	report, err := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptWAL}) || report.FindingFile != walName || report.EvidenceSHA256 == "" {
		t.Fatalf("inspection=%+v,%v", report, err)
	}
	proposal, err := ProposeQuarantine(report)
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(filepath.Dir(dir), "quarantine-final")
	result, err := ApplyRecovery(faultfs.NewOS(nil), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: quarantine, RequestID: "request", Timestamp: time.Unix(456, 0).UTC(), Audit: AuditOptions{MaxLineBytes: 4096, MaxFileBytes: 8192, KeepGenerations: 2}})
	if err != nil || !result.OperationApplied {
		t.Fatalf("apply=%+v,%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(quarantine, walName)); err != nil {
		t.Fatalf("quarantine=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, walName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source WAL=%v", err)
	}
	value, err := os.ReadFile(filepath.Join(dir, AuditFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(bytes.NewReader(value), 4096); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRecoveryRejectsEvidenceChangedAfterProposal(t *testing.T) {
	dir, id, walName := makeCorruptDatabase(t)
	report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
	proposal, err := ProposeQuarantine(report)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, walName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{9}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(filepath.Dir(dir), "stale-quarantine")
	_, err = ApplyRecovery(faultfs.NewOS(nil), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: quarantine})
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRecoveryActionMismatch}) {
		t.Fatalf("stale apply=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, walName)); err != nil {
		t.Fatalf("stale apply moved WAL: %v", err)
	}
}

func TestApplyRecoveryBindsCompleteArtifactContentsAndSize(t *testing.T) {
	positions := []int64{1, 64 << 20, (64 << 20) + 8}
	for _, position := range positions {
		t.Run(fmt.Sprintf("offset-%d", position), func(t *testing.T) {
			dir, id, walName := makeCorruptDatabase(t)
			path := filepath.Join(dir, walName)
			if err := os.Truncate(path, (64<<20)+16); err != nil {
				t.Fatal(err)
			}
			report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
			if report.EvidenceSize != (64<<20)+16 || report.EvidenceSHA256 == "" {
				t.Fatalf("evidence = %+v", report)
			}
			proposal, err := ProposeQuarantine(report)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt([]byte{0xa5}, position); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = ApplyRecovery(faultfs.NewOS(nil), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: filepath.Join(filepath.Dir(dir), "exact-quarantine")})
			if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRecoveryActionMismatch}) {
				t.Fatalf("changed evidence applied: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("artifact moved: %v", err)
			}
		})
	}
}

func TestRecoveryDestinationRejectsSymlinksDanglingEntriesAndRenameRace(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string, string, string) string
	}{
		{name: "parent-alias-inside-database", prepare: func(t *testing.T, db, external, file string) string {
			inside := filepath.Join(db, "inside-quarantine")
			if err := os.Mkdir(inside, 0o700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(external, "alias")
			if err := os.Symlink(inside, alias); err != nil {
				t.Fatal(err)
			}
			return alias
		}},
		{name: "dangling-final-entry", prepare: func(t *testing.T, db, external, file string) string {
			q := filepath.Join(external, "quarantine")
			if err := os.Mkdir(q, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(external, "missing"), filepath.Join(q, file)); err != nil {
				t.Fatal(err)
			}
			return q
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, id, walName := makeCorruptDatabase(t)
			report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
			proposal, _ := ProposeQuarantine(report)
			external := t.TempDir()
			q := test.prepare(t, dir, external, walName)
			_, err := ApplyRecovery(faultfs.NewOS(nil), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: q})
			if err == nil {
				t.Fatal("unsafe destination applied")
			}
			if _, err := os.Stat(filepath.Join(dir, walName)); err != nil {
				t.Fatalf("artifact moved: %v", err)
			}
		})
	}
	t.Run("rename-race", func(t *testing.T) {
		dir, id, walName := makeCorruptDatabase(t)
		report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
		proposal, _ := ProposeQuarantine(report)
		q := filepath.Join(t.TempDir(), "q")
		destination := filepath.Join(q, walName)
		injector := faultfs.NewInjector(31, faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.Before, Occurrence: 1, Seed: 31, Observe: func(faultfs.Event) {
			if err := os.WriteFile(destination, []byte("sentinel"), 0o600); err != nil {
				panic(err)
			}
		}})
		_, err := ApplyRecovery(faultfs.NewOS(injector), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: q})
		if err == nil {
			t.Fatal("rename race overwrote destination")
		}
		if got, _ := os.ReadFile(destination); string(got) != "sentinel" {
			t.Fatalf("destination overwritten: %q", got)
		}
		if _, err := os.Stat(filepath.Join(dir, walName)); err != nil {
			t.Fatalf("source moved: %v", err)
		}
	})
}

func TestRecoveryRenameAfterFaultPreservesAppliedEvidence(t *testing.T) {
	dir, id, walName := makeCorruptDatabase(t)
	report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
	proposal, _ := ProposeQuarantine(report)
	q := filepath.Join(t.TempDir(), "q")
	injector := faultfs.NewInjector(72, faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.After, Occurrence: 1, Seed: 72, Err: os.ErrPermission})
	result, err := ApplyRecovery(faultfs.NewOS(injector), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: q})
	var structured *gapdb.Error
	if !errors.As(err, &structured) || !structured.OperationApplied || !result.OperationApplied {
		t.Fatalf("result=%+v err=%#v", result, err)
	}
	if _, err := os.Stat(filepath.Join(q, walName)); err != nil {
		t.Fatalf("quarantined artifact absent: %v", err)
	}
}

func TestRecoveryAnchorsSourceAndQuarantineParentAtRenameBoundary(t *testing.T) {
	for _, attack := range []string{"source-swap", "parent-swap"} {
		t.Run(attack, func(t *testing.T) {
			dir, id, walName := makeCorruptDatabase(t)
			report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
			proposal, _ := ProposeQuarantine(report)
			q := filepath.Join(t.TempDir(), "q")
			if err := os.Mkdir(q, 0o700); err != nil {
				t.Fatal(err)
			}
			injector := faultfs.NewInjector(94, faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.Before, Occurrence: 1, Seed: 94, Observe: func(faultfs.Event) {
				if attack == "source-swap" {
					path := filepath.Join(dir, walName)
					if err := os.Rename(path, path+"-inspected"); err != nil {
						panic(err)
					}
					if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
						panic(err)
					}
					return
				}
				moved := q + "-moved"
				if err := os.Rename(q, moved); err != nil {
					panic(err)
				}
				inside := filepath.Join(dir, "redirect")
				if err := os.Mkdir(inside, 0o700); err != nil {
					panic(err)
				}
				if err := os.Symlink(inside, q); err != nil {
					panic(err)
				}
			}})
			_, err := ApplyRecovery(faultfs.NewOS(injector), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: q})
			if err == nil {
				t.Fatal("identity swap applied")
			}
			if _, statErr := os.Stat(filepath.Join(q, walName)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("redirected quarantine destination: %v", statErr)
			}
		})
	}
}

func TestRecoveryPinsPublishedDirectoryThroughDurabilitySync(t *testing.T) {
	dir, id, walName := makeCorruptDatabase(t)
	report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
	proposal, _ := ProposeQuarantine(report)
	q := filepath.Join(t.TempDir(), "quarantine")
	if err := os.Mkdir(q, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := q + "-moved"
	injector := faultfs.NewInjector(96, faultfs.Rule{Point: faultfs.PointPublicationDirectorySync, Phase: faultfs.Before, Occurrence: 1, Seed: 96, Observe: func(faultfs.Event) {
		if err := os.Rename(q, moved); err != nil {
			panic(err)
		}
		if err := os.Mkdir(q, 0o700); err != nil {
			panic(err)
		}
	}})
	result, err := ApplyRecovery(faultfs.NewOS(injector), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: q})
	var structured *gapdb.Error
	if !errors.As(err, &structured) || !structured.OperationApplied || !result.OperationApplied || result.QuarantinedPath != filepath.Base(moved)+"/"+walName {
		t.Fatalf("result=%+v err=%#v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(moved, walName)); statErr != nil {
		t.Fatalf("actual published artifact missing: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(q, walName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement directory received artifact: %v", statErr)
	}
}

func TestRecoveryPublicationSyncFaultsPreserveStableEvidence(t *testing.T) {
	for _, occurrence := range []uint64{1, 2} {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(fmt.Sprintf("directory-%d-%s", occurrence, phase), func(t *testing.T) {
				dir, id, walName := makeCorruptDatabase(t)
				report, _ := InspectOffline(faultfs.NewOS(nil), dir, gapdb.DefaultOptions().Limits)
				proposal, _ := ProposeQuarantine(report)
				q := filepath.Join(t.TempDir(), "quarantine")
				injector := faultfs.NewInjector(99, faultfs.Rule{Point: faultfs.PointPublicationDirectorySync, Phase: phase, Occurrence: occurrence, Seed: 99, Err: os.ErrPermission})
				result, err := ApplyRecovery(faultfs.NewOS(injector), dir, proposal, ApplyOptions{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, QuarantineDirectory: q})
				var structured *gapdb.Error
				if !errors.As(err, &structured) || !structured.OperationApplied || !result.OperationApplied || result.QuarantinedPath != filepath.Base(q)+"/"+walName {
					t.Fatalf("result=%+v err=%#v", result, err)
				}
				if _, statErr := os.Stat(filepath.Join(q, walName)); statErr != nil {
					t.Fatalf("published result absent: %v", statErr)
				}
			})
		}
	}
}

func TestControllerExposesEffectiveConfigAndAuditsEveryAdministrativeMutation(t *testing.T) {
	controller, state, id := makeControllerFixture(t, faultfs.NewOS(nil))
	defer state.Close(t.Context())
	if _, err := state.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckDurable); err != nil {
		t.Fatal(err)
	}
	view := controller.Status()
	if view.SchemaVersion != 1 || view.EffectiveConfig.Limits.MaxKeyBytes == 0 || view.EffectiveConfig.AuditMaxLineBytes == 0 || view.Status.DatabaseID != id.String() {
		t.Fatalf("status=%+v", view)
	}
	snapshot, err := controller.Snapshot(t.Context(), SnapshotRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedRevision: 1, RequestID: "req-snapshot", EventID: "event-snapshot"})
	if err != nil || snapshot.Revision != 1 {
		t.Fatalf("snapshot=%+v,%v", snapshot, err)
	}
	compaction, err := controller.Compact(CompactionRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, ExpectedRevision: 1, ThroughRevision: 1, RequestID: "req-compact", EventID: "event-compact"})
	if err != nil || compaction.RemovedCount < 2 {
		t.Fatalf("compact=%+v,%v", compaction, err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup")
	backup, err := controller.Backup(BackupRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, ExpectedDurableRevision: 1, Destination: backupPath, BackupID: "backup-id", RequestID: "req-backup", EventID: "event-backup"})
	if err != nil || backup.DurableRevision != 1 {
		t.Fatalf("backup=%+v,%v", backup, err)
	}
	restorePath := filepath.Join(t.TempDir(), "restore")
	if _, err := controller.Restore(RestoreRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, ExpectedRevision: 1, BackupDirectory: backupPath, Destination: restorePath, RequestID: "req-restore", EventID: "event-restore"}); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(filepath.Join(controller.Directory(), AuditFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(bytes.NewReader(value), 64<<10); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"snapshot", "compact", "backup", "restore"} {
		if !bytes.Contains(value, []byte(`"operation":"`+operation+`"`)) {
			t.Errorf("audit missing %s: %s", operation, value)
		}
	}
}

func TestControllerBackupMakesAckMemoryRevisionDurableInsideWriter(t *testing.T) {
	controller, state, id := makeControllerFixture(t, faultfs.NewOS(nil))
	defer state.Close(t.Context())
	if _, err := state.Put(t.Context(), "memory", []byte("value"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if state.DurableThroughRevision() != 0 {
		t.Fatalf("test requires buffered revision, durable=%d", state.DurableThroughRevision())
	}
	destination := filepath.Join(t.TempDir(), "backup")
	metadata, err := controller.Backup(BackupRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedDurableRevision: 1, Destination: destination, BackupID: "memory-cut", RequestID: "request", EventID: "event"})
	if err != nil || metadata.DurableRevision != 1 || state.DurableThroughRevision() != 1 {
		t.Fatalf("metadata=%+v durable=%d err=%v", metadata, state.DurableThroughRevision(), err)
	}
}

func TestControllerAppliedStorageFailuresAuditDegradeAndPreserveEvidence(t *testing.T) {
	t.Run("backup-rename-after", func(t *testing.T) {
		injector := faultfs.NewInjector(91, faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.After, Occurrence: 1, Seed: 91, Err: os.ErrPermission})
		controller, state, id := makeControllerFixture(t, faultfs.NewOS(injector))
		defer state.Close(t.Context())
		if _, err := state.Put(t.Context(), "k", []byte("v"), nil, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
		result, err := controller.Backup(BackupRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedDurableRevision: 1, Destination: filepath.Join(t.TempDir(), "backup"), BackupID: "backup", RequestID: "request", EventID: "event"})
		var structured *gapdb.Error
		if !errors.As(err, &structured) || !structured.OperationApplied || result.DurableRevision != 1 || state.Lifecycle() != gapdb.LifecycleDegradedReadOnly {
			t.Fatalf("result=%+v lifecycle=%s err=%#v", result, state.Lifecycle(), err)
		}
		audit, readErr := os.ReadFile(filepath.Join(controller.Directory(), AuditFilename))
		if readErr != nil || !bytes.Contains(audit, []byte(`"outcome":"error"`)) || !bytes.Contains(audit, []byte(`"operation":"backup"`)) {
			t.Fatalf("audit=%s readErr=%v", audit, readErr)
		}
	})
	t.Run("compaction-partial", func(t *testing.T) {
		injector := faultfs.NewInjector(92, faultfs.Rule{Point: faultfs.PointCompactionRemove, Phase: faultfs.Before, Occurrence: 2, Seed: 92, Err: os.ErrPermission})
		controller, state, id := makeControllerFixture(t, faultfs.NewOS(injector))
		defer state.Close(t.Context())
		if _, err := state.Put(t.Context(), "k", []byte("v"), nil, gapdb.AckDurable); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Snapshot(t.Context(), SnapshotRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedRevision: 1, RequestID: "snapshot-request", EventID: "snapshot-event"}); err != nil {
			t.Fatal(err)
		}
		result, err := controller.Compact(CompactionRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, ExpectedRevision: 1, ThroughRevision: 1, RequestID: "request", EventID: "event"})
		var structured *gapdb.Error
		if !errors.As(err, &structured) || !structured.OperationApplied || result.RemovedCount != 1 || state.Lifecycle() != gapdb.LifecycleDegradedReadOnly {
			t.Fatalf("result=%+v lifecycle=%s err=%#v", result, state.Lifecycle(), err)
		}
	})
}

func TestControllerRetainsOriginalAppliedFailureWhenItsErrorAuditAlsoFails(t *testing.T) {
	injector := faultfs.NewInjector(95,
		faultfs.Rule{Point: faultfs.PointBackupRename, Phase: faultfs.After, Occurrence: 1, Seed: 95, Err: os.ErrPermission},
		faultfs.Rule{Point: faultfs.PointAuditSync, Phase: faultfs.Before, Occurrence: 1, Seed: 95, Err: errors.New("no space")},
	)
	controller, state, id := makeControllerFixture(t, faultfs.NewOS(injector))
	defer state.Close(t.Context())
	if _, err := state.Put(t.Context(), "k", []byte("v"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	result, err := controller.Backup(BackupRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedDurableRevision: 1, Destination: filepath.Join(t.TempDir(), "backup"), BackupID: "backup", RequestID: "request", EventID: "event"})
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeAuditFailedAfterApply || !structured.OperationApplied || result.DurableRevision != 1 || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeIOError}) {
		t.Fatalf("result=%+v err=%#v", result, err)
	}
}

func TestControllerRestoreRequiresExactAuthorityTuple(t *testing.T) {
	controller, state, id := makeControllerFixture(t, faultfs.NewOS(nil))
	defer state.Close(t.Context())
	for _, request := range []RestoreRequest{
		{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 0, ExpectedRevision: 0},
		{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedRevision: 1},
		{ExpectedDatabaseID: "ffeeddccbbaa99887766554433221100", ExpectedManifestGeneration: 1, ExpectedRevision: 0},
	} {
		request.BackupDirectory = t.TempDir()
		request.Destination = filepath.Join(t.TempDir(), "restore")
		request.RequestID, request.EventID = "request", "event"
		if _, err := controller.Restore(request); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed}) {
			t.Fatalf("stale request %+v = %v", request, err)
		}
	}
}

func TestControllerAuditFailureAfterSnapshotDegradesAndPreservesAppliedResult(t *testing.T) {
	for index, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
		t.Run(string(phase), func(t *testing.T) {
			seed := uint64(51 + index)
			injector := faultfs.NewInjector(seed, faultfs.Rule{Point: faultfs.PointAuditSync, Phase: phase, Occurrence: 1, Seed: seed, Err: os.ErrPermission})
			controller, state, id := makeControllerFixture(t, faultfs.NewOS(injector))
			defer state.Close(t.Context())
			if _, err := state.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckDurable); err != nil {
				t.Fatal(err)
			}
			result, err := controller.Snapshot(t.Context(), SnapshotRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedRevision: 1, RequestID: "request", EventID: "event"})
			var structured *gapdb.Error
			if !errors.As(err, &structured) || structured.Code != gapdb.CodeAuditFailedAfterApply || !structured.OperationApplied || result.Revision != 1 || state.Lifecycle() != gapdb.LifecycleDegradedReadOnly {
				t.Fatalf("result=%+v lifecycle=%s err=%#v", result, state.Lifecycle(), err)
			}
		})
	}
}

func TestControllerAuditFailuresAfterOtherAppliedOperationsPreserveResults(t *testing.T) {
	for index, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
		t.Run("compaction-"+string(phase), func(t *testing.T) {
			seed := uint64(61 + index)
			injector := faultfs.NewInjector(seed, faultfs.Rule{Point: faultfs.PointAuditSync, Phase: phase, Occurrence: 2, Seed: seed, Err: os.ErrPermission})
			controller, state, id := makeControllerFixture(t, faultfs.NewOS(injector))
			defer state.Close(t.Context())
			if _, err := state.Put(t.Context(), "k", []byte("v"), nil, gapdb.AckDurable); err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Snapshot(t.Context(), SnapshotRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedRevision: 1, RequestID: "snapshot-request", EventID: "snapshot-event"}); err != nil {
				t.Fatal(err)
			}
			result, err := controller.Compact(CompactionRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 2, ExpectedRevision: 1, ThroughRevision: 1, RequestID: "compact-request", EventID: "compact-event"})
			assertAuditAppliedError(t, err, state)
			if result.RemovedCount < 2 {
				t.Fatalf("result not preserved: %+v", result)
			}
		})
	}
	for index, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
		t.Run("backup-"+string(phase), func(t *testing.T) {
			seed := uint64(63 + index)
			injector := faultfs.NewInjector(seed, faultfs.Rule{Point: faultfs.PointAuditSync, Phase: phase, Occurrence: 1, Seed: seed, Err: os.ErrPermission})
			controller, state, id := makeControllerFixture(t, faultfs.NewOS(injector))
			defer state.Close(t.Context())
			if _, err := state.Put(t.Context(), "k", []byte("v"), nil, gapdb.AckDurable); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "backup")
			result, err := controller.Backup(BackupRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedDurableRevision: 1, Destination: destination, BackupID: "backup", RequestID: "request", EventID: "event"})
			assertAuditAppliedError(t, err, state)
			if result.DurableRevision != 1 {
				t.Fatalf("result not preserved: %+v", result)
			}
			if _, statErr := os.Stat(destination); statErr != nil {
				t.Fatalf("backup absent: %v", statErr)
			}
		})
	}
	for index, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
		t.Run("restore-"+string(phase), func(t *testing.T) {
			seed := uint64(65 + index)
			injector := faultfs.NewInjector(seed, faultfs.Rule{Point: faultfs.PointAuditSync, Phase: phase, Occurrence: 1, Seed: seed, Err: os.ErrPermission})
			controller, state, id := makeControllerFixture(t, faultfs.NewOS(injector))
			defer state.Close(t.Context())
			if _, err := state.Put(t.Context(), "k", []byte("v"), nil, gapdb.AckDurable); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(t.TempDir(), "backup")
			if _, err := persist.CreateBackup(backupOptionsForAdmin(controller.Directory(), backup, id, 1)); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "restore")
			result, err := controller.Restore(RestoreRequest{ExpectedDatabaseID: id.String(), ExpectedManifestGeneration: 1, ExpectedRevision: 1, BackupDirectory: backup, Destination: destination, RequestID: "request", EventID: "event"})
			assertAuditAppliedError(t, err, state)
			if result.DurableRevision != 1 {
				t.Fatalf("result not preserved: %+v", result)
			}
			if _, statErr := os.Stat(destination); statErr != nil {
				t.Fatalf("restore absent: %v", statErr)
			}
		})
	}
}

func assertAuditAppliedError(t *testing.T, err error, state *engine.DatabaseState) {
	t.Helper()
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeAuditFailedAfterApply || !structured.OperationApplied || state.Lifecycle() != gapdb.LifecycleDegradedReadOnly {
		t.Fatalf("lifecycle=%s err=%#v", state.Lifecycle(), err)
	}
}
func backupOptionsForAdmin(source, destination string, id persist.DatabaseID, revision gapdb.Revision) persist.BackupOptions {
	return persist.BackupOptions{FS: faultfs.NewOS(nil), SourceDirectory: source, Destination: destination, ExpectedDatabaseID: id, ExpectedDurableRevision: revision, CreatedAt: time.Unix(123, 0).UTC(), BackupID: "prebuilt", ToolVersion: "test", Limits: gapdb.DefaultOptions().Limits, Barrier: func() error { return nil }}
}

func makeControllerFixture(t *testing.T, fsys faultfs.FS) (*Controller, *engine.DatabaseState, persist.DatabaseID) {
	t.Helper()
	dir := t.TempDir()
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	identity, _ := persist.EncodeIdentity(persist.Identity{DatabaseID: id, ReservedRevisionEnd: 10, Generation: 1})
	if err := os.WriteFile(filepath.Join(dir, persist.IdentityFilename), identity, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, digest, err := persist.EncodeSnapshot(id, persist.SnapshotState{}, time.Unix(0, 0), gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	snapshotName := "snapshot-00000000000000000000.gdb"
	walName := "wal-00000000000000000001.gdb"
	if err := os.WriteFile(filepath.Join(dir, snapshotName), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	wal, err := persist.CreateWAL(fsys, filepath.Join(dir, walName), persist.WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1}, 4096, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	manifest := persist.Manifest{DatabaseID: id, Generation: 1, SnapshotFile: snapshotName, SnapshotRevision: 0, SnapshotSHA256: fmt.Sprintf("%x", digest[:]), WALFile: walName, WALStartRevision: 1}
	encoded, _ := persist.EncodeManifest(manifest)
	if err := os.WriteFile(filepath.Join(dir, persist.ManifestFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	allocator, err := engine.NewRangeAllocator(0, persist.RevisionRange{First: 1, End: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.New(engine.Config{Limits: gapdb.DefaultOptions().Limits, Log: wal, Allocator: allocator, DatabaseID: id, SnapshotRevision: 0, ReservedRevisionEnd: 10, ActiveWALStart: 1})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(ControllerConfig{FS: fsys, Directory: dir, State: state, Manifest: manifest, Limits: gapdb.DefaultOptions().Limits, WALBufferSize: 4096, Audit: AuditOptions{MaxLineBytes: 64 << 10, MaxFileBytes: 4 << 20, KeepGenerations: 2}, Now: func() time.Time { return time.Unix(123, 0).UTC() }, ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return controller, state, id
}

func makeCorruptDatabase(t *testing.T) (string, persist.DatabaseID, string) {
	t.Helper()
	dir := t.TempDir()
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	identity, _ := persist.EncodeIdentity(persist.Identity{DatabaseID: id, ReservedRevisionEnd: 10, Generation: 1})
	if err := os.WriteFile(filepath.Join(dir, persist.IdentityFilename), identity, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, digest, err := persist.EncodeSnapshot(id, persist.SnapshotState{}, time.Unix(0, 0), gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	snapshotName := "snapshot-00000000000000000000.gdb"
	walName := "wal-00000000000000000001.gdb"
	if err := os.WriteFile(filepath.Join(dir, snapshotName), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	wal, err := persist.CreateWAL(faultfs.NewOS(nil), filepath.Join(dir, walName), persist.WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 2}, 128, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, walName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := persist.Manifest{DatabaseID: id, Generation: 2, SnapshotFile: snapshotName, SnapshotRevision: 0, SnapshotSHA256: fmt.Sprintf("%x", digest[:]), WALFile: walName, WALStartRevision: 1}
	encoded, err := persist.EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, persist.ManifestFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, id, walName
}
