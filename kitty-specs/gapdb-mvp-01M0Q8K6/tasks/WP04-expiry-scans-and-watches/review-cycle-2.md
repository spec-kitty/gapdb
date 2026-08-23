---
affected_files:
  - internal/engine/watch.go
  - internal/engine/watch_test.go
  - internal/engine/history.go
  - internal/engine/history_test.go
  - internal/engine/expiry.go
  - internal/engine/expiry_test.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./internal/engine -run '^TestReviewerWP04' -count=1
reviewed_at: '2026-08-23T17:14:25Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP04
---

# WP04 Review Cycle 2

## Verdict

Rejected. Scan ordering, cursor integrity, ordinary history eviction, serialized
registration, logical expiry, and the normal live-lag path are well covered, but
three boundary defects violate the whole-commit, recovery, and bounded-resource
contracts.

## Blocking findings

### 1. Lag can terminate a watch after delivering only part of a backlog commit

**Severity:** Critical  
**Affected code:** `internal/engine/watch.go`, `watcherState.pump`

`deliver` selects `watcher.end` separately for every event. If the live queue
overflows while the pump is blocked partway through a multi-event backlog group,
the next per-event select accepts the lag end and closes the stream. The client
has then observed part of an atomic commit even though
`last_delivered_revision` remains the preceding safe revision.

Independent reproduction created a two-effect revision-1 backlog, consumed only
order 0, filled the one-event live budget with revision 2, and overflowed it with
revision 3. Ten of ten runs closed the event stream before order 1 and returned
`WATCH_LAGGED` with `last_delivered_revision: 0`. This contradicts FR-014,
GAP-001, and T021/T022's no-partial-batch rule.

Make termination group-aware. Once delivery of a commit group begins, either
deliver the complete group before honoring lag termination or do not begin that
group. The terminal evidence must identify the last wholly covered revision.
Add a deterministic test where lag occurs while a multi-event backlog group is
partially blocked; the test must assert that a partial group is impossible.

### 2. An empty post-snapshot recovery gap is silently reclassified as compacted

**Severity:** High  
**Affected code:** `internal/engine/history.go`, `historyLog.rebuild`

When `RecoveredCommits` is empty, `rebuild` advances `compactedThrough` to
`CurrentRevision` without considering `SnapshotRevision`. Thus configuration
with snapshot revision 5, current revision 7, and zero recovered frames succeeds
and silently declares revision 7 compacted. The equivalent nonempty recovery
ending at revision 6 is correctly rejected.

This makes the empty boundary bypass the completeness invariant and fails T020's
requirement to rebuild post-snapshot history with the snapshot revision as the
compaction boundary. Reject `current > snapshot` when no post-snapshot commits
are supplied, or introduce an explicit, separately validated mode for callers
that intentionally discard otherwise recoverable history. Add empty, nil, and
single/multi-frame boundary tests.

### 3. Stale expiry candidates create an unbounded memory path

**Severity:** High  
**Affected code:** `internal/engine/expiry.go`, `scheduleRecordExpiry`

Every expiring replacement pushes a candidate, while stale candidates remain
until their absolute deadline. Replacing one key 100 times with the same
far-future expiry leaves 100 heap entries for one live record. Repeating the
operation grows memory without a configured ceiling and can retain copied keys
for decades.

Stale entries are authority-safe but not resource-safe. Bound or compact the
heap—for example, periodically rebuild it from current records, replace an
existing per-key candidate, or enforce a discoverable limit with explicit
behavior. Add a stress test proving repeated far-future replacements do not
grow retained candidates without bound while preserving stale-version safety.

## Verified behavior and gates

- Logical expiry is authoritative at equality; cleanup is writer-serialized,
  durable, sorted, stale-revision checked, and chunked to batch/frame limits.
- Scan pages are bytewise key-sorted and count/byte bounded. Cursors are bounded,
  canonical base64url, CRC32C checked, and bound to database, prefix, revision,
  last key, and as-of time. Continuations reuse as-of and reject intervening
  commits with the documented evidence.
- History copies values, accounts by events and bytes, and evicts normal commits
  only as whole revision groups. Prefix replay ordering and compacted/ahead errors
  are stable.
- Watch registration is serialized; backlog precedes live traffic; prefix
  filtering, normal live overflow, cancellation, shutdown, and client bounds are
  explicit. The blocker is the backlog/live lag interaction above.
- Deletion probes are sensitive: removing scan staleness, history compaction, the
  watch event reservation, or recovered-expiry frame sizing makes their targeted
  tests fail.
- Go `1.26.7` gates passed: full tests, full race suite, engine race suite five
  times, vet, staticcheck, govulncheck (no vulnerabilities), module verify/tidy
  diff, gofmt, and git diff checks. Storage fuzz completed 79,666 executions.
- The lane has no `kitty-specs` diff and no unrelated product scope drift.

## WP anti-pattern checklist

1. **Dead code:** PASS — all four feature modules are integrated into production engine paths.
2. **Synthetic-fixture test:** PASS — tests invoke the production state, writer, history, scan, expiry, and watch paths.
3. **Silent empty return:** PASS — intentional empty history/expiry results are explicit normal states; no swallowed failure found.
4. **FR coverage:** FAIL — FR-014 allows a partially delivered backlog commit, and FR-015's cleanup structure is not resource bounded.
5. **Frozen surface:** PASS — no frozen contract or protocol file changed.
6. **Locked decision:** FAIL — partial watch groups and unbounded retained expiry candidates contradict GAP-001/GAP-005 and the plan.
7. **Shared-file ownership:** PASS — the small WP03 integration edits are necessary to route writer-owned expiry/history/watch behavior.
8. **Production fragility:** PASS — no new panic-style production escape was introduced.
