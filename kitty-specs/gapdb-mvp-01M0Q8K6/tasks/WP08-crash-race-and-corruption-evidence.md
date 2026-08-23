---
work_package_id: WP08
title: Crash, race, and corruption hardening
dependencies:
- WP02
- WP03
- WP04
- WP05
- WP06
- WP07
requirement_refs:
- FR-007
- FR-008
- FR-009
- FR-010
- FR-011
- FR-012
- FR-014
- FR-015
- FR-016
- FR-017
- FR-020
- FR-021
- FR-022
- FR-023
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
subtasks:
- T039
- T040
- T041
- T042
- T043
- T044
phase: Phase 8 - Safety evidence
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: ''
authoritative_surface: tests/crash/
create_intent:
- tests/crash/fault_matrix_test.go
- tests/crash/process_test.go
- tests/crash/ledger_test.go
- tests/crash/race_test.go
- tests/compatibility/fuzz/fuzz_test.go
- docs/evidence/crash/README.md
execution_mode: code_change
model: ''
owned_files:
- tests/crash/**
- tests/compatibility/fuzz/**
- docs/evidence/crash/**
role: ''
tags: []
tracker_refs: []
---

# Work Package Prompt: WP08 – Crash, race, and corruption hardening

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `agent_profile`
- **Role**: `role`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Prove Gapdb's authority claims under deterministic filesystem failures, abrupt
process death, hostile bytes, high concurrency, and resource-boundary pressure.
Preserve reproducible evidence for every schedule instead of relying on coverage.

## Context

Durable acknowledgements are mandatory after recovery. Memory acknowledgements
may disappear, but cannot cause revision reuse or partial commits. A commit can
recover even when its response was lost. Only an incomplete final WAL frame may
be auto-truncated. The suite must test public results and recovered state, not
private function call counts alone.

Planning and merge target are `main`; use the finalized lane workspace. Start:

`spec-kitty agent action implement WP08 --agent <name>`

### Subtask T039: Run deterministic persistence fault schedules

**Purpose**: Exercise every before/after durability hook across identity, WAL,
snapshot, manifest, compaction, backup, recovery, response, and audit.

**Steps**:

1. Generate deterministic operation histories containing both acknowledgement
   modes, conditions, batches, expiry, snapshot, and administration.
2. Enumerate named fault boundaries and occurrence counts with stable seeds.
3. After each injected error/stop, reopen through production recovery and compare
   public state, revisions, files, health, and diagnostics to an external oracle.
4. Require every durable acknowledgement and forbid partial commits/revision reuse;
   permit memory/unacknowledged complete commits according to the contract.
5. Run at least 1,000 schedules in the acceptance configuration and retain the
   smallest reproducer for failure.

**Files**: `tests/crash/fault_matrix_test.go`, `ledger_test.go`.

**Validation**: Deterministic rerun from printed seed/schedule; no schedule skips a
declared fault hook and evidence reports hook coverage.

### Subtask T040: Build subprocess crash tests with acknowledgement ledgers

**Purpose**: Validate OS/process buffering, locks, sockets, and sync behavior that
in-process fault tests cannot prove.

**Steps**:

1. Launch real `gapdbd`, record framed requests/results to an external fsynced
   ledger, and kill at controlled response/durability milestones.
2. Restart the same directory and reconcile through client/CLI APIs.
3. Cover kill before/after append, flush, sync, apply, publish, response, snapshot
   rename, manifest rename, and graceful-shutdown drain.
4. Verify lock release and stale-socket cleanup only by the next lock holder.
5. Bound subprocess time, reap all children, and preserve stderr/artifacts on failure.

**Files**: `tests/crash/process_test.go`, `ledger_test.go`.

**Validation**: Recovered durable ledger is a subset equality requirement; no
partial batch or reused observed revision is possible.

### Subtask T041: Fuzz corruption, framing, and compatibility boundaries

**Purpose**: Ensure untrusted wire/storage bytes fail predictably without panic,
out-of-bounds reads, huge allocation, or persistent mutation.

**Steps**:

1. Seed wire, identity, manifest, WAL, snapshot, cursor, audit, and backup fuzzers
   from golden fixtures.
2. Mutate lengths, counts, checksums, versions, UTF-8/base64, reserved fields,
   sort order, duplicates, and arithmetic edges.
3. Enforce small configured hard ceilings in fuzz harnesses.
4. For file decoders, hash the test directory before/after rejected input and
   assert no mutation except explicitly tested incomplete-tail recovery.
5. Promote every discovered crash/corruption confusion into a permanent seed.

**Files**: `tests/compatibility/fuzz/**`.

**Validation**: Bounded fuzz runs and seed regression suite pass with race detector.

### Subtask T042: Run full-stack race and concurrency stress tests

**Purpose**: Prove the one-writer/RWMutex/watch/expiry design remains race-free at
the process and client seams.

**Steps**:

1. Run eight or more readers, one mutation stream, expiry cleanup, scans, watches,
   snapshots, and status requests concurrently.
2. Race competing conditional authority changes and validate exactly one legal
   winner and a total revision order.
3. Force watcher lag, disconnects, client deadlines, and shutdown concurrently.
4. Repeat fixed seeds under `go test -race` with bounded deadlines and goroutine
   leak checks.
5. Do not hide races by serializing the test harness more than the product contract.

**Files**: `tests/crash/race_test.go`.

**Validation**: Zero race reports, deadlocks, partial batches, silent event loss,
or nondeterministic assertion failures.

### Subtask T043: Verify permissions, bounds, and degraded-mode behavior

**Purpose**: Test the safety envelope around the core crash invariants.

**Steps**:

1. Verify directory/file/socket modes under varied umasks.
2. Hit every request/value/key/batch/frame/scan/watch/history/client/diagnostic limit
   at maximum and maximum+1.
3. Inject append/flush/sync/audit failures and assert mutation admission stops while
   Get/status/verify evidence remains available as documented.
4. Test unsupported formats and ownership conflicts make no persistent changes.
5. Bound all returned diagnostics and assert truncation markers when applicable.

**Files**: tests owned by this WP.

**Validation**: Stable codes/evidence/safe actions and no resource growth beyond
configured limits.

### Subtask T044: Produce reproducible crash and safety evidence

**Purpose**: Make the acceptance result independently rerunnable rather than a
one-line claim that tests passed.

**Steps**:

1. Record commit, Go/kernel/filesystem details, commands, seeds, schedule counts,
   durations, and per-boundary coverage.
2. Summarize durable/memory/unacknowledged recovery classifications.
3. List any platform skips or flaky retry as test failures until resolved.
4. Store compact machine-readable results plus a human index; exclude temp DB
   payloads or host secrets.
5. Provide one command for the acceptance configuration and a shorter smoke mode.

**Files**: `docs/evidence/crash/**`.

**Validation**: A clean checkout can reproduce the suite and link each headline
claim to a test/result field.

## Definition of Done

- At least 1,000 deterministic schedules satisfy the acknowledgement oracle.
- Real-process crashes cover persistence, response, ownership, and snapshot seams.
- All public/storage decoders have bounded seeded fuzz coverage.
- Full-stack race tests pass with no silent event or partial commit.
- Reproducible evidence records exact environment and commands.
- Record completion with `spec-kitty agent tasks mark-status T039 T040 T041 T042 T043 T044 --status done`.

## Risks

- The oracle must permit recovered but unacknowledged commits.
- In-memory injection alone cannot prove OS lock/socket/subprocess behavior.
- Avoid enormous CI defaults; keep smoke and acceptance profiles explicit.

## Reviewer Guidance

Audit oracle logic before trusting pass counts. Sample fault coverage, reproduce
seeds, verify external ledgers are independent of Gapdb state, and ensure no test
silently skips unsupported filesystem or race behavior.
