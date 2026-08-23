---
work_package_id: WP03
title: Serialized in-memory engine
dependencies:
- WP01
- WP02
requirement_refs:
- FR-001
- FR-002
- FR-003
- FR-004
- FR-005
- FR-006
- FR-007
- FR-008
- FR-009
- FR-010
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
subtasks:
- T012
- T013
- T014
- T015
- T016
- T017
phase: Phase 3 - Engine
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: ''
authoritative_surface: internal/engine/
create_intent:
- internal/engine/state.go
- internal/engine/state_test.go
- internal/engine/writer.go
- internal/engine/writer_test.go
- internal/engine/revision.go
- internal/engine/revision_test.go
- internal/engine/mutation.go
- internal/engine/mutation_test.go
execution_mode: code_change
model: ''
owned_files:
- internal/engine/state.go
- internal/engine/state_test.go
- internal/engine/writer.go
- internal/engine/writer_test.go
- internal/engine/revision.go
- internal/engine/revision_test.go
- internal/engine/mutation.go
- internal/engine/mutation_test.go
role: ''
tags: []
tracker_refs: []
---

# Work Package Prompt: WP03 – Serialized in-memory engine

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `agent_profile`
- **Role**: `role`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Build the small concurrency core: a synchronized in-memory map, direct reads, and
one goroutine that totally orders validation, revision allocation, persistence,
application, and publication for every mutation.

## Context

The engine owns coordination semantics; persistence owns bytes and barriers.
Failed validation and conditions consume no revision. A successful batch has one
shared revision and becomes visible under one write lock. The WAL frame must be
accepted before state changes. Durable requests cross the barrier before apply;
memory requests apply after append and may be lost on abrupt crash.

Planning and merge target are `main`; use the finalized lane worktree. Start with:

`spec-kitty agent action implement WP03 --agent <name>`

### Subtask T012: Build synchronized database state and the single writer loop

**Purpose**: Give exactly one owner authority to mutate the map, allocate
revisions, maintain lifecycle health, and execute internal work.

**Steps**:

1. Define `DatabaseState` with record map, current/durable revisions, health,
   effective time hook, and interfaces for WAL/allocator.
2. Protect map/current revision with one `sync.RWMutex`; only writer commands take
   the write side. Avoid lock-free state and per-key locks.
3. Implement a bounded command queue and result channels with explicit close,
   drain, and cancellation behavior.
4. A disconnected client does not cancel a command already admitted to the
   serialized commit path.
5. Convert unexpected invariant failures into degraded/closed mutation behavior,
   never continued uncertain writes.

**Files**: `internal/engine/state.go`, `writer.go` and tests.

**Validation**: Queue saturation, close/drain, caller cancellation, and concurrent
reader tests finish without deadlock or goroutine leak.

### Subtask T013: Implement direct reads and immutable byte ownership

**Purpose**: Serve `Get` without routing through the writer while preserving map
safety and logical expiry.

**Steps**:

1. Validate/copy the key before lookup; take `RLock`, evaluate expiry at effective
   time, copy the immutable record view, and release promptly.
2. Return structured `NOT_FOUND` with current revision for missing/expired keys.
3. Copy values on insertion and again on return so callers cannot mutate storage.
4. Ensure expiry checks cannot move effective time backward.
5. Keep Get independent of WAL health when the owner is degraded read-only.

**Files**: `internal/engine/state.go`, `state_test.go`.

**Validation**: Mutate input/output slices, race reads with replacement/deletion,
and test exact expiry boundary and degraded-read behavior.

### Subtask T014: Implement all single-key conditional mutations

**Purpose**: Provide Put, PutIfAbsent, CAS, and DeleteIfRevision as explicit thin
forms of one validated mutation model.

**Steps**:

1. Validate key, value, expiry, acknowledgement, and expected revision before
   enqueue and recheck state-dependent conditions inside the writer.
2. Treat expired records as absent for every condition.
3. On condition failure, return documented expected/actual evidence and safe
   actions without WAL append or revision allocation.
4. On success, create a complete effect, persist in commit order, apply under the
   write lock, and return revision/durable-through evidence.
5. Permit unconditional identical Put to create a new revision; do not add request
   deduplication.

**Files**: `internal/engine/mutation.go`, `mutation_test.go`.

**Validation**: Table-test missing/live/expired states for every operation and
race competing PutIfAbsent/CAS calls to exactly one correct winner.

### Subtask T015: Implement atomic batch validation and application

**Purpose**: Make a bounded ordered group of distinct-key effects one indivisible
commit.

**Steps**:

1. Reject empty/oversized batches, duplicate keys, invalid kinds/conditions, and
   encoded-size overflow before examining mutable state.
2. Evaluate every condition against one pre-batch live view; never let an earlier
   mutation affect a later condition.
3. Identify failing mutation index/key/condition and apply nothing.
4. Encode one WAL frame, allocate one revision, and apply all effects inside one
   write-lock section in request order.
5. Produce ordered change-effect data for later history/watch integration.

**Files**: `internal/engine/mutation.go`, `mutation_test.go`.

**Validation**: Prove shared revision/order, no intermediate read visibility, no
revision on failure, duplicate rejection before conditions, and replay atomicity.

### Subtask T016: Integrate revisions and acknowledgement barriers

**Purpose**: Preserve ordering and durability truth across the allocator, WAL,
map, and responses.

**Steps**:

1. Request the next value only after validation succeeds and only from a durably
   reserved range.
2. Append the complete frame before apply for both modes.
3. For durable mode, flush/sync before apply and report the resulting barrier; for
   memory mode, apply after buffered append and report the prior durable-through.
4. If append/barrier fails before apply, return storage failure with no commit.
5. If persistence becomes uncertain after prior memory acknowledgements, enter
   write-disabled degraded state and keep reads/inspection available.

**Files**: `internal/engine/revision.go`, `writer.go` and tests.

**Validation**: A durable commit makes all earlier memory commits recoverable;
failed conditions allocate nothing; restart gaps are accepted but never reused.

### Subtask T017: Prove engine concurrency and conditional-winner behavior

**Purpose**: Turn the central one-writer/RWMutex claims into race-enabled evidence.

**Steps**:

1. Run at least eight readers with one writer and mixed Get/Put/CAS/delete/batch.
2. Race many PutIfAbsent and same-revision CAS attempts; assert exact winner count.
3. Probe reads during a multi-key batch and assert only pre/post states.
4. Stress queue bounds, disconnects, drain, and degraded transitions.
5. Record deterministic seeds and timeouts; avoid sleep-based correctness.

**Files**: all package-local engine test files owned by this WP.

**Validation**: `go test -race ./internal/engine` repeatedly with no race, leak,
partial visibility, or flaky timing dependency.

## Definition of Done

- All mutations and revisions pass through exactly one writer goroutine.
- Direct reads are synchronized, expiry-aware, and copy-safe.
- Conditions and batches match every acceptance scenario.
- Memory and durable response evidence is honest under injected failures.
- Race-enabled tests prove serialization and atomic visibility.
- Record completion with `spec-kitty agent tasks mark-status T012 T013 T014 T015 T016 T017 --status done`.

## Risks

- Keep persistence calls out of the map write-lock where possible, but never allow
  a second mutation to interleave with the active command.
- Avoid context cancellation that reports non-application after commit admission.
- Never recover from a panic by continuing writes with uncertain state.

## Reviewer Guidance

Trace the exact successful and failing mutation sequence. Verify there is only one
revision-allocation call site, no condition outside writer authority, no slice
aliasing, and no possible read of a partially applied batch.
