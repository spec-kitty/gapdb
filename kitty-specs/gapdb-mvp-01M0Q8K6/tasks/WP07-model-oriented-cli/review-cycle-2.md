---
affected_files:
  - cmd/gapctl/command.go
  - cmd/gapctl/output_test.go
  - tests/contract/cli/cli_test.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go run ./cmd/gapctl -request-id inspect schema
reviewed_at: '2026-08-23T22:16:36Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP07
---

# WP07 Independent Final Re-review

Verdict: changes requested.

Repair commit reviewed: `ba076146f44d0e783290b7e66fa9e5fe1ad35a26`.

## Blocking finding

1. **Malformed known global options in split form still let their value impersonate the command.** `lexEarlyCommand` understands only the exact `--name` spelling as a value-taking global. For a rejected single-dash or over-dashed spelling it skips the option token but does not consume the following split value, then classifies that value as the command. The real binary proves the mismatch:

   - `gapctl -request-id inspect schema` exits 2 but emits `"operation":"offline_inspect"` instead of `schema`.
   - `gapctl ---request-id inspect schema` has the same result.
   - `gapctl -output watch schema` and `gapctl ---output watch schema` emit terminal watch JSONL even though the requested command is unary `schema`.
   - The same construction affects split `socket`, `db`, and other global options.

   This leaves both the exact operation and unary-versus-watch machine shape dependent on an invalid option's value. Recognize malformed spellings of every known global solely for early attribution, consume their required split value, but continue rejecting the spelling as `INVALID_REQUEST`; do not let malformed syntax alter target authority or become accepted. Cover all five globals in single-dash and over-dashed split and equals forms, including command-like values, missing values, `--`, command-before-flags, nested recover, and watch. Assert exact operation, exactly one unary object or one ended/error JSONL frame as appropriate, schema/limits, and zero stdin/dial side effects. Deleting malformed-known-option consumption must fail the test.

## Prior findings remain closed

- Exact duplicate-value attribution for valid `--name value` and `--name=value` globals is fixed across every top-level, nested recovery, offline, and watch command.
- Recovery proposal/apply remains limited to canonical `recover_propose` authority and revalidates action ID, finding/action set, database ID, generation, artifact path, full digest, size, device, inode, destination, and owner lock. UNKNOWN_FORMAT and all other unauthorized findings remain read-only.
- Ordinary offline success/error paths retain `offline_inspect`, `offline_verify`, `offline_recover_propose`, and `offline_recover_apply`.
- Watch validation, dial/status, ahead, compacted, lagged, disconnect, deadline, and SIGINT paths retain exactly one complete terminal JSONL frame, typed error, stable nonzero exit, and resume evidence.
- Strict exact long-flag rejection itself remains closed; the defect is only early error attribution after that rejection.
- Daemon readiness/signal ordering, recovery-before-readiness, ownership, clean shutdown/restart, and model-only lifecycle regressions pass.

## Independent evidence and gates

- Toolchain: `go1.26.7 linux/amd64`.
- A temporary deletion-sensitive test covering split `request-id`, `output`, `db`, and `socket` malformed spellings failed immediately on the first case with `offline_inspect`, then was removed.
- Focused lexer, CLI, recovery, watch, and model-operations suites passed x10 normally and x3 under `-race`; the existing matrix does not contain the failing split malformed cases.
- Full uncached `go test ./... -count=1` and `go test -race ./... -count=1` passed.
- `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (`No vulnerabilities found`), `go mod verify`, `go mod tidy` with no diff, diff-scoped `gofmt`, and `git diff --check` passed.
- Fuzz passed for 5 seconds each: `FuzzDecodeSnapshotNeverPanics` (482,397 executions) and `FuzzStorageDecoders` (28,806 executions).
- Temporary probes were removed; the lane has no reviewer implementation changes. `.spec-kitty/` is runtime-owned untracked state.

## Anti-pattern checklist

1. Dead code: **PASS** — the new lexer has a production caller.
2. Synthetic-fixture test: **PASS** — tests call `execute` or real binaries and daemon/admin paths.
3. Silent empty return: **PASS** — no relevant silent empty-return path exists.
4. FR coverage: **FAIL** — FR-018 lacks malformed split-form attribution and shape coverage.
5. Frozen surface: **PASS** — no frozen mission artifact changed in the lane.
6. Locked decision: **FAIL** — stable operation/stream evidence can name an unrequested operation.
7. Shared-file ownership: **PASS** — earlier necessary WP05/WP06 seam changes and rationale remain documented; this repair is WP07-owned.
8. Production fragility: **FAIL** — model correlation and parser mode depend on a malformed flag value.
