---
affected_files:
- tests/crash/fault_matrix_test.go
- tests/crash/ledger_test.go
- tests/crash/process_test.go
- tests/crash/race_test.go
- tests/crash/safety_test.go
- docs/evidence/crash/results.json
- docs/evidence/crash/README.md
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1 -v
reviewed_at: '2026-08-23T23:25:39Z'
reviewer_agent: codex-review-1
verdict: rejected
wp_id: WP08
---

# WP08 Review — Cycle 1

**Verdict: Changes requested.** The ordinary test and static gates pass, but the
acceptance evidence is not deletion-sensitive to several of the claims it records.

## Issue 1 — The 1,024-schedule acknowledgement/recovery oracle can pass after its evidence wiring is broken

`tests/crash/fault_matrix_test.go` does not prove the three recovery classes it
reports. The schedule's `Class`, `Revision`, and `ShouldLive` fields are not used
as independent expectations. A reviewer probe changed the successful-response
branch in `runInjectedMutation` to never classify an actual response; the complete
`TestAcceptanceDeterministicFaultSchedules` suite still passed all 1,024 schedules.
An independent count over the real `oracleCommit` values produced by those
schedules was `durable=13, memory=0, unacknowledged=119`, while both the test log
and `results.json` claim durable, memory, and unacknowledged coverage. Thus there
is no recovered, actually acknowledged memory-mode operation in the acceptance
matrix.

The suite also ignores failed observation for initialization schedules. A probe
requiring the public uninjected reopen result for the 128-schedule smoke matrix
found 26 identity/WAL-header schedules with no public observation
(`RECOVERY_REQUIRED` or `IO_ERROR`); they pass because `RequiresRecovery` remains
false. Revision reuse is not tested either: the failed mutation's revision is
hard-coded to 2 and no post-recovery successful commit is issued to distinguish a
legally unused revision from an illegally reused burned revision.

Repair the oracle so schedule inputs drive real histories that include successful
memory and durable acknowledgements plus response-lost operations, and derive the
class only from the actual response. Count the produced classes and fail if any is
absent. Require every schedule to reconcile either public reopened state or an
explicit expected fail-closed public recovery error and filesystem outcome. Add a
post-recovery commit/restart check for revision non-reuse, and include the promised
condition, expiry, and multi-operation history cases. Remove or make authoritative
the currently dead expectation fields. Add deletion probes that fail when response
classification or recovered `Status`/`Get` observation is disconnected.

## Issue 2 — The subprocess ledger and crash-kind claims are not part of the oracle

`TestGapdbdCrashLedgerAndRestart` validates public recovery against in-memory
variables, then only parses the external ledger for a line count and nonempty
milestone. A reviewer probe changed the ledger's durable revision to
`batch.Revision+999` and its keys to `bogus/not-recovered`; the test still passed.
The per-hook `hook-ledger.jsonl` is likewise written but never read to drive restart
reconciliation. This does not prove that an independently persisted acknowledgement
ledger agrees with public recovered state.

`waitForCrash` also accepts any nonzero process exit. Replacing the evidence hook's
`SIGKILL` with `os.Exit(17)` left all 14 hooked subprocess milestone cases passing,
so the recorded assertion that these are abrupt SIGKILL crashes is unverified.

After restart, reopen and strictly decode the synced ledger from disk and derive
the required/optional commit set from it rather than from retained Go variables.
Make corrupt, stale, or contradictory ledger content fail the test, and fsync the
ledger directory if its pathname durability is part of the claim. Inspect
`ProcessState.Sys()`/`WaitStatus` and require termination by `SIGKILL` at every
crash milestone. Keep graceful termination as a separately asserted path.

## Issue 3 — Bounds and concurrency evidence overclaim the exercised surface

`TestEveryConfiguredBoundaryAtMaximumAndMaximumPlusOne` configures but never tests
the max/max+1 behavior for `MaxBatchBytes`, `MaxScanBytes`,
`MaxConcurrentClients`, `WatchBufferEvents`, `MaxHistoryEvents`, or
`MaxHistoryBytes`; it also does not exercise the promised total request or bounded
diagnostic limits. Nevertheless `results.json` records
`configured_limits_at_max_and_max_plus_one: true`.

`TestFullStackRaceStress` consumes its watch normally and checks only that observed
revisions do not regress. It does not force watcher lag, prove an exact no-loss
event sequence, inspect the terminal lag evidence, or race an actual disconnect,
deadline, and shutdown as required by T042. The README claim that lag, deadline,
and shutdown activity is covered is therefore stronger than the test.

Exercise every configured limit at max and max+1 with exact codes, no-mutation or
bounded-growth assertions, and explicit diagnostic truncation. Add a deterministic
full-stack lag/disconnect/deadline/shutdown matrix that compares the complete watch
sequence and terminal evidence against an independent commit ledger. Generate the
machine-readable booleans/counts from observed test results, and do not publish a
headline claim until its deletion-sensitive assertion exists.

## Governance and checks

The release binary correctly excludes the evidence environment strings and the
evidence-tagged binary includes them. All 78 declared point/phase hooks were
observed. The full normal/race suites, focused x10 repetitions, vet, staticcheck,
govulncheck, module, format/diff checks, and all three bounded fuzz targets passed
on Go 1.26.7. Environment and code-under-test commit fields match the rerun.

The WP also changes shared production files outside `owned_files` without the
required one-line crossing rationale in commit `64014cc`. Record the narrow reason
for the build-tagged evidence seams and their coordination with the owning WPs in
the repair handoff.

Anti-pattern checklist: dead code PASS; synthetic-fixture/deletion sensitivity
FAIL; silent empty return N/A; FR coverage FAIL; frozen surface PASS; locked
decision PASS; shared-file ownership FAIL (missing rationale); production
fragility PASS.
