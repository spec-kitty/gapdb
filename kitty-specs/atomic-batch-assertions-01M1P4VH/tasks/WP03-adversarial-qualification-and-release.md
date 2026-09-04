---
work_package_id: WP03
title: Adversarial qualification and release
dependencies:
- WP02
requirement_refs:
- NFR-001
- NFR-002
- NFR-004
- NFR-005
- NFR-006
planning_base_branch: feat/atomic-batch-assertions
merge_target_branch: feat/atomic-batch-assertions
branch_strategy: Planning artifacts for this mission were generated on feat/atomic-batch-assertions. During /spec-kitty.implement this WP may branch from a dependency-specific base, but completed changes must merge back into feat/atomic-batch-assertions unless the human explicitly redirects the landing branch.
base_branch: kitty/mission-atomic-batch-assertions-01M1P4VH
base_commit: a4cc9fe0b10e49f8c241357e2b1800522244360e
created_at: '2026-09-04T13:38:09.303860+00:00'
subtasks:
- T009
- T010
- T011
- T012
phase: Phase 3 - Qualification
history: []
agent_profile: implementer-ivan
authoritative_surface: tests
create_intent: []
execution_mode: code_change
owned_files:
- tests/adoption/**
- tests/crash/**
- tests/performance/**
- docs/evidence/atomic-batch-assertions/**
role: implementer
tags: []
tracker_refs: []
---

# Work Package Prompt: WP03 – Adversarial Qualification and Release

## Objective

Attack the integrated assertion contract through real transport, concurrency, crashes, recovery, compatibility, and performance, then publish immutable consumable evidence.

## Context

Do not replace full-stack proof with a cooperative fake. The critical failure is a stale assertion incorrectly authorizing a real mutation while process lifetime, persistence, and transport disagree.

### T009 — Stale-authority contention

1. Run at least 1,000 deterministic trials racing authority revision/creation against guarded mutations.
2. Assert exactly one valid serialization and zero stale successes.
3. Run with the race detector.
4. Include a controlled mutant that evaluates assertions outside writer authority; the suite must reject it.

### T010 — Crash, recovery, and real Unix path

1. Start a real daemon and use the first-party Unix client.
2. Inject failure around predicate evaluation, WAL append, sync, apply/publication, and response boundaries available in the harness.
3. Recover by normal startup and reconcile acknowledgement evidence.
4. Verify failed predicates never reappear and successful commits recover only mutation data.
5. Verify watches contain no assertion events.

### T011 — Bounds and performance

1. Test combined operation and encoded-byte limits immediately below, at, and above boundaries.
2. Measure mutation-only and assertion-bearing controls on the same reference host.
3. Require memory p95 overhead <= max(15%, 100 microseconds) and durable p95 < 4 ms.
4. Record host, toolchain, configuration, trial counts, raw output, and calculation.

### T012 — Frozen qualification and consumption

1. Run the entire repository gate ladder, race detector, vet, staticcheck, govulncheck, module verification, formatting, and binary builds.
2. Run all existing adoption scenarios unchanged.
3. From a temporary external Go module, consume the immutable GapDB commit without a local `replace` and execute an assertion batch.
4. Record failures honestly; qualification is tied to exact commit/tree hashes.
5. Update public documentation only where required to describe the shipped additive API.

## Definition of Done

- All mission success criteria have checkable evidence.
- All material changes are committed and the work package is independently approved.
- The immutable module revision is available for Go Kitty consumption.
- Mark T009–T012 done and move WP03 to review.

## Reviewer guidance

Confirm real daemon/client use, exact zero-effect evidence, meaningful mutant sensitivity, fixed trial counts, benchmark comparability, and external consumption without `replace`.
