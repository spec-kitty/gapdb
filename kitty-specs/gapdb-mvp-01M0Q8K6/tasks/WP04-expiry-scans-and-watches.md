---
work_package_id: WP04
title: Expiry, scans, history, and watches
dependencies:
- WP03
requirement_refs:
- FR-013
- FR-014
- FR-015
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
subtasks:
- T018
- T019
- T020
- T021
- T022
phase: Phase 4 - Query and observation
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: ''
authoritative_surface: internal/engine/
create_intent:
- internal/engine/expiry.go
- internal/engine/expiry_test.go
- internal/engine/scan.go
- internal/engine/scan_test.go
- internal/engine/history.go
- internal/engine/history_test.go
- internal/engine/watch.go
- internal/engine/watch_test.go
execution_mode: code_change
model: ''
owned_files:
- internal/engine/expiry.go
- internal/engine/expiry_test.go
- internal/engine/scan.go
- internal/engine/scan_test.go
- internal/engine/history.go
- internal/engine/history_test.go
- internal/engine/watch.go
- internal/engine/watch_test.go
role: ''
tags: []
tracker_refs: []
---

# Work Package Prompt: WP04 – Expiry, scans, history, and watches

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `agent_profile`
- **Role**: `role`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Add bounded reconciliation features without adding MVCC: logical expiry, stable
point-in-time scan pages, retained whole-commit history, and resumable watches
whose failure modes explicitly instruct callers to rescan.

## Context

Readers already enforce logical expiry and the writer already produces ordered
commit effects. Physical cleanup, history mutation, and watch registration must
also be serialized by that writer. A scan cursor is valid only while the database
revision remains unchanged. History and watcher queues are bounded and never
silently discard events.

Planning and merge target are `main`; work in the lane workspace. Start with:

`spec-kitty agent action implement WP04 --agent <name>`

### Subtask T018: Implement nondecreasing time and deterministic expiry cleanup

**Purpose**: Make expired authority immediately absent while keeping physical map
mutation inside the single-writer invariant.

**Steps**:

1. Add a writer-owned min-heap of expiry candidates containing absolute time,
   key, and the record revision that scheduled it.
2. Push a candidate when a put includes expiry; stale candidates are harmless.
3. Drive one writer timer from the earliest candidate. On wake, advance effective
   time, collect due entries, validate current record revision/expiry, sort keys,
   and commit one internal expiry batch.
4. Use normal revision/WAL/barrier rules for physical cleanup and emit `expire`
   effects. Logical reads/conditions never wait for cleanup.
5. A replacement after expiry may proceed directly; an obsolete heap entry must
   never delete it.

**Files**: `internal/engine/expiry.go`, `expiry_test.go`.

**Validation**: Manual-clock tests cover exact boundary, delayed cleanup, clock
rollback, replacement races, restart rebuild, and deterministic event ordering.

### Subtask T019: Implement bounded point-in-time prefix scans and cursors

**Purpose**: Reconcile a namespace deterministically without retaining historical
maps or allowing silent pagination drift.

**Steps**:

1. Under `RLock`, capture current database revision/effective as-of time and copy
   matching live record views; sort bytewise by UTF-8 key outside the lock.
2. Enforce record-count and encoded-byte page bounds.
3. Encode a versioned opaque base64url cursor containing database ID, prefix,
   last key, observed revision, as-of Unix nanos, and CRC32C.
4. On continuation, validate every bound field and require current revision to
   equal observed revision; otherwise return `SCAN_STALE`.
5. Reuse the cursor's as-of time so expiry does not reshape pages absent a commit.

**Files**: `internal/engine/scan.go`, `scan_test.go`.

**Validation**: Test ordering, prefix boundaries, empty prefix, expiry exclusion,
count/byte truncation, cursor tampering, wrong database/prefix, and intervening
commit behavior.

### Subtask T020: Implement bounded whole-commit change history

**Purpose**: Retain enough ordered effects for watch resumption while evicting
only complete atomic commits.

**Steps**:

1. Represent history as commit groups with revision, ordered events, and encoded
   byte accounting.
2. Append after map application in the writer. Copy values held by put events.
3. Enforce count and byte limits by evicting complete oldest revisions.
4. Expose earliest available revision and ordered prefix-filtered replay.
5. Rebuild bounded post-snapshot history from recovered WAL effects; make snapshot
   revision the compaction boundary.

**Files**: `internal/engine/history.go`, `history_test.go`.

**Validation**: Batch events are never split; limits are deterministic; replay
order is revision then within-batch order; values cannot alias live map or caller.

### Subtask T021: Implement resumable watch registration and lag handling

**Purpose**: Close the replay/live race and give slow or stale consumers explicit
reconciliation evidence.

**Steps**:

1. Send watch registration through the writer with prefix and exclusive cursor.
2. Reject cursors ahead of current revision or older than retained history with
   the documented evidence and safe actions.
3. Under writer ordering, collect backlog through registration revision and add a
   bounded live queue that receives only later commits.
4. Deliver backlog first, then live events; preserve event order and allow clean
   cancellation/unregistration.
5. On queue overflow, close with `WATCH_LAGGED` and the last completely delivered
   revision. Never drop an event silently.

**Files**: `internal/engine/watch.go`, `watch_test.go`.

**Validation**: Deterministically interleave registration and commits, exercise
prefix filtering, compaction, cursor-ahead, batch ordering, lag, and cancellation.

### Subtask T022: Test expiry, scan, history, and watch reconciliation

**Purpose**: Prove the four features compose at their boundaries rather than only
in isolated happy paths.

**Steps**:

1. Create controlled sequences of puts, batches, expiries, cleanup, scans, and
   watcher reconnects.
2. Reconstruct authoritative state from scan plus events and compare byte-for-byte
   with current live state.
3. Force history eviction and assert the only recovery path is explicit rescan.
4. Restart from a snapshot/WAL fixture and verify logical expiry and replay cursor
   boundaries remain correct.
5. Run under race detector with slow watchers and concurrent scanners.

**Files**: package-local tests owned by this WP.

**Validation**: No missing/reordered event, partial batch, resurrection, stale
page success, race, or goroutine leak.

## Definition of Done

- Expired records are absent from every read and condition at the boundary.
- Scan pages are bounded, ordered, and stale-safe without MVCC.
- History eviction preserves whole commits.
- Watch registration has no replay/live gap and lag is explicit.
- Race-enabled reconciliation tests pass deterministically.
- Record completion with `spec-kitty agent tasks mark-status T018 T019 T020 T021 T022 --status done`.

## Risks

- Effective time and scan as-of time are different concerns; preserve both.
- A watch cursor at snapshot/earliest-history boundaries needs exact off-by-one
  tests because `after_revision` is exclusive.
- Do not block the writer on a watcher send.

## Reviewer Guidance

Inspect boundary revisions, expiry equality, cursor integrity, history eviction,
and the serialized registration handoff. Require a test that would fail if one
batch event were evicted or delivered alone.
