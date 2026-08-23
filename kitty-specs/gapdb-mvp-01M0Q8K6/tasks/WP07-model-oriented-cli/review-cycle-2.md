---
affected_files:
  - cmd/gapctl/command.go
  - cmd/gapctl/output_test.go
  - tests/contract/cli/cli_test.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go run ./cmd/gapctl --request-id correlation-17 --unknown schema
reviewed_at: '2026-08-23T22:27:52Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP07
---

# WP07 Independent Re-review

Verdict: changes requested.

Repair commit reviewed: `3f4182f6cdeb1b7e459b29b94d2dce21ab30c41a`.

## Blocking finding

1. **Early parse failures violate the caller request-ID echo and length contract.** `lexEarlyCommand` now recovers exact global options, but `execute` discards `early.options.requestID` on unary failures by passing an empty string to `writeError`, while watch failures pass the recovered value to `emitWatchError` without validating it. The real binary demonstrates both sides:

   - `gapctl --request-id correlation-17 --unknown schema` exits 2 with exact operation/schema/limits but omits `request_id`, contrary to FR-024's requirement to accept and echo a caller-supplied correlation ID.
   - The identical command ending in `watch` echoes `"request_id":"correlation-17"`, so correlation behavior changes solely with unary-versus-stream routing.
   - A watch command with one exact 257-byte request ID emits that entire rejected value in the terminal frame while reporting `request ID exceeds 256 bytes`. This violates the protocol's at-most-256-byte request-ID field and makes invalid input escape into contracted output.

   Preserve one unambiguous, syntactically valid exact `--request-id` on both unary and watch early errors. Omit malformed, missing, over-limit, and duplicate/ambiguous request IDs rather than selecting a rejected value. Add direct and real-binary matrices for valid split/equals IDs combined with unknown/malformed flags, over-limit IDs, and duplicate split/equals IDs; assert unary/watch parity, the 256-byte bound, exact operation/shape, and no stdin/dial. The valid-ID test must fail if the unary forwarding is deleted, and the invalid-ID tests must fail if unvalidated early options are forwarded.

## Requested repair independently closed

- All five known globals passed a temporary exhaustive matrix for one through eight leading dashes, split and equals forms, every top-level/offline command-like value, missing values, `--`, command-before-flags, nested recover, and watch.
- Malformed known flags remain `INVALID_REQUEST`, consume their split value only for attribution, never populate socket/database/deadline/request-ID/output options, and cannot establish offline authority or change stream shape.
- Genuinely unknown single/over-dashed flags do not consume arbitrary following tokens. The first non-option token remains the attributed command.
- Direct tests use a panic-on-read stdin, and the real Unix listener probe proves malformed early errors do not dial.
- Deleting malformed-known recognition made `TestMalformedKnownGlobalsConsumeSplitValuesForAttributionOnly` fail immediately; the repair test is behaviorally reachable.

## Prior findings remain closed

- Destructive recovery authority is limited to canonical `recover_propose` findings and apply revalidates the complete proposal/artifact/owner/destination evidence under lock. UNKNOWN_FORMAT and all other unauthorized findings remain read-only.
- Offline names remain exact on normal success/error paths, and early attribution now distinguishes all unary, offline, nested recovery, and watch commands correctly.
- Watch validation, dial/status, ahead, compacted, lagged, disconnect, deadline, and SIGINT paths retain one complete terminal JSONL frame, typed error, nonzero exit, and safe resume evidence.
- Strict long-flag rejection, daemon recovery-before-readiness and signal shutdown, model-only lifecycle, bounded input, stable exit classes, and no implicit retry/prompt/TTY behavior remain intact.

## Independent gates

- Toolchain: `go1.26.7 linux/amd64`.
- Focused lexer, CLI, recovery, watch, daemon-facing, and modelops suites passed x10 normally and x3 under `-race`.
- Full uncached `go test ./... -count=1` and `go test -race ./... -count=1` passed.
- `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (`No vulnerabilities found`), `go mod verify`, `go mod tidy` with no diff, diff-scoped `gofmt`, and `git diff --check` passed.
- Fuzz passed for 5 seconds each: `FuzzDecodeSnapshotNeverPanics` (471,392 executions) and `FuzzStorageDecoders` (44,333 executions).
- Temporary reviewer tests were removed; the lane has no reviewer implementation changes. `.spec-kitty/` is runtime-owned untracked state.

## Anti-pattern checklist

1. Dead code: **PASS** — the attribution helper is called from production.
2. Synthetic-fixture test: **PASS** — tests exercise `execute`, real binaries/listeners, daemon/client, and offline admin seams.
3. Silent empty return: **PASS** — no relevant silent empty-return path exists.
4. FR coverage: **FAIL** — FR-024 lacks early unary echo and invalid/ambiguous request-ID suppression coverage.
5. Frozen surface: **PASS** — no frozen mission artifact changed in the lane.
6. Locked decision: **FAIL** — stable correlation evidence is omitted when valid and exceeds its contract bound when invalid.
7. Shared-file ownership: **PASS** — earlier necessary WP05/WP06 seam changes remain explicitly documented; this repair is WP07-owned.
8. Production fragility: **FAIL** — request correlation depends on output mode and rejected values can escape into output.
