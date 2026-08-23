# Phase 0 Research: Gapdb MVP

## Scope

This research resolves implementation choices left open by the specification.
It favors the smallest design that protects authority, durability, crash
recovery, and autonomous operation. No item remains marked for clarification.

## Decision 1 — Go toolchain and dependency budget

**Decision**: Use Go 1.26 with the locally installed Go 1.26.4 toolchain. Use the
standard library except for `golang.org/x/sys/unix`, pinned in `go.mod`, for
advisory ownership locking.

**Rationale**: The standard library provides Unix domain sockets through
`net.Listen`/`net.ListenUnix`, file syncing through `os.File.Sync`, binary
encoding, JSON, SHA-256, and CRC32C. The Go-maintained `x/sys/unix` package
provides `Flock`; kernel-managed ownership is released after process death and
does not require guessing whether a PID or socket is stale.

**Alternatives considered**:

- PID or `O_EXCL` lock files: rejected because crash leftovers require unsafe
  liveness inference.
- `syscall.Flock`: rejected because the frozen low-level `syscall` package is a
  worse long-term boundary than the Go-maintained `x/sys/unix` wrapper.
- A third-party storage or protocol framework: rejected because it expands the
  trusted surface without protecting a missing invariant.

**Primary references**:

- [Go `net` package](https://pkg.go.dev/net)
- [Go `os` package](https://pkg.go.dev/os)
- [`golang.org/x/sys/unix`](https://pkg.go.dev/golang.org/x/sys/unix)

## Decision 2 — Wire protocol

**Decision**: Use a four-byte big-endian frame length followed by one versioned
JSON envelope. Represent opaque values as base64. Unary requests have one response;
watch requests produce a start frame, ordered event frames, and one terminal frame.

**Rationale**: Framing makes stream boundaries and limits unambiguous. JSON gives
applications and large models one stable representation without introducing a
schema compiler or generated code. Struct-based Go encoding yields deterministic
field names, and tests can compare normalized or byte-stable output.

**Alternatives considered**:

- Newline-delimited JSON: rejected because large base64 values and partial reads
  make framing and limits less explicit.
- Protobuf or MessagePack: rejected for the MVP because schema generation or a
  third-party codec adds machinery before evidence shows JSON is inadequate.
- HTTP over a Unix socket: rejected because routing, headers, and HTTP semantics
  add surface without improving the local coordination contract.

## Decision 3 — WAL encoding and checksum

**Decision**: Use a fixed binary file header and length-delimited commit frames.
Each frame contains one complete single-key commit or atomic batch and ends with a
CRC32C (Castagnoli) checksum over its header and payload.

**Rationale**: A complete frame is the recovery atomicity unit. Fixed lengths and
explicit counts permit bounded allocation before decoding. CRC32C is available in
Go's standard library and has stronger error-detection characteristics than the
IEEE polynomial for this use.

**Alternatives considered**:

- JSON WAL: rejected because base64 expansion, flexible decoding, and canonical
  checksum rules complicate a format whose only consumers are Gapdb versions.
- Gob: rejected because it is tied to Go type evolution and is not an explicit
  cross-version storage contract.
- Per-operation frames inside a batch: rejected because recovery could observe a
  partial atomic batch.

**Primary reference**: [Go `hash/crc32` package](https://pkg.go.dev/hash/crc32)

## Decision 4 — Memory and durable acknowledgement pipeline

**Decision**: Encode and write each complete frame to a buffered WAL before map
application. Apply and acknowledge a memory request without flushing. For a
durable request, flush and `Sync` before map application and acknowledgement; the
sync is a barrier for all earlier buffered commits.

**Rationale**: This preserves one mutation owner and keeps memory acknowledgements
cheap while ensuring a durable result is never reported before persistence. A
crash after durable sync but before response may recover an unacknowledged commit,
which clients reconcile through conditions and revisions.

**Alternatives considered**:

- Apply before WAL encoding: rejected because an encoding or immediate write
  failure could leave authoritative RAM state with no recoverable representation.
- Separate persistence writer: rejected because coordination and error propagation
  become more complex than the latency saved in the MVP.
- Sync every commit: rejected because it makes the memory mode dishonest and
  removes its purpose.

## Decision 5 — Never reuse revisions after lost memory commits

**Decision**: Reserve revision ranges durably in `IDENTITY` before use. Startup
and range exhaustion atomically advance and sync the reserved upper bound. Gaps
after a crash are valid; reuse is not.

**Rationale**: A memory-acknowledged commit can be lost, but a client may retain
its revision. Reusing that revision for unrelated state could make stale CAS or
watch cursors appear valid. Range reservation pays one startup sync and one sync
per million commits instead of one sync per memory acknowledgement.

**Alternatives considered**:

- Allow revision rollback/reuse: rejected as an authority defect.
- Sync a high-water mark per commit: rejected because it collapses memory mode
  into durable mode.
- Boot epoch plus counter: viable, but range reservation keeps revisions as one
  ordinary monotonic integer and handles multiple ranges in a long process.
- Random commit IDs: rejected because the public contract requires ordered global
  revisions.

## Decision 6 — Snapshot and compaction protocol

**Decision**: Pause mutation execution for the complete snapshot installation
sequence while allowing reads. Sync the current WAL, serialize and sync the
snapshot, create and sync the next WAL, atomically replace `CURRENT`, sync the
directory, then resume writes. Retain old files until a separate guarded compact.

**Rationale**: Stop-the-writer avoids dual-WAL copying, generation races, and a
large crash-state matrix. Snapshots are occasional and the reference data set is
small enough to favor correctness over uninterrupted write latency.

**Alternatives considered**:

- Concurrent snapshot plus WAL handoff: deferred because it requires a more
  complicated manifest and crash protocol.
- Copy-on-write maps: rejected as unnecessary memory amplification.
- Snapshot without WAL rotation: rejected because it does not provide bounded log
  compaction or a clear retained-history boundary.

## Decision 7 — Point-in-time scans without MVCC

**Decision**: Copy matching live record views and the observed revision under a
read lock, then sort and encode outside the lock. Continuation tokens include
database ID, prefix, last key, observed revision, as-of time, version, and
checksum. Continuation fails if the database revision changed.

**Rationale**: This provides a deterministic page and explicit restart behavior
without retaining historical maps. The as-of time keeps expiry evaluation stable
when no commit occurs between pages.

**Alternatives considered**:

- MVCC snapshots: rejected as general-database machinery.
- Weak pagination across mutations: rejected because missing or duplicated keys
  would be silent.
- Return every matching record: rejected because response memory would be
  unbounded.

## Decision 8 — Watch retention and handoff

**Decision**: Keep bounded in-memory history rebuilt from post-snapshot WAL
frames. Register watches through the writer, return a bounded backlog, and begin
live queueing after the registration revision. Evict history by complete commit,
not individual event.

**Rationale**: Serialized registration closes the replay/live race. Whole-commit
eviction prevents replaying part of an atomic batch. A bounded live queue makes
slow consumers explicit through `WATCH_LAGGED`.

**Alternatives considered**:

- One unbounded channel per watcher: rejected as an unbounded memory failure.
- Replay arbitrary WAL history on every watch: rejected because compaction and
  large scans would make connection cost unpredictable.
- Silently drop old events: rejected because the caller could act on incomplete
  authority history.

## Decision 9 — Expiry scheduling and clock behavior

**Decision**: Use an injected nondecreasing effective clock plus a min-heap of
expiry candidates. Readers enforce logical expiry immediately. The writer batches
due cleanup in deterministic key order and ignores heap entries whose record
revision no longer matches.

**Rationale**: Logical absence protects leases even if cleanup is delayed. The
revision check prevents an old timer from deleting a replacement record. Clock
injection enables deterministic tests for rollback and boundary behavior.

**Alternatives considered**:

- Reader-driven deletion: rejected because reads would mutate the map outside the
  writer.
- One timer per record: rejected because resource use scales poorly.
- Wall clock without monotonic clamping: rejected because backward adjustment
  could resurrect observed-expired authority.

## Decision 10 — Administrative and recovery surface

**Decision**: Offer online status, health, stats, configuration, verify, snapshot,
compact, and backup operations through the socket. Offer offline `inspect`,
`verify`, and `recover propose/apply` through `gapctl` only after acquiring the
database lock. Non-tail corruption has no automatic repair; supported actions are
restore of a verified backup or an explicitly guarded, evidence-preserving
quarantine/truncation chosen by the human owner.

**Rationale**: A model can operate normal and degraded states through the same
versioned JSON vocabulary. Locking prevents offline inspection from racing a live
owner. Separating proposal from apply makes destructive intent reviewable.

**Alternatives considered**:

- Repair automatically on startup: prohibited by the charter.
- Human-only logs: rejected because the expected operator is a model.
- General file-edit recovery shell: rejected because it is unbounded and unsafe.

## Decision 11 — Testing and reference evidence

**Decision**: Treat the public contracts and v1 golden fixtures as the first
implementation artifacts. Add an injected filesystem boundary with deterministic
failure hooks, subprocess crash tests, fuzz targets for every decoder, race tests,
and a dual-backend adoption suite.

**Rationale**: The main risks are ordering and recovery, not algorithm novelty.
Tests must observe public results and recovered state rather than private call
sequences. The reference benchmark record includes hardware, kernel, filesystem,
device, Go version, command, seed, and raw output.

**Alternatives considered**:

- Unit tests only: rejected because they cannot prove fsync, socket, process, or
  CLI behavior.
- Coverage percentage as the primary gate: rejected because it can miss the
  persistence boundary that matters.
- Replacing SQLite after API tests alone: rejected because authority and crash
  evidence are explicit adoption requirements.
