---
work_package_id: WP06
title: Unix server and first-party Go client
dependencies:
- WP01
- WP03
- WP04
- WP05
requirement_refs:
- FR-002
- FR-003
- FR-004
- FR-005
- FR-006
- FR-007
- FR-009
- FR-010
- FR-013
- FR-014
- FR-015
- FR-016
- FR-017
- FR-018
- FR-019
- FR-020
- FR-023
- FR-024
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
subtasks:
- T028
- T029
- T030
- T031
- T032
- T033
phase: Phase 6 - Integration
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: implementer-ivan
authoritative_surface: internal/server/
create_intent:
- internal/server/owner.go
- internal/server/server.go
- internal/server/handler.go
- internal/server/server_test.go
- gapdb/client.go
- gapdb/client_test.go
- cmd/gapdbd/main.go
- tests/contract/api/api_test.go
execution_mode: code_change
model: ''
owned_files:
- internal/server/**
- gapdb/client.go
- gapdb/client_test.go
- cmd/gapdbd/**
- tests/contract/api/**
role: implementer
agent: codex
tags: []
tracker_refs: []
---

# Work Package Prompt: WP06 – Unix server and first-party Go client

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `implementer-ivan`
- **Role**: `implementer`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Expose the complete engine/admin contract to local processes through one bounded,
owner-only Unix socket and a typed first-party Go client. Compose `gapdbd` as a
thin lifecycle root with honest readiness and drain behavior.

## Context

The server acquires a kernel advisory lock before inspecting/removing a stale
socket. It validates/replays/reserves revisions before readiness. Connections are
bounded and sequential; watches become terminal streams. Request cancellation
cannot retract a mutation already admitted to the writer.

Planning and merge target are `main`; use the computed lane worktree. Start with:

`spec-kitty agent action implement WP06 --agent <name>`

### Subtask T028: Enforce ownership and secure Unix socket lifecycle

**Purpose**: Prevent cross-process corruption and make local access defaults
owner-only.

**Steps**:

1. Open/create `LOCK` mode `0600` and acquire `LOCK_EX|LOCK_NB` using
   `golang.org/x/sys/unix`; hold its descriptor until full shutdown.
2. Treat lock contents as diagnostics only and return structured `OWNER_EXISTS`
   evidence when acquisition fails.
3. Only after lock acquisition may startup remove a stale configured socket.
4. Create database directory as `0700` when permitted and Unix socket as effective
   `0600`; verify modes after creation.
5. Unlink socket on normal close but let kernel lock release establish crash safety.

**Files**: `internal/server/owner.go`, `server_test.go`.

**Validation**: Start competing owners/subprocesses, stale socket cases, permission
modes, and lock release after abrupt process death.

### Subtask T029: Implement bounded connection admission, framing, and dispatch

**Purpose**: Route strict v1 requests without unbounded clients, memory, or goroutines.

**Steps**:

1. Listen only on Unix sockets; cap active clients with deterministic `SERVER_BUSY`.
2. Apply read/write/idle deadlines and the WP01 frame codec.
3. Permit sequential unary requests or one terminal watch per connection; no
   multiplexing or TCP listener.
4. Validate envelope/operation/arguments before engine calls and map every failure
   to the common stable error response.
5. Recover handler panics by closing/degrading safely; never expose stack traces.

**Files**: `internal/server/server.go`, `handler.go`, tests.

**Validation**: Oversized/partial/malformed frames, too many clients, deadline,
disconnect, and shutdown tests remain bounded and leak-free.

### Subtask T030: Expose data, watch, and administrative handlers

**Purpose**: Connect every documented protocol operation to the narrow public
engine/admin method with evidence preserved.

**Steps**:

1. Implement Get, four single mutations, AtomicBatch, ScanPrefix, and Watch mappings.
2. Implement status, health, stats, describe-config, verify, snapshot, compact,
   and backup mappings.
3. Echo request/database IDs and relevant revisions consistently.
4. For watch, emit started, ordered events, and one ended/error frame; update last
   delivered only after a successful full frame write.
5. Reject offline-only operations on the socket and unknown operations explicitly.

**Files**: `internal/server/handler.go`, `server_test.go`.

**Validation**: Contract tables cover every operation and error; one lost response
scenario proves reconciliation fields remain sufficient.

### Subtask T031: Implement the first-party Go client and watch stream

**Purpose**: Give local Go callers the complete typed API without exposing wire
envelopes or filesystem internals.

**Steps**:

1. Implement dialing/options/deadlines and sequential framed round trips.
2. Add Get, Put, PutIfAbsent, CompareAndSwap, DeleteIfRevision, AtomicBatch,
   ScanPrefix, Watch, and admin methods.
3. Preserve structured remote errors for `errors.As`, including evidence and safe
   actions; distinguish local transport failure.
4. Copy input/result value bytes and validate obvious bounds client-side while
   retaining server authority.
5. Implement cancellable watch receive with ordered events and typed terminal reason.

**Files**: `gapdb/client.go`, `client_test.go`.

**Validation**: Fixture server tests cover partial I/O, request IDs, timeouts,
remote errors, stream termination, cancellation, and retry classification.

### Subtask T032: Compose `gapdbd` startup, readiness, drain, and shutdown

**Purpose**: Make process lifecycle machine-observable and preserve acknowledgement
guarantees during normal termination.

**Steps**:

1. Parse explicit database/socket/config flags without interactive input.
2. Acquire ownership, recover/validate, reserve revisions, start engine, then
   listener. Emit one JSON readiness result only after all are ready.
3. On SIGTERM/SIGINT stop admission, close watches, drain accepted mutations,
   flush/sync WAL, close socket/engine, and release lock before completion.
4. On inspection-only damage, emit the structured failure and safe offline actions;
   do not create a writable socket.
5. Keep human logs on stderr and JSON result on stdout when requested.

**Files**: `cmd/gapdbd/main.go`, server lifecycle code.

**Validation**: Subprocess tests cover clean shutdown, forced kill, readiness
ordering, corrupt startup, config errors, and a second owner.

### Subtask T033: Add black-box socket and client contract tests

**Purpose**: Verify behavior at the supported integration seam rather than through
internal package state.

**Steps**:

1. Launch the actual daemon against temp databases and wait on structured readiness.
2. Exercise all data operations with both acknowledgement modes and request IDs.
3. Race conditional clients and observe atomic batches through independent readers.
4. Exercise scans/watches, expiry, snapshot/admin calls, permissions, and shutdown.
5. Capture deterministic diagnostics and kill/reap every subprocess on failure.

**Files**: `tests/contract/api/**`.

**Validation**: `go test ./tests/contract/api`, plus race runs where supported,
uses only the public Go client/raw socket and never imports internal packages.

## Definition of Done

- One live owner and an effective `0600` socket are enforced.
- All protocol operations reach their documented public seam.
- Client errors retain stable server evidence.
- Daemon readiness and shutdown preserve recovery/acknowledgement contracts.
- Black-box tests pass against real processes and sockets.
- Record completion with `spec-kitty agent tasks mark-status T028 T029 T030 T031 T032 T033 --status done`.

## Risks

- Umask alone is insufficient; verify effective modes.
- Do not let handler contexts cancel accepted writer work.
- A watch write failure must unregister promptly without blocking the writer.

## Reviewer Guidance

Focus on ownership order, connection/resource bounds, socket modes, operation
coverage, lifecycle sequencing, and the separation between transport errors and
structured server errors. Confirm black-box tests do not bypass the socket.
