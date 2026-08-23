---
affected_files:
  - internal/owner/runtime.go
  - internal/persist/identity.go
cycle_number: 3
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/owner -run '^TestReviewerOpenRejectsReleasedOwnerLock$' -count=1
reviewed_at: '2026-08-23T19:20:00Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP05
---

# WP05 Cycle 3 Re-review

Verdict: changes requested.

## Closure evidence

- `PublishNoReplaceAnchored` now keeps source and destination parent descriptors open continuously across `renameat2`, moved-inode/ancestry checks, and both required directory syncs. Recovery, backup, and restore return the descriptor-resolved stable destination. Repeated parent-identity changes and before/after sync fault matrices pass.
- The superseded `RenameNoReplace`, `RenameNoReplaceAnchored`, and `SyncDirAnchored` APIs have zero declarations. All three publication paths use the single anchored primitive.
- The real writer-owned backup barrier, exact restore database/generation/revision guards, bounded streaming evidence, and applied-result/audit/health-degradation semantics remain intact.
- `owner.Open` is a non-test composition entrypoint that constructs the engine and guarded controller together and exposes bounded status plus administrative routing.

## Blocking finding

1. **`owner.Open` accepts an already released `OwnerLock`, so the runtime can start without exclusive database authority.** `OwnerLock.Close` clears only `file`; `OwnerLock.Directory` continues returning the remembered directory. `owner.Open` checks only that directory string, so a caller can acquire and close the lock, pass the closed object to `owner.Open`, and then successfully acquire a second `OwnerLock` for the same database while the returned runtime is active. The independent deletion probe failed with `owner.Open accepted a released lock while another owner acquired the database`. This violates the sole-owner lifecycle boundary and the requirement that the runtime retain a matching lock until engine shutdown. Make live lock ownership an explicit synchronized state: `Open` must atomically validate/consume a still-held lock (not merely its path), reject nil/closed/mismatched ownership before constructing the engine, and make runtime/lock close exactly-once and race-safe while draining the engine before releasing authority. Add black-box tests for close-before-open, duplicate/invalid transfer, concurrent runtime closes under `-race`, and reacquisition only after completed shutdown.

## Independent evidence

- Go toolchain: `go1.26.7 linux/amd64`.
- Passed uncached: `go test -count=1 ./...` and `go test -race -count=1 ./...`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities), `go mod verify`, `go mod tidy -diff`, full `gofmt` check, and `git diff --check 16e2c3b..HEAD`.
- Passed ten times: the recovery publication/identity/sync-fault matrix, backup/restore publication/identity/sync-fault matrix, writer-owned backup barrier, applied-failure audit preservation, exact restore authority tuple, and bounded owner status routing.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` for 5 seconds (257,140 executions) and `FuzzStorageDecoders` for 5 seconds (45,513 executions).
- The temporary closed-lock adversarial test used production `AcquireOwner`, `OwnerLock.Close`, `owner.Open`, and a second `AcquireOwner`; it failed as described and was removed after execution.

## Anti-pattern checklist

1. Dead code: **PASS** — obsolete publication APIs are removed; `owner.Open` is the production composition entrypoint for the dependent server package.
2. Synthetic-fixture test: **PASS** — repair tests exercise production filesystem, controller, persistence, and owner paths.
3. Silent empty return: **PASS** — no relevant silent empty-return pattern found.
4. FR coverage: **FAIL** — exclusive ownership is not actually retained across owner runtime construction.
5. Frozen surface: **PASS** — no mission contract/spec file changed in `0b1c0aa`.
6. Locked decision: **FAIL** — the one-owner process invariant can be bypassed with a closed lock object.
7. Shared-file ownership: **PASS** — `0b1c0aa` is isolated to WP05 ancestry.
8. Production fragility: **FAIL** — a valid-looking but released capability starts a runtime while another owner holds the same database.
