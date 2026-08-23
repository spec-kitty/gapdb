---
affected_files:
  - internal/server/owner.go
  - internal/protocol/envelope.go
  - internal/server/server.go
  - gapdb/client_codec.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/server -run '^TestReviewer(LockedReplacementDoesNotImpersonateHeldLock|WatchRegistrationErrorRemainsStructured)$' -count=1
reviewed_at: '2026-08-23T20:32:00Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP06
---

# WP06 Repair Re-review

Verdict: changes requested.

## Closure evidence

- An ordinary existing `LOCK` is now changed to effective `0600`; symlink and unlocked replacement-inode probes fail before socket creation.
- The public client now rejects the prior unknown nested field, duplicate field, noncanonical base64, non-UTC time, invalid presence/EOF, result/event, and code-specific error fixtures. The canonical decoder gained matching explicit result DTO schemas.
- Pending watches are reserved before dialing. The delayed-start/`Client.Close` race passes repeatedly and under `-race`; Close cancels the pending connection and no successful watch is returned.
- Inspection and online-admin handlers now emit explicit lowercase DTOs with status ownership evidence, config source/ceilings, verification files, snapshot authority, compaction sync, and backup checksum/byte/verification fields. The raw/client admin matrix passes.
- Existing server/client/daemon, ownership, permissions, recovery refusal, watch, accepted-work drain, remote-error, and real-process restart tests remain green.

## Blocking findings

1. **A different locked replacement inode can impersonate the runtime-held `LOCK`.** `secureHeldLock` reopens the current pathname and interprets `EWOULDBLOCK` from `flock` as proof that it is the owner runtime's inode. That proves only that *someone* holds the replacement open-file description. The independent hook renamed the genuinely held `LOCK`, created a new `LOCK`, acquired a separate exclusive flock on that replacement, and let startup continue. `server.Open` accepted it and reached readiness while its `OwnerLease` retained the unlinked original inode. Bind mode and identity validation to the descriptor acquired by `AcquireOwner`: `fchmod` and `fstat` that exact descriptor, compare it to an anchored `fstatat(..., AT_SYMLINK_NOFOLLOW)` path identity immediately before socket publication, and reject every mismatch regardless of whether the named replacement is itself locked. Add this locked-replacement probe normally, 10x, and under `-race`.

2. **Valid watch registration failures are no longer encodable/decodable and become ambiguous transport failures.** Both new decoders require `stream` on every `operation:"watch"` response, but `handleWatch` still calls `writeFailure` for registration errors without setting `stream:"ended"`. `protocol.EncodeResponse` rejects that response, sends no frame, and `Client.Watch("", after_revision=1)` against an empty real server returns `*TransportError{Ambiguous:true}` instead of structured `REVISION_AHEAD`. Choose and enforce one locked shape—prefer a terminal `stream:"ended"`, `ok:false`, `reason:"error"`, common error envelope—and make server, canonical codec, client codec, and golden fixtures agree for `REVISION_AHEAD`, `REVISION_COMPACTED`, `SERVER_BUSY`, and every other pre-start watch error. The public client must preserve the typed remote error rather than report response-loss ambiguity.

## Independent evidence

- Go toolchain: `go1.26.7 linux/amd64`.
- Passed focused 10x normally and under `-race`: server reviewer/lock/watch tests and public-client reviewer/deadline tests.
- Passed real-process API tests normally and under `-race`.
- Passed uncached: `go test -count=1 ./...` and `go test -race -count=1 ./...`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities), `go mod verify`, `go mod tidy -diff`, full `gofmt`, and `git diff --check 9838355..HEAD`.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` for 5 seconds (442,412 executions) and `FuzzStorageDecoders` for 5 seconds (38,823 executions).
- Both temporary adversarial tests used production server/client paths, failed as described, and were removed.

## Anti-pattern checklist

1. Dead code: **PASS** — new codec/DTO/lifecycle surfaces have production callers.
2. Synthetic-fixture test: **PASS** — repaired tests invoke production client, codec, server, persistence, and daemon paths.
3. Silent empty return: **PASS** — no relevant silent empty-return pattern found.
4. FR coverage: **FAIL** — FR-016 and FR-014/017 remain incomplete at exact lock identity and watch registration-error transport.
5. Frozen surface: **PASS** — mission contracts/spec artifacts were not modified.
6. Locked decision: **FAIL** — one-owner inode identity and stable structured watch errors are violated.
7. Shared-file ownership: **PASS** — repair changes remain within WP06 integration surfaces.
8. Production fragility: **FAIL** — a locked pathname substitution permits false readiness, and a normal watch condition error is misclassified as ambiguous transport loss.
