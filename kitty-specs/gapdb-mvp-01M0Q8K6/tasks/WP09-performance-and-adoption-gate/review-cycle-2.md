---
affected_files:
  - tests/adoption/docs_test.go
  - tests/adoption/evidence.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./tests/adoption -run TestSemanticEvidenceAdversarialRewriteMatrix -count=1
reviewed_at: '2026-08-24T02:42:28Z'
reviewer_agent: reviewer-renata
verdict: approved
wp_id: WP09
---

# WP09 Final Independent Re-review

Verdict: approved. Repair `0106707` closes the sole remaining deletion-
sensitivity blocker without weakening the previously approved benchmark,
adoption, evidence-authority, or documentation behavior.

## Allocation-ceiling closure

- The committed adversarial matrix coherently rewrites the results metric and
  the raw `REFERENCE_RESULT` allocation value to `1,000,001`, then recomputes
  the corresponding outer digest and size. Both inputs are rejected by
  `ValidateReleaseManifest`.
- An independent deletion probe removed only the results-evidence
  `AllocsPerOp > 1_000_000` clause. The results allocation subtest failed while
  the raw allocation subtest passed.
- A second independent deletion probe restored the first guard and removed
  only the raw-evidence clause. The raw allocation subtest failed while the
  results allocation subtest passed.
- Both guards were restored; the complete semantic matrix passed and lane-i
  returned to the exact committed tree.
- A temporary reviewer-only test rewrote the results JSON through the
  production test helper and decoded its seed as `uint64`. The value remained
  exactly `5134473304279631186`. Inspection confirms `json.Decoder.UseNumber`
  retains integer lexemes during coherent re-signing. The probe was removed.

These checks establish both error-path reachability and independence: deleting
either upper ceiling is detected by exactly its corresponding committed case.

## Preserved semantic and runtime evidence

- The complete coherent-rewrite matrix continues to reject source/config/
  command/count/status changes, exact-sync changes to 155 or 157, raw trailing
  JSON, schema/seed/transport/filesystem/socket changes, metric identity/order/
  budget/outcome changes, recovery revisions/count/target/order/outcome changes,
  adoption scenario/SQLite/human changes, and forged criterion mappings.
- Two fresh official reference-profile runs retained exactly 100,000 live
  1-KiB records, eight active readers plus one writer, 151 durable successes,
  156 observed successful WAL-sync after-events, snapshot revision 1329, and
  exactly 10,000 later WAL commits. All latency and readiness targets passed.
- The prior real-socket, pass-through OS filesystem, sync-fault, response-loss,
  restart reconciliation, caller-pinned release authority, and SQLite
  `not_approved` adoption findings remain closed under the full suites.

## Independent verification

- Toolchain: `go1.26.7 linux/amd64`.
- Passed the complete semantic rewrite/manifest matrix and focused performance
  and adoption packages 10 times normally and three times under `-race`.
- Passed the official reference profile twice. Get p95 was 0.648/0.835 ms,
  memory Put p95 0.520/0.692 ms, durable Put p95 1.058/2.349 ms, and recovery
  readiness 507.442/366.456 ms.
- Passed uncached `go test -count=1 ./...` and
  `go test -race -count=1 ./...`.
- Passed documentation-link checks x10, protocol schema, CLI/help, `go vet`,
  `staticcheck`, `govulncheck` (no vulnerabilities), `go mod verify`,
  `go mod tidy -diff`, complete `gofmt`, and `git diff --check`.
- Passed two-second fuzz gates for snapshot, storage, wire/storage, cursor, and
  backup verification decoders (respectively 108,941; 21,526; 18,053; 16,552;
  and 4,447 executions in this run).
- No reviewer implementation or probe artifact remains in lane-i.

## WP anti-pattern checklist

1. **Dead code:** PASS.
2. **Synthetic-fixture/deletion sensitivity:** PASS; each allocation ceiling
   has a coherent outer rewrite and independent deletion proof.
3. **Silent empty return:** PASS.
4. **FR coverage:** PASS for T045-T050 and SC-001-SC-009/NFR-001-NFR-012.
5. **Frozen surface:** PASS.
6. **Locked decision:** PASS; SQLite remains production and human approval is
   not artifact-mutable.
7. **Shared-file ownership:** PASS.
8. **Production fragility:** PASS.
