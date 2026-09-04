---
affected_files: []
cycle_number: 1
mission_slug: atomic-batch-assertions-01M1P4VH
reproduction_command:
reviewed_at: '2026-09-04T12:39:49Z'
reviewer_agent: user
wp_id: WP01
---

# WP01 Review Feedback — Cycle 1

Review target: `a336c7aaa8225e536695bd820c1af77ec44dc8e4`

## Issue 1 — Assertion-failure condition evidence is not semantically strict

`internal/protocol.validateStructuredError` and `gapdb.validateRemoteErrorRaw` validate only the presence of a non-empty `condition`. Both production paths accept `condition:"any"`, an unknown condition string, a `revision` failure without `expected_revision`, and an `absent` failure carrying revision-only evidence. This violates T002.1/T003.2 and the contract's deterministic rejection of unknown condition kinds. It also allows the first-party client to admit malformed server evidence.

Repair both strict validators with the same condition-dependent rules: an assertion failure permits only `absent` or `revision`; `revision` requires a positive `expected_revision`; `absent` forbids revision-only expected evidence. Preserve the exactly-one assertion/mutation index rule. Add malformed fixtures through both `DecodeResponse` and the public client response path so one validator cannot drift from the other.

Independent overlay evidence: all four malformed protocol responses returned `nil`, and the client validator likewise accepted `unknown`, `any`, and revision-without-expected responses.

## Issue 2 — Assertion failure encodings can exceed the required 4 KiB bound

`internal/protocol.EncodeResponse` clones and marshals a structured error but enforces only the general hard frame limit. A valid `CONDITION_FAILED` assertion error containing a key at `Limits.MaxKeyBytes` encoded to 4,400 bytes and was accepted. The existing bounded-diagnostic test exercises only `Batch.Validate`'s duplicate-key error, not the actual assertion-condition failure response path. This violates T002.6, FR-015, and NFR-003.

Apply bounded, value-free diagnostic shaping to assertion condition failures before encoding (without changing general frame accounting or exposing stored values), and add exact-boundary tests through `EncodeResponse` plus the first-party client path. Prove every assertion error encoding is strictly below 4 KiB for a legal maximum-length key.

## Preserved evidence

The public assertion model, `ConditionAny` refusal, duplicate/overlap checks, combined public operation/semantic-byte accounting, defensive cloning, additive schema-v1 fields, exact legacy fixtures, and assertion/mutation count separation passed review. WP01 does not alter engine/WAL accounting: the existing engine loop remains mutation-only, as required.
