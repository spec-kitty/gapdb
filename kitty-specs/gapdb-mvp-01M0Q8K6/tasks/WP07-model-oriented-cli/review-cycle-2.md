---
affected_files:
  - cmd/gapctl/command.go
  - tests/contract/cli/cli_test.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: gapctl --request-id inspect --request-id=again schema
reviewed_at: '2026-08-23T22:02:38Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP07
---

# WP07 Independent Re-review

Verdict: changes requested.

Repair commit reviewed: `404425378c7ef3a044819896a19836604bf5b311`.

## Blocking finding

1. **Global parse errors can carry an operation name taken from an option value instead of the requested command.** `detectedMachineOperation` scans every argument without skipping global option values. The independent production-binary probe `gapctl --request-id inspect --request-id=again schema` correctly exits 2 for the duplicate flag, but emits `"operation":"offline_inspect"` even though the requested command is `schema`. Likewise, `gapctl --request-id verify --request-id=again status` emits `"operation":"verify"` instead of `status`. For a model-managed machine API, the operation field is correlation authority; silently attributing a parser error to an unrequested offline/destructive operation violates the requirement that every success and error use its exact stable operation name. Extract the command with an option-aware tokenizer even when global validation fails (or make the duplicate validator preserve command position), and never classify option values as commands. Add a pre-dial matrix in which values for `--request-id`, `--db`, `--socket`, `--deadline`, and `--output` equal command spellings while a duplicate/unknown global flag triggers the error; assert the actual requested operation on every result. The test must fail if value-skipping is removed.

## Original findings independently closed

- **Recovery authority: closed.** `ProposeQuarantine` derives the exact canonical action set and refuses every stable error definition that lacks `recover_propose`; `ApplyRecovery` rechecks the proposal ID/action set, takes the owner lock, re-inspects under that lock, and compares finding code, file, database ID, generation, full SHA-256, size, device, and inode before publication. A temporary reviewer matrix enumerated every non-authorized stable code and self-consistently mutated proposal ID, database ID, generation, action, finding, action list, file, digest, size, device, and inode. Every case failed and preserved both source and destination. The UNKNOWN_FORMAT production path remained read-only.
- **Offline operation names: closed on ordinary success/error paths.** `offline_inspect`, `offline_verify`, `offline_recover_propose`, and `offline_recover_apply` are emitted by the production CLI and model-only lifecycle. The blocker above is a distinct pre-parse attribution defect.
- **Watch terminal shape: closed.** Local validation, target, dial, status, ahead, compacted, lagged, disconnect, deadline, and signal paths use complete JSONL. Pre-registration failures emit exactly one `ok:false`, `stream:"ended"`, `reason:"error"` terminal with limits, last-delivered revision, and typed error; no unary frame is emitted.
- **Strict long flags: closed.** Single-dash and over-dashed aliases are rejected, and split/equals duplicate long flags are rejected before file, stdin, or dial side effects.

## Additional verified behavior

- Recovery apply remains guarded by exact action ID, database ID, manifest generation, destination validation, owner exclusion, and fresh artifact evidence. Tamper/stale/live-owner/relative-destination cases caused zero database-tree mutation.
- Watch ahead, compaction, lag, disconnect, deadline, and SIGINT retain one terminal frame, stable nonzero exit class, and safe resume evidence. Broken-pipe behavior remains bounded and non-retrying.
- The daemon readiness/signal ordering change installs signal handling before publishing readiness; existing real-process clean shutdown, restart, recovery, and ownership tests pass without durability regression. Startup still does not publish readiness before recovery and listener authority are complete.
- No prompt, TTY detection, color, pager, implicit stdin, retry, or prose-dependent model step was introduced. The model-only lifecycle succeeds using stdout, exit classes, and safe actions.
- The repair necessarily updates the previously approved WP05 recovery seam (`internal/admin`) to close destructive-authority findings and the WP06 daemon root to remove the readiness/signal race; this cross-WP ownership is explicit here. Approved WP06 commit `29c9136` remains an ancestor and its authority tests pass.

## Independent gates

- Toolchain: `go1.26.7 linux/amd64`.
- Focused repair/CLI tests passed x10 normally and x3 under `-race`.
- Full uncached `go test ./... -count=1` and `go test -race ./... -count=1` passed.
- `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (`No vulnerabilities found`), `go mod verify`, `go mod tidy` with no diff, diff-scoped `gofmt`, and `git diff --check` passed.
- Fuzz passed for 5 seconds each: `FuzzDecodeSnapshotNeverPanics` (439,989 executions) and `FuzzStorageDecoders` (26,412 executions).
- Temporary reviewer tests were removed. The lane has no reviewer implementation changes; `.spec-kitty/` is the runtime-owned untracked invocation directory.

## Anti-pattern checklist

1. Dead code: **PASS** — new helpers and modules have production callers.
2. Synthetic-fixture test: **PASS** — repair tests invoke real CLI binaries, daemon/client, and offline admin paths.
3. Silent empty return: **PASS** — no relevant silent empty-return path exists.
4. FR coverage: **FAIL** — FR-018's exact stable machine envelope is not covered for global parse errors whose option values equal command names.
5. Frozen surface: **PASS** — no frozen mission artifact changed in the lane.
6. Locked decision: **FAIL** — the locked exact operation field can name an unrequested operation.
7. Shared-file ownership: **PASS** — necessary WP05/WP06 seam changes and their rationale/regression evidence are explicitly recorded above.
8. Production fragility: **FAIL** — model correlation depends on incidental option values during parse failure.
