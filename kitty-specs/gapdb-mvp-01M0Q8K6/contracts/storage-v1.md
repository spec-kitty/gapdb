# Gapdb Storage Format v1

## Scope and invariants

This contract defines database identity, revision reservation, WAL, snapshots,
manifest installation, audit records, recovery, and backup for format version 1.

- All binary integers are unsigned big-endian unless stated otherwise.
- All lengths are validated against configured hard ceilings before allocation.
- All file names recorded in metadata are base names without separators.
- Database directories default to `0700`; regular files default to `0600`.
- Unknown magic, version, reserved nonzero bits, identity mismatch, duplicate keys,
  length overflow, checksum mismatch, or revision regression fails closed.
- Only an incomplete final WAL frame is truncated automatically.

## Directory layout

```text
LOCK
IDENTITY
CURRENT
snapshot-<20-digit-revision>.gdb
wal-<20-digit-start-revision>.gdb
audit.jsonl
gapdb.sock
```

Temporary files are created in the same directory with exclusive creation and a
`.tmp-<random>` suffix. A temp file is never authoritative.

## `LOCK`

The owner opens `LOCK` read/write, creates it as `0600` when absent, and acquires
`LOCK_EX|LOCK_NB` with `unix.Flock`. The descriptor remains open until shutdown.

After locking, the owner may overwrite diagnostic JSON containing PID, process
start time, database ID when known, and socket path. Diagnostic content never
proves ownership; the kernel lock does. Only the lock holder may remove a stale
socket.

## `IDENTITY`

Fixed 64-byte structure:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 8 | ASCII `GAPID001` |
| 8 | 2 | format version `1` |
| 10 | 2 | header length `64` |
| 12 | 16 | database ID |
| 28 | 8 | reserved revision end |
| 36 | 8 | identity generation |
| 44 | 16 | zero reserved bytes |
| 60 | 4 | CRC32C of bytes 0–59 |

### Revision reservation

Before accepting clients, startup writes a new identity generation whose reserved
end is the prior end plus the configured range size, syncs the temp file, renames
it over `IDENTITY`, and syncs the directory. The process allocates only inside
that synced range. Range exhaustion repeats the protocol before another revision
is assigned.

A crash can leave a valid old or new generation, never a partially accepted one.
If the new generation is authoritative, unused numbers remain permanently burned.

## `CURRENT`

`CURRENT` is one checksummed metadata frame:

```text
8 bytes  magic = GAPCUR01
2 bytes  version = 1
2 bytes  flags = 0
4 bytes  JSON payload length
N bytes  UTF-8 JSON payload
4 bytes  CRC32C over header and payload
```

The payload is encoded from a Go struct in this exact field order:

```json
{
  "database_id": "32-lowercase-hex",
  "generation": 7,
  "snapshot_file": "snapshot-00000000000000000110.gdb",
  "snapshot_revision": 110,
  "snapshot_sha256": "64-lowercase-hex",
  "wal_file": "wal-00000000000000000111.gdb",
  "wal_start_revision": 111
}
```

Installation is temp create → write → file sync → rename → directory sync.
Recovery follows only the installed `CURRENT`; other complete files are retained
cleanup candidates.

## WAL file

### File header

Fixed 64 bytes:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 8 | ASCII `GAPWAL01` |
| 8 | 2 | format version `1` |
| 10 | 2 | header length `64` |
| 12 | 16 | database ID |
| 28 | 8 | first allowed revision |
| 36 | 8 | WAL generation |
| 44 | 16 | zero reserved bytes |
| 60 | 4 | CRC32C of bytes 0–59 |

### Commit frame

```text
4 bytes  magic = CMIT
2 bytes  entry version = 1
2 bytes  flags = 0
4 bytes  total frame length including trailer
8 bytes  commit revision
4 bytes  mutation count
4 bytes  payload length
N bytes  mutation payload
4 bytes  CRC32C over frame header and payload
```

The total frame length must equal `32 + payload_length`. Mutation count is
1–1,024. Frames are strictly increasing by revision; gaps are valid.

### Mutation payload item

```text
1 byte   kind: 1=put, 2=delete, 3=expire
1 byte   flags: bit 0=has_expiry; all other bits zero
2 bytes  reserved zero
4 bytes  key length
4 bytes  value length
8 bytes  resulting expiry Unix nanoseconds, zero when absent
N bytes  UTF-8 key
M bytes  raw value
```

Rules:

- `put` may contain value bytes and optional expiry.
- `delete` and `expire` require value length zero and no expiry.
- Keys are unique inside one frame.
- Conditions are not stored: the frame represents effects already validated.
- Replay applies every item at the frame revision or none.
- Item order becomes watch event order.

### Tail handling

At end of file:

- Fewer than four bytes of next magic: incomplete tail, truncate to prior frame.
- Header present but declared frame bytes absent: incomplete tail, truncate.
- Complete declared frame with wrong magic, fields, or checksum: corruption, fail.
- Invalid payload within a complete checksummed frame: corruption, fail.
- Any invalid frame before the final physical frame: corruption, fail.

Truncation is followed by file sync and an audit record before writable startup.

## Snapshot file

### Header

```text
8 bytes  magic = GAPSNAP1
2 bytes  version = 1
2 bytes  header length
16 bytes database ID
8 bytes  snapshot revision
8 bytes  record count
8 bytes  payload length
12 bytes zero reserved
```

### Record item

```text
4 bytes  key length
4 bytes  value length
8 bytes  record revision
1 byte   flags: bit 0=has_expiry
7 bytes  zero reserved
8 bytes  expiry Unix nanoseconds, zero when absent
N bytes  UTF-8 key
M bytes  raw value
```

Records are strictly sorted by UTF-8 key bytes. Each record revision is nonzero
and at most the snapshot revision. Records expired at snapshot as-of time are
excluded.

### Trailer

The final 32 bytes are SHA-256 over the complete header and payload. File size,
payload length, record count, sort order, uniqueness, fields, and SHA-256 must all
validate before any record is exposed.

## Snapshot installation and WAL rotation

With mutation execution paused:

1. Flush and sync the active WAL; record revision `R`.
2. Encode the stable map to a snapshot temp file, sync, rename to
   `snapshot-R.gdb`, and sync the directory.
3. Create `wal-(R+1).gdb` with a valid header, sync, rename, and sync directory.
4. Write and atomically install the next `CURRENT` generation referencing both.
5. Switch the in-memory active WAL handle and resume mutations.

If any step before `CURRENT` installation fails, the old manifest remains
authoritative. If directory sync after manifest rename fails, startup validates
whichever complete manifest generation is present and never assumes the response
was durable. Old files are not deleted by snapshot creation.

## Compaction

`Compact` requires matching database ID and a `through_revision` no newer than the
active snapshot revision. It resolves the exact unreferenced snapshot/WAL file
list before mutation, returns that list in dry-run/proposal output, deletes only
those explicit paths, and syncs the directory.

Temporary files and unknown names are reported but not removed automatically.
The active manifest, identity, lock, active snapshot, active WAL, audit, and socket
are never compaction targets.

## Audit log

Each line is one UTF-8 JSON object with a final `crc32c` field calculated over the
canonical field-ordered JSON object with `crc32c` omitted. Entries are bounded and include schema version,
event ID, timestamp, database ID, operation, request ID, relevant revisions,
outcome/error code, paths, and safe actions.

State-changing administration appends and syncs its audit entry before reporting
ordinary success. If state changed but audit sync fails, the result is
`AUDIT_FAILED_AFTER_APPLY` with `operation_applied: true`; the server becomes
degraded until verification.

Audit rotation is size-bounded and keeps a configured number of generations.
Audit history is evidence, not a state-recovery input.

## Recovery algorithm

1. Acquire `LOCK`; otherwise return `OWNER_EXISTS` without reading mutable files.
2. Validate `IDENTITY` without modifying it.
3. Validate `CURRENT` checksum, database ID, names, and generation.
4. Validate the entire referenced snapshot before materializing records.
5. Validate the WAL header and replay complete frames strictly after the snapshot revision.
6. Truncate only an incomplete final frame; sync and audit that action.
7. Rebuild expiry heap and bounded change history while omitting logically expired records from visibility.
8. Confirm recovered revision does not exceed the identity reserved end.
9. Atomically reserve and sync the next revision range only after authoritative state is valid.
10. Create the socket only after recovery reaches `ready`.

Non-tail corruption stops at `inspection_only`; it never exposes a partially
replayed writable database.

## Backup format

A backup is a directory containing `IDENTITY`, `CURRENT`, one verified snapshot,
one WAL beginning after the snapshot, and `backup.json`. It excludes lock, socket,
audit, and unrelated generations.

`backup.json` records schema version, source database ID, backup ID, durable
revision, creation time, file sizes and hashes, and tool version. The backup is
built in a sibling temp directory, verified independently, synced recursively,
and renamed to the requested final destination.

Restore never overwrites an existing database directory. It verifies the backup,
copies to a new temp directory, syncs, renames, and preserves the source database
ID so clients can recognize restored lineage.

## Fault-injection boundaries

Tests can fail or terminate immediately before and after:

- identity temp create/write/sync/rename/directory sync;
- WAL header/frame write, buffer flush, and file sync;
- map application and response publication;
- snapshot create/write/sync/rename/directory sync;
- next-WAL create/sync/rename;
- manifest write/sync/rename/directory sync;
- compaction remove and directory sync;
- backup file copy/sync/rename; and
- audit append/sync.

Recovered state is compared to the externally observed acknowledgement ledger,
with durable acknowledgements required and memory acknowledgements permitted but
never allowed to cause revision reuse or partial commits.
