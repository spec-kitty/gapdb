---
affected_files: []
cycle_number: 3
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/admin -run '^TestReviewerRecoveryPinsPublishedDirectoryThroughSync$' -count=1 -v
reviewed_at: '2026-08-23T18:52:53Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP05
---

# WP05 Cycle 3 Re-review

Verdict: changes requested.

## Closure evidence

- Anchored final rename now rejects source-inode replacement and source/destination parent replacement before publication for recovery, backup, and restore.
- `Controller.Backup` now crosses the real sole-writer WAL barrier; an `AckMemory` revision is flushed/synced and selected consistently before copying while mutation execution remains paused.
- Applied backup, compaction, restore, recovery, and snapshot failures now preserve results, record an error-outcome audit when possible, return `AUDIT_FAILED_AFTER_APPLY` if that audit fails, and degrade live health.
- Restore now requires the exact database ID, manifest generation, and current revision tuple and continues to verify backup lineage.
- The bounded `owner.Runtime` wrapper routes all administrative operations through `Controller`.

## Blocking findings

1. **The published directory is not pinned continuously from rename through its durability sync.** `RenameNoReplaceAnchored` closes its pinned destination descriptor before the caller invokes `SyncDirAnchored`, which independently reopens the destination path. An adversarial recovery probe let the anchored WAL rename complete, then while the source directory sync hook ran renamed `quarantine` to `quarantine-moved` and created a new real `quarantine` directory. The second `SyncDirAnchored` opened and synced that replacement directory, and `ApplyRecovery` returned ordinary success with `QuarantinedPath: quarantine/<wal>`, while the actual WAL was under `quarantine-moved/<wal>` and its parent directory had not been synced. This violates the requested anchored rename+directory-sync authority boundary and reports false location/durability evidence. Keep the pinned destination descriptor alive through `fsync`, or perform publication plus destination-directory sync in one anchored primitive; verify the moved inode on that same descriptor and return success only if the original published parent was synced. Add the between-rename-and-sync real-directory replacement probe for recovery, backup, and restore.

2. **The explicit dead-code gate still fails.** `faultfs.FS.RenameNoReplace` and `(*faultfs.OS).RenameNoReplace` now have zero production callers after all publication paths moved to `RenameNoReplaceAnchored`. Separately, `owner.Compose` has no non-test caller; the only construction is in `internal/owner/runtime_test.go`, so the newly introduced top-level runtime is not yet composed by a live owner/server entry point. Remove the superseded unanchored primitive and connect `owner.Runtime` from a production owner composition point (with a black-box routing test), or otherwise narrow the public surface so each new function/module has a production caller.

## Independent evidence

- Go toolchain: `go1.26.7 linux/amd64`.
- Passed: `go test ./...`, `go test -race ./...`, `go vet ./...`, `staticcheck ./...`, `govulncheck ./...`, `go mod verify`, `go mod tidy -diff`, full `gofmt` check, and `git diff --check`.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` (5 seconds, 513,092 executions) and `FuzzStorageDecoders` (5 seconds, 30,401 executions).
- The temporary publication-through-sync adversarial test independently failed and was removed after execution. Existing cycle-2/cycle-3 boundary and fault tests passed.

## Anti-pattern checklist

1. Dead code: **FAIL** — the unanchored rename primitive has no caller, and `owner.Compose` is test-only.
2. Synthetic-fixture test: **PASS** — repair tests exercise production filesystem/controller paths.
3. Silent empty return: **PASS** — no relevant silent empty-return pattern found.
4. FR coverage: **FAIL** — FR-021/FR-023 fail at the publication-to-directory-sync race.
5. Frozen surface: **PASS** — no mission contract/spec file changed in `ee1961f`.
6. Locked decision: **FAIL** — ordinary success can be reported without syncing the actual published destination parent.
7. Shared-file ownership: **PASS** — `ee1961f` is isolated on WP05 ancestry.
8. Production fragility: **FAIL** — a benign pathname replacement between two supposedly anchored phases yields false success/location evidence.
