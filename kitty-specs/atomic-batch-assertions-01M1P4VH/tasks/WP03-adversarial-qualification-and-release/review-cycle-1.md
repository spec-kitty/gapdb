---
affected_files: []
cycle_number: 1
mission_slug: atomic-batch-assertions-01M1P4VH
reproduction_command:
reviewed_at: '2026-09-04T14:06:17Z'
reviewer_agent: user
wp_id: WP03
---

# WP03 Review Feedback — Cycle 1

**Verdict:** REQUEST CHANGES

**Reviewed evidence commit:** `4808311d809c4a9b171066b1218c64e295a76684`  
**Qualified candidate:** `a5f79adb7cd49c4328d132179b8828a5806e68fa`  
**Reviewer:** independent Reviewer Renata

## Blocking findings

### 1. The 1,000-trial contention oracle does not prove guarded state

`tests/adoption/assertion_contention_test.go:23-31,104-107` classifies a history solely from the returned authority revision and guarded result/error. It never reads the per-trial target. A stale request that writes the target and then returns `CONDITION_FAILED` with revision zero is accepted as a valid failed history; a success response that does not publish its target is likewise accepted. This leaves NFR-001 and SC-003's actual “never permits its guarded mutation” law unproved.

**Required repair:** in every revision and absence trial, reconcile the real target through the Unix client. For the successful serialization require the exact value and guarded commit revision; for the failed serialization require `NOT_FOUND` and unchanged authoritative revision/evidence. Add controlled mutants for mutate-then-error and success-without-publication, not only a client-side omitted-assertion result-shape mutant.

### 2. The watch oracle permits trailing assertion events

`tests/adoption/assertion_transport_test.go:137-184` reads exactly the first two expected mutation events and then closes or returns. An implementation that emits an assertion event after `watch/a` and `watch/b` passes both live and replay checks, yet qualification records `watch_live_mutation_only=true` and `watch_replay_mutation_only=true`.

**Required repair:** delimit each live and replay stream with a known later mutation/barrier (or another deterministic bounded end) and assert the complete event sequence through that delimiter. Add a controlled trailing-assertion-event mutant and prove both live and recovered replay kill it.

### 3. The signed evidence manifest is self-certifying

`tests/adoption/assertion_evidence_test.go:113-148` verifies only `qualification.json` with a public key and signature supplied by the same unsigned manifest. The text artifacts are merely hashed by that manifest, and the required filenames are not exact—only `len(manifest.Files)==4` is enforced. In an isolated clone I appended `tampered performance evidence` to `performance.txt`, updated only its manifest SHA-256, and:

```text
go test ./tests/adoption -run '^TestAssertionQualificationEvidenceIsSignedHashedAndSemanticallySealed$' -count=1
PASS
```

The signature remained untouched. Replacing the public key and re-signing the qualification file is also not anchored outside the mutable envelope.

**Required repair:** anchor the signer key/fingerprint in immutable test code or another independently trusted input, sign the canonical manifest that contains the exact closed filename set and hashes, and reject missing, substituted, extra, symlinked, non-regular, or oversized evidence files. Add composed mutants that alter and rehash a text artifact, substitute a filename, and replace the key/re-sign; each must be killed.

### 4. The performance qualification is neither stable nor independently recomputable

The frozen single run passes, but the same acceptance command failed on 4 of 11 independent runs on the recorded reference machine:

```text
window 1: asserted p95 586.507us, control p95 476.718us, allowance 100us
window 2: asserted p95 433.407us, control p95 331.078us, allowance 100us
window 1: asserted p95 528.987us, control p95 417.678us, allowance 100us
window 1: asserted p95 469.708us, control p95 363.688us, allowance 100us
```

`performance.txt` contains only three already-computed p95 summaries. It omits the 512 memory and 64 durable raw samples per window, exact effective configuration, and a machine-recomputable derivation, so the p95 values cannot be independently reconstructed. The control and asserted target keys also have different lengths (`control` versus `asserted`) at `assertion_performance_test.go:79-83`.

**Required repair:** preserve the ratified thresholds, equalize all non-assertion request inputs, emit bounded machine-readable raw samples plus sample counts/configuration, and independently recompute nearest-rank p95 and allowances from that artifact. Establish a non-cherry-picked repeat rule or otherwise remove the reproduced gate instability before sealing new evidence.

### 5. Boundary qualification does not prove zero-effect production behavior

`TestCombinedOperationAndPublicByteBoundariesLMinusOneLAndLPlusOne` calls only `Batch.Validate`; it never invokes the public database/client mutation path. The Unix JSON L-1 case checks only the error code and returns without checking target absence, current/durable revision, watch/history, or restart state. Thus the evidence booleans can remain true if rejection occurs after an effect.

**Required repair:** exercise L-1/L/L+1 through the public mutation API and Unix protocol. At each rejecting boundary assert bounded typed diagnostics and an exact zero-effect fingerprint including target absence, current/durable revision, watch/history, expiry state where applicable, and normal restart. Add after-effect and off-by-one mutants.

### 6. The recorded formatting gate is narrower than the claimed repository gate

`gates.txt` records `gofmt -l owned test directories`, while T012/NFR-006 require the entire repository gate ladder. The current tree happens to pass `gofmt -l .`, but that full command and result are not what the sealed evidence records.

**Required repair:** run and record the repository-wide formatting gate in the regenerated, signed evidence.

## Evidence that did pass

- Candidate tree independently resolves to `e0f674eb2c947129bf141221059b432553494947`.
- The remote qualification ref currently resolves exactly to `a5f79adb7cd49c4328d132179b8828a5806e68fa`.
- A fresh external module with `GOWORK=off`, `GOPROXY=direct`, fresh module/build caches and binary directory resolved the recorded pseudo-version and sums, installed the exact-revision daemon, and passed the real assertion/stale-refusal program without `replace`.
- Focused Unix, race, predicate SIGKILL, ten durability-boundary crash schedules, response-loss, full test, full race, vet, staticcheck, govulncheck, module, build, formatting, and diff checks otherwise passed.
- The exact two WP03 commits modify only the declared `tests/adoption/**`, `tests/crash/**`, `tests/performance/**`, and `docs/evidence/atomic-batch-assertions/**` surfaces. The frozen 14-scenario contract and SQLite adoption decision were not changed.

## Anti-pattern checklist

1. Dead code — **N/A** (tests and evidence only; no production API added).
2. Synthetic-fixture test — **PASS** (qualification tests invoke real engine/server/client paths, though the incomplete oracles above must be strengthened).
3. Silent empty return — **N/A**.
4. FR coverage — **FAIL** (NFR-001, NFR-004, and the checkable NFR-006 evidence are not yet proved).
5. Frozen surface — **PASS**.
6. Locked decision — **PASS** (the production-pending SQLite decision is unchanged).
7. Shared-file ownership — **N/A** (exact implementation/evidence commits stay within WP03 ownership).
8. Production fragility — **N/A** (no production code changed).
