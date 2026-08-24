---
affected_files:
  - tests/adoption/docs_test.go
  - tests/adoption/evidence.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./tests/adoption -run TestSemanticEvidenceAdversarialRewriteMatrix -count=1
reviewed_at: '2026-08-24T02:28:36Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP09
---

# WP09 Final Independent Re-review

Verdict: changes requested for one narrow deletion-sensitivity blocker. Final
repair `50f68d5` closes every previously demonstrated semantic rewrite, and all
functional and repository-wide gates pass. The new allocation upper-bound
guards are correct but have no test that would fail if those exact guards were
removed.

## Blocking finding

### The new allocation ceilings are not deletion-tested

**Severity:** Medium
**Affected code:** `tests/adoption/evidence.go:190`,
`tests/adoption/evidence.go:274`, `tests/adoption/docs_test.go`

The repair adds `metric.AllocsPerOp > 1_000_000` rejection to both
`validatePerformanceEvidence` and `validateRawEvidence`. That is an appropriate
bound for a machine-readable release claim. However, the adversarial matrix's
only allocation mutation changes raw evidence from 882 to 0. Zero was already
rejected by the pre-existing `metric.AllocsPerOp <= 0` branch; there is no
results-evidence allocation mutation and no value above the new ceiling.

An independent deletion probe temporarily removed only the two new
`> 1_000_000` clauses and ran:

```text
go test ./tests/adoption -run TestSemanticEvidenceAdversarialRewriteMatrix -count=1
```

The complete matrix remained green. The clauses were restored immediately and
the lane returned to its exact committed state. Thus deleting the final
repair's upper-bound behavior is invisible to the committed suite, contrary to
the explicit final-review requirement to deletion-test every new guard and the
review skill's error-path reachability gate.

Add two coherent re-signing cases: change `allocs_per_op` to `1_000_001` in
`performance-results` and in the raw `REFERENCE_RESULT`, recompute the
corresponding outer SHA-256/size, and require `ValidateReleaseManifest` to
reject both. Temporarily deleting either upper-bound clause must then fail its
own focused subtest. Keep the existing zero-allocation tests as lower-bound
coverage.

## Prior findings independently closed

- **Exact nonvolatile semantic authority:** closed. Coherent
  `observed_wal_syncs` rewrites to 155 and 157 fail for both results and raw
  evidence. The independently compiled validator pins 156 rather than deriving
  it from mutable evidence. Raw decoding requires EOF and rejects mutations to
  schema, seed, transport, filesystem, socket mode, metric identity/order/
  budgets/outcome, window identity/budgets/readiness/activity, recovery
  revision/count/target/order/outcome, and reference outcome.
- **Real reference run:** closed. Two fresh runs both produced exactly 151
  durable successes, 156 successful WAL-sync after-events, snapshot revision
  1329, and exactly 10,000 later WAL commits. Both retained exact eight-reader/
  one-writer window budgets and passed all latency/recovery targets.
- **Socket/concurrency/sync provenance:** closed. Reference and ordinary windows
  use the ready -> active -> work barrier; filesystem identity is the exact
  pass-through `faultfs.OS`; public clients bind the real `0600` Unix socket;
  fake/no-sync/direct substitutions fail; a Before-sync fault yields neither an
  After event nor durable success.
- **Response loss:** closed. The adapter writes a canonical durable raw Unix
  request, closes response reading before decode, returns portable ambiguity,
  independently observes acceptance, restarts, and reconciles exact value,
  public revision, and durable authority. Ordinary success, no application,
  wrong value, and insufficient durable-through evidence all fail.
- **Manifest trust:** closed apart from the deletion-test blocker. Caller-side
  authority pins are not artifact-derived; performance/raw/adoption bind to
  `3a98149`, crash binds internally and externally to `d34eada`, and exact
  criterion relevance covers SC-001--SC-009 and NFR-001--NFR-012. Checked
  evidence deletion, ordinary tampering, coherent source/config/command/count/
  status rewrites, crash schedule/class/hook changes, adoption scenario/SQLite/
  human changes, and document token changes fail closed.
- **Adoption/docs:** SQLite remains the production backend and adoption remains
  `not_approved`, including when technical results could be complete. Links,
  help/schema examples, operations, limits, permissions, format identifiers,
  and the memory-not-durable warning pass their focused checks.

## Independent verification

- Toolchain: `go1.26.7 linux/amd64`.
- Focused performance/adoption suites passed 10 times normally and 10 times
  under `-race`.
- Official reference profile passed twice: Get p95 0.692/0.790 ms, memory Put
  p95 0.446/0.680 ms, durable Put p95 1.318/2.657 ms, readiness
  433.697/547.261 ms. Nonvolatile counts and revisions were identical.
- Ordinary `BenchmarkReferenceUnixSocket` passed at `-benchtime=100x` with
  `ns/op`, `B/op`, and `allocs/op` for all three operations.
- Passed uncached: `go test -count=1 ./...` and
  `go test -race -count=1 ./...`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no
  vulnerabilities), `go mod verify`, `go mod tidy -diff`, complete `gofmt`,
  `git diff --check`, documentation x10, protocol schema, and CLI/help tests.
- Passed fuzz: snapshot decoder (374,358 executions), storage decoders (27,955),
  wire/storage decoder (21,641), cursor decoder (25,649), and backup verifier
  (14,357).
- Temporary deletion mutations were restored; no reviewer product/probe change
  remains in lane-i.

## WP anti-pattern checklist

1. **Dead code:** PASS.
2. **Synthetic-fixture/deletion sensitivity:** FAIL -- removing both newly added
   allocation upper-bound guards leaves the complete semantic rewrite matrix
   green.
3. **Silent empty return:** PASS.
4. **FR coverage:** FAIL narrowly for T050's bounded semantic evidence guard.
5. **Frozen surface:** PASS.
6. **Locked decision:** PASS for shipped behavior; the blocker is missing proof
   that the bound cannot regress.
7. **Shared-file ownership:** PASS.
8. **Production fragility:** PASS.
