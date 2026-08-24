---
affected_files:
- tests/crash/fault_matrix_test.go
- tests/crash/ledger_test.go
- tests/crash/process_test.go
- docs/evidence/crash/results.json
- docs/evidence/crash/README.md
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1 -v
reviewed_at: '2026-08-23T23:57:25Z'
reviewer_agent: codex-review-2
verdict: rejected
wp_id: WP08
---

# WP08 Review — Repair Cycle 2

**Verdict: Changes requested.** The repair closes most cycle-1 findings, but
the revision and external-ledger oracles still accept authority evidence that
does not match what the product actually committed.

## Issue 1 — The fault oracle records the wrong attempted revision and cannot prove non-reuse

`runInjectedMutation` assigns a response-lost operation
`nextExpectedRevision(result.Commits)`. That is the prior committed maximum plus
one, but a clean reopen durably reserves a new revision range and burns the
unused part of the previous range. It is therefore not the revision actually
attempted by the injected operation.

An independent public-path probe selected `map.apply/before`, ran the production
schedule, reopened normally, and read the recovered target record. The oracle
recorded attempted revision `5`, while public `Get` reported revision `1048577`;
the recovered authority was `1048577` and the post-recovery durable revision was
`2097153`. The official 1,024-schedule suite nevertheless passes. Consequently,
`reconcile`'s `seen[PostRevision]` check cannot detect a product that illegally
reuses the real attempted/burned revision: it compares against `5`, not
`1048577`. The recovery observation also retains values but not each record's
public revision, so the mismatch is invisible.

Capture the actual attempted revision from independent durable reservation
evidence before admission (for example, the pre-operation public reserved-range
boundary) and replace the synthetic `nextExpectedRevision` value. Preserve the
actual response revision when a response exists. Record public `Get` revisions
in the recovery observation and require every recovered effect to carry its
oracle commit revision. Add a deletion/adversarial test in which the
post-recovery allocator returns the real attempted revision and prove the
official oracle rejects that reuse.

## Issue 2 — Valid-shaped ledger revision and class tampering is accepted

`readProcessLedger` strictly parses JSON and rejects internally contradictory
lines, but the ledger has no checksum/hash-chain or independent digest and the
semantic oracle does not compare public record revisions. The committed tamper
test changes only the first of two revision/effect entries, so it proves
contradiction detection rather than integrity of the persisted authority
evidence.

A reviewer probe changed both lines from revision `7` to revision `6`; parsing
and reconciliation against public revision `7` and the recovered value passed.
Changing the response classification from `durable` to `memory` also passed.
Both are valid-shaped changes to authority evidence that `results.json` and the
README currently claim are rejected. The same weakness means corruption can
lower a revision or weaken an acknowledgement class without detection whenever
the effects happen to be present.

Give the synced external ledger independently verifiable integrity (for example,
CRC-framed/hash-chained records plus a separately durable terminal digest), and
validate it before using entries as oracle input. Compare ledger revisions with
the public record revisions as well as global authority. Add independent tests
that mutate revision, key/value, acknowledgement class, trailing JSON, and
transition/duplicate entries one at a time; each must fail for the intended
integrity or semantic reason. Update the evidence manifest only after these
valid-shaped mutation probes fail.

## Confirmed closures and gates

- Disconnecting `classifyAcknowledgement` now makes the official 1,024 suite
  fail with zero durable schedules. The clean rerun reports exactly 2,433
  durable, 812 memory, and 119 unacknowledged commits; injected-operation counts
  are 9, 4, and 119 across all 78 point/phase pairs.
- Disconnecting stable fail-closed evidence makes the smoke matrix fail on the
  first unavailable identity recovery. Repeated structured code and tree-digest
  checks are now authoritative for those schedules.
- Replacing the evidence hook's SIGKILL with `os.Exit(17)` now fails exact
  `WaitStatus` verification.
- All twelve configured limit fields have max/max+1 coverage; total request
  frames and bounded compaction diagnostics are exercised.
- Exact watch ordering, whole-group lag evidence, a live disconnect, accepted
  deadline, concurrent shutdown, and restart reconciliation pass in focused
  repetitions and under race.
- Release binary strings contain no evidence hook/environment names; the tagged
  evidence binary contains them. The cross-owner rationale is recorded in
  `dc8f850`.
- The clean 1,024 acceptance run passed in 10.92s. Focused tests passed x10 and
  focused race tests x5. Full normal and race suites, vet, staticcheck,
  govulncheck, module verification/tidy diff, formatting/diff checks, and all
  three five-second fuzz targets pass on Go 1.26.7. Commit and environment
  metadata match; rerun duration is within ordinary host variance.

Anti-pattern checklist: dead code PASS; synthetic-fixture/deletion sensitivity
FAIL (revision/ledger authority); silent empty return N/A; FR coverage FAIL
(FR-008 non-reuse evidence); frozen surface PASS; locked decision PASS;
shared-file ownership PASS; production fragility PASS.
