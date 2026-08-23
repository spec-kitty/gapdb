---
affected_files:
  - internal/persist/recovery.go
  - internal/protocol/envelope.go
cycle_number: 4
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/persist -run '^TestReviewerCycle3AuditAndWireAggregateEvidence/reservation-validation$' -count=1
reviewed_at: '2026-08-23T15:50:47Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP02
---

# WP02 Review Cycle 4

## Verdict

Rejected for one newly substantiated protocol-boundary defect. Repair commit
`fd6ea9a` correctly preserves aggregate applied state inside recovery and closes
the cycle-3 finding. All three cycle-2 fixes also remain closed. The remaining
failure is that one newly correct stable error cannot be emitted by protocol v1.

## Blocking finding

### Post-tail revision-reservation validation produces an unencodable stable error

**Severity:** High, approval blocking  
**Affected code:** `internal/persist/recovery.go`, `withRecoveryAppliedState` and
`RecoverDatabase`; `internal/protocol/envelope.go`, `errorSchema`

With a valid WAL header plus one incomplete byte, successful tail truncate/sync,
successful audit, and `ReservationSize: 0`, `RecoverDatabase` now correctly
returns `REVISION_RANGE_EXHAUSTED` with `operation_applied: true`. The WAL is
authoritatively shortened from 65 to 64 bytes and `IDENTITY` remains unchanged.

However, protocol v1's code-specific evidence schema for
`REVISION_RANGE_EXHAUSTED` allows only `reserved_revision_end` and `reason`.
Passing the returned `*gapdb.Error` to `wire.EncodeResponse` therefore fails with
`INVALID_REQUEST` because `operation_applied` is considered irrelevant evidence.
The server cannot send the stable failure that recovery is required to report.

This contradicts the global `errors-v1.md` rule that every later failure after a
possibly applied operation includes `operation_applied: true`. It also defeats
the purpose of the cycle-3 repair at the public protocol boundary.

Make the aggregate recovery error encodable without weakening its evidence.
The protocol schema and compatibility fixtures should accept
`operation_applied: true` for `REVISION_RANGE_EXHAUSTED` (or recovery must return
another contractually correct, encodable stable error that preserves both the
reservation failure and already-applied tail repair). Add an end-to-end regression
that obtains this exact recovery error and encodes it through protocol v1; a unit
assertion on the in-memory `OperationApplied` field is insufficient.

## Cycle-3 finding closure evidence

- **Aggregate applied state:** CLOSED internally. Independent probes confirmed
  `operation_applied: true` after successful tail repair followed by missing or
  failed audit, cleanup `ReadDir` failure, zero reservation, and every before/after
  phase for identity temp create/write/sync, rename, and directory sync.
- **Authoritative bytes:** CLOSED. Every failure left the WAL at the complete
  64-byte header. `IDENTITY` remained old through pre-authority phases and became
  the complete new identity only at rename-after and directory-sync phases.
- **No-tail behavior/helper safety:** CLOSED. No-tail cleanup errors retained
  `operation_applied: false`, did not invoke audit, and did not mutate WAL bytes.
  `withRecoveryAppliedState` preserved nil and unstructured errors, returned the
  original error when no earlier apply occurred, and cloned rather than mutated a
  structured error while retaining its cause.
- **Deletion sensitivity:** CLOSED. Replacing `replay.Tail != nil` with false made
  cleanup, reservation, and identity-phase regression cases fail.

## Cycle-2 finding closure evidence

1. **Non-EOF prefix reads:** still closed. Zero-byte and partial-prefix device
   failures return `IO_ERROR`, retain their cause, and never propose truncation.
   Removing the EOF guard makes both cases incorrectly return successful tail
   recovery.
2. **Authority before tail mutation:** still closed. A generation-mismatched WAL
   preserves all bytes and reaches no tail/audit stage. Moving tail application
   before the authority check makes the committed test fail with a 65-to-64-byte
   WAL mutation.
3. **Atomic metadata application evidence:** still closed. Identity and manifest
   rename-before, rename-after, directory-sync-before, and directory-sync-after
   errors match actual authoritative bytes. Removing applied-stage handling makes
   all post-authority assertions fail.

## Other verification evidence

- SnapshotLoader validation boundary, bounded format arithmetic, endian/CRC
  layouts, short-write handling, sync barriers, durable-through semantics,
  revision burn/non-reuse/overflow, flock ownership, atomic install order, strict
  torn-tail versus corruption recovery, cleanup ordering, and golden decoder
  reachability remain sound.
- Go toolchain is `go1.26.7 linux/amd64`; `go.mod` pins `go 1.26` and
  `toolchain go1.26.7`.
- Passed: `go mod verify`, `go mod tidy -diff`, `gofmt`, `git diff --check`,
  `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities),
  `go test -count=1 ./...`, and `go test -race -count=1 ./...`.
- Storage decoder fuzz smoke completed 30,126 executions from 38 seeds without
  failure.
- The lane is tracked-clean after restoring reviewer probes; it has no net
  `kitty-specs` diff or scope drift.

## WP anti-pattern checklist

1. **Dead code:** N/A — persistence primitives are staged for dependent WPs.
2. **Synthetic-fixture test:** PASS — independent byte fixtures and production
   wire encoding reproduced the boundary failure.
3. **Silent empty return:** PASS — no silent failure path found.
4. **FR coverage:** FAIL — FR-021/FR-022 evidence is correct in memory but cannot
   cross the required protocol boundary.
5. **Frozen surface:** PASS — the implementation lane did not modify mission
   planning or contract artifacts.
6. **Locked decision:** FAIL — the returned stable error violates the executable
   v1 evidence schema despite satisfying the written global rule.
7. **Shared-file ownership:** N/A — WP02 owns lane B; WP01 is approved ancestry.
8. **Production fragility:** N/A — no panic-style transient path introduced.
