package persist

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
)

type CompactionPlan struct {
	DatabaseID      DatabaseID
	Generation      uint64
	ThroughRevision gapdb.Revision
	Remove          []string
	Skipped         []string
}

type CompactionResult struct {
	BeforeRevision gapdb.Revision
	AfterRevision  gapdb.Revision
	Removed        []string
	Skipped        []string
}

func PlanCompaction(fsys faultfs.FS, directory string, active Manifest, expectedID DatabaseID, through gapdb.Revision) (CompactionPlan, error) {
	if expectedID.IsZero() || expectedID != active.DatabaseID {
		return CompactionPlan{}, adminPrecondition("database ID does not match active authority")
	}
	if through > active.SnapshotRevision {
		return CompactionPlan{}, &gapdb.Error{Code: gapdb.CodeCompactionNotSafe, Message: "Compaction is not safe at the requested revision.", Retry: gapdb.RetryAfterReconcile, Reason: "through_revision exceeds the active snapshot revision", SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionCreateSnapshot, gapdb.ActionAbort}}
	}
	entries, err := fsys.ReadDir(faultfs.PointReadDir, directory)
	if err != nil {
		return CompactionPlan{}, ioFailure("compaction_list", filepath.Base(directory), err)
	}
	plan := CompactionPlan{DatabaseID: expectedID, Generation: active.Generation, ThroughRevision: through}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == active.SnapshotFile || name == active.WALFile || isProtectedStorageName(name) {
			plan.Skipped = append(plan.Skipped, name)
			continue
		}
		revision, known := generationFileRevision(name)
		if !known || revision > through {
			plan.Skipped = append(plan.Skipped, name)
			continue
		}
		plan.Remove = append(plan.Remove, name)
	}
	sort.Strings(plan.Remove)
	sort.Strings(plan.Skipped)
	return plan, nil
}

func RunCompaction(fsys faultfs.FS, directory string, active Manifest, plan CompactionPlan) (CompactionResult, error) {
	if plan.DatabaseID != active.DatabaseID || plan.Generation != active.Generation || plan.ThroughRevision > active.SnapshotRevision {
		return CompactionResult{}, adminPrecondition("compaction plan is stale")
	}
	result := CompactionResult{BeforeRevision: active.SnapshotRevision, AfterRevision: active.SnapshotRevision, Skipped: append([]string(nil), plan.Skipped...)}
	for _, name := range plan.Remove {
		revision, known := generationFileRevision(name)
		if !known || revision > plan.ThroughRevision || name == active.SnapshotFile || name == active.WALFile || filepath.Base(name) != name {
			return result, adminPrecondition("compaction plan contains an unsafe target")
		}
		if err := fsys.Remove(faultfs.PointCompactionRemove, filepath.Join(directory, name)); err != nil {
			failure := ioFailure("compaction_remove", name, err).(*gapdb.Error)
			failure.OperationApplied = len(result.Removed) != 0 || faultfs.FailedAfter(err)
			return result, failure
		}
		result.Removed = append(result.Removed, name)
	}
	if len(result.Removed) != 0 {
		if err := fsys.SyncDir(faultfs.PointCompactionDirectorySync, directory); err != nil {
			return result, ioFailureApplied("compaction_directory_sync", filepath.Base(directory), err)
		}
	}
	return result, nil
}

func generationFileRevision(name string) (gapdb.Revision, bool) {
	var prefix string
	switch {
	case strings.HasPrefix(name, "snapshot-"):
		prefix = "snapshot-"
	case strings.HasPrefix(name, "wal-"):
		prefix = "wal-"
	default:
		return 0, false
	}
	if len(name) != len(prefix)+20+len(".gdb") || !strings.HasSuffix(name, ".gdb") {
		return 0, false
	}
	value := name[len(prefix) : len(prefix)+20]
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || fmt.Sprintf("%020d", parsed) != value {
		return 0, false
	}
	return gapdb.Revision(parsed), true
}

func isProtectedStorageName(name string) bool {
	return name == IdentityFilename || name == ManifestFilename || name == "LOCK" || name == "audit.jsonl" || strings.HasPrefix(name, "audit.jsonl.") || strings.HasSuffix(name, ".sock") || strings.Contains(name, ".tmp-")
}

func adminPrecondition(reason string) error {
	return &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed, Message: "Administrative preconditions did not match.", Retry: gapdb.RetryAfterReconcile, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionRebuildRequest, gapdb.ActionAbort}}
}
