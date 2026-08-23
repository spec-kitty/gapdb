---
affected_files:
  - internal/persist/identity.go
  - internal/server/owner.go
  - internal/server/server.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/server -run '^TestReviewerDatabaseDirectorySwapFailsBeforeReadiness$' -count=1
reviewed_at: '2026-08-23T20:48:00Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP06
---

# WP06 Final Repair Re-review

Verdict: changes requested.

## Closure evidence

- The exact runtime-held `LOCK` descriptor is now chmodded and compared with `LOCK` through the acquisition-time directory descriptor. Symlink, unlocked replacement, and separately flocked replacement-inode probes all reject before socket creation; descriptor close paths are synchronized and leak-free in the normal/race tests.
- Watch registration errors now use the strict terminal shape `ok:false`, `stream:"ended"`, `reason:"error"`. The public client returns typed `REVISION_AHEAD`, `REVISION_COMPACTED`, and `SERVER_BUSY`, and all stable error schemas encode/decode as watch terminal failures.
- Prior strict client/canonical response validation, pending-watch Close race, complete lowercase admin DTOs, permission checks, recovery refusal, accepted-work drain, real-process restart, and daemon behavior remain green.

## Blocking finding

1. **Replacing the entire database directory can still redirect readiness and the Unix socket.** `OwnerLock` retains an acquisition-time directory descriptor, and `SecureHeldPath` verifies `LOCK` relative to that descriptor. If the whole database directory is renamed, the old directory descriptor and held `LOCK` remain mutually consistent, so both pre-listen lock checks pass. But directory chmod/stat, stale-socket removal, and `net.ListenUnix` resolve `config.Directory`/`SocketPath` again by pathname. The independent hook renamed `<parent>/database` to `database.held`, created a new owner-only `database` directory, and `server.Open` returned success with `gapdb.sock` in the replacement directory while WAL/lease authority remained in the renamed original. This is false readiness and splits the published socket from storage authority. Bind the acquisition-time database-directory inode to its parent/name and verify that exact identity at every pathname publication boundary, or perform socket lifecycle operations through a continuously anchored directory capability. After bind, verify the created socket's parent and inode before readiness; on any directory rename/replacement, close without publishing readiness and remove only a socket proven to belong to the anchored directory. Add full-directory rename/replacement probes before the first lock check, between stale cleanup and the final check, and across listener bind, repeated and under `-race`.

## Independent evidence

- Go toolchain: `go1.26.7 linux/amd64`.
- Passed focused 10x normally and under `-race`: server reviewer/lock/watch tests, public-client reviewer/deadline tests, and canonical/compatibility protocol suites.
- Passed real-process API tests normally and under `-race`.
- Passed uncached: `go test -count=1 ./...` and `go test -race -count=1 ./...`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities), `go mod verify`, `go mod tidy -diff`, full `gofmt`, and `git diff --check ac0bdd5..HEAD`.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` for 5 seconds (502,405 executions) and `FuzzStorageDecoders` for 5 seconds (98,229 executions).
- The temporary database-directory substitution probe used the production server lifecycle, failed as described, and was removed.

## Anti-pattern checklist

1. Dead code: **PASS** — new ownership, codec, DTO, and watch paths have production callers.
2. Synthetic-fixture test: **PASS** — repair tests invoke production server/client/protocol/persistence/daemon paths.
3. Silent empty return: **PASS** — no relevant silent empty-return pattern found.
4. FR coverage: **FAIL** — FR-016 is incomplete because socket readiness is not bound to the acquired database-directory authority.
5. Frozen surface: **PASS** — mission contract/spec artifacts were not modified.
6. Locked decision: **FAIL** — exactly one owner and owner-only local socket authority are split by directory replacement.
7. Shared-file ownership: **PASS** — repair changes are scoped to WP06 and the approved ownership seam.
8. Production fragility: **FAIL** — a pathname substitution yields ordinary readiness against the wrong directory.
