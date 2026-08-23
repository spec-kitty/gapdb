# Implementation Plan: Gapdb MVP

**Branch**: `gapdb-mvp` | **Date**: 2026-08-23 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `kitty-specs/gapdb-mvp-01M0Q8K6/spec.md`

## Summary

Build Gapdb as one Go module with two binaries (`gapdbd` and `gapctl`) and a
first-party client package. The owner process holds a Unix advisory lock, loads a
checksummed snapshot plus WAL into an in-memory map, and accepts framed JSON
requests over an owner-only Unix socket. Exact reads use an `RWMutex`; every
mutation, expiry cleanup, watch registration, revision allocation, and snapshot
barrier passes through one writer goroutine.

The writer appends a complete checksummed frame to a buffered WAL before applying
the corresponding in-memory commit. Memory acknowledgement follows visibility in
RAM; durable acknowledgement additionally flushes and syncs the WAL, forming a
barrier for every earlier commit. Snapshots deliberately pause mutations while
reads continue, trading rare write latency for a small and auditable crash model.

Revision ranges are reserved durably before use so a crash may create revision
gaps but can never cause a memory-acknowledged revision to be reused for unrelated
state. This protects stale CAS values and watch cursors without turning every
memory acknowledgement into a disk sync.

## Engineering Alignment

The user confirmed that the existing specification provides enough planning
context and requested no further interrogation. This plan therefore applies the
smallest design consistent with the confirmed invariants:

- one process, one database directory, one writer, and ordinary Go locks;
- one shared database-wide revision per atomic commit;
- fail-closed recovery and explicit guarded administration;
- resumable watches only within bounded retained history;
- deterministic JSON for model operation and compact binary persistence;
- a single narrow OS dependency for advisory file locking; and
- SQLite retained until the external adoption gate passes.

No product-level decision remains deferred.

## Technical Context

**Language/Version**: Go 1.26.4 (the project reference toolchain; `go 1.26` module directive)  
**Primary Dependencies**: Go standard library; `golang.org/x/sys/unix` only for advisory `flock` ownership  
**Storage**: In-memory `map[string]Record`; binary CRC32C WAL; binary SHA-256 snapshots; atomic checksummed manifest and identity files  
**Testing**: `go test`, `go test -race`, black-box Unix-socket/CLI contract tests, deterministic clock tests, filesystem fault injection, crash subprocess tests, fuzzing, golden format fixtures, and benchmarks  
**Target Platform**: Linux on local `amd64` and `arm64`; Unix domain sockets and filesystem durability are required  
**Project Type**: Single Go module with server, CLI, public client, and internal packages  
**Performance Goals**: `Get` p95 ≤2 ms, memory write p95 ≤5 ms, durable write p95 ≤50 ms, and recovery of the reference data set ≤5 s  
**Constraints**: One owner; one mutation goroutine; synchronized direct reads; owner-only socket; bounded queues and frames; no TCP, SQL, distribution, cgo, code generation, lock-free state, or embedded model  
**Scale/Scope**: Reference profile of 100,000 live 1 KiB records, eight concurrent readers, one writer, 256 clients, 100,000 retained change events or 64 MiB of event history (whichever is reached first)  

### Default Limits

| Limit | Default | Failure behavior |
|---|---:|---|
| Key bytes | 4 KiB | `KEY_TOO_LARGE` |
| Value bytes | 8 MiB | `VALUE_TOO_LARGE` |
| Protocol frame | 16 MiB | `FRAME_TOO_LARGE`, close connection |
| Batch operations | 1,024 | `BATCH_TOO_LARGE` |
| Batch encoded bytes | 16 MiB | `BATCH_TOO_LARGE` |
| Scan page records | 1,000 | Return continuation token |
| Scan encoded bytes | 16 MiB | Stop page before limit |
| Watch live buffer | 256 events | `WATCH_LAGGED`, close watch |
| Change history | 100,000 events or 64 MiB | Evict complete oldest commits |
| Concurrent clients | 256 | `SERVER_BUSY` |
| Revision reservation | 1,048,576 revisions | Reserve and sync next range before use |

Limits are configurable at startup, reported by `Status` and `DescribeConfig`,
and validated against safe hard ceilings. Configuration never changes wire or
storage interpretation.

## Charter Check

*GATE: Passes before Phase 0 and after Phase 1 design.*

| Charter rule | Design response | Gate |
|---|---|---|
| GAP-001 fail closed | Durable barriers, atomic batches, non-reused revisions, complete-tail-only truncation, and corruption refusal are explicit contracts. | PASS |
| GAP-002 small boundary | One Go module, one writer, one map, one Unix socket, one small OS dependency; no general database features. | PASS |
| GAP-003 stable evidence | Versioned framed JSON, stable errors, offline inspection, and guarded admin operations are specified. | PASS |
| GAP-004 versioned formats | Protocol, WAL, snapshot, identity, and manifest formats are versioned and checksummed. | PASS |
| GAP-005 bounded resources | All frames, values, batches, scans, histories, queues, clients, and outputs have limits. | PASS |
| GAP-006 SQLite adoption | External dual-backend contract and authority-safety suite remains a release gate. | PASS |
| GAP-007 explicit exceptions | No exception is introduced by this plan. | PASS |
| Test-first and black-box doctrine | Contracts and golden fixtures precede implementation; public client, socket, and CLI remain the integration seams. | PASS |
| Living documentation | Format contracts, quickstart, error catalog, and spec references ship with behavior. | PASS |

Post-design recheck: all Phase 1 artifacts preserve the same boundaries. The one
external dependency is justified because advisory locks are released by the OS on
process death; a stale lock-file protocol would require unsafe liveness inference.

## Architecture

### Component boundaries

```text
applications / model operator
        │ Go client or gapctl
        ▼
versioned framed protocol over Unix socket
        │
        ▼
server connection handlers ── read-only admin/status
        │
        ├── Get / ScanPrefix ── synchronized in-memory view
        │
        └── mutation/watch/admin commands
                    │
                    ▼
             single writer loop
        ┌───────────┼────────────┐
        ▼           ▼            ▼
   WAL/barriers  map+expiry   history+watchers
        │
        ▼
 snapshot / manifest / backup / recovery
```

Public packages expose records, conditions, operations, errors, client methods,
and watch streams. Persistence frames, lock handling, mutable engine state, and
fault-injection seams remain internal.

### Mutation pipeline

1. A connection handler validates the envelope and sends a bounded command to the writer.
2. The writer evaluates all conditions against one locked pre-commit state.
3. On success, it obtains the next never-reused revision from the reserved range.
4. It builds one complete WAL frame and writes it to the buffered WAL.
5. For `durable`, it flushes and calls `Sync`; the barrier covers all earlier frames.
6. It applies the infallible map mutation under the write lock.
7. It appends ordered change events, updates expiry scheduling, and publishes to watchers.
8. It returns the structured result. A response lost after commit is reconciled with conditions and revisions.

If WAL write, flush, or sync fails, the commit is not applied when failure is
known beforehand. A failure discovered after earlier memory acknowledgements
places the owner in write-disabled degraded state: reads and inspection remain
available, all new mutations fail with `STORAGE_DEGRADED`, and restart/recovery is
required. The server never fabricates a durable result.

### Read pipeline

- `Get` takes `RLock`, evaluates logical expiry using a nondecreasing effective
  clock, copies the immutable record view, and releases the lock.
- `ScanPrefix` copies matching live record views and the observed revision under
  `RLock`, then sorts and encodes outside the lock. Continuation tokens bind the
  database ID, prefix, last key, observed revision, and as-of time. A later commit
  produces `SCAN_STALE`; no MVCC snapshot is retained.
- Input and output byte slices are copied at public boundaries. Stored values are
  immutable after insertion.

### Revision allocation

`IDENTITY` stores the database ID, format version, and the upper bound of the
currently reserved revision range. Before accepting clients, startup atomically
advances and syncs that bound by 1,048,576. The first new commit uses the first
revision in the new range. Exhaustion reserves another range before allocation.

An unclean restart can burn unused revisions, which is acceptable. It cannot
reuse a revision observed by a client. Failed validation does not allocate a
revision. A request to watch after a revision beyond current state returns
`REVISION_AHEAD`; a cursor below retained history returns `REVISION_COMPACTED`.

### Expiry

The engine holds an expiry min-heap containing `(expires_at, key, record_revision)`.
Stale heap entries are ignored. Readers always enforce logical expiry. The writer
timer collects due entries, sorts keys for deterministic events, and commits them
as one internal expiry batch. If an expired record has already been replaced, its
heap entry cannot delete the replacement. Replacing an expired record through
`PutIfAbsent` produces the new put event; a separate expire event is unnecessary.

### Watches

Watch registration is serialized by the writer to close the history/live race.
It returns a bounded matching backlog and registers live delivery beginning after
the registration revision. The handler sends backlog first and then the bounded
live queue. Overflow closes the stream with `WATCH_LAGGED` and the last completely
delivered revision.

History is rebuilt from post-snapshot WAL frames during recovery and bounded by
count and bytes. Eviction removes whole commit revisions so a batch is never
partially replayable. Snapshot installation makes revisions at or before the
snapshot revision compacted for new watches.

### Snapshots, compaction, and backup

The MVP chooses a stop-the-writer snapshot protocol for auditability:

1. Pause new mutation execution while reads continue.
2. Flush and sync the WAL, making the current revision a durability barrier.
3. Serialize the stable map to a temporary checksummed snapshot and sync it.
4. Rename the snapshot, sync the database directory, create and sync a new WAL
   beginning after the snapshot revision, then atomically install and sync `CURRENT`.
5. Resume mutations on the new WAL.

`CreateSnapshot` retains superseded files. Guarded `Compact` deletes only files
not referenced by `CURRENT`, after verifying database ID and expected revision,
then syncs the directory. `Backup` first establishes the same durable barrier and
writes a self-contained verified generation to a temporary destination directory
before atomic rename. Large snapshots may delay writes; status exposes progress.

### Ownership and transport

The owner opens `LOCK` and acquires nonblocking exclusive `flock` through
`golang.org/x/sys/unix`. The file descriptor remains open for the process
lifetime, so the kernel releases ownership after a crash. Only after acquiring
the lock may startup remove a stale socket. The socket is created with `0600`
permissions and unlinked on normal close.

Frames use a four-byte big-endian length followed by one JSON envelope. Values
are base64 in JSON. Requests and unary responses are one frame each; watch streams
send a start response, event frames, and one terminal frame. Connection deadlines,
frame limits, and client limits prevent resource exhaustion.

## Persistent Layout

```text
<database-dir>/
├── LOCK                         # advisory lock; owner metadata is diagnostic only
├── IDENTITY                     # database ID + reserved revision bound + checksum
├── CURRENT                      # active snapshot/WAL names + revisions + checksum
├── snapshot-<revision>.gdb      # immutable verified snapshot
├── wal-<start-revision>.gdb     # active or retained immutable WAL generation
├── audit.jsonl                  # bounded/rotated structured administrative audit
└── gapdb.sock                   # live Unix socket; never part of a backup
```

Temporary files use the final directory and a `.tmp-<random>` suffix so rename
does not cross filesystems. Recovery trusts `CURRENT`, validates every referenced
file, replays only revisions after the snapshot, and treats unreferenced complete
files as cleanup candidates rather than authority.

## Project Structure

### Documentation (this mission)

```text
kitty-specs/gapdb-mvp-01M0Q8K6/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── protocol-v1.md
│   ├── storage-v1.md
│   └── errors-v1.md
└── tasks/                       # populated only by /spec-kitty.tasks
```

### Source Code (repository root)

```text
go.mod
go.sum
cmd/
├── gapdbd/
│   └── main.go
└── gapctl/
    └── main.go
gapdb/
├── client.go                   # public Go client
├── types.go                    # public records, batches, watches, errors
└── options.go
internal/
├── engine/                     # map, writer, conditions, expiry, history
├── persist/                    # identity, WAL, snapshots, manifest, recovery
├── protocol/                   # framed JSON schemas and codecs
├── server/                     # Unix listener, handlers, lifecycle
├── admin/                      # inspect, verify, compact, backup, recovery proposals
├── clock/                      # real and deterministic clocks
└── faultfs/                    # filesystem interface and deterministic fault hooks
docs/
├── formats/                    # promoted public format documentation
└── operations/                 # configuration and recovery runbooks
tests/
├── contract/                   # black-box client/socket/CLI behavior
├── crash/                      # subprocess termination and recovery matrix
├── compatibility/              # golden wire/storage fixtures
├── adoption/                   # originating-app dual-backend contract
└── performance/                # reproducible reference benchmarks
```

**Structure Decision**: A single Go module keeps every invariant in one
repository while separating public API from replaceable internal concerns. The
two commands are thin composition roots. No repository abstraction or plugin
system is introduced.

## Testing and Verification Strategy

1. Write public contract fixtures before production packages.
2. Implement pure engine transitions against an injected clock with table,
   property, fuzz, and race tests.
3. Implement persistence behind a narrow filesystem interface with hooks before
   and after write, flush, sync, rename, directory sync, truncate, remove, and
   response publication.
4. Run at least 1,000 deterministic crash/fault schedules and compare recovered
   state to acknowledged results.
5. Exercise the actual binaries through Unix sockets and JSON stdout; integration
   tests do not import internal packages.
6. Freeze v1 protocol and storage golden fixtures before implementation is called complete.
7. Record the reference CPU, kernel, filesystem, storage device, Go version,
   command, data generator seed, and raw benchmark output.
8. Run the originating application's contract against SQLite and Gapdb; do not
   change the production selection until explicit human approval.

## Complexity Tracking

No charter violation is accepted. The one non-standard-library dependency is a
narrow syscall wrapper used to make single ownership kernel-enforced and
crash-releasing; a stale lock-file design cannot prove owner death safely.

## Implementation Concern Map

### IC-01 — Public contracts and compatibility fixtures

- **Purpose**: Freeze public types, operation semantics, limits, frames, errors, and v1 golden fixtures before implementation.
- **Relevant requirements**: FR-001–FR-008, FR-013–FR-018, FR-024; NFR-006–NFR-008, NFR-011
- **Affected surfaces**: `gapdb/`, `internal/protocol/`, `contracts/`, `tests/contract/`, `tests/compatibility/`
- **Sequencing/depends-on**: none
- **Risks**: Accidental implementation detail in the public API; ambiguous retry or expiry behavior.

### IC-02 — In-memory engine and never-reused revisions

- **Purpose**: Implement serialized conditional commits, atomic batches, synchronized reads, revision reservation, and immutable value ownership.
- **Relevant requirements**: FR-001–FR-010, C-001–C-003; NFR-001, NFR-002, NFR-005
- **Affected surfaces**: `internal/engine/`, `gapdb/types.go`, `internal/clock/`
- **Sequencing/depends-on**: IC-01
- **Risks**: Revision reuse after lost memory commits; partial batches; slice aliasing; reader/writer races.

### IC-03 — WAL, recovery, and degraded-state handling

- **Purpose**: Provide ordered buffered appends, durable barriers, checksums, torn-tail recovery, corruption refusal, and fault seams.
- **Relevant requirements**: FR-009–FR-011, FR-016, FR-021, FR-022; NFR-004, NFR-010, NFR-011
- **Affected surfaces**: `internal/persist/`, `internal/faultfs/`, `tests/crash/`, `tests/compatibility/`
- **Sequencing/depends-on**: IC-01, IC-02
- **Risks**: Acknowledgement before sync; accepting checksum corruption; applying a commit whose frame was not accepted.

### IC-04 — Expiry, scans, history, and watches

- **Purpose**: Add logical expiry, deterministic cleanup, stale-safe pagination, bounded retained events, and gap-free watch registration.
- **Relevant requirements**: FR-013–FR-015; NFR-005, NFR-006, NFR-008
- **Affected surfaces**: `internal/engine/`, `gapdb/client.go`, `tests/contract/`
- **Sequencing/depends-on**: IC-02, IC-03
- **Risks**: Resurrection on clock rollback; partial batch eviction; scan inconsistency; history/live watch gap.

### IC-05 — Snapshot, compaction, backup, and ownership

- **Purpose**: Install atomic generations, rotate WAL safely, delete only superseded files, create verified backups, and enforce one owner.
- **Relevant requirements**: FR-012, FR-016, FR-020–FR-023; NFR-004, NFR-009–NFR-011
- **Affected surfaces**: `internal/persist/`, `internal/admin/`, `internal/server/`, `tests/crash/`
- **Sequencing/depends-on**: IC-02, IC-03
- **Risks**: Manifest points at incomplete files; directory entry not durable; active file deletion; stale-socket corruption.

### IC-06 — Unix server and first-party client

- **Purpose**: Expose the full data API through bounded framed connections while preserving engine ordering and structured failures.
- **Relevant requirements**: FR-002–FR-018, FR-024; NFR-001–NFR-003, NFR-007–NFR-009
- **Affected surfaces**: `internal/protocol/`, `internal/server/`, `gapdb/client.go`, `cmd/gapdbd/`
- **Sequencing/depends-on**: IC-01, IC-02, IC-03, IC-04
- **Risks**: Unbounded connection goroutines; response loss ambiguity; watcher backpressure; socket permission drift.

### IC-07 — Model-oriented CLI and administration

- **Purpose**: Provide deterministic non-interactive data, inspection, lifecycle, verification, recovery-proposal, and guarded apply commands.
- **Relevant requirements**: FR-018–FR-024; NFR-006–NFR-009, NFR-012
- **Affected surfaces**: `cmd/gapctl/`, `internal/admin/`, `docs/operations/`, `tests/contract/`
- **Sequencing/depends-on**: IC-01, IC-03, IC-05, IC-06
- **Risks**: Human-only output; unsafe default repair; inconsistent offline and online diagnostics.

### IC-08 — Evidence, performance, and adoption gate

- **Purpose**: Prove crash safety, concurrency, compatibility, model-only operation, performance thresholds, and dual-backend application fidelity.
- **Relevant requirements**: NFR-001–NFR-012; SC-001–SC-009; C-008
- **Affected surfaces**: `tests/`, benchmark tooling, `docs/`, originating-application adapter
- **Sequencing/depends-on**: IC-01–IC-07
- **Risks**: Vacuous fault tests; reference-machine ambiguity; replacing SQLite on incomplete evidence.

## Phase Boundary

Phase 0 research and Phase 1 contracts are complete in the companion artifacts.
This plan does not create work packages. `/spec-kitty.tasks` must translate the
implementation concerns into executable packages after plan review.
