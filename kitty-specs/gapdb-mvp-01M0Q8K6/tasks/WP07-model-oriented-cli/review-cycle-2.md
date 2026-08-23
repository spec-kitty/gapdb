---
affected_files:
  - cmd/gapctl/command.go
  - cmd/gapctl/output_test.go
  - tests/contract/cli/cli_test.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./cmd/gapctl -run '^TestReviewerContractValidControlRequestIDsEchoOnEarlyErrors$' -count=1
reviewed_at: '2026-08-23T22:38:44Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP07
---

# WP07 Independent Re-review

Verdict: changes requested.

Repair commit reviewed: `b2ba0ff27a3bad125178e12211e60c5b128e1abb`.

## Blocking finding

1. **The repair invents a control-character restriction that contradicts the locked request-ID contract.** Protocol v1 defines `request_id` as UTF-8 and at most 256 bytes; `internal/protocol` Encode/Decode request and response validation enforces the byte bound and does not reject Unicode control code points. JSON safely escapes those characters. `echoableRequestID`, however, rejects every `unicode.IsControl`, so an otherwise valid single exact request ID is omitted only on early-error paths. A temporary direct parity probe used newline, tab, DEL, and U+0085 IDs for both unary and watch errors and failed immediately: `line\nbreak` was absent from the unary envelope. Normal successful CLI/protocol paths echo the same valid IDs, producing inconsistent FR-024 behavior. The new committed tests currently encode the incorrect tightening by expecting newline and DEL IDs to be omitted.

   Make early echo eligibility match the canonical contract exactly: one unambiguous exact ID, valid UTF-8, at most 256 bytes. Remove the undocumented control-category rejection and invert the control tests to require exact unary/watch echo; keep JSON line completeness assertions so escaped controls cannot create physical extra lines. Invalid UTF-8, 257-byte values, duplicates, malformed spellings, missing values, and exact-plus-malformed ambiguity must remain omitted. The control test must fail if contract-valid controls are filtered again.

## Requested repair independently closed otherwise

- Split/equals request IDs have unary/watch parity. Exact valid IDs at 1 and 256 bytes echo; 257-byte and invalid-UTF-8 values are omitted and bounded.
- Duplicate, malformed, missing, and exact-plus-malformed ambiguous request IDs do not echo. Command-like values remain correlation data and cannot impersonate operations.
- Exact operation, unary-versus-terminal JSONL shape, schema, limits, and one physical JSON line remain stable. Direct tests use panic-on-read stdin; real Unix listener probes prove no early-error dial.
- Deletion-sensitive prior lexer tests still fail when malformed-known consumption is removed.

## Earlier WP07 findings remain closed

- Destructive recovery authority stays limited to canonical `recover_propose` findings and apply revalidates complete proposal/artifact/owner/destination evidence under lock. UNKNOWN_FORMAT and all other unauthorized findings remain read-only.
- Offline names and early command attribution remain exact across all global options, one through eight dashes, split/equals forms, top-level/offline/nested recovery/watch commands, missing values, and `--`.
- Watch validation, dial/status, ahead, compacted, lagged, disconnect, deadline, and SIGINT paths retain one complete terminal frame, typed error, nonzero exit, and resume evidence.
- Strict flags, daemon readiness/signal lifecycle, model-only operation, bounded input, stable exit classes, and no retry/prompt/TTY behavior remain intact.

## Independent gates

- Toolchain: `go1.26.7 linux/amd64`.
- Focused request-correlation, lexer, CLI, recovery, watch, and modelops suites passed x10 normally and x3 under `-race`; the committed control expectations are contract-inverted.
- Full uncached `go test ./... -count=1` and `go test -race ./... -count=1` passed.
- `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (`No vulnerabilities found`), `go mod verify`, `go mod tidy` with no diff, diff-scoped `gofmt`, and `git diff --check` passed.
- Fuzz passed for 5 seconds each: `FuzzDecodeSnapshotNeverPanics` (512,837 executions) and `FuzzStorageDecoders` (113,818 executions).
- Temporary reviewer tests were removed; the lane has no reviewer implementation changes. `.spec-kitty/` is runtime-owned untracked state.

## Anti-pattern checklist

1. Dead code: **PASS** — request eligibility helpers have production callers.
2. Synthetic-fixture test: **PASS** — tests exercise `execute`, real binaries/listeners, daemon/client, and offline admin seams.
3. Silent empty return: **PASS** — no relevant silent empty-return path exists.
4. FR coverage: **FAIL** — FR-024 tests assert behavior contrary to the canonical UTF-8/256-byte contract.
5. Frozen surface: **PASS** — no frozen mission artifact changed in the lane.
6. Locked decision: **FAIL** — the CLI narrows a locked protocol field without an approved contract change.
7. Shared-file ownership: **PASS** — earlier necessary WP05/WP06 seam changes remain documented; this repair is WP07-owned.
8. Production fragility: **FAIL** — identical valid correlation IDs are mode/error-path dependent.
