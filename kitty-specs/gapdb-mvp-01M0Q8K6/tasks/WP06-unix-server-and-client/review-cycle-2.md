---
affected_files:
  - internal/server/owner.go
  - internal/server/handler.go
  - gapdb/client.go
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command: go test ./gapdb ./internal/server -run '^TestReviewer' -count=1
reviewed_at: '2026-08-23T20:05:00Z'
reviewer_agent: reviewer-renata
verdict: rejected
wp_id: WP06
---

# WP06 Cycle 1 Review

Verdict: changes requested.

## Blocking findings

1. **An existing `LOCK` file is not forced to effective mode `0600`.** `AcquireOwner` opens the path with mode `0600`, but that argument does not change an existing inode's permissions, and server startup never calls `Chmod` or verifies the lock mode. An independent production-path probe pre-created `LOCK` as `0666`, called `server.Open`, and observed it still at `0666` while the owner was ready. Enforce and verify owner-only mode on the exact opened lock inode before publishing readiness; reject safely if that cannot be done. Add existing-file modes and identity-swap coverage, not only fresh creation.

2. **The public client fails open on noncanonical server responses.** `readResponse` uses ordinary `encoding/json` for the outer envelope and every typed `Result`; it therefore accepts duplicate fields, invalid-UTF-8 replacement, unknown nested result fields, and operation-inappropriate presence. Record/event base64, UTC/time, null/omitted, enum, ordering, cursor, acknowledgement, and code-specific error-evidence validation are materially weaker than the canonical `internal/protocol.DecodeResponse`. The independent fixture returned a `get` result containing an unknown nested field and the client accepted it as a successful record. Implement one deletion-sensitive client-side v1 decoder with the same strict schema, EOF, duplicate-key, UTF-8, canonical padded-base64, canonical UTC, presence, result/event/watch, and all 41 error-evidence rules. Adversarially compare acceptance/rejection against the canonical codec for every response shape.

3. **`Client.Close` can race watch registration and return a live watch after the client is closed.** A fixture delayed the `stream:"started"` frame until after `Close`; because `Watch` stores its cancel function only after that frame, `Close` observed no watch, returned, and `Watch` then registered and returned a usable stream. Make closed-state transition and watch admission atomic, close the in-flight connection on rejection, and guarantee no watch can become live after `Close` returns. Cover the race repeatedly under `-race`, including Close before dial, after write, before started, and during terminal delivery.

4. **Online administrative response shapes do not implement the locked protocol.** The production `create_snapshot` response was `{"Revision":1,"RecordCount":1,"Duration":...}`. It omits the required lowercase `revision`, `filename`, `checksum`, and `new_wal_start` fields. The handler passes internal engine/admin structs directly to a generic response path, and `DecodeResponse` does not schema-check admin results. `status`, `stats`, `describe_config`, `compact`, and `backup` are likewise thinner than their documented owner/counter/source/ceiling/directory-sync/checksum/byte-count/verification contracts. Define explicit wire/result DTOs for every inspection and online-admin operation, strictly validate them in both codecs, preserve post-apply evidence, and add raw-socket plus public-client contract tables for every field.

## Independent evidence

- Go toolchain: `go1.26.7 linux/amd64`.
- Passed uncached: `go test -count=1 ./...` and `go test -race -count=1 ./...`.
- Passed focused: `go test ./internal/server -count=10`, `go test -race ./internal/server -count=10`, `go test ./gapdb -count=10`, `go test -race ./gapdb -count=10`, and real-process API tests normally and under `-race`.
- Passed: `go vet ./...`, `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities), `go mod verify`, `go mod tidy -diff`, full `gofmt`, and `git diff --check 9a70177..HEAD`.
- Passed fuzz: `FuzzDecodeSnapshotNeverPanics` for 5 seconds (546,869 executions) and `FuzzStorageDecoders` for 5 seconds (33,925 executions).
- Four temporary adversarial tests exercised the production server/client paths, failed as described, and were removed after execution.

## Anti-pattern checklist

1. Dead code: **PASS** — new client/server/daemon entrypoints have production callers.
2. Synthetic-fixture test: **FAIL** — existing client fixtures do not exercise canonical nested response validation, and admin tests assert only a subset of the documented wire results.
3. Silent empty return: **PASS** — no relevant silent empty-return pattern found.
4. FR coverage: **FAIL** — FR-016/017/019/020/023/024 are incomplete at lock permissions, client protocol validation, admin results, and watch Close lifecycle.
5. Frozen surface: **PASS** — `9838355` does not modify the approved protocol/error/spec artifacts.
6. Locked decision: **FAIL** — owner-only lock permissions and strict canonical protocol decoding are explicit locked decisions.
7. Shared-file ownership: **PASS** — WP06 changes are isolated to its owned integration surfaces atop approved ancestry.
8. Production fragility: **FAIL** — a Close/registration race leaks a live stream and malformed server data is accepted as authoritative success.
