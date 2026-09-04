# Quickstart: Atomic Batch Assertions

## Library use

```go
batch := gapdb.Batch{
    Ack: gapdb.AckDurable,
    Assertions: []gapdb.Assertion{
        {
            Key: "execution/run-7",
            Condition: gapdb.Condition{
                Kind:             gapdb.ConditionRevision,
                ExpectedRevision: 42,
            },
        },
        {
            Key:       "workspace/path/project-a/wp-8",
            Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent},
        },
    },
    Mutations: []gapdb.Mutation{
        gapdb.NewPutMutation(
            "workspace/W2",
            []byte("opaque"),
            gapdb.Condition{Kind: gapdb.ConditionAbsent},
            nil,
        ),
    },
}

result, err := client.AtomicBatch(ctx, batch)
if err != nil {
    // Reconcile on CONDITION_FAILED. Do not assume any mutation committed
    // after a transport error unless acknowledgement evidence proves it.
    return err
}
fmt.Printf("revision=%d mutations=%d assertions=%d\n",
    result.Revision, result.MutationCount, result.AssertionCount)
```

## Local verification

```bash
go test ./gapdb ./internal/engine ./internal/protocol ./internal/server
go test -race ./gapdb ./internal/engine ./internal/protocol ./internal/server
go test ./tests/adoption ./tests/crash ./tests/performance
go vet ./...
```

Run the repository's complete Makefile qualification ladder before release. Record comparative benchmark output from the same host for mutation-only and assertion-bearing batches.

## Expected behavior

- If both assertions pass, the mutation commits once and the result reports two assertions and one mutation.
- If either assertion fails, the mutation is absent and the database revision is unchanged.
- Watch output includes the `workspace/W2` mutation only.
- Restart recovery reconstructs the committed mutation without any assertion record.
