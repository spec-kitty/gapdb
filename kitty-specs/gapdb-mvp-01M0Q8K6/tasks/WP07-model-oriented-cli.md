---
work_package_id: WP07
title: Model-oriented CLI and lifecycle operations
dependencies:
- WP06
requirement_refs:
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
- T034
- T035
- T036
- T037
- T038
phase: Phase 7 - Model operations
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: implementer-ivan
authoritative_surface: cmd/gapctl/
create_intent:
- cmd/gapctl/main.go
- cmd/gapctl/command.go
- cmd/gapctl/output.go
- cmd/gapctl/data.go
- cmd/gapctl/admin.go
- cmd/gapctl/offline.go
- docs/operations/configuration.md
- docs/operations/recovery.md
- tests/contract/cli/cli_test.go
- tests/contract/modelops/lifecycle_test.go
execution_mode: code_change
model: ''
owned_files:
- cmd/gapctl/**
- docs/operations/**
- tests/contract/cli/**
- tests/contract/modelops/**
role: implementer
agent: codex
tags: []
tracker_refs: []
---

# Work Package Prompt: WP07 – Model-oriented CLI and lifecycle operations

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `implementer-ivan`
- **Role**: `implementer`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Provide a stable non-interactive `gapctl` surface that a capable but fallible model
can use for every routine, degraded, backup, verification, and recovery workflow
without parsing prose or guessing a destructive next step.

## Context

The CLI is a thin adapter over the Go client and offline admin packages. Unary JSON
mode prints exactly one result to stdout; watch prints a documented JSONL stream.
Errors use stable exit classes. Offline operations require the owner lock, recovery
proposal is read-only, and apply carries exact identity/generation/proposal guards.

Planning and merge target are `main`; use the finalized lane workspace. Start:

`spec-kitty agent action implement WP07 --agent <name>`

### Subtask T034: Build the deterministic `gapctl` command and output framework

**Purpose**: Centralize parsing, validation, output, and exit behavior so commands
cannot drift into ad-hoc prose or interactive defaults.

**Steps**:

1. Use the standard `flag` package or a small explicit parser; do not add a CLI
   framework dependency.
2. Define global socket/database, deadline, request ID, and `--output=json|jsonl`
   behavior with no prompts, color, paging, or terminal detection.
3. Route stdout only through one field-ordered encoder; diagnostics/logging go to
   stderr only when they cannot be represented as the contracted result.
4. Map protocol error families to exits 2–6 exactly and reserve zero for success.
5. Provide discoverable command/schema/limit help in bounded JSON as well as text.

**Files**: `cmd/gapctl/main.go`, `command.go`, `output.go`.

**Validation**: Golden tests compare byte output/exit codes, unknown flags and
commands are structured, and no command reads stdin unless a documented file/stdin
value source is explicitly selected.

### Subtask T035: Implement all data and conditional mutation commands

**Purpose**: Expose Get, Put, PutIfAbsent, CAS, conditional delete, AtomicBatch,
and ScanPrefix without weakening binary-value or durability semantics.

**Steps**:

1. Parse keys as UTF-8 and values from explicit file, base64, or stdin sources;
   never confuse empty input with missing input.
2. Parse expiries as strict UTC RFC3339Nano and acknowledgement as exact enums.
3. Require expected revisions where defined and pass correlation IDs through.
4. Parse atomic-batch files strictly using the shared protocol validation and
   reject duplicate/unknown fields before dialing.
5. Render returned bytes only in an explicit output representation and preserve
   all revision/durable-through evidence.

**Files**: `cmd/gapctl/data.go` and command tests.

**Validation**: Every valid/invalid acceptance case runs against a real daemon;
lease/review examples always select durable mode in docs/tests.

### Subtask T036: Implement scan output and terminal watch JSONL behavior

**Purpose**: Make reconciliation streams unambiguous and resumable after lag,
disconnect, or compaction.

**Steps**:

1. Emit bounded scan result including observed revision, as-of, truncation, and
   opaque cursor exactly as returned.
2. For watch, print start, each ordered event, and one terminal frame as JSONL;
   flush each completed line.
3. Preserve last-delivered/current/earliest evidence on terminal errors and map
   the final exit after writing the terminal frame.
4. Handle SIGINT as explicit local cancellation without fabricating a server
   success or overwriting a received terminal result.
5. Document rescan then resume as the only recovery for lag/compaction/stale scan.

**Files**: `cmd/gapctl/data.go`, CLI contract tests.

**Validation**: Slow-pipe and disconnect tests prove no mixed output, partial JSON
line, silent skip, or zero exit on terminal watch error.

### Subtask T037: Implement online administration and guarded offline recovery

**Purpose**: Expose bounded status/health/stats/config/verify and guarded state-
changing administration through stable commands.

**Steps**:

1. Add online status, health, stats, describe-config, verify, snapshot, compact,
   and backup commands with exact precondition flags.
2. Add offline inspect and verify that acquire ownership and remain read-only.
3. Add `recover propose` and `recover apply`; apply requires proposal, database ID,
   manifest generation, and explicit quarantine/backup destination.
4. Reject ambiguous online/offline target combinations and relative unsafe backup
   or quarantine destinations.
5. Render `operation_applied` and safe actions prominently as stable JSON fields,
   not special prose.

**Files**: `cmd/gapctl/admin.go`, `offline.go`, tests.

**Validation**: Stale preconditions and live-owner offline attempts make no file
changes; proposal/apply evidence round-trips and tampering is rejected.

### Subtask T038: Prove and document the non-interactive model-only lifecycle

**Purpose**: Test the explicit additional requirement that a roughly 200B model
can safely manage the service through machine-readable affordances alone.

**Steps**:

1. Build a harness that uses only command JSON/JSONL, exit status, and documented
   safe-action tokens to start, inspect, mutate, watch, snapshot, compact, backup,
   stop, restart, detect injected corruption, and request recovery proposal.
2. Disallow string matching against human messages and disallow private file edits.
3. Assert every failure presents enough identifiers/revisions and at least one
   enumerated safe next action.
4. Verify repeated inspection is byte-equivalent after excluding documented
   volatile fields and that every output advertises schema version/limits.
5. Publish configuration and recovery runbooks containing copyable JSON workflows
   and explicit authority boundaries.

**Files**: `tests/contract/modelops/**`, `docs/operations/configuration.md`,
`docs/operations/recovery.md`.

**Validation**: The harness completes with no prompt/TTY/prose parser and refuses
destructive recovery until all exact preconditions are available.

## Definition of Done

- Every data/admin operation is reachable through deterministic JSON.
- Unary stdout is exactly one object; watch stdout is only the defined JSONL stream.
- Stable exit codes and safe-action evidence cover every failure.
- Offline recovery remains lock-safe and proposal-before-apply.
- The model-only lifecycle harness passes end-to-end.
- Record completion with `spec-kitty agent tasks mark-status T034 T035 T036 T037 T038 --status done`.

## Risks

- Keep human-friendly defaults from becoming ambiguous automation behavior.
- Never let a convenience flag weaken durable acknowledgement or admin guards.
- Broken-pipe handling must terminate cleanly without corrupting stdout.

## Reviewer Guidance

Review the CLI as a machine API: exact stdout, exit, fields, bounds, safe actions,
and lack of prompts. Run the modelops harness without reading logs or internals.
