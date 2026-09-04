# Data Model: Atomic Batch Read Assertions

## Assertion

```go
type Assertion struct {
    Key       string    `json:"key"`
    Condition Condition `json:"condition"`
}
```

An assertion is an admission predicate over one key in the batch's pre-mutation logical view. It is request data only and is never stored.

Validation:

- `Key` uses existing key validation and limits.
- `Condition.Kind` is `absent` or `revision`.
- `revision` requires a positive `ExpectedRevision`.
- `any` and unknown kinds are invalid.
- Assertion keys are unique and disjoint from mutation keys.

## Batch

```go
type Batch struct {
    Ack        AckMode     `json:"ack"`
    Assertions []Assertion `json:"assertions,omitempty"`
    Mutations  []Mutation  `json:"mutations"`
}
```

The batch remains a mutation operation and therefore requires at least one mutation. Its logical operation count is the sum of assertions and mutations. `Clone` owns both slices and all nested caller-owned bytes.

## MutationResult

```go
type MutationResult struct {
    Revision               Revision `json:"revision"`
    Ack                    AckMode  `json:"ack"`
    DurableThroughRevision Revision `json:"durable_through_revision"`
    MutationCount          int      `json:"mutation_count,omitempty"`
    AssertionCount         int      `json:"assertion_count,omitempty"`
}
```

`AssertionCount` is evidence that all submitted assertions passed for the committed batch. It is not a database revision and does not count mutation conditions. Existing write-oriented fields retain their meanings.

## Assertion failure evidence

`gapdb.Error` gains an optional `AssertionIndex`. Assertion failures use the established condition code and may include:

- assertion index;
- bounded key;
- condition kind;
- expected revision;
- actual revision or absent state;
- existing retry and safe-action fields.

It never includes record values.

## State transitions

```text
Validated batch
    -> queued under writer authority
    -> capture effective time
    -> assertions pass
    -> mutation conditions pass
    -> allocate one commit revision
    -> apply mutations
    -> WAL/durability/watch as today

Any failure before revision allocation
    -> return bounded error
    -> no state transition
```

## Persistence model

No new persisted entity exists. Snapshots contain records; WAL commits contain mutations/events. Assertions are discarded after deciding whether the mutation commit may proceed.
