---
affected_files:
  - internal/persist/recovery.go
  - internal/persist/recovery_test.go
cycle_number: 3
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/persist -run '^TestRecoveryReportsEarlierTailApplyWhenLaterStageFails$' -count=1
reviewed_at: '2026-08-23T15:30:21Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP02
---

# WP02 Review Cycle 3

## Verdict

Rejected for one newly substantiated stable-evidence defect. Repair commit
`1e28f00` genuinely closes all three cycle-2 findings; those closures must be
preserved. The remaining defect occurs after a legitimate tail repair when a
later startup stage fails.

## Blocking finding

### A later startup failure reports `operation_applied: false` after tail repair changed the WAL

**Severity:** High, approval blocking  
**Affected code:** `internal/persist/recovery.go`, `RecoverDatabase`

`RecoverDatabase` truncates and syncs an incomplete final WAL tail, successfully
calls `AuditTail`, and then performs cleanup discovery and revision reservation.
If one of those later stages fails, its error is returned directly. Errors such
as cleanup `IO_ERROR` therefore retain `operation_applied: false` even though the
same recovery invocation already changed authoritative storage.

Independent reproduction used a valid 64-byte WAL header plus one incomplete
byte, an audit callback that succeeded, and an injected
`PointReadDir/Before` error. Recovery truncated the WAL from 65 to 64 bytes and
called the audit callback once, then returned `IO_ERROR` with
`OperationApplied: false`.

This contradicts `errors-v1.md`: if an operation may have applied before a later
failure, the envelope must set `operation_applied: true`. A model otherwise sees
bytes changed and a successful audit while the stable error evidence says no
operation applied.

Track whether tail repair has applied and preserve that fact on every subsequent
error. Pure reservation validation and cleanup discovery may also be moved before
tail mutation where contract ordering permits, but reservation I/O can still fail
after a tail repair and must inherit `operation_applied: true`. Add regression
coverage for:

- successful tail truncate/sync/audit followed by cleanup enumeration failure;
- successful tail repair followed by revision-reservation validation failure;
- successful tail repair followed by identity temp create/write/sync/rename or
  directory-sync failure, asserting both authoritative bytes and the aggregate
  recovery `operation_applied` value.

## Cycle-2 finding closure evidence

1. **Non-EOF prefix reads:** CLOSED. Independent zero-byte and partial-prefix
   readers returned `IO_ERROR`, preserved the underlying cause, and never
   returned a tail proposal. Deleting the new EOF guard made both committed and
   reviewer probes fail by returning successful tail recovery.
2. **Manifest/WAL authority before truncation:** CLOSED. A generation-mismatched
   WAL with an incomplete tail preserved WAL and `IDENTITY` bytes and reached no
   tail, audit, cleanup-discovery, or reservation fault point. Moving tail apply
   before the generation check made the committed and reviewer probes fail with
   a 65-to-64-byte mutation.
3. **Identity/manifest application evidence:** CLOSED. Rename-before,
   rename-after, directory-sync-before, and directory-sync-after cases for both
   artifacts matched actual authoritative bytes and `operation_applied`.
   Deleting the applied-stage handling made every post-authority case fail.

## Other verification evidence

- SnapshotLoader trust boundary: PASS. Nil loaders and invalid returned snapshot
  revision/order fail before WAL mutation, cleanup discovery, or reservation;
  the loader remains responsible for byte/hash/identity verification while
  recovery independently revalidates semantic snapshot state.
- Raw endian/CRC formats, short-write handling, bounded arithmetic under validated
  hard limits, durable-through barriers, non-reused reservation ranges, overflow,
  flock ownership, atomic install order, complete corruption refusal, cleanup
  sorting, and golden decoder reachability remain sound.
- Go toolchain is `go1.26.7 linux/amd64`; module pins `go 1.26` and
  `toolchain go1.26.7`.
- Passed: `go mod verify`, `gofmt`, `git diff --check`, `go vet ./...`,
  `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities),
  `go test -count=1 ./...`, and `go test -race -count=1 ./...`.
- Storage decoder fuzz smoke completed about 30,400 executions without failure.
- Lane is tracked-clean after restoring all temporary probes; repair commit
  `1e28f00` has no `kitty-specs` diff or scope drift.

## WP anti-pattern checklist

1. **Dead code:** N/A — explicitly staged persistence foundation for dependent WPs.
2. **Synthetic-fixture test:** PASS — independent encoders and deletion probes
   exercise production decoders and recovery paths.
3. **Silent empty return:** PASS — no silent empty failure path found.
4. **FR coverage:** FAIL — the post-tail/later-failure branch violates
   FR-021/FR-022 and lacks a committed regression.
5. **Frozen surface:** PASS — no mission planning or contract artifact changed on
   the lane.
6. **Locked decision:** FAIL — the returned stable evidence contradicts the v1
   error contract’s mandatory post-apply rule.
7. **Shared-file ownership:** N/A — WP02 owns lane B alone; WP01 files are approved
   dependency ancestry.
8. **Production fragility:** N/A — no panic/raise-style transient path introduced.
