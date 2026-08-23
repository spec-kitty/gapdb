---
affected_files: []
cycle_number: 2
mission_slug: gapdb-mvp-01M0Q8K6
reproduction_command:
reviewed_at: '2026-08-23T15:15:07Z'
reviewer_agent: unknown
verdict: rejected
wp_id: WP02
---

# WP02 Review Cycle 1

## Verdict

Rejected. The byte layouts, checksum coverage, short-write loop, durable barrier,
revision reservation, ownership lock, bounded normal decoder path, atomic install
order, complete-frame corruption checks, and independent golden encoders are
sound, but three recovery/error paths violate the fail-closed storage contract.

## Blocking findings

### 1. Non-EOF WAL read failures are treated as automatically repairable torn tails

**Severity:** Release blocker  
**Affected code:** `internal/persist/recovery.go`, `ScanWAL`

After `io.ReadFull` reads the next frame prefix, every result shorter than the
fixed frame header is classified as `TAIL_TRUNCATED`. The branch does not first
require `readErr` to be `io.EOF` or `io.ErrUnexpectedEOF`. A device/read failure
at a frame boundary or after a partial prefix therefore returns successful tail
recovery; `RecoverWALFile` can then truncate and sync the file.

Independent reproduction used a valid WAL header followed by
`iotest.ErrReader(errors.New("device read failure"))`. `ScanWAL` returned no error
and proposed truncation from 65 to 64 bytes. Only physical EOF may authorize the
one automatic repair. Propagate non-EOF read failures as a structured storage
error without mutation, and add regression cases for both zero-byte and partial
prefix reads ending in a non-EOF error.

### 2. Recovery truncates a WAL before validating its generation against `CURRENT`

**Severity:** Release blocker  
**Affected code:** `internal/persist/recovery.go`, `RecoverDatabase` and
`RecoverWALFile`

`RecoverDatabase` calls the mutating `RecoverWALFile` before comparing the WAL
header generation with the manifest generation. A WAL with a complete, valid
generation-2 header, a generation-1 `CURRENT`, and one trailing byte was truncated
from 65 to 64 bytes before recovery returned `CORRUPT_WAL`. No tail audit ran.

This mutates an artifact that has not passed the sole-authority relationship
check and contradicts the required order “validate WAL header, then replay/tail
handling.” Validate all manifest/WAL header relationships before any truncate,
sync, audit, reservation, or other mutation. Add a regression that asserts the
file remains byte-identical for generation mismatch combined with an otherwise
truncatable tail.

### 3. Post-rename identity and manifest failures omit `operation_applied`

**Severity:** High, approval blocking  
**Affected code:** `internal/persist/identity.go`, `installIdentity`;
`internal/persist/manifest.go`, `InstallManifest`

Errors returned by the rename after-hook and by directory sync use `ioFailure`,
which leaves `operation_applied` false. At those stages `IDENTITY` or `CURRENT`
has already been renamed over the authoritative file, so the operation may have
applied even though its durability is uncertain. Independent faults immediately
after each rename confirmed that the new file was authoritative while the
returned `IO_ERROR` reported `OperationApplied: false`.

The v1 error contract requires `operation_applied: true` whenever a later failure
can follow an applied operation. Make error construction stage-aware for rename
after-hooks and directory-sync failures, and cover identity and manifest at the
rename-after plus directory-sync before/after boundaries.

## Verification evidence

- Go toolchain: `go1.26.7 linux/amd64`; module pins `go 1.26` and
  `toolchain go1.26.7`.
- Passed: `go mod verify`, `gofmt`, `git diff --check`, `go vet ./...`,
  `staticcheck ./...`, `govulncheck ./...` (no vulnerabilities),
  `go test -count=1 ./...`, and `go test -race -count=1 ./...`.
- A two-second storage decoder fuzz run completed about 30,500 executions with
  no crash.
- Deletion probes proved the committed tests reach production CRC rejection,
  revision-reservation bounds, and WAL file-sync barriers: deleting each guard
  made its targeted test fail. All temporary production and probe changes were
  restored; the lane has no tracked review diff.
- Approved WP01 commits were distinguished from WP02 commit `210ef9b`; WP02 has
  no net `kitty-specs` change and no out-of-scope implementation files.

## WP anti-pattern checklist

1. **Dead code:** N/A — this is an explicitly staged persistence foundation for
   dependent WPs; its internal APIs are exercised through production composition
   paths inside `internal/persist` and compatibility decoders.
2. **Synthetic-fixture test:** PASS — independent field-by-field golden encoders
   feed production decoders; deletion probes demonstrate production reachability.
3. **Silent empty return:** PASS — no fail-open empty return pattern found.
4. **FR coverage:** FAIL — the three authority/error branches above lack contract
   assertions and currently violate FR-011/FR-021/FR-022.
5. **Frozen surface:** PASS — WP02 commit `210ef9b` does not modify mission
   planning or contract artifacts.
6. **Locked decision:** FAIL — findings 1 and 2 contradict C-010 and the
   fail-closed recovery clauses; finding 3 contradicts stable evidence rules.
7. **Shared-file ownership:** N/A — WP02 owns lane B alone; apparent WP01 files
   are approved dependency ancestry.
8. **Production fragility:** N/A — no panic/raise-style transient failure path
   was introduced.
