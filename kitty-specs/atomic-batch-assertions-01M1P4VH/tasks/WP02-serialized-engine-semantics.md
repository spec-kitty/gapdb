---
work_package_id: WP02
title: Serialized engine semantics
dependencies:
- WP01
requirement_refs:
- FR-007
- FR-008
- FR-009
- FR-010
- FR-011
- FR-016
- FR-017
- FR-018
planning_base_branch: feat/atomic-batch-assertions
merge_target_branch: feat/atomic-batch-assertions
branch_strategy: Planning artifacts for this mission were generated on feat/atomic-batch-assertions. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into feat/atomic-batch-assertions unless the human explicitly redirects the landing branch.
subtasks:
- T005
- T006
- T007
- T008
phase: Phase 2 - Engine
history: []
agent_profile: implementer-ivan
authoritative_surface: internal
create_intent: []
execution_mode: code_change
owned_files:
- internal/engine/**
- internal/server/**
role: implementer
tags: []
tracker_refs: []
---

# Work Package Prompt: WP02 – Serialized Engine Semantics

## Objective

Evaluate assertions and existing mutation conditions against exactly one pre-batch state under serialized writer authority, preserving zero-effect failure and existing durability/recovery/watch laws.

## Context

WP01 freezes the public and protocol types. Assertions are transient commit-admission predicates. They never enter WAL or snapshots and never produce watch events.

### T005 — Serialized predicate phase

1. Clone and validate before submission as today, but perform definitive assertion evaluation only inside writer execution.
2. Capture one effective time inside that serialized execution.
3. Evaluate assertions in request order, then mutation conditions, before revision allocation or commit construction.
4. Return indexed bounded condition evidence on the first failure.

### T006 — Exact state effects

1. Prove any predicate failure changes no record, global/per-key revision, WAL, snapshot lineage, expiry structure, or watch history.
2. On success, allocate one revision for the mutation subset only.
3. Report `AssertionCount` exactly while leaving mutation count and ack/durable-through semantics unchanged.
4. Ensure an assertion at an expiry boundary observes the same logical state as mutation conditions.

### T007 — Server parity

1. Pass decoded assertions through the existing handler to `AtomicBatch`.
2. Test direct state and real server handler success/failure equivalence.
3. Preserve cancellation, queue admission, shutdown, and bounded result handling.

### T008 — Recovery, watch, and unknown outcome

1. Verify successful assertion batches recover exactly as their mutations.
2. Verify failed assertions create no replayable material.
3. Verify watchers receive only mutation events.
4. Exercise cancellation before admission and response loss after commit; no result field may overstate outcome knowledge.
5. Add a mutant or structural assertion proving evaluation cannot be moved outside writer authority unnoticed.

## Definition of Done

- Engine and server tests pass under `go test -race`.
- Controlled clocks prove one effective-time view.
- Failure-side state evidence is byte/revision/event identical.
- Mark T005–T008 done and move WP02 to review.

## Reviewer guidance

Audit the exact line where writer ownership begins, where revision allocation occurs, and every early return. Reject preflight-only checks, duplicate clock reads, or assertion data entering persistence/watch structures.
