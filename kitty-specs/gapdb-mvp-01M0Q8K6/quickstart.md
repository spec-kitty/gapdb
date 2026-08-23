# Gapdb MVP Quickstart

This is the planned developer and operator path. Commands become executable as
their implementation concerns land; they also define black-box acceptance seams.

## Prerequisites

- Linux `amd64` or `arm64`
- Go 1.26.x (`go version` records the exact patch)
- A local filesystem that supports Unix sockets, advisory locks, file sync, and
  atomic rename within one directory

## Build

```bash
go mod download
go build ./cmd/gapdbd
go build ./cmd/gapctl
```

The module starts as `gapdb`. Publishing under a remote module path is outside the
MVP and must not change the public package contract accidentally.

## Start an isolated database

```bash
gapdb_dir="$(mktemp -d)"
chmod 700 "$gapdb_dir"
./gapdbd --db "$gapdb_dir" --output=json
```

Startup emits one JSON readiness object containing database ID, socket path,
current revision, durable-through revision, and active limits. A second owner for
the same directory must fail with `OWNER_EXISTS`.

## Inspect status

```bash
./gapctl --socket "$gapdb_dir/gapdb.sock" status --output=json
./gapctl --socket "$gapdb_dir/gapdb.sock" describe-config --output=json
```

Automation reads stable JSON fields. It does not parse log prose.

## Put and read opaque bytes

```bash
printf 'owner-1' > /tmp/gapdb-value.bin

./gapctl --socket "$gapdb_dir/gapdb.sock" put leases/build \
  --value-file /tmp/gapdb-value.bin \
  --expires-at 2026-08-23T20:00:00Z \
  --ack durable \
  --request-id lease-create-1 \
  --output=json

./gapctl --socket "$gapdb_dir/gapdb.sock" get leases/build \
  --value-output base64 \
  --output=json
```

Lease, review, and integration-authority callers always select `--ack durable`.

## Conditional update

Read the current revision, then:

```bash
printf 'owner-2' > /tmp/gapdb-next.bin

./gapctl --socket "$gapdb_dir/gapdb.sock" cas leases/build \
  --expected-revision 42 \
  --value-file /tmp/gapdb-next.bin \
  --ack durable \
  --request-id lease-transfer-1 \
  --output=json
```

A mismatch exits nonzero and returns `REVISION_MISMATCH` with expected/actual
revisions and safe next actions.

## Atomic batch

Create `batch.json`:

```json
{
  "ack": "durable",
  "mutations": [
    {
      "kind": "delete",
      "key": "leases/old",
      "condition": { "kind": "revision", "expected_revision": 91 }
    },
    {
      "kind": "put",
      "key": "leases/new",
      "condition": { "kind": "absent" },
      "value_base64": "b3duZXItMg==",
      "expires_at": "2026-08-23T20:00:00Z"
    }
  ]
}
```

Then run:

```bash
./gapctl --socket "$gapdb_dir/gapdb.sock" atomic-batch \
  --file batch.json \
  --request-id lease-transfer-batch-1 \
  --output=json
```

Both changes receive one revision or neither changes.

## Scan and watch

```bash
./gapctl --socket "$gapdb_dir/gapdb.sock" scan-prefix leases/ \
  --limit 1000 \
  --output=json

./gapctl --socket "$gapdb_dir/gapdb.sock" watch leases/ \
  --after-revision 100 \
  --output=jsonl
```

If pagination becomes stale, restart the scan. If watch history is compacted or a
consumer lags, scan a fresh point-in-time view and resume after its observed
revision.

## Snapshot, compact, and backup

First obtain current database ID and revision from `status`, then use them as
guarded preconditions:

```bash
./gapctl --socket "$gapdb_dir/gapdb.sock" snapshot \
  --expected-database-id 0198f4d4f26a7b1ca3df00c30ca93e73 \
  --expected-revision 110 \
  --output=json

./gapctl --socket "$gapdb_dir/gapdb.sock" compact \
  --expected-database-id 0198f4d4f26a7b1ca3df00c30ca93e73 \
  --through-revision 110 \
  --output=json

./gapctl --socket "$gapdb_dir/gapdb.sock" backup \
  --expected-database-id 0198f4d4f26a7b1ca3df00c30ca93e73 \
  --expected-revision 110 \
  --destination /tmp/gapdb-backup-110 \
  --output=json
```

Snapshot creation is a durability barrier and may temporarily pause mutations.

## Offline inspection and recovery

Stop the owner or confirm it cannot start, then:

```bash
./gapctl inspect --db "$gapdb_dir" --output=json
./gapctl verify --db "$gapdb_dir" --mode full --output=json
./gapctl recover propose --db "$gapdb_dir" --output=json
```

Proposal is read-only. Apply requires the returned proposal ID plus exact database
and manifest preconditions:

```bash
./gapctl recover apply --db "$gapdb_dir" \
  --proposal-id proposal-123 \
  --expected-database-id 0198f4d4f26a7b1ca3df00c30ca93e73 \
  --expected-manifest-generation 7 \
  --quarantine /tmp/gapdb-quarantine-123 \
  --output=json
```

Never delete or edit WAL/snapshot files directly.

## Verification commands

```bash
gofmt -w -l cmd gapdb internal tests
go vet ./...
staticcheck ./...
go test ./...
go test -race ./...
go test ./tests/contract/...
go test ./tests/crash/...
go test ./tests/compatibility/...
```

Fault injection runs at least 1,000 deterministic schedules before acceptance.
Fuzz tests cover every wire and storage decoder.

## Reference benchmarks

```bash
go test -run '^$' -bench . -benchmem ./tests/performance/... \
  | tee benchmark-results.txt
```

The benchmark record also captures CPU, memory, kernel, filesystem, storage
device, Go version, data-generator seed, and repository commit.

## SQLite adoption gate

Run the originating application's Phase 1 storage contract once with SQLite and
once with Gapdb through the thin adapter. Keep SQLite selected until contract,
crash, race, expiry, watch, and authority-safety evidence is complete and a human
explicitly approves the switch.
