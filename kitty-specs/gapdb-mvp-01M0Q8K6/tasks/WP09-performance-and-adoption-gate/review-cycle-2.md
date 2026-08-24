---
affected_files:
  - tests/performance/benchmark_test.go
  - tests/performance/recovery_test.go
  - tests/adoption/contract.go
  - tests/adoption/manifest.go
  - tests/adoption/manifest_test.go
  - tests/adoption/docs_test.go
  - docs/evidence/performance/results.json
  - docs/evidence/performance/raw.txt
  - docs/evidence/performance/release-manifest.json
  - docs/evidence/performance/README.md
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: GAPDB_REFERENCE_ACCEPTANCE=1 go test ./tests/performance -run TestReferencePerformanceProfile -count=1 -v
reviewed_at: '2026-08-24T01:23:55Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP09
---

# WP09 Independent Review

Verdict: changes requested. The numerical workload passes on the current host,
the in-repository Gapdb adapter passes all 14 declared scenarios, and the
automated adoption result correctly keeps SQLite selected and human approval
false. Three evidence-authority gaps prevent WP09 from satisfying T045, T047,
and T050.

## Blocking findings

### 1. The benchmark does not prove its concurrency or durable-sync claims

**Severity:** High  
**Affected code:** `tests/performance/benchmark_test.go`, reference evidence

`measureGets` releases eight readers and the writer through one start channel,
but it has no ready/active barrier proving that the writer began before any
reader samples. `measureWrites` starts eight background readers and immediately
begins writer samples without waiting for even one read from each goroutine.
The ordinary `BenchmarkReferenceUnixSocket` is weaker still: its `Get`
subbenchmark has readers but no writer, while each write subbenchmark has a
writer but no concurrent readers. A scheduler can therefore produce measured
windows that never contain the required eight simultaneous readers plus one
writer.

Durability evidence is also synthesized rather than observed:
`SuccessfulDurableBarriers` is assigned `durable_put.samples + 20`. No counter,
filesystem observation, or fault seam supplies those 20 or proves the 128
sampled calls each crossed an OS sync. The public `StatsResult.SyncCount` field
has no production update site, and the reference harness never reads it. A
string literal declares `Filesystem: "os_fsync_enabled"`; no committed deletion
or substitution test fails if a fake filesystem, disabled sync, or direct
in-process data path replaces the required production path. This directly
misses T045's explicit validation requirement while the performance README
claims those substitutions are rejected.

The evidence is not reproducible as declared. A fresh official run passed all
thresholds but produced snapshot revision 1484 versus checked-in 1473. That
field is not listed as volatile. The difference comes from the unbounded
background memory writer: its scheduling changes how many revisions exist
before recovery. Add explicit participant-ready and operation-start evidence,
measure a bounded deterministic mixed window, derive sync counts from an
independent production observation/fault-sensitive seam, and make the specified
substitutions fail. Either make revision/count evidence deterministic or mark
and validate all legitimately volatile fields without weakening workload
authority.

### 2. The response-loss adoption scenario never loses a response

**Severity:** High  
**Affected code:** `tests/adoption/contract.go`, `Backend`, `runResponseLoss`

`runResponseLoss` receives a definite successful durable `Put` response and
then discards only the returned revision before a graceful `Restart`. It does
not interrupt or lose a response, receive an ambiguity error, or reconcile an
operation whose application is unknown. The backend contract exposes no
response-loss/fault seam, so an external SQLite or Gapdb adapter cannot make
this scenario exercise the required ambiguity at all. Consequently the checked
`adoption.json` reports `response-loss/reconcile` as passed without testing
response loss, and a backend can satisfy this case with an ordinary successful
put/restart/get sequence.

Expose a portable response-loss operation or runner-controlled cut point that
both external adapters can implement without depending on Gapdb internals.
Require loss/ambiguity to be observed, restart independently, reconcile the
exact key/value and authority evidence, and make a backend that returns an
ordinary successful response fail this scenario. Keep the current strict
scenario-ID/backend/version validation and explicit missing-SQLite state.

### 3. The release manifest authenticates bytes but not their semantic authority

**Severity:** Critical  
**Affected code:** `tests/adoption/manifest.go`, manifest tests and checked evidence

The checked release manifest labels every evidence reference with WP09 commit
`eabfc76`, including `docs/evidence/crash/results.json`; that crash document
internally identifies the tested code as `d34eada`. `ValidateReleaseManifest`
compares only the outer reference's copied `code_commit`, count, and digest. It
does not strictly decode the referenced result, compare its internal commit,
configuration, command, scenario/schedule count, or claimed outcome, or verify
that the evidence is relevant to the criterion that cites it.

An independent temporary probe copied the checked manifest/evidence, changed
the crash document's internal `code_under_test_commit` to all zeroes, recomputed
the referenced SHA-256 and byte count, and called `ValidateReleaseManifest`.
The validator returned success. The probe was then removed. The same design
trusts `ObservedCount`, `coverage`, and every criterion's `status` as manifest
literals. The committed deletion tests use synthetic `one\n`/`two\n` files and
prove only envelope hashing, not SC-001--SC-009 or NFR-001--NFR-012 evidence
meaning. Thus stale or unrelated evidence can be re-signed locally and still
mark `mvp_status: complete`.

Define strict schemas/validators for each evidence kind, bind their internal
code/config/command/count/result identity to the release manifest, and enforce
criterion-to-evidence relevance. Add checked-in adversarial tests that mutate
each semantic field, recompute the outer digest/size, and still fail. Preserve
the correct separation already present: MVP completion may coexist with SQLite
pending, `production_backend: sqlite`, and `adoption_status: not_approved`.

## Confirmed working behavior

- The official reference test used a real `0600` Unix socket, 100,000 reported
  live records, 1,024-byte generated values, snapshot plus exactly 10,000 later
  mutations, and public reopen/status/get. On this host, Get p95 was 0.953 ms,
  memory Put p95 0.545 ms, durable Put p95 2.953 ms, and readiness 539.163 ms;
  all declared thresholds passed.
- The ordinary 100-iteration benchmark ran through the public socket API and
  reported allocations for Get, memory Put, and durable Put.
- The Gapdb portable adapter passes all 14 current scenarios; the intentionally
  empty backend fails. Exact scenario-set, duplicate-ID, backend, contract
  version, and pass-state validation is present.
- Missing SQLite evidence remains explicit and cannot set human approval or the
  production backend to Gapdb through `EvaluateAdoption`.
- Documentation links resolve, the locked operation/limit/permission tokens are
  present, and memory acknowledgement is explicitly documented as not durable.

## Independent gates

- Toolchain: `go1.26.7 linux/amd64`.
- Passed: `go test -count=1 ./...` and `go test -race -count=1 ./...`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no
  vulnerabilities), `go mod verify`, full `gofmt` check, and `git diff --check`.
- Passed focused: adoption suite, official reference acceptance test, and the
  ordinary reference benchmark at `-benchtime=100x`.
- Passed one-second fuzz runs: snapshot decode (168,981 executions), storage
  decoders (30,775), combined wire/storage decoders (25,033), cursor decoder
  (25,985), and backup verifier (7,822).
- The only untracked lane state is the runtime-owned `.spec-kitty/review-lock.json`;
  the temporary semantic-manifest reviewer probe was removed.

## WP anti-pattern checklist

1. **Dead code:** PASS for the exported adapter runner and manifest validator.
2. **Synthetic-fixture test:** FAIL -- manifest authority tests validate
   arbitrary tiny files rather than the semantics of release evidence.
3. **Silent empty return:** PASS.
4. **FR coverage:** FAIL -- response-loss reconciliation and authoritative
   benchmark concurrency/sync evidence are not exercised.
5. **Frozen surface:** PASS -- WP09 does not alter approved protocol/storage
   implementation contracts.
6. **Locked decision:** FAIL -- stale evidence can mark MVP complete and the
   benchmark claims successful syncs without an independent observation.
7. **Shared-file ownership:** PASS -- WP09 changes are isolated to its evidence,
   documentation, and test harness surfaces over approved ancestry.
8. **Production fragility:** PASS -- no new production data-path implementation
   is introduced; the blockers are release/adoption authority defects.
