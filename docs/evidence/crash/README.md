# Crash and safety evidence

This directory records the reproducible WP08 reliability result for Gapdb code
commit `d34eadaea72b5763e0270472635e03423f7bffcc`. The compact machine-readable
record is [`results.json`](results.json). It contains no hostname, username,
device serial, database payload, or unrelated environment value.

## Acceptance command

From a clean checkout of the code-under-test commit, run:

```sh
go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1 -v
go test -count=1 ./...
go test -race -count=1 ./...
```

The first command runs 1,024 deterministic schedules. Each schedule names its
seed, production fault point, before/after phase, acknowledgement class, and
occurrence. The suite requires all 78 point/phase pairs to fire. It drives real
server, client, persistence, snapshot, compaction, backup, audit, apply, and
response paths; then it reopens without injection and constructs the recovery
observation only from public `Status` and `Get` results, including each record's
revision. Before the injected mutation, the oracle records the preceding
owner's public durable reservation boundary; the attempted revision is the
first revision after that boundary, not the prior committed maximum plus one.
A post-recovery durable write must allocate a different revision and survive
another restart. The focused probe observes boundary `1,048,576`, attempted and
recovered revision `1,048,577`, and post-recovery revision `2,097,153`. A
successful response is accepted only when its exact revision equals the first
revision after the retained reservation boundary; focused tests reject zero,
lower, higher, and overflow-adjacent responses while preserving exact equality.
A schedule that cannot reopen must instead return the same structured recovery
error twice while the database tree digest remains unchanged. The external
oracle rejects a lost durable acknowledgement, a partial atomic effect, a
value or record revision that disagrees with its commit, a reused attempted
revision, or state beyond recovered authority.

The acceptance run observed 2,433 durable, 812 memory, and 119
unacknowledged commits. Looking only at the specifically injected operation (and
excluding baseline history), it observed 9 durable successes, 4 memory
successes, and 119 unacknowledged outcomes. These are response-derived counts;
the suite fails if any injected class is absent.

A failure reports the smallest schedule reached in the stable form:

```text
seed-<hex>/<point>/<phase>/<durable|memory|unacknowledged>/occurrence-1
```

Re-run that name with Go's `-run`/subtest filtering after using the printed seed
and point in the corresponding test table. The schedule catalog and seed
derivation are source-controlled in `tests/crash/fault_matrix_test.go`.

## Short smoke command

```sh
go test -short ./tests/crash -count=1
go test ./tests/compatibility/fuzz -count=1
```

Short mode executes 128 production schedules while still covering every named
point in both phases. It also runs the real-process ledger, unacknowledged batch,
race-stress, permission, bound, degraded-mode, compatibility, and ownership
checks.

## What is independently observed

- An external JSONL ledger is created with a synced parent. Each canonical
  entry is SHA-256 chained to its predecessor and synced, then a separate
  terminal digest is installed through temp-write, file sync, rename, and
  directory sync. The surviving parent independently retains the writer's
  in-memory terminal digest and the acknowledgement mode it requested; neither
  authority value is persisted beside the ledger. Restart reconciliation
  requires both before using ledger revisions/effects against public values and
  record revisions. Absent, stale, and wrong retained anchors fail closed.
  One-at-a-time revision, key, value, acknowledgement class, trailing JSON,
  transition, and duplicate mutations fail. Even if the entire chain and
  sidecar are coherently rewritten, a revision edit fails public record-revision
  reconciliation and a durable-to-memory edit contradicts the parent-retained
  requested acknowledgement.
- An evidence-tagged daemon can stop at named persistence milestones. The
  test inspects `WaitStatus` and requires `SIGKILL` at every crash milestone.
  Graceful `SIGTERM` is asserted separately. The default release binary has no
  environment-driven stop hook.
- Durable results must recover. Memory and unacknowledged complete commits may
  recover or disappear, but an atomic batch may never recover partially and an
  observed revision may never be reused.
- Stale-socket cleanup is performed only by the next owner, and a live owner
  prevents a second process from starting.
- Rejected compatibility, ownership, bound, and backup-verification inputs are
  checked for no unauthorized filesystem change.
- Every configured public limit is exercised at its maximum and maximum+1,
  including encoded batch bytes, scan bytes, concurrent clients, watch queue,
  retained history event/byte bounds, total request frames, and bounded
  compaction path diagnostics. Rejected mutation limits leave the revision
  unchanged.
- Concurrency stress runs eight readers with mutation, expiry, scan, watch,
  snapshot, and status activity under the race detector. A separate deterministic
  public-socket matrix compares an exact watch sequence to writer results, forces
  an atomic multi-event `WATCH_LAGGED` terminal with its last/current revision
  evidence, separates writer and watch clients, applies an accepted-watch
  deadline, disconnects a live watch, and reconciles every acknowledged durable
  write after concurrent shutdown and restart.

## Bounded fuzz commands

```sh
go test ./internal/persist -run '^$' -fuzz '^FuzzDecodeSnapshotNeverPanics$' -fuzztime=5s
go test ./tests/compatibility/storage -run '^$' -fuzz '^FuzzStorageDecoders$' -fuzztime=5s
go test ./tests/compatibility/fuzz -run '^$' -fuzz '^FuzzWireAndStorageDecoders$' -fuzztime=5s
go test ./tests/compatibility/fuzz -run '^$' -fuzz '^FuzzScanCursorDecoder$' -fuzztime=5s
go test ./tests/compatibility/fuzz -run '^$' -fuzz '^FuzzBackupVerifierDoesNotMutate$' -fuzztime=5s
```

The committed seeds cover wire envelopes, identity, manifest, WAL, snapshot,
cursor, audit, and backup inputs. Harness limits are deliberately small. File
decoder and backup rejection paths hash their directories before and after; the
only separately tested automatic mutation is incomplete final-WAL-tail recovery.

## Recorded platform

The acceptance record was produced with Go 1.26.7 on Linux/amd64, kernel
7.0.0-28-generic, ext4, 12 logical CPUs, and 65,721,852 kB reported memory. No
platform test was skipped and no flaky retry was accepted. Numerical durations
are evidence for this run, not cross-machine performance claims.
