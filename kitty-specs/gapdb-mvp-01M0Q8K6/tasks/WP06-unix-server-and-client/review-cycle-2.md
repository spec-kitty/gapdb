---
affected_files:
  - internal/server/server.go
  - internal/server/socket_anchor.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/server -run '^TestReviewerSeparateSocketParentSwapBeforeFirstCheckFailsClosed$' -count=10
reviewed_at: '2026-08-23T20:55:23Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP06
---

# WP06 Final Directory-Authority Re-review

Verdict: changes requested.

## Closure evidence

- Database-directory replacement is now detected at the before-lock-check, after-stale-cleanup, and after-listener-bind boundaries. The retained owner directory/parent descriptors bind the named database directory to the acquired inode, and failed startup removes only the socket in the anchored original directory while preserving a plausible flocked replacement `LOCK`, replacement socket, and sentinel.
- Later socket-parent replacement is also detected once `directoryAnchor` exists. Stale unlink, bind through `/proc/self/fd`, socket chmod/identity verification, and normal cleanup are descriptor-relative. Procfs failure has no pathname fallback and therefore fails closed. The public `SocketPath` remains dialable in the normal daemon/client contract tests, and normal close, restart, stale cleanup, second-owner refusal, and corrupt-start refusal pass normally and under race.
- All earlier WP06 findings remain closed: the exact held `LOCK` is mode/identity checked; a separately flocked replacement inode cannot impersonate it; strict client/canonical decoding rejects unknown nested fields, duplicates, non-canonical base64/time, invalid null/presence, and trailing input; watch registration and `Client.Close` are atomic; pre-registration watch failures retain typed stable errors; online admin DTOs preserve the complete canonical lowercase evidence shape.

## Blocking finding

1. **A separately configured socket parent can be replaced before its authority is acquired, and the replacement receives readiness.** `server.Open` completes `openRuntime` (including `BeforeLockModeCheck`) before it calls `openDirectoryAnchor(filepath.Dir(config.SocketPath))`. An independent production-path probe used a database directory and a separate socket parent, renamed the socket parent during `BeforeLockModeCheck`, created a replacement parent with a sentinel and plausible live Unix socket, and observed `server.Open` succeed 10/10. Startup anchored the replacement after the swap, removed its existing socket, and published the Gapdb listener there. This violates the required no-readiness/no-mutation behavior at the first seam and makes the result depend on whether `SocketPath` happens to share the database directory. Acquire and retain the configured socket-parent capability before the first post-ownership authority check/race seam (reusing the held database-directory capability when appropriate), then use that same capability through stale cleanup, bind, verification, readiness, and close. Add the separate-socket-parent three-seam matrix, including a plausible socket and sentinel, x10 and under `-race`; the first seam must fail without unlinking or replacing anything in the substituted directory.

## Independent evidence

- Go toolchain: `go1.26.7 linux/amd64`.
- Reproduction: the temporary separate-parent substitution test failed 10/10 with `replacement socket parent received readiness`; the temporary probe was removed afterward.
- Passed focused 10x normally and under `-race`: all server reviewer/ownership/watch/admin tests, public-client reviewer codec/watch tests, and real daemon API/restart/second-owner/corruption tests.
- Passed uncached: `go test -count=1 ./...` and `go test -race -count=1 ./...`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (`No vulnerabilities found`), `go mod verify`, `go mod tidy` with no module diff, full `gofmt`, and `git diff --check`.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` for 5 seconds (372,481 executions) and `FuzzStorageDecoders` for 5 seconds (58,232 executions).
- Lane scope is clean apart from runtime-owned untracked `.spec-kitty/`; no review probe or mission-artifact change remains in lane-f.

## Anti-pattern checklist

1. Dead code: **PASS** — the new socket anchor has production callers from server startup and shutdown.
2. Synthetic-fixture test: **PASS** — lifecycle, protocol, client, and daemon tests invoke production paths.
3. Silent empty return: **PASS** — no relevant silent empty-return path was introduced.
4. FR coverage: **FAIL** — FR-016 remains incomplete for a separately configured socket parent at the earliest authority seam.
5. Frozen surface: **PASS** — no frozen specification or contract surface changed.
6. Locked decision: **FAIL** — owner-only socket authority can be redirected to a replacement parent before anchoring.
7. Shared-file ownership: **PASS** — repair changes are scoped to WP06 and the approved persistence ownership seam.
8. Production fragility: **FAIL** — a pathname substitution can obtain ordinary readiness and cause mutation of replacement state.
