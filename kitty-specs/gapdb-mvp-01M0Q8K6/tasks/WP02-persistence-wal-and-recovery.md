---
work_package_id: WP02
title: Persistence, WAL, and recovery foundation
dependencies:
- WP01
requirement_refs:
- FR-008
- FR-009
- FR-010
- FR-011
- FR-016
- FR-021
- FR-022
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
subtasks:
- T006
- T007
- T008
- T009
- T010
- T011
phase: Phase 2 - Persistence
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: ''
authoritative_surface: internal/persist/
create_intent:
- internal/faultfs/fs.go
- internal/faultfs/inject.go
- internal/persist/identity.go
- internal/persist/identity_test.go
- internal/persist/manifest.go
- internal/persist/manifest_test.go
- internal/persist/wal.go
- internal/persist/wal_test.go
- internal/persist/recovery.go
- internal/persist/recovery_test.go
- tests/compatibility/storage/golden_test.go
execution_mode: code_change
model: ''
owned_files:
- internal/faultfs/**
- internal/persist/identity.go
- internal/persist/identity_test.go
- internal/persist/manifest.go
- internal/persist/manifest_test.go
- internal/persist/wal.go
- internal/persist/wal_test.go
- internal/persist/recovery.go
- internal/persist/recovery_test.go
- tests/compatibility/storage/**
role: ''
tags: []
tracker_refs: []
---

# Work Package Prompt: WP02 – Persistence, WAL, and recovery foundation

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `agent_profile`
- **Role**: `role`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Implement the authoritative identity, revision-reservation, WAL, manifest, and
recovery core. Durable acknowledgements must be honest; incomplete tails may be
discarded, but complete corruption or unknown formats must stop writable startup.

## Context

One WAL frame is one complete commit. The writer appends before map application.
Memory mode may acknowledge before flush; durable mode flushes and syncs every
earlier frame as a barrier. `IDENTITY` reserves revision ranges durably so memory-
acknowledged revisions can be lost but never reused. `CURRENT` is the only active
generation authority. Follow `contracts/storage-v1.md` byte-for-byte.

Planning branch and merge target are `main`; the execution worktree is resolved
from `lanes.json`. Start with:

`spec-kitty agent action implement WP02 --agent <name>`

### Subtask T006: Create the fault-injectable filesystem boundary

**Purpose**: Make every durability boundary observable and deterministically
fallible without replacing ordinary filesystem semantics in production.

**Steps**:

1. Define a narrow interface for open/create, write, flush, file sync, rename,
   directory sync, truncate, remove, stat, and directory enumeration.
2. Wrap `os.File` closely; preserve short-write and underlying error behavior.
3. Add named before/after hooks for every boundary listed in the storage contract.
4. Let tests inject an error, process-stop signal, or observation at an exact
   occurrence count and seed.
5. Avoid a virtual filesystem or mocks that cannot represent rename/fsync order.

**Files**: `internal/faultfs/fs.go`, `internal/faultfs/inject.go`.

**Validation**: Unit tests prove hooks fire in deterministic order and production
mode delegates without extra filesystem mutation.

### Subtask T007: Implement identity files and durable revision reservation

**Purpose**: Establish stable database identity and prevent revision reuse across
crashes without syncing every memory acknowledgement.

**Steps**:

1. Encode/decode the fixed 64-byte `GAPID001` structure with big-endian fields,
   reserved-byte validation, database ID, generation, and CRC32C.
2. Create identity once with cryptographically random 128-bit ID and mode `0600`.
3. Reserve a configured range with temp create, write, sync, rename, and directory
   sync before returning an allocator range.
4. Reject generation regression, overflow, checksum/version mismatch, and recovered
   revisions above the reserved end.
5. Burn unused reserved values after restart; never infer continuity from WAL.

**Files**: `internal/persist/identity.go`, `identity_test.go`.

**Validation**: Fault every installation step and accept only a complete old/new
identity. Test overflow and non-reuse across simulated restart.

### Subtask T008: Implement checksummed manifests and strict binary validation

**Purpose**: Make one atomically installed `CURRENT` frame the sole authority for
snapshot/WAL generation selection.

**Steps**:

1. Implement `GAPCUR01` framing, ordered JSON payload, CRC32C, and strict decode.
2. Validate database ID, monotonic generation, snapshot/WAL revision relation,
   lowercase hashes, and base filenames without separators.
3. Install through same-directory temp, sync, rename, and directory sync.
4. Treat unreferenced files as inspection/compaction candidates, never authority.
5. Expose typed validation evidence without leaking arbitrary paths.

**Files**: `internal/persist/manifest.go`, `manifest_test.go`.

**Validation**: Golden bytes round-trip; unknown fields/versions, traversal names,
checksum changes, and inconsistent revisions fail without writes.

### Subtask T009: Implement WAL frames, buffering, flush, and sync barriers

**Purpose**: Persist each atomic effect in one bounded checksummed frame and
provide explicit memory/durable barrier semantics.

**Steps**:

1. Encode/validate the 64-byte WAL header and `CMIT` frame exactly as specified.
2. Bound total frame, mutation count, key/value lengths, and arithmetic before
   allocation. Require unique keys and valid put/delete/expire representations.
3. Maintain one buffered active WAL handle; append complete encoded frames in
   increasing revision order.
4. Expose append and durability-barrier methods. Barrier flushes and calls file
   `Sync`, updating durable-through only after success.
5. Return enough stage evidence for the engine to enter degraded read-only state
   when an earlier memory commit can no longer be persisted honestly.

**Files**: `internal/persist/wal.go`, `wal_test.go`.

**Validation**: Test short writes, flush/sync failures, barriers covering earlier
frames, gaps allowed, duplicates/regressions rejected, and one-frame batch atomicity.

### Subtask T010: Implement recovery, torn-tail handling, and degraded state

**Purpose**: Reconstruct only authoritative complete state and distinguish the one
automatic recovery action from corruption requiring operator involvement.

**Steps**:

1. Validate identity and manifest before loading snapshot/WAL references.
2. Scan WAL strictly, stage a complete frame before applying it, and enforce
   increasing revisions no greater than the reserved bound.
3. Truncate only an incomplete physical final frame to the prior complete offset;
   sync it and return a structured `TAIL_TRUNCATED` audit event.
4. Treat a complete bad checksum/payload, earlier invalid frame, version mismatch,
   or database-ID mismatch as inspection-only startup failure.
5. Return recovered revision, durable-through revision, post-snapshot events,
   cleanup candidates, and structured damage evidence.

**Files**: `internal/persist/recovery.go`, `recovery_test.go`.

**Validation**: Enumerate truncation at every byte of a frame and corruption at
every complete field. No proper subset of a batch may appear.

### Subtask T011: Add storage golden fixtures, decoder fuzzing, and format tests

**Purpose**: Pin format v1 independently of implementation structs and ensure
hostile bytes cannot panic or allocate beyond limits.

**Steps**:

1. Commit small golden identity, manifest, WAL, and tail variants.
2. Assert their exact sizes, checksums, field order, and decoded meaning.
3. Seed fuzz tests with every valid/invalid fixture and bound execution memory.
4. Verify unsupported magic/version and reserved bits never mutate files.
5. Document fixture provenance and make regeneration an explicit developer step.

**Files**: `tests/compatibility/storage/**` plus package-local tests.

**Validation**: `go test ./internal/persist ./tests/compatibility/storage`; run
bounded fuzz smoke tests and `go test -race` for the injected filesystem.

## Definition of Done

- Version-1 identity, manifest, and WAL encodings match the storage contract.
- Revision reservation is durably installed before any revision is handed out.
- Durable barriers cover all earlier appends and never report early success.
- Recovery truncates only incomplete tails and otherwise fails closed.
- Every filesystem boundary has deterministic fault coverage.
- Record completion with `spec-kitty agent tasks mark-status T006 T007 T008 T009 T010 T011 --status done`.

## Risks

- `Sync` success and directory-entry durability are distinct; preserve the exact
  install sequence.
- Do not apply decoded batch items incrementally.
- Path validation must occur before joining names to the database directory.

## Reviewer Guidance

Review raw layout arithmetic, checksum coverage, allocation bounds, error-stage
propagation, and fault tests. Require evidence that a corrupt complete frame is
never mistaken for a torn tail and that revision reuse is impossible.
