# Gapdb model-operation configuration

`gapctl` is a non-interactive command API. JSON is the default for unary
commands; `watch` requires JSONL. It never detects a terminal, prompts, pages,
adds color, or reads stdin unless `--value-stdin`, `--stdin`, or
`--proposal-stdin` is explicitly selected.

```text
gapctl [--socket PATH | --db PATH] [--deadline 30s]
       [--request-id ID] [--output json|jsonl] COMMAND [COMMAND FLAGS]
```

Use `gapctl help`, `gapctl schema`, and `gapctl limits` to discover the bounded
version-1 surface. `gapctl help --text` is the only human-oriented output and is
also deterministic and bounded. All JSON/JSONL frames contain
`schema_version: 1` and the active client `limits` object.
Only the documented exact `--long-option` spelling is accepted. Single-dash
long options and duplicate options, including mixed split and `=value` forms,
fail before any input read or connection attempt.

## Targets and input

- Online commands require `--socket` and reject `--db`.
- `inspect`, offline `verify`, and `recover` require a clean absolute `--db`
  path, reject `--socket`, and acquire the exclusive owner lock.
- A value comes from exactly one of canonical padded standard
  `--value-base64`, `--value-file`, or `--value-stdin`. Empty values are valid.
- Expiry is optional canonical UTC RFC3339Nano ending in `Z`.
- Mutations require an explicit `--ack memory|durable`. Leases, reviews,
  workflow authority, and integration authority must use `durable`.
- Atomic batch input is a version-1 `arguments` object supplied by exactly one
  of `--file` or `--stdin`. Duplicate fields, unknown fields, duplicate keys,
  non-canonical base64, and over-limit input fail before dialing.

Example:

```sh
gapctl --socket /srv/gapdb/gapdb.sock --request-id lease-41 put-if-absent \
  --key leases/integration --value-base64 'b3duZXItMQ==' --ack durable
```

Unary stdout is exactly one ordered JSON object and one newline. Automation
branches on `ok`, `error.code`, `error.retry`, and `error.safe_actions`; the
advisory `message` is never a decision input. Process exits are fixed:

| Exit | Stable class |
|---:|---|
| 0 | success |
| 2 | request, validation, or protocol |
| 3 | conditional state, stale scan, or watch cursor |
| 4 | ownership, availability, deadline, or permission |
| 5 | storage, compatibility, administrative, backup, or recovery |
| 6 | internal invariant or audit failure after apply |

`owner_started_at`, administrative durations, and live counters are documented
volatile result fields. The envelope, limits, identifiers, revisions, codes,
retry class, and safe-action ordering are stable.

## Commands

Data commands are `get`, `put`, `put-if-absent`, `compare-and-swap`,
`delete-if-revision`, `atomic-batch`, `scan-prefix`, and `watch`. Online
administration is `status`, `health`, `stats`, `describe-config`, `verify`,
`create-snapshot`, `compact`, and `backup`.

`scan-prefix` returns the server's `observed_revision`, `as_of`, ordered records,
`truncated`, and opaque `cursor`. Never modify or interpret a cursor. A stale
cursor requires a new first-page scan.

`watch --output jsonl` emits one flushed `started` frame, ordered `event` frames,
and one `ended` frame. Every frame has schema and limits. A lagged, compacted,
ahead, disconnected, deadline, or local-signal ending is nonzero and preserves
the last-delivered/current/earliest evidence supplied by the contract. Every
failure after recognizing `watch`, including local flag, target, dial, status,
and registration failures, is exactly one `stream:"ended"`, `reason:"error"`
JSONL frame. Before status identifies the database, `database_id` is the empty
string; callers must not infer database authority from it.
