---
affected_files: []
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command:
reviewed_at: '2026-08-23T18:11:28Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP05
---

# WP05 Review Cycle 2

Verdict: changes requested.

## Blocking findings

1. **Recovery evidence is not an exact artifact identity.** `internal/admin/verify.go:evidenceHash` hashes at most the first 64 MiB and does not bind the file size. An adversarial probe extended the corrupt authoritative WAL beyond 64 MiB, generated a proposal, changed a byte after the 64 MiB boundary, and then successfully applied the stale proposal. `ApplyRecovery` moved the changed WAL and returned `operation_applied: true`. This violates T026/FR-021's exact immediate revalidation and permits a destructive action against evidence different from what was proposed. Use a bounded-memory streaming hash over the complete artifact (including detection of read errors/size changes), and deletion-test mutations before, at, and beyond the old boundary.

2. **Lexical destination checks do not provide the required symlink/no-overwrite guarantees.** `ApplyRecovery` validates `QuarantineDirectory` using `Abs`/`Rel` and follows it through `os.Mkdir`/`os.Stat`; a lexically external symlink resolving to a directory inside the database was accepted and the authoritative WAL was moved into the database-local target. `validateSeparateNewDestination` has the same escape for backup/restore destinations. It also uses `os.Stat`, so an existing dangling destination symlink is treated as absent and the final rename can replace that existing directory entry. Resolve and validate existing path components without following an attacker-controlled final entry, reject symlink aliases back into the source database, and establish no-overwrite atomically rather than with a check-then-rename. Add real symlink, dangling-symlink, parent-alias, and race probes for backup, restore, and quarantine.

3. **Snapshot panic evidence overclaims authority application.** `internal/engine/snapshot.go:executeSnapshot` maps every panic to `INTERNAL` with `operation_applied: true`. A probe using a log whose first `Barrier` panics confirmed that the installer was never called and no snapshot/WAL/CURRENT action occurred, yet the returned error claimed application. Track snapshot progress just as the mutation append path does: definite pre-write/pre-authority panics must remain unapplied, while ambiguous/after-rename/CURRENT phases may conservatively claim applied and degrade.

4. **FR-022/T027 auditability is not integrated with the administrative operations it governs.** Production call-site inspection found `AppendAuditAfterApply` is called only by `ApplyRecovery`; successful snapshot installation, compaction, backup creation, and restore cannot append and sync a correlated audit entry because their production paths expose no audit integration. Consequently ordinary success can be returned without durable audit evidence, and post-apply audit failure cannot map to `AUDIT_FAILED_AFTER_APPLY` or degrade health for those operations. Wire audit into every state-changing administrative operation and fault both before/after each apply boundary. Ensure audit failures preserve the already-applied result.

5. **The required administrative surface is incomplete and currently disconnected.** `RunningView` exposes lifecycle plus record/watch counts but not the effective configuration required by FR-019. The new exported snapshot/compaction/backup/restore/status/inspection entry points also have no live production caller outside their defining packages (tests are the only callers, except `VerifyGeneration` and recovery's audit helper), failing the prompt's dead-code gate and leaving the guarded workflows unavailable to the application. Add the bounded configuration/statistics view and a production orchestration seam that applies ownership, preconditions, audit, and degraded-state behavior consistently.

## Independent evidence

- `go version`: `go1.26.7 linux/amd64`.
- Passed: `go test ./...`, `go test -race ./...`, `go vet ./...`, `staticcheck ./...`, `govulncheck ./...`, `go mod tidy -diff`, `gofmt -l`, and `git diff --check`.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` (5 s, 489,470 executions) and `FuzzStorageDecoders` (5 s, 93,731 executions).
- The committed tests do not exercise symlink destinations, recovery evidence changes beyond 64 MiB, snapshot pre-authority panic evidence, audit integration for snapshot/compaction/backup/restore, or after-phase coverage for the WP05 fault matrices. The review probes failed deletion-sensitively on each of the first three behaviors above and were removed after execution.

## Anti-pattern checklist

1. Dead code: **FAIL** — the administrative/snapshot/backup orchestration surfaces lack production callers.
2. Synthetic-fixture test: **PASS** for tests present, but required integration paths are missing.
3. Silent empty return: **PASS** — no relevant silent empty-return pattern found.
4. FR coverage: **FAIL** — FR-019 effective configuration and FR-022 all-operation audit behavior are absent.
5. Frozen surface: **PASS** — no frozen contract/spec files were modified by `6d79fcd`.
6. Locked decision: **FAIL** — no-overwrite, exact proposal revalidation, and state-changing audit MUSTs are violated.
7. Shared-file ownership: **PASS** — the WP05 commit is isolated from approved ancestry and no task/spec lane diff is present.
8. Production fragility: **FAIL** — snapshot panic recovery emits false applied evidence; several recovery filesystem failures also escape without stable structured classification.
