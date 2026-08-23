---
affected_files: []
cycle_number: 3
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/admin -run '^TestReviewer' -count=1 -v
reviewed_at: '2026-08-23T18:33:56Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP05
---

# WP05 Repair Re-review

Verdict: changes requested.

## Closure evidence

- Full streaming artifact hashes now reject content mutations before, at, and beyond 64 MiB, bind size/device/inode, and reject a replacement made before revalidation.
- Static parent/final/dangling symlinks and final-entry creation races are rejected; final publication uses `renameat2(RENAME_NOREPLACE)`.
- Snapshot barrier and durable-evidence panics before installer entry now report `operation_applied: false`; installer-entry panics remain conservatively applied/ambiguous.
- Ordinary successful snapshot, compaction, backup, restore, and recovery operations now have audit call paths, and the controller exposes bounded status/configuration fields.

## Blocking findings

1. **Recovery still has source-identity and destination-parent TOCTOU windows at the destructive rename.** In one injected `PointBackupRename/Before` probe, the already-validated WAL pathname was atomically moved aside and replaced with different bytes. `ApplyRecovery` moved the uninspected replacement and returned success. In another, the already-validated external quarantine directory was renamed and replaced by a symlink to a directory inside the database; the WAL was moved through that alias into the database and success was returned. `RENAME_NOREPLACE` protects only the final entry, not the identities of the source or parent directories. Anchor source/destination operations to validated directory descriptors and re-check the moved inode/evidence, or otherwise make identity and ancestry part of the atomic operation. Add source swap and parent swap probes at the exact rename boundary for recovery, backup, and restore.

2. **Controller backup does not establish the required writer-serialized durability barrier.** `Controller.Backup` passes `Barrier: func() error { return nil }` to `CreateBackup` and is not a writer command. After a successful `AckMemory` commit at revision 1, a backup requested for durable revision 1 failed `ADMIN_PRECONDITION_FAILED` because the buffered WAL was never flushed/synced. The operation must pause/serialize against mutations and execute the real WAL barrier before selecting/copying authority; it must not rely on the caller already having made the revision durable.

3. **Applied persistence failures bypass audit, result preservation, and health degradation.** The controller audits only when the underlying operation returns nil. A backup `RenameNoReplace` after-phase fault published the verified destination but returned empty metadata, left the state `ready`, and wrote no backup audit entry. A compaction failure on the second removal reported partial application but left the state `ready` and wrote no compact failure audit. The same early-return shape exists for restore and recovery rename/directory-sync failures; `Controller.ApplyRecovery` neither serializes nor inspects applied errors to degrade health. Every state-changing result, including failed-after-apply paths, must preserve the applied result, append a result/error audit when possible, and degrade until verification; if that audit fails, retain `AUDIT_FAILED_AFTER_APPLY` without erasing the original applied evidence.

4. **Restore lacks exact authority/revision preconditions.** `RestoreRequest` carries only `ExpectedDatabaseID`; `Controller.Restore` does not require or validate expected manifest generation or current revision before creating the restored database. This does not satisfy the guarded-administration/current-revision revalidation requested by FR-020/T026 and allows a stale restore request to apply after live authority has advanced. Add exact current database ID, manifest generation, and revision preconditions, while continuing to verify backup lineage and durable revision.

5. **The new production controller remains unintegrated dead code.** A complete non-test call-site search finds `NewController` and every controller operation only in `internal/admin/admin_test.go`; no production server, owner, or administrative entry point constructs or calls it. This still fails the review prompt's explicit dead-code gate and means the new barrier/audit/precondition routing is not the application's live path. Add a production composition seam (or narrow the WP surface so its exported operations have a live caller) and exercise that seam with black-box tests.

## Independent evidence

- Go toolchain: `go1.26.7 linux/amd64`.
- Passed: `go test ./...`, `go test -race ./...`, `go vet ./...`, `staticcheck ./...`, `govulncheck ./...`, `go mod verify`, `go mod tidy -diff`, full `gofmt` check, and `git diff --check`.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` (5 seconds, 488,653 executions) and `FuzzStorageDecoders` (5 seconds, 33,774 executions).
- Five temporary adversarial tests independently reproduced the source swap, parent swap, missing backup barrier, applied-backup evidence loss, and partial-compaction audit/health defects. They were removed after execution.

## Anti-pattern checklist

1. Dead code: **FAIL** — `Controller` and its operations have no production caller.
2. Synthetic-fixture test: **PASS** — the new tests invoke production paths, though they omit the failing race/applied-error cases above.
3. Silent empty return: **PASS** — no relevant silent empty-return pattern found.
4. FR coverage: **FAIL** — FR-020/FR-022/FR-023 fail on stale restore, applied-error audit, and missing backup barrier paths.
5. Frozen surface: **PASS** — no mission contract/spec file was changed in the lane.
6. Locked decision: **FAIL** — exact revalidation, outside-database quarantine, durable barrier, and audit-after-state-change MUSTs remain violable.
7. Shared-file ownership: **PASS** — repair commit `94af5e6` is isolated on WP05 ancestry.
8. Production fragility: **FAIL** — failed-after-apply admin operations can leave externally changed state while reporting empty results and healthy lifecycle.

