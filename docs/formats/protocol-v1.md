# Gapdb protocol v1

This is the living public protocol reference. The executable authorities are
the [canonical codec](../../internal/protocol), the [public Go client](../../gapdb/client.go),
and the committed [request](../../tests/compatibility/protocol/testdata/requests.golden.jsonl),
[success](../../tests/compatibility/protocol/testdata/success.golden.jsonl),
[error](../../tests/compatibility/protocol/testdata/errors.golden.jsonl), and
[negative](../../tests/compatibility/protocol/testdata/negative.golden.jsonl)
goldens. Changes to a field or operation require updating all of them.

## Transport and envelopes

Gapdb v1 uses only an owner-local Unix stream socket. The default path is
`<database-directory>/gapdb.sock`, effective mode `0600`. TCP and remote access
are out of scope. Each message is a four-byte unsigned big-endian payload length
followed by exactly that many UTF-8 JSON bytes. Zero length and payloads over
`max_frame_bytes` fail. One connection carries sequential unary operations or a
single terminal watch; requests are not multiplexed.

Requests have `schema_version:1`, optional `request_id` (valid UTF-8, at most 256
bytes), a locked lowercase `operation`, and an operation-specific `arguments`
object. Responses have `schema_version`, `ok`, echoed request ID when present,
database ID, operation, and exactly one of `result` or `error`. Watches instead
use `stream:"started"`, `"event"`, or one final `"ended"` frame. Duplicate or
unknown fields, trailing JSON, invalid UTF-8, noncanonical base64/time/enums,
missing required fields, and irrelevant evidence fail closed.

Opaque bytes are padded standard base64. Timestamps are canonical UTC
RFC3339Nano ending in `Z`. Optional expiry is omitted when absent; explicit
`null` is accepted only where the canonical decoder documents presence.

## Operations and acknowledgements

The exact v1 operations are:

| Class | Operations |
|---|---|
| Data | `get`, `put`, `put_if_absent`, `compare_and_swap`, `delete_if_revision`, `atomic_batch`, `scan_prefix`, `watch` |
| Inspection | `status`, `health`, `stats`, `describe_config`, `verify` |
| Guarded online admin | `create_snapshot`, `compact`, `backup` |
| Offline CLI envelopes | `offline_inspect`, `offline_verify`, `offline_recover_propose`, `offline_recover_apply` |

Mutations require `ack:"memory"` or `ack:"durable"`. Memory acknowledgement
means the mutation is ordered and visible in RAM; it can be lost on abrupt
failure and is **not durable**. Leases, reviews, workflow state, and integration
authority must use durable acknowledgement. Durable success is returned only
after the commit and every earlier commit have passed the WAL sync barrier.
`durable_through_revision` is authority, not advisory text.

Records contain `key`, `value_base64`, `revision`, and optional `expires_at`.
`scan_prefix` returns byte-sorted bounded records, `observed_revision`, `as_of`,
`truncated`, and an opaque cursor. A later commit invalidates continuation with
`SCAN_STALE`. Watch resume is exclusive of `after_revision`; backlog and live
events are revision/order sorted. `WATCH_LAGGED` never silently drops an event.

Atomic batch conditions are `any`, `absent`, or `revision` with
`expected_revision`. Keys are distinct; all conditions see one pre-batch state;
all effects share one revision or none apply.

## Default limits

These defaults come from [gapdb/options.go](../../gapdb/options.go). Effective
values are returned by `status`, `describe_config`, `gapctl limits`, and every
CLI machine envelope.

| Field | Default | Hard ceiling |
|---|---:|---:|
| `max_key_bytes` | 4 KiB | 64 KiB |
| `max_value_bytes` | 8 MiB | 64 MiB |
| `max_frame_bytes` | 16 MiB | 64 MiB |
| `max_batch_bytes` | 16 MiB | 64 MiB |
| `max_batch_operations` | 1,024 | 65,536 |
| `max_scan_records` | 1,000 | 10,000 |
| `max_scan_bytes` | 16 MiB | 64 MiB |
| `watch_buffer_events` | 256 | 8,192 |
| `max_watch_clients` | 256 | 4,096 |
| `max_concurrent_clients` | 256 | 4,096 |
| `max_history_events` | 100,000 | 1,000,000 |
| `max_history_bytes` | 64 MiB | 1 GiB |

Every limit is positive. Batch/scan bytes cannot exceed frame bytes, watch
clients cannot exceed total clients, and retained history cannot be smaller
than one watch buffer.

## Stable errors

Automation branches on `error.code`, `retry`, typed evidence, and
`safe_actions`; `message` is advisory. The canonical schema is
[gapdb/types.go](../../gapdb/types.go) and its deletion-sensitive error golden.
The complete locked codes are:

- validation: `INVALID_REQUEST`, `UNSUPPORTED_VERSION`, `FRAME_TOO_LARGE`,
  `KEY_TOO_LARGE`, `VALUE_TOO_LARGE`, `BATCH_TOO_LARGE`, `DUPLICATE_KEY`,
  `EXPIRY_NOT_FUTURE`, `INVALID_CURSOR`;
- state/stream: `NOT_FOUND`, `ALREADY_EXISTS`, `REVISION_MISMATCH`,
  `CONDITION_FAILED`, `SCAN_STALE`, `REVISION_AHEAD`, `REVISION_COMPACTED`,
  `WATCH_LAGGED`;
- availability: `OWNER_EXISTS`, `SERVER_BUSY`, `SERVER_SHUTTING_DOWN`,
  `DEADLINE_EXCEEDED`, `SERVER_UNAVAILABLE`, `PERMISSION_DENIED`;
- storage/admin: `STORAGE_DEGRADED`, `CORRUPT_IDENTITY`, `CORRUPT_MANIFEST`,
  `CORRUPT_SNAPSHOT`, `CORRUPT_WAL`, `UNKNOWN_FORMAT`,
  `DATABASE_ID_MISMATCH`, `REVISION_RANGE_EXHAUSTED`, `IO_ERROR`,
  `ADMIN_PRECONDITION_FAILED`, `SNAPSHOT_IN_PROGRESS`,
  `COMPACTION_NOT_SAFE`, `BACKUP_DESTINATION_EXISTS`, `BACKUP_INVALID`,
  `RECOVERY_REQUIRED`, `RECOVERY_ACTION_MISMATCH`,
  `AUDIT_FAILED_AFTER_APPLY`, `INTERNAL`.

`operation_applied:true` means authority may already have changed: reconcile
before any retry. CLI exits are 2 validation/protocol, 3 conditional/stream, 4
availability/permission, 5 storage/recovery, and 6 internal/post-apply audit.

## Public examples

The Go client always traverses the socket:

```go
client, err := gapdb.Dial("/srv/gapdb/gapdb.sock", gapdb.ClientOptions{Timeout: 5*time.Second})
record, err := client.Get(ctx, "leases/integration")
put, err := client.Put(ctx, "workflow/42", value, nil, gapdb.AckMemory)
winner, err := client.PutIfAbsent(ctx, "leases/integration", owner, &expiry, gapdb.AckDurable)
replaced, err := client.CompareAndSwap(ctx, "workflow/42", put.Revision, next, nil, gapdb.AckDurable)
deleted, err := client.DeleteIfRevision(ctx, "workflow/42", replaced.Revision, gapdb.AckDurable)
batch, err := client.AtomicBatch(ctx, gapdb.Batch{Ack: gapdb.AckDurable, Mutations: mutations})
page, err := client.ScanPrefix(ctx, "workflow/", 100, "")
watch, err := client.Watch(ctx, "workflow/", page.ObservedRevision)
```

The strict noninteractive CLI equivalents are:

```sh
gapctl --socket /srv/gapdb/gapdb.sock get --key leases/integration
gapctl --socket /srv/gapdb/gapdb.sock put --key workflow/42 --value-base64 'AQID' --ack memory
gapctl --socket /srv/gapdb/gapdb.sock put-if-absent --key leases/integration --value-base64 'b3duZXI=' --expires-at 2026-08-24T12:00:00Z --ack durable
gapctl --socket /srv/gapdb/gapdb.sock compare-and-swap --key workflow/42 --expected-revision 7 --value-base64 'BAUG' --ack durable
gapctl --socket /srv/gapdb/gapdb.sock delete-if-revision --key workflow/42 --expected-revision 8 --ack durable
gapctl --socket /srv/gapdb/gapdb.sock atomic-batch --file /run/gapdb/batch-v1.json
gapctl --socket /srv/gapdb/gapdb.sock scan-prefix --prefix workflow/ --limit 100
gapctl --socket /srv/gapdb/gapdb.sock --output jsonl watch --prefix workflow/ --after-revision 8
```

Online administration uses `status`, `health`, `stats`, `describe-config`, and:

```sh
gapctl --socket /srv/gapdb/gapdb.sock verify --mode full
gapctl --socket /srv/gapdb/gapdb.sock create-snapshot --expected-database-id DATABASE_ID --expected-revision REVISION
gapctl --socket /srv/gapdb/gapdb.sock compact --expected-database-id DATABASE_ID --through-revision REVISION
gapctl --socket /srv/gapdb/gapdb.sock backup --expected-database-id DATABASE_ID --expected-revision REVISION --destination /srv/gapdb-backups/run-42
```

Offline work refuses a live owner and separates observation, proposal, and apply:

```sh
gapctl --db /srv/gapdb inspect
gapctl --db /srv/gapdb verify --mode full
gapctl --db /srv/gapdb recover propose
gapctl --db /srv/gapdb recover apply --proposal-file /run/gapdb/proposal.json --action-id ACTION_ID --expected-database-id DATABASE_ID --expected-manifest-generation GENERATION --destination /srv/gapdb-quarantine/run-42
```

Only exact `--long-option` spelling is accepted; duplicate options fail before
stdin or dialing. JSON is unary output and JSONL is mandatory for watch.
