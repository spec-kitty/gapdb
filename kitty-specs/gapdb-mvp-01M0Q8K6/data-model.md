# Phase 1 Data Model: Gapdb MVP

## Modeling principles

- Application values are opaque; Gapdb models coordination metadata only.
- Mutable authority lives inside one `DatabaseState` owned by the writer.
- Public byte slices are copied on ingress and egress; stored values are immutable.
- One `CommitRevision` identifies every change in one atomic commit.
- Revisions may have gaps after restart but are never reused within a database ID.
- Logical expiry is authoritative even before physical cleanup.

## Value objects

### DatabaseID

A random 128-bit identifier created once with the database directory.

**Rules**:

- Immutable for the lifetime of the database.
- Present in identity, manifest, WAL, snapshot, protocol responses, cursors, and backups.
- A file or request carrying another database ID is rejected.

### Revision

An unsigned 64-bit database-wide commit identifier.

**Rules**:

- Zero means the empty database before its first commit.
- Each successful single mutation or batch receives one revision.
- Failed validation does not allocate a revision.
- Reserved but unused revisions may be skipped after crash or clean restart.
- A revision is never assigned to more than one commit for a given `DatabaseID`.

### Key

A non-empty UTF-8 string compared and sorted by its UTF-8 bytes.

**Rules**:

- Maximum 4 KiB encoded bytes by default.
- Slash has no storage semantics; callers may use it for namespaces.
- Prefix matching is raw byte-prefix matching.

### Value

Opaque bytes, including the valid empty value.

**Rules**:

- Maximum 8 MiB by default.
- Copied on public ingress and egress.
- Encoded as base64 in JSON contracts and as raw bytes in storage formats.

### Expiry

An optional absolute UTC instant encoded internally and on disk as signed Unix
nanoseconds.

**Rules**:

- A new expiry must be strictly later than the server's effective current time.
- At or after expiry, the record is absent from reads, scans, and conditions.
- The effective clock never moves backward during one process lifetime.

### RequestID

An optional caller-supplied UTF-8 correlation string.

**Rules**:

- Maximum 256 bytes.
- Echoed in every response and relevant audit record.
- Does not provide exactly-once deduplication.

## Core entities

### Record

| Field | Type | Meaning |
|---|---|---|
| `key` | `Key` | Record identity |
| `value` | `Value` | Opaque application payload |
| `revision` | `Revision` | Commit that created the current version |
| `expires_at` | optional `Expiry` | Logical absence boundary |

Records have no stored creation time, update time, schema, tags, or secondary
attributes.

### Condition

| Kind | Fields | Passes when |
|---|---|---|
| `any` | none | Always; valid only for put |
| `absent` | none | No live record exists |
| `revision` | `expected_revision` | A live record exists at exactly that revision |

Expired records satisfy `absent` and fail `revision`.

### Mutation

| Kind | Required fields | Allowed conditions |
|---|---|---|
| `put` | key, value, optional expiry | any, absent, revision |
| `delete` | key | revision |

`Put`, `PutIfAbsent`, `CompareAndSwap`, and `DeleteIfRevision` are convenience
forms of one mutation. An `AtomicBatch` contains 1–1,024 mutations with distinct
keys. Batch conditions all observe the pre-batch live state.

### Commit

| Field | Type | Meaning |
|---|---|---|
| `database_id` | `DatabaseID` | Ownership domain |
| `revision` | `Revision` | Shared commit identity |
| `ack_mode` | `memory` or `durable` | Requested response barrier |
| `mutations` | ordered list | Complete all-or-nothing state change |
| `origin` | client, expiry, or admin | Audit classification |
| `request_id` | optional `RequestID` | Correlation only |

**Invariant**: WAL encoding, map application, event creation, and recovery use the
same mutation order. No decoder may recover a proper subset.

### ChangeEvent

| Field | Type | Meaning |
|---|---|---|
| `database_id` | `DatabaseID` | Database identity |
| `revision` | `Revision` | Commit identity |
| `order` | unsigned integer | Zero-based position inside the commit |
| `kind` | put, delete, or expire | Observable change |
| `key` | `Key` | Changed key |
| `record` | optional `Record` | Present for put |

Delete and expire events omit values. History eviction removes every event with
the same revision together.

### WatchSubscription

| Field | Type | Meaning |
|---|---|---|
| `prefix` | UTF-8 string | Event key filter |
| `after_revision` | `Revision` | Exclusive replay cursor |
| `registration_revision` | `Revision` | Last revision covered by backlog |
| `last_delivered_revision` | `Revision` | Recovery evidence on termination |
| `live_queue` | bounded event queue | Events after registration |

### ScanCursor

Versioned base64url-encoded structure containing:

- database ID;
- prefix;
- last returned key;
- observed database revision;
- as-of Unix nanoseconds; and
- CRC32C checksum.

A cursor is rejected if malformed, from another database, for another prefix, or
if the current database revision differs from its observed revision.

## In-memory aggregate

### DatabaseState

| Field | Ownership | Purpose |
|---|---|---|
| `records map[string]Record` | writer mutates; readers under `RWMutex` | Live and not-yet-cleaned expired records |
| `current_revision` | writer | Latest successfully applied commit |
| `revision_allocator` | writer | Current reserved range and next value |
| `expiry_heap` | writer | Candidate deadlines with record revisions |
| `history_ring` | writer | Bounded complete change commits |
| `watchers` | writer | Registered live queues |
| `effective_time` | atomic monotonic max | Prevent expiry resurrection |
| `health` | writer/atomic view | ready, degraded, shutting_down |

The map and revision change together under one write lock. A batch is never
visible in an intermediate state.

## Persisted entities

### IdentityFile

| Field | Purpose |
|---|---|
| format magic/version | Compatibility gate |
| database ID | File ownership |
| reserved revision end | Never-reuse high-water mark |
| checksum | Integrity |

### Manifest

| Field | Purpose |
|---|---|
| format version | Compatibility gate |
| database ID | File ownership |
| snapshot filename/revision/checksum | Active base image |
| WAL filename/start revision | Active replay suffix |
| generation | Monotonic manifest generation |
| checksum | Integrity |

### Snapshot

A point-in-time ordered list of live records plus database ID, snapshot revision,
record count, format version, and whole-file SHA-256. Keys are stored in bytewise
sort order so output is deterministic and duplicates are rejected during decode.

### WALFrame

One complete `Commit` encoded as a bounded binary payload with file/entry version,
revision, mutation count, length, and CRC32C. The active WAL begins strictly after
the manifest snapshot revision.

### AuditEntry

Versioned JSONL for startup recovery and state-changing administration:

- event ID and UTC timestamp;
- database ID;
- operation and request ID;
- expected, before, and after revisions where applicable;
- outcome/error code;
- affected file names; and
- safe next actions.

Audit records do not participate in state recovery and cannot authorize a commit.

## State transitions

### Owner lifecycle

```text
stopped
  └─ acquire lock → validating
       ├─ valid files + revision reservation synced → ready
       ├─ incomplete final WAL frame → truncate tail, audit → ready
       └─ corruption/version/identity conflict → inspection_only

ready
  ├─ persistence error → degraded_read_only
  ├─ shutdown requested → draining → stopped
  └─ forced termination → stopped (memory acknowledgements may be lost)
```

### Client mutation

```text
received → validate envelope/limits → evaluate condition
  ├─ invalid/condition failed → structured failure; no revision
  └─ success → allocate revision → append complete WAL frame
       ├─ append error → storage failure; no apply
       ├─ memory → apply → publish → acknowledge
       └─ durable → flush+sync
            ├─ sync error → storage failure; no apply
            └─ apply → publish → acknowledge durable
```

### Record visibility

```text
missing ── put ──► live ── matching put ──► live(new revision)
   ▲                 │  ├─ matching delete ──► missing
   │                 │  └─ expiry instant ──► logically expired
   │                 │
   └─ cleanup ◄──── logically expired ── put-if-absent ──► live(new revision)
```

Physical expiry cleanup is observable but is not required for logical absence.

## Cross-entity invariants

1. `Record.revision` always names the commit that wrote that exact record value.
2. Every event in one commit has the commit revision and a unique contiguous order.
3. Manifest snapshot and WAL database IDs equal the identity database ID.
4. WAL replay revisions are strictly increasing; gaps are valid, duplicates and regressions are corruption.
5. Snapshot keys are unique and strictly sorted.
6. The revision allocator never returns a value above its durably reserved bound.
7. A durable response is sent only after its WAL frame and every earlier frame pass one successful sync.
8. A watch never silently crosses a history gap or delivers half a batch.
9. A stale expiry heap item cannot delete a record with another revision.
10. Offline inspection or recovery never runs while another process owns the lock.
