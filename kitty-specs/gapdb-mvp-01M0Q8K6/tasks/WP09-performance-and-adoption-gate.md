---
work_package_id: WP09
title: Performance, formats, and SQLite adoption gate
dependencies:
- WP01
- WP02
- WP03
- WP04
- WP05
- WP06
- WP07
- WP08
requirement_refs:
- FR-001
- FR-002
- FR-003
- FR-004
- FR-005
- FR-006
- FR-007
- FR-010
- FR-011
- FR-012
- FR-013
- FR-014
- FR-015
- FR-016
- FR-017
- FR-018
- FR-019
- FR-020
- FR-021
- FR-022
- FR-023
- FR-024
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
subtasks:
- T045
- T046
- T047
- T048
- T049
- T050
phase: Phase 9 - Acceptance evidence
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: implementer-ivan
authoritative_surface: tests/performance/
create_intent:
- tests/performance/benchmark_test.go
- tests/performance/recovery_test.go
- tests/adoption/contract.go
- tests/adoption/runner_test.go
- docs/formats/protocol-v1.md
- docs/formats/storage-v1.md
- docs/evidence/performance/README.md
- README.md
execution_mode: code_change
model: ''
owned_files:
- tests/performance/**
- tests/adoption/**
- docs/formats/**
- docs/evidence/performance/**
- README.md
role: implementer
agent: codex
tags: []
tracker_refs: []
---

# Work Package Prompt: WP09 – Performance, formats, and SQLite adoption gate

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `implementer-ivan`
- **Role**: `implementer`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Measure the specified latency/recovery targets, publish the implemented v1 formats,
and provide a backend-neutral adoption seam that keeps SQLite selected until the
external Phase 1 contract, safety evidence, and explicit human approval all pass.

## Context

Performance may not weaken durability, bounds, or serialized ownership. Reference
claims require recorded hardware/kernel/filesystem/device/toolchain/seed/command.
The originating application lives outside this repository, so Gapdb supplies a
thin portable contract/runner and evidence interface—not an invented replacement
for that application's SQLite adapter or an automatic production switch.

Planning and merge target are `main`; use the finalized lane workspace. Start:

`spec-kitty agent action implement WP09 --agent <name>`

### Subtask T045: Implement reference read, write, and recovery benchmarks

**Purpose**: Measure NFR-001/002/003/010 with workloads that preserve real product
semantics.

**Steps**:

1. Generate 100,000 live 1 KiB records deterministically and retain eight local
   concurrent readers plus one writer for latency runs.
2. Measure end-to-end client `Get`, memory mutation, and durable mutation p50/p95/
   p99 with allocations; durable requests must execute actual successful syncs.
3. Measure recovery from a verified snapshot plus 10,000 WAL commits to readiness.
4. Separate setup/warmup from measured windows and avoid shared-key contention
   unless a specific benchmark names it.
5. Provide ordinary Go benchmark output and a machine-readable summary with the
   target and observed value; never silently relax thresholds.

**Files**: `tests/performance/benchmark_test.go`, `recovery_test.go`.

**Validation**: Benchmarks validate fixture counts/bytes/revisions and fail if a
fake filesystem, disabled fsync, or in-process API accidentally replaces the
specified socket/durable path.

### Subtask T046: Record reproducible reference-machine benchmark metadata

**Purpose**: Keep numerical claims meaningful across machines and future changes.

**Steps**:

1. Record CPU, memory, architecture, kernel, filesystem/mount, storage device,
   Go version, commit, command, seed, configuration, and raw benchmark output.
2. Mark volatile timestamps/durations and keep field ordering deterministic.
3. Distinguish reference acceptance from developer smoke runs.
4. Do not commit hostnames, usernames, serial numbers, full mount layouts, or
   unrelated environment values.
5. Document variance policy and compare changes without claiming unsupported
   cross-machine regression precision.

**Files**: `docs/evidence/performance/**`.

**Validation**: A reader can reproduce the exact workload/configuration and tell
whether all four thresholds were evaluated on the claimed reference profile.

### Subtask T047: Define the originating-application storage contract harness

**Purpose**: Express the Phase 1 behaviors needed by the other project without
coupling Gapdb internals to that application's implementation.

**Steps**:

1. Define a narrow test-only backend interface matching Get, conditional writes,
   batch, scan, watch, expiry, acknowledgement, restart, and inspection needs.
2. Encode authority-safety scenarios for leases, reviews, workflow state, and
   integration authority from the mission acceptance cases.
3. Require durable mode for authority-bearing scenarios and verify response-loss
   reconciliation.
4. Make external adapters injectable/optional; absence of the other repository is
   reported as pending adoption evidence, not a fake pass.
5. Publish fixture/runner version so the other project can pin the contract.

**Files**: `tests/adoption/contract.go`, `runner_test.go`.

**Validation**: The in-repo Gapdb adapter runs every applicable scenario; a stub
backend cannot pass without satisfying observable behavior.

### Subtask T048: Provide the SQLite/Gapdb dual-backend adoption runner seam

**Purpose**: Let the originating project compare both implementations while
preserving human control over production selection.

**Steps**:

1. Accept adapter registration or an external test command/result manifest for
   SQLite and Gapdb.
2. Normalize scenario IDs and evidence without requiring identical internal
   revisions or storage files.
3. Require both correctness results plus Gapdb crash/race/expiry/watch/authority
   evidence before marking technical gates complete.
4. Keep `production_backend: sqlite` in the generated recommendation by default.
5. Represent the human switch approval as an external signed/recorded decision;
   this repository never flips another application's configuration.

**Files**: `tests/adoption/**`, evidence documentation.

**Validation**: Missing SQLite/external results produce an explicit incomplete
gate, and no automated invocation can mark human approval true.

### Subtask T049: Promote v1 formats and operator guidance into living docs

**Purpose**: Ship documentation tied to actual codecs, flags, defaults, and tested
recovery behavior.

**Steps**:

1. Promote protocol/storage contracts into `docs/formats` and cross-link exact
   source/test fixtures rather than duplicating divergent prose.
2. Document public Go and CLI examples for all operations, acknowledgement modes,
   scans/watches, snapshots, backup, verification, and recovery proposal/apply.
3. State every limit, permission default, compatibility rule, and out-of-scope
   boundary prominently.
4. Generate/check command examples against binaries in tests where practical.
5. Add a concise project README that keeps the one-map/one-writer description and
   explicit SQLite adoption status.

**Files**: `docs/formats/**`, `README.md`.

**Validation**: Links resolve, examples execute, field/flag/default names match
code, and no doc implies memory acknowledgements are durable.

### Subtask T050: Implement the release evidence manifest and adoption checklist

**Purpose**: Make MVP completion and external adoption separate, objectively
reviewable decisions.

**Steps**:

1. Emit a bounded versioned manifest linking SC-001 through SC-009 and all NFRs
   to commands/results, with pass/fail/not-run status.
2. Include race, crash schedule count, format fixtures, modelops, permissions,
   performance, and dual-backend fields.
3. Fail release validation when required evidence is missing, stale, mismatched to
   commit/configuration, or beyond documented bounds.
4. Keep the adoption result `not_approved` until an explicit human record is
   supplied outside the automated test run.
5. Document known platform/reference limitations without hiding them as success.

**Files**: `tests/adoption/**`, `docs/evidence/performance/**`, `README.md`.

**Validation**: Delete/tamper each evidence input and verify the manifest fails
closed; a complete MVP can pass while the external SQLite switch remains pending.

## Definition of Done

- Reference benchmarks exercise real socket and sync paths with full metadata.
- The portable Phase 1 contract runs against Gapdb and accepts external adapters.
- Missing external SQLite/human approval remains visibly incomplete.
- Living docs match shipped fields, flags, formats, limits, and recovery behavior.
- Release evidence accounts for every success criterion and NFR.
- Record completion with `spec-kitty agent tasks mark-status T045 T046 T047 T048 T049 T050 --status done`.

## Risks

- Performance thresholds depend on a declared reference machine; do not turn
  developer-laptop variance into product failure or success.
- Do not add a SQLite runtime dependency merely to simulate the external project.
- Documentation copied from planning must be verified against final code.

## Reviewer Guidance

Verify benchmark integrity, environmental metadata, and evidence freshness. Ensure
the external adapter seam is genuinely thin, all format docs match golden bytes,
and no automated path can replace SQLite without the explicit human gate.
