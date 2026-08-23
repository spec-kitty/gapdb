# Mission Specification: Gapdb MVP

**Mission Branch**: `main`  
**Mission Slug**: `gapdb-mvp-01M0Q8K6`  
**Created**: 2026-08-23  
**Status**: Draft  
**Input**: Build a small local memory store that fills the gap between a Go map and a general database, with first-class conditional writes, crash recovery, and an operational surface suitable for autonomous model management.

## Mission Intent

Gapdb is a tiny, durable, local key-value coordination store. It serves processes on one host through a Unix socket, keeps the active data set in RAM, serializes mutations through one owner, and recovers from an append-only log plus occasional snapshots.

The MVP exists to support coordination state such as leases, reviews, workflow progress, and integration authority without importing SQL or distributed-database complexity. It does not replace the existing SQLite implementation until it passes the originating application's Phase 1 contract and recovery tests.

The product should remain understandable as:

> A Go map in RAM, one serialized writer, conditional mutations, an append-only durability log, occasional snapshots, and a Unix socket.

## User Scenarios & Testing

### User Story 1 - Safely coordinate local processes (Priority: P1)

As a local application process, I can read and conditionally mutate opaque records so competing workers cannot silently overwrite one another's decisions.

**Why this priority**: Conditional state transitions are the central reason Gapdb exists. Without them, an ordinary file or embedded map would be sufficient.

**Independent Test**: Start one Gapdb owner and multiple clients, race conditional mutations against the same keys, and verify that only mutations whose preconditions match are committed.

**Acceptance Scenarios**:

1. **Given** a missing or expired key, **when** two clients concurrently call `PutIfAbsent`, **then** exactly one succeeds and the other receives a structured condition-failed response containing the winning revision.
2. **Given** a live record at revision 41, **when** a client calls `CompareAndSwap` expecting revision 41, **then** the value is replaced and the response contains a revision greater than 41.
3. **Given** a live record at revision 42, **when** a client calls `DeleteIfRevision` expecting revision 41, **then** no state changes and the response reports expected revision 41 and actual revision 42.
4. **Given** successful and failed mutations, **when** revisions are inspected, **then** successful commits advance one database-wide revision and failed conditions do not.

---

### User Story 2 - Recover acknowledged coordination state (Priority: P1)

As an application owner, I can choose whether a mutation only needs in-memory acknowledgement or must survive a process or machine crash before it is acknowledged.

**Why this priority**: Leases, reviews, and integration authority require honest durability. Pretending every fast acknowledgement is durable would make the coordination primitives unsafe.

**Independent Test**: Execute mutations under both acknowledgement modes, terminate the server at injected points around append and sync, restart it, and compare recovered state with the acknowledgements observed by clients.

**Acceptance Scenarios**:

1. **Given** a mutation requested with `durable` acknowledgement, **when** the client receives success and the owner is immediately terminated, **then** the mutation is present after restart.
2. **Given** a mutation requested with `memory` acknowledgement, **when** the client receives success, **then** it is immediately visible to later reads but is explicitly allowed to be absent after an abrupt crash.
3. **Given** earlier memory-acknowledged mutations followed by a durable mutation, **when** the durable mutation is acknowledged, **then** that acknowledgement acts as a barrier and all earlier committed mutations recover as well.
4. **Given** a log whose final entry was torn by a crash, **when** Gapdb restarts, **then** it discards only the incomplete tail, reports the recovery action, and restores the last complete commit.
5. **Given** checksum or framing corruption before the final incomplete entry, **when** Gapdb starts, **then** it fails closed with a structured diagnostic and does not silently discard committed history.

---

### User Story 3 - Change related keys atomically (Priority: P1)

As a workflow engine, I can validate conditions and mutate several records as one commit so externally visible state never contains only half of a transition.

**Why this priority**: Multi-key transitions are necessary for transferring leases and integration authority safely while remaining much smaller than transactions in a general database.

**Independent Test**: Submit batches containing multiple conditions and mutations, inject failures at every durability boundary, and verify all-or-nothing visibility and recovery.

**Acceptance Scenarios**:

1. **Given** a batch whose conditions all match, **when** it commits, **then** every mutation becomes visible together at one database-wide commit revision.
2. **Given** a batch with one failing condition, **when** it is submitted, **then** no mutation is applied, no revision is consumed, and the failed condition is identified.
3. **Given** a batch that updates multiple keys, **when** it is watched, **then** events carry the same commit revision and appear in request order.
4. **Given** a batch containing a key more than once, **when** it is submitted, **then** it is rejected as invalid before any condition or mutation is applied.

---

### User Story 4 - Discover records and resume observation (Priority: P2)

As a local process or autonomous operator, I can scan a namespace and observe later changes without guessing whether a disconnect caused me to miss state transitions.

**Why this priority**: Scans enable restart and reconciliation; resumable watches enable efficient reaction while preserving an authoritative recovery path.

**Independent Test**: Populate multiple prefixes, scan with pagination, start and reconnect watches at known revisions, and compact retained history.

**Acceptance Scenarios**:

1. **Given** records under several prefixes, **when** `ScanPrefix` is called, **then** it returns a key-sorted bounded page with an explicit continuation token and observed database revision.
2. **Given** a watch starting after revision 50 and retained events through revision 55, **when** the watch connects, **then** it first receives matching events 51 through 55 in commit order and then continues live.
3. **Given** a requested watch revision older than retained history, **when** the watch connects, **then** it receives `REVISION_COMPACTED` with the earliest available revision and a safe instruction to rescan.
4. **Given** a slow watcher whose bounded buffer fills, **when** it cannot keep up, **then** the server closes that watch with `WATCH_LAGGED` and the last completely delivered revision.

---

### User Story 5 - Use expiring coordination records (Priority: P2)

As a workflow engine, I can attach an absolute expiry to a record so an abandoned lease ceases to block later work.

**Why this priority**: Expiry is required for practical leases, but it must not complicate the primary conditional-write semantics.

**Independent Test**: Write records with controlled clock values, cross their expiry boundaries, and exercise reads, conditions, scans, watches, restart, and snapshot recovery.

**Acceptance Scenarios**:

1. **Given** the current time is at or after a record's expiry, **when** it is read, scanned, or used by a condition, **then** it behaves as absent even if physical cleanup has not run.
2. **Given** an expired record, **when** `PutIfAbsent` is called, **then** the put may succeed.
3. **Given** an expiry cleanup, **when** observers are connected, **then** they eventually receive an `expire` event with the cleanup commit revision.
4. **Given** expired records in a snapshot or log, **when** Gapdb restarts, **then** they are never exposed as live records.

---

### User Story 6 - Operate Gapdb with an autonomous model (Priority: P2)

As a capable but fallible autonomous model, I can inspect, operate, and recover Gapdb through stable non-interactive commands without relying on prose parsing or unstated operational intuition.

**Why this priority**: The application is expected to be managed by a roughly 200-billion-parameter model. Safe model operation depends on precise affordances, not on model size alone.

**Independent Test**: Drive all routine and failure-recovery workflows using only the versioned JSON CLI surface and verify that every response provides enough structured state to select a safe next action.

**Acceptance Scenarios**:

1. **Given** any client or administrative command, **when** JSON output is requested, **then** stdout contains exactly one documented JSON result or a documented stream of JSON events and no interactive prompt is emitted.
2. **Given** a conditional failure, invalid request, compacted revision, ownership conflict, or corrupt file, **when** the command fails, **then** it returns a stable error code, relevant identifiers and revisions, and enumerated safe next actions.
3. **Given** an administrative mutation such as compaction or recovery, **when** its database-ID or revision precondition does not match, **then** it makes no changes.
4. **Given** normal or degraded storage, **when** the operator calls status, health, configuration description, statistics, or verification commands, **then** it receives bounded, versioned JSON with explicit limits and truncation markers.
5. **Given** a database that cannot start normally, **when** the operator uses read-only inspection, **then** it can identify the damaged artifact and receive a proposed recovery action without modifying database files.

### Edge Cases

- Empty keys, invalid UTF-8 keys, keys beyond the configured limit, oversized values, oversized batches, and oversized protocol frames are rejected before mutation.
- Values are opaque bytes; empty values are valid and are distinct from missing records.
- Expiry timestamps at or before the server's effective current time are rejected before mutation.
- A client disconnect during a request does not cancel a mutation that has already entered the serialized commit path.
- If a response is lost, clients use conditions and returned revisions to reconcile; Gapdb does not promise unbounded deduplication of arbitrary request IDs in the MVP.
- An unconditional repeated `Put` may create another revision even when the bytes are identical. Callers that require retry idempotence use a conditional operation or atomic batch.
- A scan continuation token is bound to the observed database revision. If a commit occurs between pages, continuation fails with `SCAN_STALE` and instructs the caller to rescan; the MVP does not retain MVCC snapshots for pagination.
- A live process already owning the database prevents a second owner from starting. A stale socket is removed only after the implementation proves no live owner holds the database lock.
- A snapshot is usable only when its database ID, format version, checksum, and recorded revision are valid.
- Unknown protocol, log, or snapshot versions fail with explicit compatibility errors; they are never guessed or rewritten automatically.
- Normal shutdown drains accepted mutations and syncs the log before reporting completion. Forced termination retains only the guarantees implied by acknowledgement mode.
- Wall-clock movement must not make an already observed-expired record live again during one server process lifetime.

## Requirements

### Functional Requirements

| ID | Title | User Story | Priority | Status |
|----|-------|------------|----------|--------|
| FR-001 | Record model | Store a non-empty UTF-8 key, opaque value bytes, database-wide revision, and optional absolute UTC expiry. | High | Open |
| FR-002 | Get | Return a live record by exact key or an explicit not-found result without routing the read through the mutation queue. | High | Open |
| FR-003 | Put | Create or replace a record unconditionally and return its commit revision. | High | Open |
| FR-004 | Put if absent | Create a record only when no live record exists and otherwise return the existing revision. | High | Open |
| FR-005 | Compare and swap | Replace a live record only when its revision equals the expected revision. | High | Open |
| FR-006 | Conditional delete | Delete a live record only when its revision equals the expected revision. | High | Open |
| FR-007 | Atomic batch | Evaluate every condition against one pre-batch state and apply all distinct-key mutations at one commit revision, or apply none. | High | Open |
| FR-008 | Global revisions | Allocate monotonically increasing database-wide revisions only for successful commits; all changes in one batch share its revision. | High | Open |
| FR-009 | Memory acknowledgement | Support acknowledgement after the mutation is ordered and visible in RAM, explicitly allowing loss on abrupt failure. | High | Open |
| FR-010 | Durable acknowledgement | Acknowledge only after the commit and every earlier commit have been appended and synced to durable storage. | High | Open |
| FR-011 | Crash recovery | Reconstruct state from the newest valid snapshot plus complete log records, tolerating only an incomplete final log entry. | High | Open |
| FR-012 | Snapshot and compaction | Create checksummed snapshots atomically and retire log history only after the snapshot is synced and installed successfully. | High | Open |
| FR-013 | Prefix scan | Return deterministic, key-sorted, bounded pages at an observed revision, excluding expired records; reject continuation if that revision is no longer current. | Medium | Open |
| FR-014 | Resumable watch | Stream retained matching events after an exclusive revision and then live events, with explicit compaction and lag errors. | Medium | Open |
| FR-015 | Expiry semantics | Treat records as absent at or after expiry for every operation and serialize physical expiry cleanup as a normal commit. | Medium | Open |
| FR-016 | Single ownership | Enforce exactly one owner process per database directory and expose it through a permission-restricted Unix socket. | High | Open |
| FR-017 | Versioned protocol | Provide versioned, framed request and response envelopes for the Unix socket with stable operation and error codes. | High | Open |
| FR-018 | Model-oriented CLI | Provide a non-interactive `gapctl` interface with versioned JSON output for all data and administrative operations. | High | Open |
| FR-019 | Administrative inspection | Expose status, health, statistics, effective configuration, and read-only verification without requiring a healthy writable server. | Medium | Open |
| FR-020 | Guarded administration | Require database identity and/or revision preconditions for state-changing snapshot, compaction, and recovery operations. | High | Open |
| FR-021 | Structured recovery | Detect and identify damaged artifacts, propose non-mutating recovery steps first, and require an explicit apply action before changing files. | High | Open |
| FR-022 | Auditability | Emit structured records for startup recovery and state-changing administrative actions, including database ID, relevant revisions, result, and error code. | Medium | Open |
| FR-023 | Backup | Produce a self-consistent backup from a durable revision and verify it before reporting success. | Medium | Open |
| FR-024 | Request correlation | Accept and echo caller-supplied request IDs for tracing while documenting which operations are naturally safe to retry. | Medium | Open |

### Non-Functional Requirements

| ID | Title | Requirement | Category | Priority | Status |
|----|-------|-------------|----------|----------|--------|
| NFR-001 | Read latency | With 100,000 live 1 KiB records and eight concurrent local readers on the project reference machine, `Get` p95 latency is at most 2 ms. | Performance | High | Open |
| NFR-002 | Memory-write latency | Under the same profile with one writer, memory-acknowledged single-key mutations have p95 latency at most 5 ms. | Performance | Medium | Open |
| NFR-003 | Durable-write latency | On the project reference local SSD, durable single-key mutations have p95 latency at most 50 ms while acknowledging only after a successful sync barrier; group commit is permitted. | Performance | Medium | Open |
| NFR-004 | Recovery correctness | In at least 1,000 deterministic fault-injection runs covering every append, sync, snapshot, rename, and response boundary, no durable-acknowledged commit is lost and no partial commit becomes visible. | Reliability | High | Open |
| NFR-005 | Concurrency correctness | Race-enabled tests complete with zero reported data races while at least eight readers operate concurrently with the serialized writer, expiry cleanup, scans, and watches. | Reliability | High | Open |
| NFR-006 | Deterministic output | Repeating an inspection against unchanged state yields byte-equivalent JSON after excluding explicitly documented volatile timestamp and duration fields. | Operability | High | Open |
| NFR-007 | Structured errors | Every externally observable failure maps to a documented stable code; 100% of condition and administration errors include relevant revisions or identifiers and at least one safe next action. | Operability | High | Open |
| NFR-008 | Bounded interfaces | Requests, values, batches, scans, watch buffers, and diagnostic output have discoverable configurable limits and fail explicitly rather than growing without bound. | Reliability | High | Open |
| NFR-009 | Local access control | The Unix socket defaults to owner-only permissions (`0600`), database files are not made group/world writable, and tests verify the effective modes. | Security | High | Open |
| NFR-010 | Recovery performance | Rebuilding 100,000 live 1 KiB records from a valid snapshot plus 10,000 later log commits completes within 5 seconds on the project reference machine. | Performance | Medium | Open |
| NFR-011 | Compatibility safety | All persisted and wire formats carry explicit versions; 100% of unsupported versions fail without mutating persistent state. | Reliability | High | Open |
| NFR-012 | Model-only operation | Every routine lifecycle, inspection, backup, verification, and documented recovery workflow can be completed using non-interactive commands and machine-readable results only. | Operability | High | Open |

### Constraints

| ID | Title | Constraint | Category | Priority | Status |
|----|-------|------------|----------|----------|--------|
| C-001 | Go implementation | The server and first-party client are implemented in Go and keep live records in process memory. | Technical | High | Open |
| C-002 | One mutation owner | All mutations, expiry cleanup, and revision allocation are serialized through one writer goroutine. | Technical | High | Open |
| C-003 | Safe concurrent reads | The in-memory map is never read concurrently with mutation without synchronization; the initial design favors a simple read/write lock over lock-free complexity. | Technical | High | Open |
| C-004 | Local transport only | The MVP serves local processes through a Unix domain socket and provides no TCP listener. | Scope | High | Open |
| C-005 | Minimal data model | Records contain only key, opaque bytes, revision, and optional expiry; Gapdb does not interpret application values. | Scope | High | Open |
| C-006 | No general database features | The MVP has no SQL, joins, vectors, secondary indexes, query planner, stored procedures, or general transaction language. | Scope | High | Open |
| C-007 | No distribution | The MVP has no replication, sharding, leader election, quorum, or distributed consensus. | Scope | High | Open |
| C-008 | Honest fallback | SQLite remains the production fallback until Gapdb passes the originating application's Phase 1 contract, crash-recovery, and authority-safety tests. | Adoption | High | Open |
| C-009 | Explicit limits | Default limits may be configurable, but unbounded values and queues are prohibited. | Technical | High | Open |
| C-010 | No silent repair | Gapdb never truncates non-tail corruption, rewrites unknown formats, or performs destructive recovery without an explicit guarded command. | Safety | High | Open |
| C-011 | Model-managed, not model-powered | Gapdb exposes an operational surface for an external model but does not embed, call, or depend on a language model. | Scope | High | Open |

### Key Entities

- **Database**: One owned store with a stable database ID, current global revision, effective limits, snapshot lineage, and log history.
- **Record**: A non-empty UTF-8 key, opaque byte value, commit revision, and optional absolute UTC expiry.
- **Commit**: One successful mutation or atomic batch assigned one global revision and one acknowledgement mode.
- **Batch**: A bounded ordered set of conditions and distinct-key mutations evaluated against a single pre-batch state.
- **Change Event**: A put, delete, or expiry event with database ID, commit revision, within-batch order, key, and enough record metadata for reconciliation.
- **Snapshot**: A versioned, checksummed point-in-time image identified by database ID and revision.
- **Watch Cursor**: An exclusive revision from which retained events are replayed before live delivery begins.
- **Diagnostic Result**: A versioned, bounded machine-readable result containing status, identifiers, revisions, error code, and safe next actions.

## Protocol Semantics

### Revision and batch rules

- Revision zero represents an empty database before its first successful commit.
- Each successful single-key mutation increments the global revision once.
- A successful atomic batch increments the global revision once; every record and change event produced by that batch carries the same revision.
- Events within a batch carry a zero-based order matching request order.
- Failed conditions, invalid requests, and read-only operations do not consume revisions.
- Conditions in a batch observe only the pre-batch state. Duplicate keys in one batch are invalid.

### Acknowledgement rules

- `memory` success means the commit is ordered and visible to subsequent operations in the running owner. It can be lost after abrupt failure.
- `durable` success means the commit and all earlier commits have crossed a successful log sync barrier.
- The exact persistence pipeline is a planning decision, but it must preserve revision order and may never acknowledge a durable commit before its barrier.
- Reads do not select an acknowledgement mode.

### Expiry rules

- Expiry is an absolute UTC instant. At or after that instant, the record is logically absent from all reads and conditions.
- The server uses a nondecreasing effective time during one process lifetime so a backward wall-clock adjustment cannot resurrect a record.
- Physical cleanup is serialized through the writer and produces an `expire` change event. Cleanup timing does not weaken logical absence.

### Watch rules

- `Watch(prefix, after_revision)` treats `after_revision` as exclusive.
- Retained matching events are delivered in revision and within-batch order before live events.
- Watches use bounded buffers. They never silently skip events.
- Reconnect safety is bounded by retained history. A caller receiving `REVISION_COMPACTED` must perform a point-in-time scan and resume after that scan revision.

### Error envelope

Every failed operation provides:

- protocol schema version;
- stable error code and concise message;
- database ID when known;
- request ID when supplied;
- relevant key, expected revision, actual revision, or earliest retained revision;
- retry classification; and
- enumerated safe next actions.

Human-readable text is advisory. Automation branches only on stable fields.

## Out of Scope for the MVP

- Remote network access and authentication protocols
- Multi-owner writes or multi-host operation
- Linearizable reads across multiple machines
- SQL or an application-value query language
- Secondary indexes, full-text search, and vector search
- Cross-database transactions
- Transparent encryption at rest
- Online schema migration for arbitrary future formats
- Unbounded exactly-once request deduplication
- Automatic destructive repair
- Replacing SQLite before the explicit adoption gate passes

## Assumptions and Decisions

- Keys are UTF-8 strings so namespaces and model-facing diagnostics remain unambiguous; values remain opaque bytes.
- All clients are local and access control is supplied primarily by filesystem and socket permissions.
- One database directory is owned by one server process at a time.
- Prefix scans use bytewise UTF-8 key ordering for deterministic output.
- A batch commit uses one shared revision rather than allocating a revision per mutated key.
- Durable acknowledgement is also a durability barrier for earlier memory acknowledgements.
- Watches are resumable only within retained history; scans are the authoritative recovery mechanism after compaction.
- Request IDs provide correlation. Conditional APIs provide mutation retry safety in the MVP.
- The reference performance machine and benchmark harness will be recorded in the implementation plan so thresholds remain reproducible.

## Success Criteria

### Measurable Outcomes

- **SC-001**: The complete API (`Get`, `Put`, `PutIfAbsent`, `CompareAndSwap`, `DeleteIfRevision`, `AtomicBatch`, `ScanPrefix`, and `Watch`) passes deterministic contract tests through both the Go client and `gapctl` JSON surface.
- **SC-002**: Across at least 1,000 fault-injection runs, every durable acknowledgement recovers, no unacknowledged partial batch becomes visible, and only an incomplete final log entry is automatically truncated.
- **SC-003**: Race-enabled concurrency tests report zero races and prove that competing conditional mutations produce exactly one valid winner where required.
- **SC-004**: A model-driven harness can start, inspect, exercise, snapshot, compact, back up, stop, restart, diagnose injected corruption, and propose recovery without interactive input or parsing human-only prose.
- **SC-005**: Scans and resumable watches reconcile without missed or silently reordered events, including explicit recovery after watch lag and compaction.
- **SC-006**: Expired records never authorize a lease or block `PutIfAbsent` at or after their expiry, including across restart and clock-adjustment tests.
- **SC-007**: The benchmark suite meets NFR-001, NFR-002, NFR-003, and NFR-010 on the documented project reference machine.
- **SC-008**: The originating application's Phase 1 storage contract runs unchanged or through a documented thin adapter against both SQLite and Gapdb, and SQLite remains selected until all correctness and authority-safety gates pass.
- **SC-009**: A reviewer can account for every persisted file and wire field through a versioned format specification and corresponding compatibility test.

## Spec Review Gate

The mission is ready for planning when reviewers agree that:

1. Conditional, batch, expiry, revision, acknowledgement, scan, and watch semantics are unambiguous enough to generate tests before implementation.
2. The model-operability surface is bounded and does not turn Gapdb into a general administration platform.
3. Crash recovery never silently promotes uncertain state to authoritative state.
4. The implementation plan can meet these requirements without adding SQL, distribution, or lock-free data structures.
5. The SQLite adoption gate remains explicit and testable.
