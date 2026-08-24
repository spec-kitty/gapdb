---
affected_files:
  - tests/adoption/evidence.go
  - tests/adoption/docs_test.go
  - docs/evidence/performance/results.json
  - docs/evidence/performance/raw.txt
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./tests/adoption -run TestSemanticEvidenceAdversarialRewriteMatrix -count=1 -v
reviewed_at: '2026-08-24T02:10:00Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP09
---

# WP09 Cycle-2 Independent Re-review

Verdict: changes requested. Commits `3a98149` and `83979e4` close the three
original review findings at their production boundaries. One new semantic
evidence gap still permits coherently re-signed, nonvolatile performance claims
to mark the schema-v2 release manifest complete.

## Blocking finding

### Performance evidence decoding is not strict or exact for nonvolatile authority

**Severity:** High  
**Affected code:** `tests/adoption/evidence.go`, `validatePerformanceEvidence`,
`validateRawEvidence`

The new caller-side `ReleaseAuthority` correctly prevents an evidence file from
rewriting its own source commit, configuration, command, or criterion mapping.
However, several nonvolatile fields are not part of those pins and their kind
decoders accept altered values:

1. `validatePerformanceEvidence` requires only
   `observed_wal_syncs >= successful_durable_barriers`. A temporary reviewer
   probe changed the checked result from the independently reproduced value 156
   to 157, recomputed its SHA-256/size in the outer manifest, and
   `ValidateReleaseManifest` returned success. The repair's own adversarial
   matrix tests only 150, which is below 151 and therefore does not deletion-test
   an altered but plausible sync claim.
2. `validateRawEvidence` decodes the one-line `REFERENCE_RESULT` once but never
   performs a second decode requiring EOF. Appending a second JSON value (`{}`)
   on that same physical line, then recomputing the raw evidence digest/size,
   was accepted by the public manifest validator.
3. The raw semantic check decodes `schema_version` but never validates it. A
   coherent `schema_version:1` to `schema_version:2` rewrite was also accepted.
   The same branch omits exact checks for other raw workload identity fields
   already present in the type, including seed, transport, filesystem, and
   socket mode.

All three attacks used the checked manifest and caller-side authority, changed
only the copied evidence plus its outer digest/size, and passed
`ValidateReleaseManifest`. The temporary reviewer probe was removed afterward.
This contradicts T050's stale/mismatched evidence gate and the fresh repair
claim that strict kind decoders reject coherent sync/workload/raw rewrites.

Require EOF after decoding the raw `REFERENCE_RESULT`; validate its schema,
seed, transport, filesystem, socket mode, metric ordering, recovery target, and
all other nonvolatile identity/outcome fields to the same standard as
`results.json`. Bind the exact independently reproduced WAL-sync count (156 for
this recorded run) or another explicit caller-side expected relation that
rejects both lower and higher fabricated counts. Add committed coherent rewrite
probes for 155/157, raw trailing JSON, schema, seed, transport, filesystem, and
socket mode; deleting any new guard must make the focused test fail.

## Original findings independently closed

- **Benchmark concurrency and sync provenance:** closed. All three reference
  windows use fixed ready -> active -> work barriers for exactly eight readers
  and one writer, with fixed operation budgets. The ordinary benchmark uses the
  same mixed-window runner. The pass-through `faultfs.OS` is the exact
  configured production filesystem and records after-events only following a
  successful underlying `Sync`. The real public socket inode is checked at
  `0600`, every client exposes that same `SocketPath`, and fake/no-sync/direct
  substitutions fail. A Before-sync injection produces no After event and no
  durable success.
- **Deterministic reference profile:** closed. Two fresh official runs both
  reported 151 successful durable operations, 156 successful WAL syncs,
  snapshot revision 1329, exactly 10,000 later WAL commits, exact fixed window
  budgets, and all four targets passing. Only documented latency/readiness
  fields varied. The ordinary 100-iteration benchmark reproduced all three
  operations and allocations through the Unix client.
- **Response-loss reconciliation:** closed. The Gapdb adapter writes a canonical
  durable request over a raw Unix connection, closes the read direction before
  decoding any response, returns only stable portable ambiguity, independently
  observes request acceptance, restarts, and reconciles exact key/value/public
  revision plus durable-through authority. Ordinary success, no apply, wrong
  value, and weak durable authority all fail. The seam remains portable for an
  external adapter.
- **Manifest source/criterion authority:** closed except for the blocker above.
  Schema v2 uses caller-side pins not decoded from artifacts; performance/raw/
  adoption evidence binds to `3a98149`, inherited crash evidence binds internally
  and externally to `d34eada`, and the criterion command/evidence map is exact
  for SC-001--SC-009 and NFR-001--NFR-012. Commit/config/command/count/status,
  crash schedule/class/hook, adoption scenario/SQLite/human, document-token,
  deletion, symlink, path, oversize, and ordinary byte-tamper probes fail.
- **Adoption and docs:** SQLite remains `production_backend: sqlite`, technical
  gates remain incomplete without the external result, and automated/human
  adoption remains `not_approved`. Links, format tokens, operation names,
  limits, permissions, memory-durability warning, and recovery references pass.

## Independent gates

- Toolchain: `go1.26.7 linux/amd64`.
- Focused performance/adoption suites passed 10 times normally and 10 times
  under `-race`.
- Official reference profile passed twice; Get p95 was 0.637/0.808 ms, memory
  Put p95 0.537/0.502 ms, durable Put p95 1.648/1.270 ms, and recovery readiness
  400.725/545.184 ms. Nonvolatile budgets, revisions, durable successes, and
  sync observations were identical.
- The ordinary reference benchmark passed at `-benchtime=100x` with allocation
  output for Get, memory Put, and durable Put.
- Passed uncached: `go test -count=1 ./...` and
  `go test -race -count=1 ./...`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no
  vulnerabilities), `go mod verify`, `go mod tidy -diff`, complete `gofmt`, and
  `git diff --check`.
- Passed fuzz: snapshot decoder (374,778 executions), storage decoders (21,852),
  combined wire/storage (34,467), cursor decoder (19,944), and backup verifier
  (15,996).
- No reviewer product change or temporary probe remains in lane-i.

## WP anti-pattern checklist

1. **Dead code:** PASS -- the exported contract/manifest surfaces have live
   callers in the review/release harness.
2. **Synthetic-fixture test:** PASS -- benchmark, response-loss, and manifest
   tests invoke the production socket/filesystem/validator paths.
3. **Silent empty return:** PASS.
4. **FR coverage:** FAIL -- T050/NFR evidence authority does not strictly reject
   coherent nonvolatile raw/sync rewrites.
5. **Frozen surface:** PASS.
6. **Locked decision:** FAIL -- a coherently altered evidence document can still
   support `mvp_status: complete`.
7. **Shared-file ownership:** PASS -- repairs remain within WP09-owned test,
   evidence, and documentation surfaces over approved ancestry.
8. **Production fragility:** PASS -- no product request/owner code is changed by
   this repair.
