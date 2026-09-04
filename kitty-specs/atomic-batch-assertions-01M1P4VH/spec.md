# Mission Specification: Atomic Batch Read Assertions

**Mission:** `atomic-batch-assertions-01M1P4VH`  
**Status:** Draft  
**Target:** GapDB public API, engine, Unix protocol, client, and qualification evidence

## 1. Problem statement

GapDB batches currently attach conditions only to keys that the batch also mutates. A caller that needs to mutate one record only if a second authority record is unchanged must therefore rewrite the authority record with identical bytes. That is not a read assertion: it fabricates a mutation, advances revisions, produces durability and watch artifacts, and obscures the caller's intent.

GapDB needs a first-class, no-write assertion that is evaluated atomically with a non-empty mutation batch. Assertions must observe the same pre-batch logical view as mutation conditions and must not create any state change of their own.

## 2. Goals

1. Add revision and absence assertions to the public batch model.
2. Evaluate all assertions and mutation conditions against one serialized pre-batch view before any mutation is applied.
3. Preserve GapDB's all-or-nothing mutation, acknowledgement, durability, expiry, recovery, and watch semantics.
4. Expose identical behavior through the in-process API and Unix protocol client/server.
5. Prove the contract under contention, crash recovery, stale observations, protocol compatibility, and representative performance load.

## 3. Non-goals

- Assertion-only transactions or read transactions.
- Predicates over stored values, ranges, prefixes, or multiple snapshots.
- Cross-process or cross-store distributed transactions.
- Provider-specific, Git-specific, or Spec Kitty-specific authority concepts.
- A compatibility shim that rewrites unchanged records.
- SQLite or any other secondary production authority backend.

## 4. User stories

### US-1: Guard a mutation with unchanged external authority

As a caller, I can mutate key `workspace/W2` only if `execution/run-7` is still at revision 42, so a concurrent authority change causes the entire batch to fail without writing anything.

### US-2: Assert absence without reserving a key

As a caller, I can create one record only if another key remains absent, without creating, deleting, or revising the asserted key.

### US-3: Observe honest acknowledgement evidence

As a caller, I receive the existing mutation acknowledgement plus an exact count of successfully evaluated assertions, while revisions and mutation counts continue to describe writes only.

### US-4: Use the same contract over the Unix protocol

As an out-of-process caller, I can submit the same assertion-bearing batch and receive the same result or bounded diagnostic as an in-process caller.

### US-5: Upgrade without breaking existing clients

As an existing GapDB client, batches with no assertions retain their current wire representation behavior and semantics.

## 5. Functional requirements

| ID | Title | Requirement | Priority | Status |
|----|-------|-------------|----------|--------|
| FR-001 | Public assertion model | The public package MUST define an `Assertion` containing a key and a condition, and `Batch` MUST accept zero or more assertions. | High | Open |
| FR-002 | Meaningful conditions | Assertions MUST support exact revision and absence conditions; `ConditionAny` MUST be rejected because it is vacuous. | High | Open |
| FR-003 | Mutation required | A batch MUST still contain at least one mutation; an assertion-only batch MUST be rejected without allocating a revision. | High | Open |
| FR-004 | Disjoint unique keys | Assertion keys MUST be unique and MUST NOT overlap mutation keys; invalid requests fail before engine execution. | High | Open |
| FR-005 | Unified limits | Existing batch operation and byte limits MUST apply to the combined assertion and mutation request with tested exact-boundary accounting. | High | Open |
| FR-006 | Defensive ownership | Public constructors, validation, and returned values MUST defensively copy caller-owned byte slices. | Medium | Open |
| FR-007 | One pre-batch view | The engine MUST evaluate every assertion and mutation condition under the same serialized writer authority against one pre-batch logical view. | High | Open |
| FR-008 | Atomic failure | If any predicate fails, the batch MUST apply zero mutations, allocate no revision, append no WAL commit, emit no watch event, and alter no snapshot or expiry state. | High | Open |
| FR-009 | No-write assertion | A successful assertion MUST itself perform no put, delete, expiry update, WAL entry, snapshot change, watch emission, per-key revision change, or global revision change. | High | Open |
| FR-010 | Coherent expiry | Expired records MUST receive the same effective-time treatment for assertions as for conditions and reads, using one coherent batch time. | High | Open |
| FR-011 | Stale and lost-response behavior | Evaluation MUST remain correct after concurrent change, stale retry, and loss of a successful mutation response. | High | Open |
| FR-012 | Exact result evidence | `MutationResult` MUST report evaluated assertion count separately while preserving existing revision, acknowledgement, durable-through, and mutation-count meanings. | High | Open |
| FR-013 | Unix parity | Unix request and response schemas MUST represent assertions and assertion counts additively with in-process semantic parity. | High | Open |
| FR-014 | Compatibility | Servers MUST accept valid pre-assertion clients, while clients and servers reject malformed assertion encodings deterministically. | High | Open |
| FR-015 | Safe diagnostics | Failures MUST identify assertion index/key and condition without exposing values, using bounded log- and protocol-safe diagnostics. | High | Open |
| FR-016 | Recovery transparency | Snapshot/WAL recovery MUST never synthesize assertion changes; a committed assertion-bearing batch recovers exactly like its mutation subset. | High | Open |
| FR-017 | Watch transparency | Watch consumers MUST observe only committed mutations from an assertion-bearing batch in existing ordering and envelopes. | High | Open |
| FR-018 | Acknowledgement continuity | Cancellation and transport failure MUST preserve existing acknowledgement and retry semantics; assertions do not imply outcome knowledge. | High | Open |

## 6. Non-functional requirements

| ID | Title | Requirement | Category | Priority | Status |
|----|-------|-------------|----------|----------|--------|
| NFR-001 | Contention correctness | At least 1,000 adversarial trials MUST show that a stale revision or absence assertion never permits its guarded mutation. | Reliability | High | Open |
| NFR-002 | Fault coverage | Fault injection MUST cover assertion evaluation, mutation-condition evaluation, WAL append, durability acknowledgement, and response loss. | Reliability | High | Open |
| NFR-003 | Bounded safe errors | Assertion error payloads MUST remain below 4 KiB and MUST never contain stored values. | Security | High | Open |
| NFR-004 | Memory performance | In-memory assertion-batch p95 overhead MUST be no more than the larger of 15% or 100 microseconds versus its mutation-only control. | Performance | Medium | Open |
| NFR-005 | Durable performance | Durable assertion-bearing batch p95 latency MUST remain below 4 ms on the reference qualification environment. | Performance | Medium | Open |
| NFR-006 | Regression safety | Existing unit, protocol, crash, recovery, race, performance, adoption, vet, staticcheck, govulncheck, formatting, and module gates MUST remain green. | Quality | High | Open |
| NFR-007 | Domain neutrality | The public contract MUST remain storage-engine neutral and uncoupled from Spec Kitty, Git, workspace, ticket, or provider terminology. | Architecture | High | Open |

## 7. Invariants

1. Assertions observe; mutations change.
2. One batch has one pre-batch logical view.
3. Failed predicates imply zero durable or observable state change.
4. Revisions and mutation counts describe mutations only.
5. Assertion success is never represented by rewriting identical bytes.
6. Protocol transport cannot weaken the in-process contract.

## 8. Edge cases and required decisions

- Empty assertion lists preserve current batch behavior.
- Duplicate assertion keys are invalid.
- An assertion key that is also mutated is invalid, even if both predicates are equivalent.
- `ConditionAny`, zero/invalid revisions, and contradictory absence/revision encodings are invalid.
- A passing assertion followed by a failing mutation condition yields zero writes.
- A record expiring at the batch's effective time is handled consistently by assertions and mutation conditions.
- Combined operation and request-size limits are tested immediately below, at, and above the boundary.
- Retrying after a lost response uses the existing acknowledgement contract; assertions do not become locks or reservations.

## 9. Success criteria

- **SC-001:** A real Unix client demonstrates that a stale authority revision rejects a guarded mutation with zero revision, WAL, watch, snapshot, or expiry side effects.
- **SC-002:** Absence and revision assertions have equivalent results through direct and Unix client APIs.
- **SC-003:** The 1,000-trial contention campaign and mutant controls prove that moving assertion evaluation outside the serialized pre-batch section is detected.
- **SC-004:** Existing clients and assertion-free batches pass unchanged compatibility fixtures.
- **SC-005:** Crash/recovery and lost-response qualification prove that only committed mutations reappear after restart.
- **SC-006:** Reference p95 targets in NFR-004 and NFR-005 pass with recorded environment and raw evidence.
- **SC-007:** A tagged or immutable GapDB module revision containing the feature can be consumed by an external Go module without a local `replace` directive.

## 10. Acceptance boundary

This mission is complete only when the public contract, engine semantics, Unix transport, client behavior, recovery behavior, contention evidence, and performance evidence ship together. A type-only API, an in-tree adapter shim, or a same-byte conditional rewrite does not satisfy the mission.
