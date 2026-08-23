---
affected_files:
  - cmd/gapctl/command.go
  - cmd/gapctl/data.go
  - cmd/gapctl/offline.go
  - internal/admin/recovery.go
  - tests/contract/cli/cli_test.go
  - tests/contract/modelops/lifecycle_test.go
  - docs/operations/configuration.md
  - docs/operations/recovery.md
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./tests/contract/cli -run '^TestReviewerUnknownFormatCannotGrantOrApplyQuarantine$' -count=1
reviewed_at: '2026-08-23T21:41:10Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP07
---

# WP07 Independent Review

Verdict: changes requested.

## Blocking findings

1. **Unsupported storage formats are incorrectly converted into destructive recovery authority.** A production-path probe changed only the active snapshot format version. Offline inspection correctly returned `UNKNOWN_FORMAT` with canonical safe actions `upgrade_gapdb`, `use_compatible_binary`, and `abort`. Nevertheless, `recover propose` exited 0 and emitted a quarantine proposal. Feeding that exact proposal to `recover apply` also exited 0, returned `operation_applied:true`, and moved the active snapshot to quarantine. `runOfflineCommand` proposes after every inspection error with complete evidence, and neither proposal creation nor apply rejects a finding whose stable safe actions do not authorize recovery. Fail closed in both propose and apply: only explicitly recoverable finding codes/actions may grant quarantine authority, and stale/self-consistent unsupported-format proposals must remain unusable. Add UNKNOWN_FORMAT, DATABASE_ID_MISMATCH, IO_ERROR, and other non-`recover_propose` findings to a zero-mutation matrix; delete either gate and the tests must fail.

2. **Offline envelopes violate the locked protocol operation names.** Protocol v1 requires offline CLI operations to use names prefixed by `offline_`, but the implementation emits CLI spellings: `inspect`, `verify`, `recover-propose`, and `recover-apply`. For example, `gapctl --db /definitely/not/a/gapdb inspect` returns an `IO_ERROR` envelope with `"operation":"inspect"`. Emit exact stable names such as `offline_inspect`, `offline_verify`, `offline_recover_propose`, and `offline_recover_apply` on every success and error path; update the model-only assertions and runbooks to treat these as the machine API.

3. **Pre-registration watch failures are not terminal JSONL watch frames.** Invalid watch flags, missing/invalid targets, dial failures, and status failures call the unary `writeError` path. For example, invalid `--after-revision` and an unavailable socket each produce one generic object with no `stream`, `reason`, `database_id`, or `last_delivered_revision`. The contracted watch surface permits only flushed complete JSONL `started`, `event`, and one `ended` frame. Route every watch failure after JSONL selection through the terminal frame encoder with `stream:"ended"`, `reason:"error"`, the requested resume boundary, and the stable typed error; define the unavailable database-ID representation explicitly. Add local validation, target, dial, status, ahead, compacted, lagged, disconnect, deadline, and signal cases, asserting exactly one terminal and no trailing/partial line.

4. **Duplicate option rejection is bypassed by the standard parser's accepted single-dash spelling.** `rejectDuplicateFlags` examines only tokens beginning `--`, while Go's `flag` package accepts `-output`, `-socket`, `-key`, and every other declared option. `gapctl -output=json -output=json schema` exits 0, and mutation flags can therefore be repeated with last-value-wins behavior. This is ambiguous machine input and contradicts strict duplicate parsing. Either reject all unadvertised single-dash forms or include every accepted spelling in one duplicate detector before any file read or dial. Add global and command-level duplicate matrices for split and `=value` forms.

## Verified behavior

- The complete stable 41-code exit table maps exactly to exits 2–6.
- Canonical base64, UTC expiry, batch unknown/duplicate fields, duplicate batch keys, bounded value/batch reads, explicit stdin selection, guarded online admin flags, absolute destinations, request IDs, revisions, acknowledgement mode, and durable-through evidence are implemented through production seams.
- Existing scan/watch lag, ahead, disconnect, signal, custom daemon, live-owner, tampered proposal, relative destination, guarded admin, and model-lifecycle suites pass. These do not cover the blockers above; the model harness currently asserts the noncanonical offline names.
- The implementation adds no CLI framework dependency, prompt, TTY detection, color, pager, or implicit stdin read.

## Independent gates

- Toolchain: `go1.26.7 linux/amd64`.
- Focused `cmd/gapctl`, CLI contract, and model-operations tests passed x10 normally and under `-race`.
- Full uncached `go test -count=1 ./...` and `go test -race -count=1 ./...` passed.
- `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (`No vulnerabilities found`), `go mod verify`, `go mod tidy` with no diff, full `gofmt`, and `git diff --check` passed.
- Fuzz passed: `FuzzDecodeSnapshotNeverPanics` for 5 seconds (506,843 executions) and `FuzzStorageDecoders` for 5 seconds (24,486 executions).
- Temporary reviewer probes were removed; lane-g has no review code change.

## Anti-pattern checklist

1. Dead code: **PASS** — all new production CLI modules are called from the binary.
2. Synthetic-fixture test: **PASS** — existing CLI tests invoke real binaries and daemon/admin paths, though required negative cases are missing.
3. Silent empty return: **PASS** — no relevant silent empty-return path exists.
4. FR coverage: **FAIL** — FR-018 and FR-022 are incomplete because watch/offline machine shapes drift and unsupported formats can grant destructive authority.
5. Frozen surface: **PASS** — no frozen mission artifact was modified.
6. Locked decision: **FAIL** — `UNKNOWN_FORMAT` is guessed into a destructive quarantine workflow contrary to GAP-001/GAP-003 and its stable safe actions.
7. Shared-file ownership: **PASS** — implementation changes are within WP07-owned CLI/docs/tests surfaces; any shared recovery-seam hardening must carry the required rationale.
8. Production fragility: **FAIL** — ambiguous duplicate flags and noncanonical output shapes make model decisions depend on undocumented parser behavior.
