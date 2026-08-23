---
affected_files: []
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command:
reviewed_at: '2026-08-23T13:59:15Z'
reviewer_agent: unknown
verdict: rejected
wp_id: WP01
---

# WP01 review cycle 1 — changes requested

Reviewer: Reviewer Renata (`reviewer-renata`), independent of the implementer  
Reviewed lane commit: `c045c2f9c34efa63db43e5b540aceedb9656a27c`

## Blocking findings

### 1. Strict decoding accepts an explicit empty acknowledgement enum

`internal/protocol/envelope.go:625-627`, `650-652`, `666-668`, and `684-685`
cannot distinguish an omitted `ack` (which defaults to `memory`) from an
explicit `"ack":""` (which is not a protocol-v1 enum). The latter currently
decodes successfully. `EncodeRequest` can also emit `"ack":""` when given a
zero-value `AckMode`, even though its self-decode treats that spelling as the
default.

Preserve field presence while decoding. Default only when `ack` is absent;
reject an explicitly empty or otherwise non-canonical enum. Make request
encoding normalize or reject zero-value ack so it cannot emit a spelling the
wire contract does not define. Add red-first tests for omitted, explicit empty,
`memory`, and `durable` across every mutation request form.

Independent probe that currently fails:

```text
DecodeRequest({...,"operation":"put","arguments":{"key":"k","value_base64":"","ack":""}})
must return INVALID_REQUEST, but returns success.
```

### 2. A valid watch start at revision zero is rejected

`internal/protocol/envelope.go:365-369` requires
`registration_revision > 0`. Revision zero is the valid empty-database state
(`data-model.md:24-34`), and a watch registered before the first commit must be
able to report that it covered backlog through revision zero.

Represent required-field presence separately from its numeric value. Require
the field on `stream:"started"`, but accept zero. Add an empty-database watch
start golden fixture and boundary test.

Independent probe that currently fails:

```text
DecodeResponse({...,"operation":"watch","stream":"started","registration_revision":0})
must succeed, but returns INVALID_REQUEST.
```

### 3. Structured errors are not validated against their code-specific contract

`internal/protocol/envelope.go:559-587` checks only that the retry class matches
and that every safe-action token exists somewhere in the global catalog. It
accepts a `NOT_FOUND` error with `safe_actions:["check_storage"]`, and it accepts
`NOT_FOUND` without the required `key` and `current_revision` evidence. The same
problem applies across the required-evidence rows in `contracts/errors-v1.md`.
The current `errors.golden.jsonl` fixtures omit required evidence for essentially
all error codes, so they freeze envelopes that are structurally decodable but
contractually incomplete.

Define per-code validation for exact retry classification, allowed/required safe
actions, and required evidence (including the operation-applied rule). Reject
irrelevant or missing authority evidence rather than accepting generic error
objects. Replace each error fixture with the smallest contract-valid example and
add negative tests for wrong-code actions and missing required evidence.

Independent probes that currently fail:

```text
NOT_FOUND + safe_actions:["check_storage"] is accepted.
NOT_FOUND without key/current_revision is accepted.
```

### 4. The guarded offline recovery-apply envelope is not implemented

`OperationOfflineRecoverApply` is declared as a v1 operation, but
`decodeArguments` routes it to `EmptyArguments` at
`internal/protocol/envelope.go:752-758`. Consequently `{}` is accepted while the
contract-required action/proposal ID, expected database ID, expected manifest
generation, and explicit quarantine/backup destination are rejected as unknown
fields. Freezing an empty destructive-recovery request contradicts
`contracts/protocol-v1.md:359-367`, GAP-001/GAP-003, and the charter's guarded
administration rule.

Add explicit offline inspect/verify/recover-propose/recover-apply argument types
and validation. `offline_recover_apply` must require every authority precondition
and destination before later packages can route it. Add request golden fixtures
for all four declared offline operations.

Independent probe that currently fails:

```text
offline_recover_apply with action_id, expected_database_id,
expected_manifest_generation, and destination is rejected as INVALID_REQUEST.
```

### 5. The negative golden corpus does not meet T004's boundary and reachability gate

`tests/compatibility/protocol/testdata/negative.golden.jsonl` contains no golden
cases for the documented key, value, frame, batch-operation, batch-byte,
request-ID, or scan-limit boundaries, despite T004 requiring negative fixtures
for every documented size boundary. Unit tests cover several boundaries, but
they do not replace the required compatibility corpus.

The duplicate-field golden fixture also fails for the wrong reason. I temporarily
removed the `rejectDuplicateFields` call and ran only
`TestNegativeGoldenFixtures/.../line-3/duplicate-field`; it still passed because
the duplicate changes `get` to `put`, after which the payload fails for missing
`value_base64`. In the same deletion state, the nested duplicate-field unit test
failed correctly. Change the golden payload so either last-wins interpretation is
otherwise valid and only duplicate detection can reject it. Add explicit
expected field/reason assertions where one stable code covers several validation
branches.

## Verification evidence

Passed independently on the submitted lane:

- Go `1.26.7` on `linux/amd64`; `go.mod` retains `go 1.26` and
  `toolchain go1.26.7`.
- `gofmt`, `git diff --check`, `go vet ./...`, `staticcheck ./...`.
- `go test -count=1 ./...` and `go test -race -count=1 ./...`.
- `go mod verify` and `govulncheck ./...`.
- `govulncheck -mode query -json golang.org/x/sys@v0.47.0` returned no
  vulnerability finding with database timestamp `2026-08-21T20:38:00Z`.
- The module checksum, module-file checksum, upstream tag hash, and local BSD
  3-Clause license SHA-256 match `docs/evidence/dependencies/x-sys.md`.
- All changes are within WP01's owned-file map. The lane has one implementation
  commit and no tracked review residue.

`go mod tidy -diff` proposes removing `x/sys` because WP01 intentionally pins the
reviewed dependency before the later ownership code imports it. This is expected
from T001 and is not a rejection reason.

The lane has only one combined implementation/test commit, so test-first
chronology is not independently auditable from Git. For this repair cycle, add
each regression test before its fix and include the red command/result in the
resubmission note. The deletion test above is evidence that the nested duplicate
unit test reaches production behavior and that the duplicate golden fixture does
not.

## Required anti-pattern verdict

1. **Dead code — N/A (staged foundation):** exported contract vocabulary and
   deterministic clock seams are explicitly created for dependent WPs; no
   accidental package or dependency framework was added.
2. **Synthetic-fixture test — FAIL:** the duplicate golden fixture passes after
   duplicate detection is deleted, and generic error fixtures omit the behavior
   their code-specific contracts require.
3. **Silent empty return — PASS:** no silent empty failure branch was found.
4. **FR coverage — FAIL:** FR-017/FR-018's frozen v1/model-operation surface is
   incomplete for offline recovery and strict enum/error behavior.
5. **Frozen surface — PASS:** no spec, plan, contract, data-model, or charter file
   was modified by the WP implementation commit.
6. **Locked decision — FAIL:** accepting an empty `offline_recover_apply`
   request contradicts the explicit guarded-recovery decision.
7. **Shared-file ownership — N/A:** WP01 owns lane A alone and every changed file
   is in its ownership map.
8. **Production fragility — PASS:** no transient-path bare raise/panic was added;
   the nil-clock constructor panic is a documented fail-loud programmer
   invariant, not a request-path race.

## Runtime observations (not implementation findings)

- The generated review prompt resolved its identity block as
  `implementer-ivan` even though the review claim/event correctly selected
  `reviewer-renata`; the WP frontmatter remained on the implementer profile.
- The prompt's `terminology-canon`, `code-review-checklist`, and
  `regression-vigilance` charter selectors all resolve to “section not found.”
  The review therefore used the committed charter, specification, data model,
  contracts, and inline checklist. These defects do not relax this rejection.
