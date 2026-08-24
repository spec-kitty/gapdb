package persist

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
)

func TestPlanAndRunCompactionOnlyRemovesKnownSupersededFiles(t *testing.T) {
	dir := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	active := Manifest{DatabaseID: id, Generation: 3, SnapshotFile: "snapshot-00000000000000000004.gdb", SnapshotRevision: 4, SnapshotSHA256: string(make([]byte, 64)), WALFile: "wal-00000000000000000005.gdb", WALStartRevision: 5}
	for _, name := range []string{"snapshot-00000000000000000002.gdb", "wal-00000000000000000003.gdb", active.SnapshotFile, active.WALFile, "CURRENT", "IDENTITY", "audit.jsonl", "junk", "snapshot-1.gdb", "CURRENT.tmp-x"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PlanCompaction(faultfs.NewOS(nil), dir, active, id, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Remove) != 2 || len(plan.Skipped) == 0 {
		t.Fatalf("plan = %+v", plan)
	}
	result, err := RunCompaction(faultfs.NewOS(nil), dir, active, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 2 {
		t.Fatalf("result = %+v", result)
	}
	for _, name := range []string{active.SnapshotFile, active.WALFile, "CURRENT", "IDENTITY", "audit.jsonl", "junk"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("protected %s: %v", name, err)
		}
	}
}

func TestCompactionPreconditionsAndPartialApplyEvidence(t *testing.T) {
	dir := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	active := Manifest{DatabaseID: id, Generation: 3, SnapshotFile: "snapshot-00000000000000000004.gdb", SnapshotRevision: 4, SnapshotSHA256: string(make([]byte, 64)), WALFile: "wal-00000000000000000005.gdb", WALStartRevision: 5}
	if _, err := PlanCompaction(faultfs.NewOS(nil), dir, active, DatabaseID{}, 4); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed}) {
		t.Fatalf("mismatch = %v", err)
	}
	if _, err := PlanCompaction(faultfs.NewOS(nil), dir, active, id, 5); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCompactionNotSafe}) {
		t.Fatalf("unsafe = %v", err)
	}
	name := "snapshot-00000000000000000002.gdb"
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCompaction(faultfs.NewOS(nil), dir, active, id, 4)
	if err != nil {
		t.Fatal(err)
	}
	injector := faultfs.NewInjector(7, faultfs.Rule{Point: faultfs.PointCompactionDirectorySync, Phase: faultfs.Before, Occurrence: 1, Seed: 7, Err: os.ErrPermission})
	_, err = RunCompaction(faultfs.NewOS(injector), dir, active, plan)
	var structured *gapdb.Error
	if !errors.As(err, &structured) || !structured.OperationApplied {
		t.Fatalf("sync failure = %#v", err)
	}
}

func TestCompactionRemoveAfterFaultReportsAppliedAndProtectsAuthority(t *testing.T) {
	dir := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	active := Manifest{DatabaseID: id, Generation: 3, SnapshotFile: "snapshot-00000000000000000004.gdb", SnapshotRevision: 4, SnapshotSHA256: string(make([]byte, 64)), WALFile: "wal-00000000000000000005.gdb", WALStartRevision: 5}
	old := "snapshot-00000000000000000002.gdb"
	for _, name := range []string{old, active.SnapshotFile, active.WALFile} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PlanCompaction(faultfs.NewOS(nil), dir, active, id, 4)
	if err != nil {
		t.Fatal(err)
	}
	injector := faultfs.NewInjector(71, faultfs.Rule{Point: faultfs.PointCompactionRemove, Phase: faultfs.After, Occurrence: 1, Seed: 71, Err: os.ErrPermission})
	result, err := RunCompaction(faultfs.NewOS(injector), dir, active, plan)
	var structured *gapdb.Error
	if !errors.As(err, &structured) || !structured.OperationApplied {
		t.Fatalf("result=%+v err=%#v", result, err)
	}
	for _, name := range []string{active.SnapshotFile, active.WALFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("active removed: %s %v", name, err)
		}
	}
}
