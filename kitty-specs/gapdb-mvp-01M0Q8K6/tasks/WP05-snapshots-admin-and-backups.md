---
work_package_id: WP05
title: Snapshots, administration, and backups
dependencies:
- WP02
- WP03
- WP04
requirement_refs:
- FR-012
- FR-019
- FR-020
- FR-021
- FR-022
- FR-023
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
subtasks:
- T023
- T024
- T025
- T026
- T027
phase: Phase 5 - Administration
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: ''
authoritative_surface: internal/persist/
create_intent:
- internal/persist/snapshot.go
- internal/persist/snapshot_test.go
- internal/persist/compaction.go
- internal/persist/compaction_test.go
- internal/persist/backup.go
- internal/persist/backup_test.go
- internal/admin/admin.go
- internal/admin/verify.go
- internal/admin/recovery.go
- internal/admin/audit.go
- internal/admin/admin_test.go
execution_mode: code_change
model: ''
owned_files:
- internal/persist/snapshot.go
- internal/persist/snapshot_test.go
- internal/persist/compaction.go
- internal/persist/compaction_test.go
- internal/persist/backup.go
- internal/persist/backup_test.go
- internal/admin/**
role: ''
tags: []
tracker_refs: []
---

# Work Package Prompt: WP05 – Snapshots, administration, and backups

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `agent_profile`
- **Role**: `role`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Implement occasional atomic snapshots and the narrow guarded administration
surface required to inspect, compact, back up, and recover a database without
silent repair or destructive guesses.

## Context

Gapdb deliberately pauses mutation execution during snapshot installation while
reads continue. The sequence flushes/syncs the current WAL, writes a verified
snapshot, creates the next WAL, and installs `CURRENT` last. Superseded files stay
until a separately guarded compaction. Offline tools must acquire the same owner
lock and propose recovery before an explicit apply.

Planning and merge target are `main`; use the finalized lane workspace. Start:

`spec-kitty agent action implement WP05 --agent <name>`

### Subtask T023: Implement snapshot encoding and atomic generation installation

**Purpose**: Produce a deterministic complete point-in-time image and make it
authoritative only after every referenced artifact is durable.

**Steps**:

1. Encode the `GAPSNAP1` header, sorted unique record items, and whole-file SHA-256
   trailer with strict bounded validation.
2. Exclude logically expired records at the stable snapshot as-of time; retain
   each record's original revision and require it not exceed snapshot revision.
3. Execute snapshot as a writer command: pause mutations, flush/sync WAL, capture
   revision/map, write+sync+rename snapshot, sync directory, create/sync/rename
   next WAL, install+sync next manifest, switch handle, resume.
4. On failure before manifest authority, keep using the old generation and retain
   complete orphan candidates for inspection.
5. Expose progress and duration without unbounded record/path output.

**Files**: `internal/persist/snapshot.go`, `snapshot_test.go`.

**Validation**: Fault every boundary and recover either old or new complete
generation. Golden tests cover sort order, duplicate detection, checksum, and expiry.

### Subtask T024: Implement guarded compaction of superseded generations

**Purpose**: Retire storage only when exact authority preconditions and targets
are known.

**Steps**:

1. Require expected database ID and `through_revision` no newer than active
   snapshot revision.
2. Resolve explicit unreferenced known-format snapshot/WAL paths before deletion.
3. Never target identity, manifest, lock, active snapshot/WAL, audit, socket,
   temp, unknown, or traversal paths.
4. Delete only the resolved list and directory-sync before ordinary success.
5. Return removed paths, skipped candidates, before/after revisions, and partial-
   apply evidence if an I/O/audit failure follows removal.

**Files**: `internal/persist/compaction.go`, `compaction_test.go`.

**Validation**: Stale preconditions make zero calls to remove; injected failure at
each removal/sync is reported precisely and active files always survive.

### Subtask T025: Implement self-consistent verified backups

**Purpose**: Produce one restorable generation from a durable revision without
copying live lock/socket/audit state.

**Steps**:

1. Require exact database ID/revision and establish a durability barrier.
2. Build in a sibling exclusive temp directory containing identity, manifest, one
   snapshot, one following WAL, and ordered `backup.json` metadata.
3. Reject existing destination and destinations inside the database directory.
4. Hash, independently reopen/verify, recursively sync, and atomically rename the
   completed directory before success.
5. Add restore/verification helpers that never overwrite an existing database and
   preserve source database identity lineage.

**Files**: `internal/persist/backup.go`, `backup_test.go`.

**Validation**: Fault copying/sync/rename, tamper each file, and verify no final
destination is reported until the independent check succeeds.

### Subtask T026: Implement inspection, verification, and recovery proposals/apply

**Purpose**: Give models structured evidence and safe actions even when writable
startup is impossible.

**Steps**:

1. Implement bounded status/health/stats/config views for a running engine.
2. Implement offline inspect/full verify after acquiring the exclusive lock,
   identifying exact damaged artifact/stage without mutation.
3. Generate stable proposal IDs from relevant evidence and enumerate supported
   safe actions (restore or evidence-preserving quarantine/truncation only where
   the contract permits).
4. Apply only an explicit proposal with exact database ID and manifest-generation
   preconditions plus a quarantine/backup destination.
5. Revalidate evidence immediately before apply and return `operation_applied`
   when a later reporting/audit step fails.

**Files**: `internal/admin/admin.go`, `verify.go`, `recovery.go` and tests.

**Validation**: Read-only modes produce no writes; live-owner conflicts stop
offline access; stale/tampered proposals make no changes.

### Subtask T027: Add administrative audit and fault-boundary tests

**Purpose**: Preserve structured evidence for startup recovery and every state-
changing administrative action.

**Steps**:

1. Emit bounded field-ordered JSONL with CRC32C, event/database/request IDs,
   revisions, result/error, paths, and safe actions.
2. Sync audit before ordinary success; map post-apply audit failure to
   `AUDIT_FAILED_AFTER_APPLY` and degrade health.
3. Bound and rotate audit generations without using them for state authority.
4. Inject faults before/after snapshot, manifest, compact, backup, recovery, and
   audit boundaries and assert exact outcome classification.
5. Keep paths safe and relative where possible; never include raw secret/system
   content in diagnostics.

**Files**: `internal/admin/audit.go`, `admin_test.go` and owned persistence tests.

**Validation**: Deterministic output aside from documented timestamps/durations;
post-apply failure always carries the applied result and safe reconciliation path.

## Definition of Done

- Snapshots install through one auditable old-or-new authority sequence.
- Compaction and backup require exact preconditions and bounded target sets.
- Offline inspection is lock-safe and non-mutating.
- Recovery is proposal-before-apply and fail-closed.
- Administrative mutations emit durable structured audit evidence.
- Record completion with `spec-kitty agent tasks mark-status T023 T024 T025 T026 T027 --status done`.

## Risks

- Snapshot map capture must not alias mutable buffers.
- Directory sync failures after rename create uncertain reporting; never fabricate
  success or delete prior generations.
- Recovery scope must remain narrow and evidence-preserving.

## Reviewer Guidance

Walk every crash boundary against `storage-v1.md`. Verify authority changes only
through `CURRENT`, all destructive operations are preconditioned, offline tools
respect ownership, and audit failure after apply is explicit.
