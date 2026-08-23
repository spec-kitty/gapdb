---
work_package_id: WP01
title: Public contracts and protocol foundation
dependencies: []
requirement_refs:
- FR-001
- FR-002
- FR-003
- FR-004
- FR-005
- FR-006
- FR-007
- FR-008
- FR-013
- FR-014
- FR-015
- FR-017
- FR-018
- FR-024
planning_base_branch: main
merge_target_branch: main
branch_strategy: Planning artifacts for this mission were generated on main. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into main unless the human explicitly redirects the landing branch.
base_branch: kitty/mission-gapdb-mvp-01M0Q8K6
base_commit: 4c559e0c4db0e09c42b2310c4f1439d8e31f694e
created_at: '2026-08-23T13:22:51.819246+00:00'
subtasks:
- T001
- T002
- T003
- T004
- T005
phase: Phase 1 - Foundation
agent: codex
history:
- timestamp: '2026-08-23T12:58:27Z'
  agent: codex
  action: Prompt generated via /spec-kitty.tasks
agent_profile: implementer-ivan
authoritative_surface: gapdb/
create_intent:
- go.mod
- go.sum
- gapdb/types.go
- gapdb/options.go
- internal/protocol/frame.go
- internal/protocol/envelope.go
- internal/protocol/errors.go
- internal/protocol/protocol_test.go
- internal/clock/clock.go
- internal/clock/manual.go
- tests/compatibility/protocol/golden_test.go
- docs/evidence/dependencies/x-sys.md
execution_mode: code_change
model: ''
owned_files:
- go.mod
- go.sum
- gapdb/types.go
- gapdb/options.go
- internal/protocol/**
- internal/clock/**
- tests/compatibility/protocol/**
- docs/evidence/dependencies/x-sys.md
role: implementer
tags: []
tracker_refs: []
---

# Work Package Prompt: WP01 – Public contracts and protocol foundation

## ⚡ Do This First: Load Agent Profile

Use the `/ad-hoc-profile-load` skill to load the agent profile specified in the frontmatter, and behave according to its guidance before parsing the rest of this prompt.

- **Profile**: `implementer-ivan`
- **Role**: `implementer`
- **Agent/tool**: `codex`

If no profile is specified, run `spec-kitty agent profile list` and select the best match for this work package's `task_type` and `authoritative_surface`.

---

## Objective

Freeze the small public vocabulary and version-1 wire contract that every later
Gapdb package can depend on. Reject ambiguity and oversized input at the boundary
without importing database or protocol-framework machinery.

## Context

Gapdb is a local Go map with one mutation owner, conditional writes, crash-safe
persistence, snapshots, and a Unix socket. Public values are opaque bytes. Keys
are non-empty UTF-8. Revisions are database-wide `uint64` values and may contain
gaps but are never reused. The protocol is a four-byte big-endian frame length
followed by strict JSON. Compatibility behavior comes from the mission contracts,
not from Go implementation accidents.

Planning branch: `main`. Final merge target: `main`. Spec Kitty allocates the
execution worktree from the finalized lane graph. Begin with:

`spec-kitty agent action implement WP01 --agent <name>`

### Subtask T001: Create the Go module, dependency review, package skeleton, limits, and stable errors

**Purpose**: Establish a minimal module and a single source of truth for bounded
inputs and machine-stable failure classification.

**Steps**:

1. Create module `gapdb` with Go directive `1.26`; pin only
   `golang.org/x/sys/unix` as the planned runtime dependency.
2. Before importing `x/sys/unix`, create a dependency review that records the
   exact version and checksum, upstream provenance, license, vulnerability query
   and date, narrow `Flock` usage, alternatives considered, and why it removes
   more risk than it adds. A known unmitigated applicable vulnerability blocks
   the dependency and this package.
3. Define public limits/options with the documented defaults: 4 KiB keys, 8 MiB
   values, 16 MiB frames/batches, 1,024 batch operations, 1,000 scan records,
   256 watch events/clients, and bounded history.
4. Define stable error codes, retry classifications, safe-action tokens, and a
   structured error type. Human messages must not be control-flow authority.
5. Validate configurable limits against conservative hard ceilings. Reject zero,
   negative-equivalent, overflow, and internally inconsistent configurations.
6. Keep commands and storage absent from this package; this is vocabulary only.

**Files**: `go.mod`, `go.sum`, `gapdb/options.go`, `internal/protocol/errors.go`,
`docs/evidence/dependencies/x-sys.md`.

**Validation**: `go test ./...`; table tests prove every documented default and
error code. Public errors support `errors.Is`/`errors.As` without losing evidence.

### Subtask T002: Define public records, conditions, batches, results, and options

**Purpose**: Give clients an implementation-independent API that exactly matches
the spec's record and atomicity model.

**Steps**:

1. Define `Record`, acknowledgement mode, condition/mutation kinds, batch input,
   mutation result, scan page, change event, watch termination, and status types.
2. Represent expiry as an optional absolute UTC instant and document logical
   absence at or after that instant.
3. Make duplicate keys, invalid delete conditions, zero/missing expected revisions,
   and invalid enums detectable before a mutation enters the writer.
4. Copy value byte slices at every public ingress/egress boundary; empty bytes are
   valid and distinct from absence.
5. Keep data structures explicit. Do not add schemas, tags, secondary fields,
   transactions, or generic query objects.

**Files**: `gapdb/types.go`, `gapdb/options.go`.

**Validation**: Tests mutate caller-owned source/result slices and prove stored or
source values do not alias. JSON field naming matches `protocol-v1.md` exactly.

### Subtask T003: Implement strict bounded framed JSON protocol codecs

**Purpose**: Make transport parsing deterministic, bounded, and fail-closed.

**Steps**:

1. Implement exact-length frame read/write with a four-byte big-endian length;
   reject zero and over-limit frames before payload allocation.
2. Decode one JSON value with duplicate-field, unknown-field, trailing-value,
   invalid UTF-8, version, enum, base64, and integer checks.
3. Encode requests, unary responses, watch start/event/end frames, and common
   errors from field-ordered Go structs.
4. Echo optional request IDs and omit genuinely absent evidence rather than
   inventing zero values.
5. Keep one connection sequential; do not implement multiplexing or TCP.

**Files**: `internal/protocol/frame.go`, `internal/protocol/envelope.go`,
`internal/protocol/errors.go`.

**Validation**: Short reads, partial writes, invalid prefixes, oversized lengths,
duplicate fields, and trailing JSON all return stable errors without panic.

### Subtask T004: Freeze protocol and error golden fixtures and decoder tests

**Purpose**: Make v1 compatibility observable before engine implementation.

**Steps**:

1. Add golden fixtures for every operation envelope, record shape, mutation
   result, scan, watch frame, and each error class.
2. Verify deterministic encoding byte-for-byte for unchanged inputs.
3. Verify valid fixtures decode and re-encode canonically.
4. Add negative fixtures for unsupported version, unknown/duplicate fields,
   invalid base64/UTF-8, numeric overflow, and every documented size boundary.
5. Keep golden updates explicit and reviewable; no auto-regeneration in tests.

**Files**: `tests/compatibility/protocol/**`, `internal/protocol/protocol_test.go`.

**Validation**: `go test ./internal/protocol ./tests/compatibility/protocol` and
review the fixtures against all three mission contract documents.

### Subtask T005: Add deterministic clock primitives and contract test helpers

**Purpose**: Supply the narrow time seam required for expiry, audit, and stable
tests without leaking a general dependency-injection framework.

**Steps**:

1. Define a tiny clock interface and real implementation.
2. Provide a concurrency-safe manual clock that tests can advance or move
   backward intentionally.
3. Provide a nondecreasing effective-time wrapper so observed expiry cannot be
   resurrected during one process lifetime.
4. Add helpers for fixed database IDs/request IDs only where deterministic
   fixtures require them.
5. Do not make production identifiers predictable.

**Files**: `internal/clock/clock.go`, `internal/clock/manual.go`.

**Validation**: Race-safe tests prove forward movement, backward clamping, exact
expiry-boundary behavior, and deterministic UTC/RFC3339Nano formatting.

## Definition of Done

- The public API contains only the planned coordination concepts.
- Every wire frame and value is bounded before allocation.
- Strict decoding rejects ambiguity and unsupported versions.
- Golden fixtures cover success, streaming, and all stable error families.
- The sole external dependency has a completed charter-compliant review artifact.
- `go test ./...` and `go vet ./...` pass.
- Record completion with `spec-kitty agent tasks mark-status T001 T002 T003 T004 T005 --status done`.

## Risks

- Standard `encoding/json` does not reject duplicate object fields by itself;
  implement an explicit strict-object validation seam.
- Public slice aliasing can corrupt state later; tests must mutate both sides.
- Avoid speculative version negotiation or generic RPC abstraction.

## Reviewer Guidance

Compare exported fields and stable codes directly to `contracts/protocol-v1.md`
and `contracts/errors-v1.md`. Verify limits before allocations, deterministic
output, no extra dependencies, and no domain semantics for opaque values.
