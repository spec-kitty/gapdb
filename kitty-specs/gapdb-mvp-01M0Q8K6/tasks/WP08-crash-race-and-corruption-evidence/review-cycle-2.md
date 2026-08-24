---
affected_files:
- path: tests/crash/fault_matrix_test.go
- path: tests/crash/ledger_test.go
- path: tests/crash/process_test.go
- path: docs/evidence/crash/results.json
- path: docs/evidence/crash/README.md
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1 -v
reviewed_at: '2026-08-24T00:25:12Z'
reviewer_agent: codex-review-3
verdict: rejected
wp_id: WP08
---

# WP08 Review — Final Repair Re-review

**Verdict: Changes requested.** The actual attempted-revision repair closes the
original authority mismatch, and ordinary ledger corruption is now rejected.
Two deletion/adversarial checks still show that the evidence can approve a
weakened oracle.

## Issue 1 — A coherent ledger and sidecar rewrite can downgrade durable authority

The SHA-256 chain and terminal digest detect edits to the JSONL file only while
the separately stored digest remains unchanged. Both files are mutable inputs in
the same directory, and `readProcessLedger` receives no independently retained
expected digest or acknowledgement contract.

A temporary reviewer probe created the normal two-frame intent/response ledger
at revision 7, changed only the response acknowledgement from `durable` to
`memory`, recomputed every frame hash and predecessor, and rewrote the terminal
sidecar with the resulting final hash and entry count. `readProcessLedger`
accepted the rewritten evidence, and `reconcile` accepted it against public
records `a` and `b` at revision 7. This downgrade can mask a lost durable
acknowledgement. By contrast, absent, malformed, truncated, and stale sidecars
all failed closed, and a re-chained revision edit with the original sidecar was
rejected as intended.

Bind the terminal digest to evidence the parser does not derive again from the
same mutable files. In the subprocess harness, the surviving parent can retain
the expected terminal digest returned by the ledger writer and require it when
reconciling after the child's SIGKILL. Also validate the expected requested
acknowledgement mode independently of the ledger's claimed response class. Add
a committed probe that coherently rewrites both the chain and sidecar for a
revision and acknowledgement-class change: the revision must fail public
record-revision reconciliation, and the class downgrade must fail the
independent request/response contract. Until then,
`ledger_tamper_and_contradiction_rejected: true` overclaims the result.

## Issue 2 — The successful-response reservation guard is not deletion-sensitive

`runInjectedMutation` currently preserves a successful response revision and
returns `EvidenceError` when it differs from `ReservedRevisionEnd + 1`; that is
the correct check. However, deleting that entire mismatch branch left the full
official 1,024-schedule suite and both focused revision tests green with the
same counts. The focused `map.apply/before` test exercises only the
unacknowledged path, so no test can falsify a successful response that carries a
revision inconsistent with the independently captured reservation boundary.

Factor the binding into a small oracle helper or expose a test seam that can
supply a successful response revision. Assert that equality preserves the exact
response revision and that zero, lower, higher, and overflow-adjacent mismatches
fail the schedule. Deleting the mismatch guard must then fail the focused test.

## Confirmed closures and independent gates

- The production `map.apply/before` probe now reports reserved end 1,048,576,
  attempted and public recovered record revision 1,048,577, and the next durable
  revision 2,097,153. Forcing post-recovery reuse of 1,048,577 fails official
  reconciliation. Replacing/removing public record-revision comparison also
  fails the focused oracle test.
- Each recovered key's value and public record revision is compared with its
  oracle commit. Single-field revision, key, value, class, trailing JSON,
  invalid-transition, and duplicate-acknowledgement probes reject. Removing the
  terminal comparison makes the re-chained revision test fail.
- The clean acceptance run completed all 1,024 schedules in 9.83 seconds with
  78 point/phase pairs, 2,433 durable, 812 memory, and 119 unacknowledged
  commits; injected counts were 9, 4, and 119. Seeds and counts match
  `results.json`; commit `c2d079f996a5e17b3cac3c8ac7c953c38c90f7b3`
  and the recorded Go, kernel, filesystem, CPU, and memory metadata match.
- Focused revision/ledger tests passed ten times normally and ten times under
  race. Full normal and race suites passed. `go vet`, `staticcheck`,
  `govulncheck`, `go mod verify`, tidy diff, `gofmt`, and `git diff --check`
  passed on Go 1.26.7. All three five-second fuzz targets passed. Release binary
  strings omit the crash-evidence environment hook while the tagged binary
  contains it.
- Earlier classification-source, recovery-observation, exact SIGKILL, all-limit
  max/max+1, watch ordering/lag/disconnect/deadline/shutdown, and real-process
  findings remain closed through the full suite.

Anti-pattern checklist: dead code PASS; synthetic-fixture/deletion sensitivity
FAIL (successful-response reservation binding); silent empty return N/A; FR
coverage FAIL (FR-010 durable evidence can be downgraded by a coherent ledger
rewrite); frozen surface PASS; locked decision PASS; shared-file ownership PASS;
production fragility PASS.
