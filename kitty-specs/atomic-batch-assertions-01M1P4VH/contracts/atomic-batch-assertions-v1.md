# Contract: Atomic Batch Assertions over GapDB Protocol v1

## Atomic batch request

The existing `atomic_batch` operation accepts an optional `assertions` array beside `ack` and `mutations`:

```json
{
  "schema_version": 1,
  "request_id": "req-42",
  "operation": "atomic_batch",
  "arguments": {
    "ack": "durable",
    "assertions": [
      {
        "key": "execution/run-7",
        "condition": {
          "kind": "revision",
          "expected_revision": 42
        }
      },
      {
        "key": "workspace/path/project-a/wp-8",
        "condition": {"kind": "absent"}
      }
    ],
    "mutations": [
      {
        "kind": "put",
        "key": "workspace/W2",
        "condition": {"kind": "absent"},
        "value_base64": "b3BhcXVl"
      }
    ]
  }
}
```

The array order defines `assertion_index`. Absence of `assertions` is equivalent to an empty array.

## Successful result

```json
{
  "revision": 43,
  "ack": "durable",
  "durable_through_revision": 43,
  "mutation_count": 1,
  "assertion_count": 2
}
```

The revision belongs to the mutation commit. Assertions do not receive revisions.

## Failed assertion

The operation returns `CONDITION_FAILED`, retry class `after_reconcile`, and safe actions from the existing condition contract. Evidence identifies the failed assertion without returning stored bytes:

```json
{
  "code": "CONDITION_FAILED",
  "message": "Atomic batch assertion failed.",
  "retry": "after_reconcile",
  "key": "execution/run-7",
  "assertion_index": 0,
  "condition": "revision",
  "expected_revision": 42,
  "actual_revision": 44,
  "safe_actions": ["get", "rebuild_batch", "abort"]
}
```

The failed request allocates no revision and creates no WAL, watch, snapshot, or expiry effect.

## Validation rules

- At least one mutation is required.
- Assertion kinds are `absent` and `revision` only.
- Expected revision is positive for a revision assertion.
- Assertion keys are unique and disjoint from mutation keys.
- Combined assertions plus mutations fit `max_batch_operations`.
- Encoded batch arguments fit both `max_batch_bytes` and frame limits.
- Unknown fields, unknown condition kinds, malformed base64, and malformed numbers fail deterministically.

## Compatibility

This is an additive evolution of schema version 1. Existing assertion-free request fixtures and result decoders remain supported. Strict allowlists explicitly admit the new fields; unrelated unknown fields remain errors.
