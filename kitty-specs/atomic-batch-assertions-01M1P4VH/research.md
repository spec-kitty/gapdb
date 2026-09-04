# Research: Atomic Batch Read Assertions

## Decision summary

GapDB should add first-class read assertions to its existing atomic mutation batch. A same-byte conditional put was rejected because it advances revisions and creates WAL/watch evidence for a change that did not occur. A separate read transaction was rejected because the use case requires one or more real mutations and does not need locks, reservations, or MVCC snapshots.

## Existing implementation findings

| Surface | Current behavior | Consequence |
|---------|------------------|-------------|
| `gapdb.Batch` | Ack mode plus a non-empty list of distinct-key mutations | Assertions fit naturally as a second bounded list while retaining mutation requirement. |
| `Batch.ValidateAt` | Validates ack, mutations, duplicate keys, expiry, and approximate byte limit | Public validation must add meaningful assertion conditions, combined uniqueness, and combined limits. |
| `engine.prepareAndSubmit` | Clones and validates before submitting a mutation command | Caller ownership remains protected; definitive predicate evaluation still belongs in the writer. |
| Serialized writer | Orders conditions, revision allocation, state mutation, WAL, and acknowledgement | This is the only location that can make read guards atomic with mutations. |
| WAL/snapshot | Persist committed changes, not request intent | Assertions need no persisted representation. |
| Watch history | Derives from committed mutation events | Assertion keys must never produce events. |
| Protocol v1 | Strict JSON argument/result field validation | Optional fields can evolve v1 additively only when strict allowlists and fixtures are updated together. |
| Error surface | Stable codes with retry class, safe actions, and bounded evidence | Assertion index is new evidence; value bytes remain forbidden. |

## Alternatives considered

### Conditional same-byte mutation

Rejected. It is semantically dishonest and observable: revisions advance, WAL grows, durability work occurs, watchers see an event, and TTL metadata may change. It also makes downstream audit records claim a write.

### Read followed by conditional mutation

Rejected. The checked authority can change between operations; this recreates the exact time-of-check/time-of-use race the feature must close.

### Assertion-only batch

Deferred. It would be a read transaction with no durable acknowledgement meaning and invites callers to mistake an observation for a lease. This mission requires at least one mutation.

### General predicate language

Rejected. Revision and absence cover GapDB's established condition model. Value predicates would enlarge the security, performance, and protocol surface and conflict with opaque values.

### Persist assertions in WAL

Rejected. Recovery needs committed state changes, not transient admission predicates. Persisting assertions would create a new format without improving correctness.

## Compatibility conclusion

`assertions` is optional in atomic-batch arguments and `assertion_count` is optional/zero for old results. No stored format changes. Old client request fixtures remain byte-valid; new client decoding must accept old servers only within the repository's existing version/error rules. The protocol remains strict about unknown and malformed fields.

## Performance hypothesis

Under the serialized writer, assertion cost is one bounded map lookup plus condition evaluation per key. No allocation proportional to record value size and no WAL/watch work is required. The target is therefore a modest relative in-memory overhead and no material durable latency change beyond predicate evaluation.

## Security and operability findings

- Keys are already bounded UTF-8 identifiers and may be returned as safe diagnostic context.
- Values must never be included in an assertion error.
- Assertion indexes need a distinct field so operators do not confuse them with mutation indexes.
- Retry remains `after_reconcile`; a condition failure is evidence of drift, not a transient server failure.
- Lost response semantics do not change. The acknowledgement token/revision, not assertion success, proves a commit.

## Open questions resolved by the plan

1. **Can asserted and mutated keys overlap?** No; use the mutation condition for that key.
2. **Does assertion success advance a revision?** No; only the required mutation subset does.
3. **What clock governs expiry?** One effective time captured in serialized execution.
4. **How are limits counted?** Assertions and mutations share the operation cap; byte accounting includes both request representations.
5. **Does recovery replay assertions?** No; it replays the mutations of successful commits.
