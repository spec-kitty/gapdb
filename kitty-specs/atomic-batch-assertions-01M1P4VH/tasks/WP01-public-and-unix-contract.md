---
work_package_id: WP01
title: Public and Unix contract
dependencies: []
requirement_refs:
- FR-001
- FR-002
- FR-003
- FR-004
- FR-005
- FR-006
- FR-012
- FR-013
- FR-014
- FR-015
- NFR-003
- NFR-007
planning_base_branch: feat/atomic-batch-assertions
merge_target_branch: feat/atomic-batch-assertions
branch_strategy: Planning artifacts for this mission were generated on feat/atomic-batch-assertions. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into feat/atomic-batch-assertions unless the human explicitly redirects the landing branch.
subtasks:
- T001
- T002
- T003
- T004
phase: Phase 1 - Contract
history: []
agent_profile: implementer-ivan
authoritative_surface: gapdb
create_intent: []
execution_mode: code_change
owned_files:
- gapdb/types.go
- gapdb/client.go
- gapdb/client_codec.go
- gapdb/client_test.go
- internal/protocol/**
role: implementer
tags: []
tracker_refs: []
---

# Work Package Prompt: WP01 – Public and Unix Contract

## Objective

Add a storage-neutral, first-class assertion model and freeze its strict additive Unix representation before engine code consumes it.

## Context

Read [spec.md](../spec.md), [plan.md](../plan.md), [data-model.md](../data-model.md), and [contracts/atomic-batch-assertions-v1.md](../contracts/atomic-batch-assertions-v1.md). Preserve existing assertion-free batch behavior. An assertion is not a mutation and may never be implemented as one.

### T001 — Public model

1. Add `Assertion{Key, Condition}` and `Batch.Assertions`.
2. Add `MutationResult.AssertionCount` without changing existing field meanings.
3. Add distinct optional assertion-index evidence to structured errors and cloning.
4. Keep the contract free of downstream application concepts.

### T002 — Validation and ownership

1. Permit `absent` and positive `revision` assertion conditions; reject `any` and unknown/invalid forms.
2. Require at least one mutation.
3. Reject duplicate assertion keys and any assertion/mutation overlap before engine execution.
4. Apply operation limits to assertions plus mutations and byte limits to the whole batch at exact boundaries.
5. Defensively clone both slices and all nested caller-owned data.
6. Keep error encodings below 4 KiB and prove secret value bytes never appear.

### T003 — Strict Unix and client surface

1. Add optional `assertions` to atomic-batch arguments and `assertion_count` to results.
2. Update every strict object allowlist and semantic validator together.
3. Send assertions from the first-party client and decode exact results/errors.
4. Preserve request IDs, acknowledgement semantics, frame bounds, and deterministic encoding.

### T004 — Compatibility evidence

1. Add success/failure fixtures for revision and absence assertions.
2. Add malformed/vacuous/duplicate/overlap/limit/unknown-field tests.
3. Retain old assertion-free request/result fixtures and demonstrate they still pass.
4. Include mutant controls proving strict-field and validation tests detect removed checks.

## Definition of Done

- Focused public, client, and protocol suites pass with race detection.
- The contract exactly matches the mission contract document.
- No engine, persistence, CLI, or downstream integration code is introduced.
- Mark T001–T004 done and move WP01 to review.

## Reviewer guidance

Look especially for vacuous `any`, overlapping keys, limit accounting mismatches between API and wire, aliased slices, optional-field compatibility, and value leakage in diagnostics.
