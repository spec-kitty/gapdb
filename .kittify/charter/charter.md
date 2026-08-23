# Gapdb Project Charter

This document is the human-readable companion to the authoritative structured
charter in `.kittify/charter/charter.yaml`. When the two disagree, the structured
charter governs automated workflows and this companion must be corrected in the
same change.

## Purpose

Gapdb fills the narrow gap between a Go map and a general database. It is a tiny,
durable, local key-value coordination store for leases, reviews, workflow state,
and integration authority.

The project preserves a deliberately small shape: one local owner, records in
RAM, serialized mutations, conditional writes, an append-only durability log,
occasional snapshots, and a Unix domain socket. We prefer explicit semantics,
boring Go, bounded resources, and checkable evidence over database breadth or
cleverness.

## Governing Priorities

When goals conflict, the order is:

1. Correct authority and durability semantics
2. Crash recovery, concurrency safety, and compatibility
3. Simple and inspectable operation
4. Small implementation and dependency surface
5. Performance within explicit mission thresholds
6. Delivery speed

Gapdb must never falsely acknowledge durable state, expose part of an atomic
commit, silently skip retained watch history, resurrect expired authority, grant
authority from corrupt or uncertain state, or silently perform destructive
recovery. These are release-blocking failures.

## Engineering Model

Gapdb is implemented in Go. The standard library is preferred. A dependency must
have a documented need, a license and vulnerability review, and evidence that it
removes more risk than it adds.

Deep Module Design governs the product surface: a small stable API must make
ordering, revisions, expiry, errors, durability, limits, and performance
expectations explicit while hiding persistence machinery. Specification by
Example governs behavior: concrete cases become executable acceptance checks and
remain synchronized with the specification.

The following boundaries require a separately approved mission before they can
be crossed:

- SQL, query planning, joins, secondary indexes, or interpreted values
- TCP service, replication, sharding, quorum, or distributed consensus
- Lock-free state machinery, cgo, or code generation
- An embedded language model or external inference dependency
- Automatic destructive repair

## Verification and Quality Gates

Externally observable behavior is specified with a failing test before production
implementation. Appropriate evidence includes:

- Unit tests for state-machine invariants
- Black-box contract tests through the public client and CLI
- `go test -race` concurrency tests
- Deterministic clock and fault-injection tests
- Crash tests at persistence and acknowledgement boundaries
- Golden compatibility tests for every wire and persisted format version

Before review, changed code passes `gofmt`, `go vet`, the configured static
analyzer, `go test ./...`, `go test -race ./...`, and every relevant contract,
fault-injection, and compatibility suite. A coverage percentage never substitutes
for exercising a durability or authority decision branch. Flaky tests are defects;
they are not hidden by retries.

Correctness, recovery, race, format-compatibility, and local-access-control gates
block merge. Performance regressions warn during development and block release
when the governing mission makes the threshold an acceptance criterion.

## Model-Managed Operation

Gapdb is model-managed, not model-powered. A capable but fallible external model
must be able to operate it without intuition or prose scraping.

Routine and recovery workflows use bounded, versioned, non-interactive,
machine-readable interfaces. Stable fields and error codes govern automation;
human-readable messages are advisory. Dangerous administration requires explicit
database-identity or revision preconditions. Recovery first reports evidence and
proposes an action, then requires a guarded apply command.

Requests, values, batches, scans, watch buffers, queues, retained history, and
diagnostics have discoverable limits. The system never silently drops state or
events when a limit is reached.

## Durable Formats and Recovery

Every wire and persisted format carries an explicit version and integrity check.
Unknown versions and non-tail corruption fail closed without mutation. Snapshots
are installed atomically only after verification and required sync barriers. Log
history is retired only after its replacement snapshot is durable.

SQLite remains the production fallback until Gapdb passes the originating
application's contract, concurrency, fault-injection, crash-recovery, and
authority-safety suites. Replacing SQLite requires recorded evidence and explicit
human approval.

## Review and Documentation

Behavior-changing work requires independent review by an agent or human who did
not implement it. Implementers do not self-approve. Reviews trace changes to
requirements and challenge crash ordering, conditional mutation, expiry,
concurrency, bounds, compatibility, and model-operability claims.

The mission spec, acceptance examples, API and error catalog, wire and storage
formats, recovery runbook, configuration reference, and JSON examples change with
the behavior they describe. Non-obvious persistence and concurrency decisions get
concise ADRs.

## Amendments and Exceptions

Charter amendments use a focused reviewed commit stating rationale, affected
rules and workflows, migration impact, and effective date. Removing or weakening
a protection requires explicit approval from the human project owner.

Exceptions require explicit human approval, narrow scope, rationale, risk
assessment, compensating control, named owner, expiry date, and rollback plan.
They cannot waive the prohibitions on false durability, partial atomic visibility,
authority from known-corrupt state, or silent destructive recovery. An expired
exception fails closed.
