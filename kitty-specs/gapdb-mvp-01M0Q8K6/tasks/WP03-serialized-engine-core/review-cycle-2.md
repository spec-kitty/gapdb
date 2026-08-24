---
affected_files:
  - path: internal/engine/revision.go
  - path: internal/engine/writer.go
  - path: internal/engine/writer_test.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/engine -run '^TestReviewerWP03' -count=1
reviewed_at: '2026-08-23T16:27:57Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP03
---

# WP03 Review Cycle 1

## Verdict

Rejected for two approval-blocking fail-closed defects: the engine can publish a
reused revision supplied by its allocator seam, and failures discovered after a
complete WAL append can report `operation_applied: false` even though restart
will replay the commit. A related post-append panic has the same false evidence.

## Blocking findings

### 1. The writer accepts zero, reused, or regressing revisions from `RevisionAllocator`

**Severity:** Critical  
**Affected code:** `internal/engine/writer.go`, `execute`

The writer calls `state.allocator.Next()` and sends the returned value directly
to the log and map. It never verifies that the revision is nonzero and strictly
greater than `state.current`. `RangeAllocator` maintains that invariant, but
`Config.Allocator` is an interface boundary and the writer's own fail-closed
invariant checks must prevent a faulty allocator or reserve integration from
corrupting published state.

Independent reproduction started the engine at revision 5 with an allocator that
returned 5. `Put("new-key")` succeeded, appended a frame, and published a new
record at the already-used revision 5. The engine remained ready. Returning zero
similarly permits revision-zero publication when a permissive log seam is used.

This violates the never-reused global revision contract and T012's requirement
that unexpected invariant failures disable uncertain writes. After allocation,
verify the returned revision is nonzero and strictly greater than the current
committed revision before constructing or appending the frame. On violation,
burn the bad allocation, enter degraded read-only state, publish nothing, and
return stable reconciliation evidence. Add zero, equal, and regressing allocator
tests plus the maximum-revision boundary.

### 2. Post-append failures and panics can falsely report that nothing applied

**Severity:** High  
**Affected code:** `internal/engine/writer.go`, `persistenceFailure` and
`executeSafely`

The append failure branch always calls
`persistenceFailure(..., operationApplied=false, ...)`. For a structured storage
error, `persistenceFailure` also overwrites any existing applied-state evidence
with that false value. This is unsafe because append errors can be reported after
the complete frame reached the file.

Independent reproduction used the production `persist.WAL` with buffer size 1
and an injected `PointWALFrameWrite/After` error. The engine did not publish the
map entry and correctly degraded, but returned `STORAGE_DEGRADED` with
`operation_applied: false`. The WAL file contained a complete frame beyond the
64-byte header; after closing and reopening it, production recovery decoded one
complete revision-1 commit with no torn-tail repair. Restart can therefore apply
the mutation while the stable response tells the caller that it did not apply.

The panic wrapper has the same phase-loss defect. A log that accepted `Append`
and then panicked in `DurableThrough` produced `INTERNAL` with
`operation_applied: false` despite retaining the frame. The writer degraded and
stopped further writes, but the caller received unsafe retry evidence.

Track commit progress across allocation, append, barrier, and publication.
Preserve an underlying structured error's true applied state; conservatively set
true whenever an append may have reached persistence. Panic conversion must use
the same progress state so a panic after append returns `INTERNAL` with
`operation_applied: true`, while an allocator panic remains false. Add a real-WAL
after-write recovery test and before/after-append panic tests.

## Verified behavior

- Exactly one production writer goroutine and one production allocator `Next`
  call site exist. All map mutations occur in the writer.
- Direct `Get` uses the shared `RWMutex`, owns input/state/output bytes, remains
  available in degraded read-only state, and applies nondecreasing logical expiry
  without introducing WP04 scheduling.
- Static and state-dependent losers allocate nothing and append nothing. Deleting
  the condition-return path makes single-key losers, failed batches, and the
  exact-winner race tests fail.
- Successful batches use one revision, one frame, ordered events, and one map
  write-lock section. Concurrent readers observe only pre- or post-batch state.
- Append/barrier failures publish no map state and disable later writes. Deleting
  the append-error return makes the committed persistence-failure test fail by
  publishing/acknowledging the failed request.
- Memory and durable acknowledgement/barrier evidence is correct on successful
  paths; durable barriers include preceding memory frames.
- Queue capacity is bounded. Saturation, cancellation before admission,
  cancellation after admission, graceful close/drain, and panic degradation do
  not deadlock. Accepted commands receive a result.
- `RangeAllocator` accepts reserved gaps, refills strictly after the prior end,
  never reuses values, and stops without wrapping at `MaxUint64`.

## Other verification evidence

- Go toolchain is `go1.26.7 linux/amd64`; `go.mod` pins `go 1.26` and
  `toolchain go1.26.7`.
- Passed: `go mod verify`, `go mod tidy -diff`, `gofmt`, `git diff --check`,
  `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities),
  `go test -count=1 ./...`, and `go test -race -count=1 ./...`.
- The engine race suite also passed five consecutive runs.
- Storage decoder fuzz smoke completed 33,506 executions without failure.
- Reviewer probes were removed and the lane is tracked-clean with no
  `kitty-specs` diff or scope drift.

## WP anti-pattern checklist

1. **Dead code:** N/A — the engine is the staged production core for dependent WPs.
2. **Synthetic-fixture test:** PASS — production writer, WAL, recovery, allocator,
   and map paths were exercised directly.
3. **Silent empty return:** PASS — no silent empty failure path found.
4. **FR coverage:** FAIL — FR-009 revision uniqueness and FR-010 stable
   acknowledgement evidence fail under the reproduced boundary conditions.
5. **Frozen surface:** PASS — only WP03-owned engine files changed.
6. **Locked decision:** FAIL — duplicate revisions and false post-append evidence
   contradict the fail-closed and never-reuse contracts.
7. **Shared-file ownership:** N/A — WP03 owns lane C and only engine files.
8. **Production fragility:** N/A — no panic-style production escape was added;
   the blocker is incomplete phase evidence in the panic conversion.
