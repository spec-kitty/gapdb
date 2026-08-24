# Gapdb storage format v1

This living format reference is tied to the production codecs in
[internal/persist](../../internal/persist) and the committed storage
[goldens](../../tests/compatibility/storage/testdata/README.md). The golden
`identity-v1.hex`, `manifest-v1.hex`, `wal-v1.hex`, and `wal-tail-v1.hex` files
are decoded and re-encoded byte-for-byte by
[golden_test.go](../../tests/compatibility/storage/golden_test.go).

## Authority and permissions

The database directory is local, defaults to `0700`, and contains:

```text
LOCK
IDENTITY
CURRENT
snapshot-<20-digit-revision>.gdb
wal-<20-digit-start-revision>.gdb
audit.jsonl
gapdb.sock
```

Regular authority files and `LOCK` default to `0600`; the socket is `0600`.
The owner holds a nonblocking exclusive `flock` on the exact `LOCK` inode for
its lifetime. Diagnostic lock content and path metadata never prove ownership.
Temporary same-directory `.tmp-*` files are never authoritative.

All binary integers are unsigned big-endian. Binary structures carry an exact
magic and version 1. CRC uses CRC32C Castagnoli. Unknown magic/version, nonzero
reserved bytes, inconsistent identity, length overflow, duplicate keys,
checksum failure, or revision regression fails before authority is exposed.

## Identity and revision reservation

`IDENTITY` is 64 bytes: `GAPID001` (8), version (2), header length 64 (2),
database ID (16), reserved revision end (8), identity generation (8), 16 zero
bytes, then CRC32C (4). Before serving, the owner atomically writes and syncs a
new reservation generation and directory entry. Allocation occurs only inside
that durable range; unused revisions after a crash stay burned.

## CURRENT manifest

`CURRENT` is `GAPCUR01` (8), version (2), zero flags (2), JSON length (4), the
canonical JSON payload, then CRC32C (4). Payload field order is database ID,
generation, snapshot filename/revision/SHA-256, and WAL filename/start revision.
Names are base names without separators. Installation is temp create, write,
file sync, rename, directory sync. Recovery follows only the installed complete
manifest; unreferenced generations are cleanup candidates, not authority.

## WAL

The 64-byte header is `GAPWAL01`, version/header length, database ID, first
allowed revision, WAL generation, 16 zero bytes, and CRC32C. Each commit is:

```text
CMIT | version=1 | flags=0 | total length | revision | mutation count
payload length | mutation payload | CRC32C
```

Total length is `32 + payload_length`; mutation count is 1–1,024. Revisions are
strictly increasing and may contain burned gaps. Each payload item records
kind (`put=1`, `delete=2`, `expire=3`), expiry flag, zero reserved bytes, key and
value lengths, expiry Unix nanoseconds, UTF-8 key, and opaque value. Conditions
are not persisted because frames contain already-authorized effects. A frame is
atomic and item order is watch order.

Only an incomplete final frame may be truncated automatically. A partial magic
or declared final frame that reaches physical EOF is truncated to the prior
frame, then the WAL and audit are synced. Complete checksum/field/payload errors,
or any earlier invalid frame, return corruption without truncation.

## Snapshot, rotation, and compaction

A snapshot begins `GAPSNAP1`, version/header length, database ID, snapshot
revision, record count, payload length, and 12 zero bytes. Sorted record items
contain key/value lengths, record revision, expiry flag, seven zero bytes,
expiry nanoseconds, UTF-8 key, and opaque value. Expired records are omitted.
The final 32 bytes are SHA-256 over the entire header and payload. Size, count,
sort order, uniqueness, revisions, expiry, and digest all validate before use.

Installation at revision R is: WAL flush/sync; snapshot temp write/sync/rename
and directory sync; create/sync/rename `wal-(R+1)`; atomically install the next
`CURRENT`; then switch the writer. Old files are retained until guarded
compaction resolves explicit unreferenced paths and directory-syncs removal.
No unknown, temporary, active, lock, identity, audit, or socket path is an
automatic compaction target.

## Audit, backup, and recovery

Audit is bounded JSONL. Each canonical object ends with `crc32c` calculated over
the object without that field. State-changing administration syncs its audit
before ordinary success. A later audit failure returns
`AUDIT_FAILED_AFTER_APPLY` with `operation_applied:true` and degrades health.
Audit is evidence, never recovery authority.

A backup is a no-overwrite directory containing `IDENTITY`, `CURRENT`, one
verified snapshot, one following WAL, and `backup.json`; it excludes lock,
socket, audit, and unrelated generations. File sizes/hashes, database lineage,
revision, backup ID, time, and tool version are verified and recursively synced
before publication. Restore verifies and publishes to a new directory without
overwriting any existing target.

Startup recovery acquires ownership, validates ID and manifest, validates the
entire snapshot, strictly replays complete WAL commits, optionally repairs only
an incomplete final tail, rebuilds expiry/history, checks the reserved range,
durably reserves the next range, then creates the socket. Non-tail corruption
stays inspection-only. Offline recovery is always inspect → propose → explicit
guarded apply; it never auto-repairs corruption.

See [protocol-v1.md](protocol-v1.md) for structured errors and exact online and
offline commands, and [the recovery runbook](../operations/recovery.md) for the
operator sequence.
