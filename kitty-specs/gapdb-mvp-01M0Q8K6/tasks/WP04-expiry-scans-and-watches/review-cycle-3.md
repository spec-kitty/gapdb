---
affected_files:
  - internal/engine/history.go
  - internal/engine/history_test.go
cycle_number: 3
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/engine -run '^TestReviewerWP04RecoveryAcceptsDurablyBurnedRevisionGaps$' -count=1
reviewed_at: '2026-08-23T17:38:00Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP04
---

# WP04 Review Cycle 3

## Verdict

Rejected for one new cross-layer recovery blocker. The repair closes the three
cycle-2 findings, but its history coverage rule rejects valid WAL sequences that
contain durably burned revision gaps.

## Blocking finding

### History rebuild incorrectly requires consecutive revision numbers

**Severity:** Critical  
**Affected code:** `internal/engine/history.go`, `historyLog.rebuild`

The repair requires every recovered commit revision to equal `previous + 1`.
That contradicts the locked revision model: `data-model.md` states that WAL
revisions are strictly increasing, gaps are valid, and only duplicates or
regressions are corruption. WP02's production `ScanWAL` implements that rule,
and `TestRecoverWALRevisionRules` explicitly accepts revisions 2 then 4.

Independent reproduction supplied snapshot revision 1, current revision 4, and
the already-validated recovered commits 2 and 4. `engine.New` returned
`INVALID_REQUEST`, preventing startup after a legitimate reservation gap.

Validate coverage by boundaries, not arithmetic consecutiveness:

- empty/nil commits are valid only when `current_revision == snapshot_revision`;
- a nonempty list must start strictly after the snapshot boundary;
- revisions must be strictly increasing, allowing numeric gaps;
- the final recovered revision must equal `current_revision`;
- duplicates, regressions, a first revision at/below snapshot, and a missing
  final-current frame must fail closed.

Add a matrix covering nil/empty, first-boundary failure, duplicate/regression,
valid middle reservation gaps, and final-current coverage. Reuse production
WP02 recovery output in at least one engine initialization test.

## Closure evidence for cycle-2 findings

- **Whole-group watch lag:** closed. Backlog size is preflighted cumulatively
  against the event budget; an oversized group never begins; a group that has
  begun completes before lag termination; last-delivered evidence advances only
  after the whole group. Internal x10 tests and independent replay passed.
- **Empty post-snapshot omission:** closed. Nil/empty input with current above
  snapshot now fails through the final coverage boundary. The blocker is only
  the over-tight rejection of valid numeric gaps inside nonempty recovery.
- **Expiry heap growth:** closed. The indexed one-candidate-per-key heap remains
  bounded through 1,000 replacements/deletes/deadline changes. Independent
  index-pointer/order checks passed. Deleting `heap.Fix`, `heap.Remove`, or Swap
  index maintenance makes targeted tests fail.

## Other verification evidence

- Original scan, expiry, history, watch, cancellation, shutdown, queue, and
  reconciliation tests pass.
- Deletion probes for group atomicity, cumulative reservation, final recovery
  coverage, heap `Fix`, heap `Remove`, and index maintenance are sensitive.
- Go `1.26.7` gates pass: full tests, full race suite, engine race x5, vet,
  staticcheck, govulncheck (no vulnerabilities), module verify/tidy diff, gofmt,
  and git diff checks. Storage fuzz completed 83,179 executions.
- Lane scope remains limited to WP04 engine integration with no `kitty-specs`
  lane diff.

## WP anti-pattern checklist

1. **Dead code:** PASS.
2. **Synthetic-fixture test:** PASS.
3. **Silent empty return:** PASS.
4. **FR coverage:** PASS for WP04 behavior; startup integration is blocked by the revision-model regression.
5. **Frozen surface:** PASS.
6. **Locked decision:** FAIL — consecutive-only history rebuilding contradicts the locked gap-permitted revision invariant.
7. **Shared-file ownership:** PASS — integration edits are narrow and required.
8. **Production fragility:** PASS.
