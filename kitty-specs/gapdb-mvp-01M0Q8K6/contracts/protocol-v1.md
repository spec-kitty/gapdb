# Gapdb Local Protocol v1

## Transport

- Unix stream socket only.
- Default socket: `<database-dir>/gapdb.sock` with effective mode `0600`.
- Each frame is `uint32_be payload_length` followed by exactly that many UTF-8
  JSON bytes.
- Zero-length frames and frames above the configured maximum are invalid.
- The v1 default maximum is 16 MiB.
- One connection may issue sequential unary requests or one terminal watch. The
  MVP does not multiplex concurrent request IDs on one connection.
- Unknown JSON fields, duplicate fields, trailing JSON values, invalid UTF-8, and
  non-canonical enum spellings are rejected.

## Common request envelope

```json
{
  "schema_version": 1,
  "request_id": "caller-optional-id",
  "operation": "get",
  "arguments": {}
}
```

| Field | Required | Rules |
|---|---|---|
| `schema_version` | yes | Integer `1` |
| `request_id` | no | UTF-8, at most 256 bytes; correlation only |
| `operation` | yes | One stable operation name below |
| `arguments` | yes | Operation-specific object |

## Common success envelope

```json
{
  "schema_version": 1,
  "ok": true,
  "request_id": "caller-optional-id",
  "database_id": "0198f4d4f26a7b1ca3df00c30ca93e73",
  "operation": "get",
  "result": {}
}
```

Optional request fields are omitted when absent. Timestamps are RFC 3339 Nano in
UTC with a `Z` suffix. Opaque bytes use padded standard base64.

## Common error envelope

```json
{
  "schema_version": 1,
  "ok": false,
  "request_id": "caller-optional-id",
  "database_id": "0198f4d4f26a7b1ca3df00c30ca93e73",
  "operation": "compare_and_swap",
  "error": {
    "code": "REVISION_MISMATCH",
    "message": "Record revision does not match the condition.",
    "retry": "after_reconcile",
    "key": "workflows/123",
    "expected_revision": 42,
    "actual_revision": 45,
    "safe_actions": ["get", "retry_with_new_condition", "abort"]
  }
}
```

Automation branches on `code`, `retry`, and other stable fields. `message` is
advisory. See [errors-v1.md](./errors-v1.md).

## Shared objects

### Record

```json
{
  "key": "workflows/123",
  "value_base64": "AQID",
  "revision": 45,
  "expires_at": "2026-08-23T18:30:00Z"
}
```

`expires_at` is omitted when absent. Empty bytes encode as `""`.

### Acknowledgement

Mutation requests include:

```json
{ "ack": "memory" }
```

or:

```json
{ "ack": "durable" }
```

The default is `memory`; callers responsible for leases, reviews, or integration
authority must request `durable`.

### Mutation result

```json
{
  "revision": 46,
  "ack": "durable",
  "durable_through_revision": 46
}
```

For memory acknowledgement, `durable_through_revision` reports the last known
successful durability barrier and may be less than `revision`.

## Data operations

### `get`

Request arguments:

```json
{ "key": "workflows/123" }
```

Success result:

```json
{ "record": { "key": "workflows/123", "value_base64": "AQID", "revision": 45 } }
```

A missing or expired key returns `NOT_FOUND`; it is not represented as a success
with a null record.

### `put`

```json
{
  "key": "workflows/123",
  "value_base64": "AQID",
  "expires_at": "2026-08-23T18:30:00Z",
  "ack": "durable"
}
```

`expires_at` is optional. Success returns a mutation result.

### `put_if_absent`

Uses the same arguments as `put`. It fails with `ALREADY_EXISTS` when a live
record exists; the error includes `actual_revision`.

### `compare_and_swap`

```json
{
  "key": "workflows/123",
  "expected_revision": 45,
  "value_base64": "BAUG",
  "ack": "durable"
}
```

An optional future expiry may be supplied for the replacement. Omission or null
means the replacement has no expiry. A missing/expired key returns `NOT_FOUND`;
a different live revision returns `REVISION_MISMATCH`.

### `delete_if_revision`

```json
{
  "key": "workflows/123",
  "expected_revision": 45,
  "ack": "durable"
}
```

Success returns a mutation result. The deleted record is not returned.

### `atomic_batch`

```json
{
  "ack": "durable",
  "mutations": [
    {
      "kind": "put",
      "key": "leases/new",
      "condition": { "kind": "absent" },
      "value_base64": "b3duZXItMQ==",
      "expires_at": "2026-08-23T18:30:00Z"
    },
    {
      "kind": "delete",
      "key": "leases/old",
      "condition": { "kind": "revision", "expected_revision": 91 }
    }
  ]
}
```

Condition forms:

```json
{ "kind": "any" }
{ "kind": "absent" }
{ "kind": "revision", "expected_revision": 91 }
```

Rules:

- Keys are distinct within the batch.
- `delete` requires a `revision` condition.
- Every condition observes the pre-batch state.
- Failure identifies `mutation_index`, key, and condition evidence.
- Success returns one shared commit revision and the number of mutations.
- Events use the same revision and zero-based request order.

### `scan_prefix`

First page:

```json
{ "prefix": "workflows/", "limit": 1000 }
```

Continuation:

```json
{
  "prefix": "workflows/",
  "limit": 1000,
  "cursor": "<opaque-base64url-token>"
}
```

Success:

```json
{
  "observed_revision": 102,
  "as_of": "2026-08-23T18:00:00Z",
  "records": [],
  "truncated": false
}
```

`cursor` is present only when `truncated` is true. Records are sorted by UTF-8
key bytes. A commit after the first page causes `SCAN_STALE` on continuation.

### `watch`

```json
{ "prefix": "workflows/", "after_revision": 102 }
```

Start frame:

```json
{
  "schema_version": 1,
  "ok": true,
  "request_id": "watch-1",
  "database_id": "0198f4d4f26a7b1ca3df00c30ca93e73",
  "operation": "watch",
  "stream": "started",
  "registration_revision": 110
}
```

Event frame:

```json
{
  "schema_version": 1,
  "ok": true,
  "request_id": "watch-1",
  "database_id": "0198f4d4f26a7b1ca3df00c30ca93e73",
  "operation": "watch",
  "stream": "event",
  "event": {
    "revision": 105,
    "order": 0,
    "kind": "put",
    "key": "workflows/123",
    "record": {
      "key": "workflows/123",
      "value_base64": "AQID",
      "revision": 105
    }
  }
}
```

Terminal frames use `stream: "ended"` plus either a normal reason or the common
error object. `WATCH_LAGGED` includes `last_delivered_revision`.

## Inspection operations

### `status`

Returns lifecycle state, database ID, current revision, durable-through revision,
reserved revision end, snapshot revision, active WAL start, earliest watch
revision, counts, active limits, owner PID/start time, and snapshot/backup progress.

### `health`

Returns one of `ready`, `degraded_read_only`, `draining`, or `inspection_only`,
plus failing subsystems and safe actions. Process liveness alone is not health.

### `stats`

Returns bounded counters and gauges: live/expired records, approximate value bytes,
commits, condition failures, WAL bytes, sync count/errors, watchers, lagged watches,
clients, snapshot duration, and recovery duration. Counter names are versioned.

### `describe_config`

Returns effective values, source (`default`, `file`, or `flag`), safe ceilings,
and whether restart is required. Secrets are never part of config.

### `verify`

Online verify checks identity, manifest, active files, in-memory revision, and
bounded sampled/full checks as requested. It does not mutate or pause the writer
unless `mode: "full"` requires a barrier.

## State-changing administration

Every request includes `expected_database_id` and the documented revision
precondition. Failure makes no changes.

### `create_snapshot`

```json
{
  "expected_database_id": "0198f4d4f26a7b1ca3df00c30ca93e73",
  "expected_revision": 110
}
```

Returns snapshot revision, filename, checksum, new WAL start, and duration.

### `compact`

Requires expected database ID and `through_revision`. Deletes only superseded
files not referenced by `CURRENT`, returns exact removed paths and directory-sync
result, and refuses a revision newer than the active snapshot.

### `backup`

Requires expected database ID, expected revision, and an absolute destination
outside the database directory. Returns backup database ID, revision, manifest
checksum, file list, byte count, and verification result.

## Offline CLI-only operations

`gapctl inspect`, `gapctl verify`, and `gapctl recover` accept `--db` and require
the exclusive ownership lock. They use the same success/error envelopes as socket
operations with `operation` names prefixed by `offline_`.

`gapctl recover propose` is read-only and emits candidate actions with evidence.
`gapctl recover apply` requires action ID, expected database ID, expected manifest
generation, and an explicit destination for quarantined bytes or verified backup.
