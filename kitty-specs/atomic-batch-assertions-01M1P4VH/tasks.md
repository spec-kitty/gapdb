# Work Packages: Atomic Batch Read Assertions

**Mission:** `atomic-batch-assertions-01M1P4VH`  
**Planning branch:** `feat/atomic-batch-assertions`  
**Merge target:** `feat/atomic-batch-assertions`  
**Generated:** 2026-09-04

## Execution overview

```text
WP01 Public and Unix contract
            |
            v
WP02 Serialized engine semantics
            |
            v
WP03 Adversarial qualification and release
```

The dependency order is deliberate: public and strict wire semantics are frozen before engine plumbing; full-stack crash, race, compatibility, and performance evidence tests the integrated result rather than a substitute fake.

## Subtask index

| ID | Description | WP | Parallel |
|----|-------------|----|----------|
| T001 | Add the public Assertion and Batch/MutationResult contract | WP01 | No |
| T002 | Enforce meaningful conditions, disjoint keys, unified limits, cloning, and bounded errors | WP01 | Yes |
| T003 | Extend strict Unix request/result codecs and first-party client | WP01 | No |
| T004 | Freeze compatibility and malformed-input protocol evidence | WP01 | Yes |
| T005 | Evaluate assertions inside the serialized pre-batch predicate phase | WP02 | No |
| T006 | Preserve zero-effect failure, coherent expiry, and write-only revision/event semantics | WP02 | No |
| T007 | Plumb server dispatch and prove direct/Unix semantic parity | WP02 | Yes |
| T008 | Prove recovery, watch, cancellation, and lost-response behavior | WP02 | Yes |
| T009 | Run 1,000 stale-authority contention trials and mutant controls | WP03 | No |
| T010 | Run subprocess crash/recovery and real Unix adoption scenarios | WP03 | Yes |
| T011 | Record comparative memory/durable p95 evidence and exact bounds | WP03 | Yes |
| T012 | Run complete qualification, external-module consumption, and release evidence | WP03 | No |

## WP01 — Public and Unix contract

**Prompt:** `tasks/WP01-public-and-unix-contract.md`  
**Priority:** P1  
**Independent test:** Public and wire table tests prove valid revision/absence assertions, invalid/vacuous predicates, duplicate/overlap rejection, exact combined limits, cloning, old-client compatibility, and bounded value-free errors.  
**Dependencies:** None

T001 Add the public Assertion and Batch/MutationResult contract (WP01)
T002 Enforce meaningful conditions, disjoint keys, unified limits, cloning, and bounded errors (WP01)
T003 Extend strict Unix request/result codecs and first-party client (WP01)
T004 Freeze compatibility and malformed-input protocol evidence (WP01)

## WP02 — Serialized engine semantics

**Prompt:** `tasks/WP02-serialized-engine-semantics.md`  
**Priority:** P1  
**Independent test:** Controlled writer/clock tests prove that assertions and mutation conditions share one pre-batch view, any failure has zero effects, successful assertions create no write artifacts, and direct/server paths agree.  
**Dependencies:** WP01

T005 Evaluate assertions inside the serialized pre-batch predicate phase (WP02)
T006 Preserve zero-effect failure, coherent expiry, and write-only revision/event semantics (WP02)
T007 Plumb server dispatch and prove direct/Unix semantic parity (WP02)
T008 Prove recovery, watch, cancellation, and lost-response behavior (WP02)

## WP03 — Adversarial qualification and release

**Prompt:** `tasks/WP03-adversarial-qualification-and-release.md`  
**Priority:** P1  
**Independent test:** The real daemon/client survives stale races, injected crashes, recovery, response loss, compatibility fixtures, and benchmark gates; an external module imports the immutable revision without `replace`.  
**Dependencies:** WP02

T009 Run 1,000 stale-authority contention trials and mutant controls (WP03)
T010 Run subprocess crash/recovery and real Unix adoption scenarios (WP03)
T011 Record comparative memory/durable p95 evidence and exact bounds (WP03)
T012 Run complete qualification, external-module consumption, and release evidence (WP03)

## Requirement coverage

| Requirements | Work package |
|--------------|--------------|
| FR-001–FR-006, FR-012–FR-015, NFR-003, NFR-007 | WP01 |
| FR-007–FR-011, FR-016–FR-018 | WP02 |
| NFR-001, NFR-002, NFR-004–NFR-006, SC-001–SC-007 | WP03 |
