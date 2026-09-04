# WP01 Review Feedback — Cycle 2

Review target: `543606e887c66a6c5908e2f5c790e386a2e33a78`

## Issue 1 — Assertion failure evidence is admitted outside `atomic_batch`

The repair correctly validates the contents of assertion `CONDITION_FAILED` evidence, but neither production codec couples `assertion_index` to the enclosing operation. Both `internal/protocol.DecodeResponse` and the first-party client's `validateClientResponse` accept a response whose operation is `put` while its error carries `assertion_index:0`, `condition:"absent"`, and assertion-specific current-state evidence.

Assertions exist only on `atomic_batch`; this impossible combination violates T003.2's strict semantic-validation requirement and the same operation/result coupling already enforced for `assertion_count`. It also lets a peer make a non-batch failure appear to be governed by an assertion that could not have existed in the request.

Require assertion-index evidence to appear only when the response operation is `atomic_batch`, in both the protocol decoder/encoder validation and the public client decoder. Add one production-path refusal fixture per codec (at minimum `put`; a table across non-batch unary/watch operations is preferable), plus a deletion-sensitive control so operation coupling cannot regress independently.

## Closed cycle-1 findings

- `any`, unknown, revision-without-positive-expected, and absent-with-expected assertion error forms are now rejected by both codecs.
- Maximum-key assertion errors are bounded without mutating the caller-owned `gapdb.Error`.
- Independent protocol and public-client probes accept exactly 4,095 bytes and reject 4,096 bytes.
- Focused tests, focused race tests, the complete repository suite, compile-only checks, vet, staticcheck, module verification, formatting, and diff checks pass.
