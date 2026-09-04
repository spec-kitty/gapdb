---
affected_files: []
cycle_number: 1
mission_slug: atomic-batch-assertions-01M1P4VH
reproduction_command:
reviewed_at: '2026-09-04T13:26:48Z'
reviewer_agent: user
wp_id: WP02
---

# WP02 Review Feedback — Cycle 1

Review target: `db1d9f159fcce7d2f8e8d1477dc87f1cef539063`

The production implementation is structurally correct: definitive predicates run under serialized writer authority using one captured timestamp and one read lock; assertions precede mutation conditions; allocation and commit construction follow all predicates; only mutations reach WAL, map application, expiry maintenance, history, recovery, and watches. The blockers are deletion/mutation-sensitive evidence gaps required by T005, T006, T008.5, and the Definition of Done.

## Issue 1 — Request-order evidence does not kill reverse assertion iteration

`TestAssertionFailureIsFirstOrderedPredicateAndHasZeroEffects` gives assertion 0 a passing condition and assertion 1 the only failing condition. Reversing the production assertion loop would still return assertion index 1 and every authored assertion test would remain green. Therefore the suite does not prove the explicit requirement to evaluate assertions in request order and report the first failing assertion.

Add a production-path case with at least two simultaneously failing assertions having distinct keys/revisions. Require exact evidence for index 0 and its key/condition. Add a controlled reverse-iteration overlay or equivalent mutation control and prove this case kills it.

Independent review confirmed the current production code returns index 0 correctly; the defect is the missing permanent proof.

## Issue 2 — Zero-effect evidence does not cover expired physical assertion state

No committed failure fingerprint contains a logically expired record used by an assertion together with its live physical record and expiry-index/heap candidate. The expiry-boundary success test observes logical absence but does not inspect the physical record or expiry structures afterward. A false solution that lazily removes an expired asserted record or its expiry candidate during predicate evaluation would survive the authored suite, violating the exact zero-effect and no-write assertion laws.

Add a failure scenario in which an expired physical record's absence assertion passes and a later assertion fails. Capture and compare the exact record map, expiry-by-key entry, expiry heap/census, revisions, allocator calls, WAL, history, snapshots, and watches before and after. Add a lazy-cleanup mutation overlay or equivalent control that this test kills. Also strengthen successful no-write evidence with a live expiring asserted record and prove its bytes, revision, expiry timestamp, heap entry, and expiry-by-key descriptor remain unchanged.

Independent review confirmed current production preserves all of this state; the defect is the missing permanent proof.

## Recommended non-blocking carry-forward

Add the independently passing failed-assertion response-loss scenario: publication loss must return conservative ambiguity with zero result counts, and restart/reconciliation must show unchanged revision/durability and no candidate record. The existing successful post-commit response-loss case already satisfies the literal T008.4 requirement, so this is strengthening evidence rather than a third blocker.

## Passing evidence

Focused engine/server suites, the 2,000 stale-queue trials, focused race suites, an independent failed-response-loss probe, the complete repository suite, compile-only checks, vet, staticcheck, module verification, formatting, diff, ownership, and no-persistence-change checks all pass.
