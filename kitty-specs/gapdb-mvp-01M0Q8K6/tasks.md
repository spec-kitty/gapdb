# Work Packages: Gapdb MVP

**Mission**: `gapdb-mvp-01M0Q8K6`  
**Planning branch**: `main`  
**Merge target**: `main`  
**Generated**: 2026-08-23

## Execution overview

The MVP is divided into nine cohesive packages. The dependency graph deliberately
keeps the storage and engine invariants ahead of transport and operator surfaces.
Each package owns a disjoint file set; execution lanes are computed by Spec Kitty
during finalization.

```text
WP01 Contracts and scaffolding
 ├─> WP02 Persistence and recovery ───────┐
 └──────────────────────> WP03 Engine core│
                              │           │
                              └─> WP04 Query, expiry, watch
                                         │
                        WP02 + WP03 + WP04 ─> WP05 Admin persistence
                                                    │
                       WP01 + WP03 + WP04 + WP05 ─> WP06 Server and client
                                                               │
                                                            WP07 CLI
                                                               │
                                                WP02–WP07 ─> WP08 Crash evidence
                                                               │
                                                WP01–WP08 ─> WP09 Adoption gate
```

## Subtask index

| ID | Description | WP | Parallel |
|---|---|---|---|
| T001 | Create the Go module, dependency review, package skeleton, limits, and stable errors | WP01 | No |
| T002 | Define public records, conditions, batches, results, and options | WP01 | Yes |
| T003 | Implement strict bounded framed JSON protocol codecs | WP01 | Yes |
| T004 | Freeze protocol and error golden fixtures and decoder tests | WP01 | Yes |
| T005 | Add deterministic clock primitives and contract test helpers | WP01 | Yes |
| T006 | Create the fault-injectable filesystem boundary | WP02 | No |
| T007 | Implement identity files and durable revision reservation | WP02 | Yes |
| T008 | Implement checksummed manifests and strict binary validation | WP02 | Yes |
| T009 | Implement WAL frames, buffering, flush, and sync barriers | WP02 | No |
| T010 | Implement recovery, torn-tail handling, and degraded state | WP02 | No |
| T011 | Add storage golden fixtures, decoder fuzzing, and format tests | WP02 | Yes |
| T012 | Build synchronized database state and the single writer loop | WP03 | No |
| T013 | Implement direct reads and immutable byte ownership | WP03 | Yes |
| T014 | Implement all single-key conditional mutations | WP03 | No |
| T015 | Implement atomic batch validation and application | WP03 | No |
| T016 | Integrate revisions and acknowledgement barriers | WP03 | No |
| T017 | Prove engine concurrency and conditional-winner behavior | WP03 | Yes |
| T018 | Implement nondecreasing time and deterministic expiry cleanup | WP04 | No |
| T019 | Implement bounded point-in-time prefix scans and cursors | WP04 | Yes |
| T020 | Implement bounded whole-commit change history | WP04 | Yes |
| T021 | Implement resumable watch registration and lag handling | WP04 | No |
| T022 | Test expiry, scan, history, and watch reconciliation | WP04 | Yes |
| T023 | Implement snapshot encoding and atomic generation installation | WP05 | No |
| T024 | Implement guarded compaction of superseded generations | WP05 | Yes |
| T025 | Implement self-consistent verified backups | WP05 | Yes |
| T026 | Implement inspection, verification, and recovery proposals/apply | WP05 | No |
| T027 | Add administrative audit and fault-boundary tests | WP05 | Yes |
| T028 | Enforce ownership and secure Unix socket lifecycle | WP06 | No |
| T029 | Implement bounded connection admission, framing, and dispatch | WP06 | No |
| T030 | Expose data, watch, and administrative handlers | WP06 | No |
| T031 | Implement the first-party Go client and watch stream | WP06 | Yes |
| T032 | Compose `gapdbd` startup, readiness, drain, and shutdown | WP06 | No |
| T033 | Add black-box socket and client contract tests | WP06 | Yes |
| T034 | Build the deterministic `gapctl` command and output framework | WP07 | No |
| T035 | Implement all data and conditional mutation commands | WP07 | Yes |
| T036 | Implement scan output and terminal watch JSONL behavior | WP07 | Yes |
| T037 | Implement online administration and guarded offline recovery | WP07 | No |
| T038 | Prove and document the non-interactive model-only lifecycle | WP07 | Yes |
| T039 | Run deterministic persistence fault schedules | WP08 | Yes |
| T040 | Build subprocess crash tests with acknowledgement ledgers | WP08 | Yes |
| T041 | Fuzz corruption, framing, and compatibility boundaries | WP08 | Yes |
| T042 | Run full-stack race and concurrency stress tests | WP08 | Yes |
| T043 | Verify permissions, bounds, and degraded-mode behavior | WP08 | Yes |
| T044 | Produce reproducible crash and safety evidence | WP08 | No |
| T045 | Implement reference read, write, and recovery benchmarks | WP09 | Yes |
| T046 | Record reproducible reference-machine benchmark metadata | WP09 | Yes |
| T047 | Define the originating-application storage contract harness | WP09 | Yes |
| T048 | Provide the SQLite/Gapdb dual-backend adoption runner seam | WP09 | No |
| T049 | Promote v1 formats and operator guidance into living docs | WP09 | Yes |
| T050 | Implement the release evidence manifest and adoption checklist | WP09 | No |

## WP01 — Public contracts and protocol foundation

**Prompt**: `tasks/WP01-public-contracts-and-protocol.md`  
**Priority**: P1  
**Independent test**: Golden v1 request, response, error, and framing fixtures
round-trip byte-stably; invalid fields, versions, and sizes fail with stable
errors before allocation or mutation.  
**Dependencies**: None  
**Estimated prompt size**: ~300 lines

T001 Create the Go module, dependency review, package skeleton, limits, and stable errors (WP01)
T002 Define public records, conditions, batches, results, and options (WP01)
T003 Implement strict bounded framed JSON protocol codecs (WP01)
T004 Freeze protocol and error golden fixtures and decoder tests (WP01)
T005 Add deterministic clock primitives and contract test helpers (WP01)

**Implementation sketch**: Establish the dependency budget and public vocabulary,
then implement a strict frame/envelope codec and freeze compatibility evidence
before storage or engine code can depend on it.

**Parallel opportunities**: T002, T003, and T005 are file-disjoint once T001
establishes the module. T004 follows the public and wire shapes.

**Risks**: Flexible JSON decoding, aliased byte slices, or unstable error fields
would undermine every later package.

## WP02 — Persistence, WAL, and recovery foundation

**Prompt**: `tasks/WP02-persistence-wal-and-recovery.md`  
**Priority**: P1  
**Independent test**: Golden identity, manifest, and WAL files recover exactly;
only an incomplete final frame is truncated, while complete corruption and
unknown versions fail closed.  
**Dependencies**: WP01  
**Estimated prompt size**: ~360 lines

T006 Create the fault-injectable filesystem boundary (WP02)
T007 Implement identity files and durable revision reservation (WP02)
T008 Implement checksummed manifests and strict binary validation (WP02)
T009 Implement WAL frames, buffering, flush, and sync barriers (WP02)
T010 Implement recovery, torn-tail handling, and degraded state (WP02)
T011 Add storage golden fixtures, decoder fuzzing, and format tests (WP02)

**Implementation sketch**: Build bounded filesystem and codec primitives first,
then identity and manifest authority, followed by append/barrier behavior and the
strict recovery state machine.

**Parallel opportunities**: Identity and manifest codecs can proceed together;
fixture and fuzz work can begin once each format stabilizes.

**Risks**: Revision reuse, accepting checksum corruption, or confusing an
incomplete tail with a corrupt complete frame are authority failures.

## WP03 — Serialized in-memory engine

**Prompt**: `tasks/WP03-serialized-engine-core.md`  
**Priority**: P1  
**Independent test**: Concurrent clients racing every conditional operation see
one valid serialization; successful batches are all-or-nothing at one revision
and failed validation consumes no revision.  
**Dependencies**: WP01, WP02  
**Estimated prompt size**: ~350 lines

T012 Build synchronized database state and the single writer loop (WP03)
T013 Implement direct reads and immutable byte ownership (WP03)
T014 Implement all single-key conditional mutations (WP03)
T015 Implement atomic batch validation and application (WP03)
T016 Integrate revisions and acknowledgement barriers (WP03)
T017 Prove engine concurrency and conditional-winner behavior (WP03)

**Implementation sketch**: Define one command/commit seam, keep reads behind an
`RWMutex`, validate before revision allocation, persist before apply, and publish
only complete commits.

**Parallel opportunities**: Direct-read semantics and table-driven validation
tests can proceed beside the writer loop once state ownership is fixed.

**Risks**: Map races, partial batches, mutation after failed persistence, and
revision allocation on condition failure.

## WP04 — Expiry, scans, history, and watches

**Prompt**: `tasks/WP04-expiry-scans-and-watches.md`  
**Priority**: P2  
**Independent test**: Controlled-clock tests prove logical expiry; scans either
continue at the same revision or return `SCAN_STALE`; watches replay and hand off
without gaps or terminate explicitly on compaction/lag.  
**Dependencies**: WP03  
**Estimated prompt size**: ~310 lines

T018 Implement nondecreasing time and deterministic expiry cleanup (WP04)
T019 Implement bounded point-in-time prefix scans and cursors (WP04)
T020 Implement bounded whole-commit change history (WP04)
T021 Implement resumable watch registration and lag handling (WP04)
T022 Test expiry, scan, history, and watch reconciliation (WP04)

**Implementation sketch**: Add logical absence independently of physical cleanup,
then deterministic cursors and whole-commit history, and finally serialize watch
registration through the writer to close the replay/live race.

**Parallel opportunities**: Cursor encoding and history-ring tests are independent
after engine interfaces settle.

**Risks**: Clock rollback resurrection, partial batch eviction, stale pagination,
and silently dropped watch events.

## WP05 — Snapshots, administration, and backups

**Prompt**: `tasks/WP05-snapshots-admin-and-backups.md`  
**Priority**: P1  
**Independent test**: Faults at every sync/rename boundary leave either the old or
new complete generation authoritative; guarded compaction, backup, and recovery
refuse stale database/revision preconditions without mutation.  
**Dependencies**: WP02, WP03, WP04  
**Estimated prompt size**: ~330 lines

T023 Implement snapshot encoding and atomic generation installation (WP05)
T024 Implement guarded compaction of superseded generations (WP05)
T025 Implement self-consistent verified backups (WP05)
T026 Implement inspection, verification, and recovery proposals/apply (WP05)
T027 Add administrative audit and fault-boundary tests (WP05)

**Implementation sketch**: Implement the stop-the-writer snapshot protocol, keep
old generations by default, then build bounded guarded operations and proposal-
before-apply recovery around the same verified file model.

**Parallel opportunities**: Backup encoding and compaction target resolution can
proceed together after snapshot generation naming is fixed.

**Risks**: Installing incomplete authority, deleting active files, silent repair,
or reporting success before audit/directory durability.

## WP06 — Unix server and first-party Go client

**Prompt**: `tasks/WP06-unix-server-and-client.md`  
**Priority**: P1  
**Independent test**: Two binaries communicate through an owner-only Unix socket;
the Go client exercises the complete API and a second owner, oversized frame, slow
watcher, or shutdown race receives a structured bounded result.  
**Dependencies**: WP01, WP03, WP04, WP05  
**Estimated prompt size**: ~370 lines

T028 Enforce ownership and secure Unix socket lifecycle (WP06)
T029 Implement bounded connection admission, framing, and dispatch (WP06)
T030 Expose data, watch, and administrative handlers (WP06)
T031 Implement the first-party Go client and watch stream (WP06)
T032 Compose `gapdbd` startup, readiness, drain, and shutdown (WP06)
T033 Add black-box socket and client contract tests (WP06)

**Implementation sketch**: Acquire the kernel lock before touching the socket,
recover before readiness, cap connections and frame allocation, and keep command
binaries as thin composition roots.

**Parallel opportunities**: Client methods can develop against frozen protocol
fixtures while server dispatch is implemented.

**Risks**: Stale-socket deletion without ownership, permission drift, unbounded
goroutines, response-loss ambiguity, and incomplete drain.

## WP07 — Model-oriented CLI and lifecycle operations

**Prompt**: `tasks/WP07-model-oriented-cli.md`  
**Priority**: P2  
**Independent test**: A non-interactive harness completes data, watch, snapshot,
compact, backup, verification, and recovery workflows using JSON/JSONL alone;
stdout and exit codes exactly match the contract.  
**Dependencies**: WP06  
**Estimated prompt size**: ~320 lines

T034 Build the deterministic `gapctl` command and output framework (WP07)
T035 Implement all data and conditional mutation commands (WP07)
T036 Implement scan output and terminal watch JSONL behavior (WP07)
T037 Implement online administration and guarded offline recovery (WP07)
T038 Prove and document the non-interactive model-only lifecycle (WP07)

**Implementation sketch**: Centralize parsing, output, and exit mapping, add thin
command adapters over the Go client/admin packages, and verify routine and
degraded workflows without prose parsing or prompts.

**Parallel opportunities**: Data commands and operations documentation can
proceed alongside the administration command group.

**Risks**: Mixed stdout, unstable JSON, interactive fallback, unsafe recovery
defaults, or error exit drift.

## WP08 — Crash, race, and corruption hardening

**Prompt**: `tasks/WP08-crash-race-and-corruption-evidence.md`  
**Priority**: P1  
**Independent test**: At least 1,000 deterministic injected schedules plus
subprocess crashes preserve every durable acknowledgement, expose no partial
commit, never reuse a revision, and pass the race detector.  
**Dependencies**: WP02, WP03, WP04, WP05, WP06, WP07  
**Estimated prompt size**: ~350 lines

T039 Run deterministic persistence fault schedules (WP08)
T040 Build subprocess crash tests with acknowledgement ledgers (WP08)
T041 Fuzz corruption, framing, and compatibility boundaries (WP08)
T042 Run full-stack race and concurrency stress tests (WP08)
T043 Verify permissions, bounds, and degraded-mode behavior (WP08)
T044 Produce reproducible crash and safety evidence (WP08)

**Implementation sketch**: Test through public process and socket seams, compare
recovery to an external acknowledgement ledger, and preserve seeds/artifacts for
every failure. Production fixes discovered here must remain minimal and recorded.

**Parallel opportunities**: Crash schedules, fuzz seeds, permissions, and race
stress suites are separate test surfaces.

**Risks**: Vacuous fault injection, nondeterministic tests, private-state
assertions, and treating an unacknowledged commit as forbidden rather than allowed.

## WP09 — Performance, formats, and SQLite adoption gate

**Prompt**: `tasks/WP09-performance-and-adoption-gate.md`  
**Priority**: P2  
**Independent test**: Reproducible benchmarks evaluate every latency/recovery
threshold, the portable Phase 1 contract can run against both backends, and the
release manifest keeps SQLite selected until all evidence and human approval
exist.  
**Dependencies**: WP01, WP02, WP03, WP04, WP05, WP06, WP07, WP08  
**Estimated prompt size**: ~340 lines

T045 Implement reference read, write, and recovery benchmarks (WP09)
T046 Record reproducible reference-machine benchmark metadata (WP09)
T047 Define the originating-application storage contract harness (WP09)
T048 Provide the SQLite/Gapdb dual-backend adoption runner seam (WP09)
T049 Promote v1 formats and operator guidance into living docs (WP09)
T050 Implement the release evidence manifest and adoption checklist (WP09)

**Implementation sketch**: Benchmark without weakening barriers, expose a thin
backend-neutral contract for the external project, publish the exact v1 formats,
and make the SQLite switch an explicit evidence-driven human decision.

**Parallel opportunities**: Benchmarks, living format docs, and adapter contract
design can proceed concurrently after WP08 establishes correctness.

**Risks**: Machine-specific claims, benchmark shortcuts, coupling Gapdb to the
external application, or accidentally automating the production backend switch.

## MVP completion boundary

All nine packages are required for the specification's MVP. WP01–WP07 produce the
working product; WP08 and WP09 produce the correctness and adoption evidence that
keeps the implementation honest. SQLite remains the selected production backend
outside this repository until the final human adoption decision.
