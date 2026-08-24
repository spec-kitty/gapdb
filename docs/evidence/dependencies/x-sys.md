# Dependency review: `golang.org/x/sys`

- Review date: 2026-08-23
- Reviewer scope: Gapdb MVP single-owner process locking on Linux
- Decision: approved, narrowly, for `golang.org/x/sys/unix.Flock` and its lock
  constants only

## Identity and provenance

- Module: `golang.org/x/sys`
- Version: `v0.47.0`, published 2026-06-30
- Upstream: <https://go.googlesource.com/sys>
- Read-only mirror and project metadata: <https://github.com/golang/sys>
- Resolved tag commit: `9e7e939dcafac07e8ab4cffa6e5fc74908413f00`
- Go module zip checksum: `h1:o7XGOvZQCADBQQ4Y7VNq2dRWQR7JmOUW8Kxx4ZsNgWs=`
- Go module-file checksum: `h1:4GL1E5IUh+htKOUEOaiffhrAeqysfVGipDYzABqnCmw=`
- Downloaded zip size: 2,022,425 bytes
- License: BSD 3-Clause
- License-file SHA-256:
  `911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad`

Checksums were resolved through `proxy.golang.org` and authenticated through
`sum.golang.org` by the Go command. The upstream tag was independently resolved
with `git ls-remote` against `go.googlesource.com/sys`.

## Intended use and boundary

Gapdb will call `unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)` to acquire the
database-directory ownership lock and `unix.Flock(fd, unix.LOCK_UN)` when
releasing it. No other `x/sys` API is approved by this review. The dependency is
not used by WP01 production code; the pin is established now for the later
ownership package.

Kernel-maintained advisory locking is released when the owning file description
is closed, including process exit. That gives Gapdb a narrow, crash-releasing
single-owner primitive without cgo or liveness guesses.

## Vulnerability query

Queried the official Go vulnerability database at <https://vuln.go.dev> on
2026-08-23. The local `govulncheck` database reported an update timestamp of
2026-08-21 20:38 UTC. The module index listed two historical entries:

- `GO-2022-0493`, an `unix.Faccessat` issue fixed before this tagged release;
- `GO-2026-5024`, a Windows `NewNTUnicodeString` issue fixed in `v0.44.0`.

Version `v0.47.0` is after both fixed versions. Neither affected symbol is in
the approved usage. No known, unmitigated vulnerability applies to the selected
version and symbol. The repository-wide source scan remains a release gate once
the ownership code imports the module.

Reproduction:

```sh
go list -m -json golang.org/x/sys@latest
go mod download -json golang.org/x/sys@v0.47.0
govulncheck -version
curl -fsSL https://vuln.go.dev/index/modules.json \
  | jq '.[] | select(.path == "golang.org/x/sys")'
curl -fsSL https://vuln.go.dev/ID/GO-2022-0493.json
curl -fsSL https://vuln.go.dev/ID/GO-2026-5024.json
```

## Alternatives considered

- `syscall.Flock`: standard-library-only, but package `syscall` is frozen and
  directs new low-level OS work to `golang.org/x/sys`.
- A PID or sentinel lock file: dependency-free, but cannot safely distinguish a
  stale owner from PID reuse or an inaccessible live process.
- Unix socket existence: unsuitable as authority because stale sockets survive
  crashes and socket removal races with ownership.
- cgo calls to `flock(2)`: adds a compiler/toolchain/runtime boundary and more
  platform risk for one syscall.

The small, Go-team-maintained wrapper removes more corruption and stale-owner
risk than its pinned, checksummed module adds.

## Linux directory-authority extension

WP06 extends the reviewed Linux syscall boundary to descriptor-relative owner
and socket readiness operations: `Open`, `Fstat`, `Fstatat`, `Fchmod`,
`Fchmodat`, `Unlinkat`, and `Close`. The owner retains database-directory and
parent descriptors, and socket publication retains equivalent descriptors for
the configured socket parent. Stale unlink, mode enforcement, identity checks,
bind verification, and cleanup therefore cannot be redirected to a replacement
directory by renaming the configured pathname.

Socket bind uses `/proc/self/fd/<directory-fd>/<socket-name>`, followed by
descriptor-relative inode and mode verification. The Gapdb MVP server runtime
therefore requires Linux with procfs mounted at `/proc`. If that capability or
the required `*at` syscall semantics are unavailable, startup fails closed; it
does not fall back to check-then-use pathname operations. Supporting another
platform requires a separately reviewed equivalent directory-capability
implementation.

## Go toolchain security finding

The first full source scan ran with the planning reference toolchain, Go
1.26.4. It found no reachable vulnerable symbols in Gapdb or `x/sys`, but it
reported current standard-library advisories fixed across Go 1.26.5 and 1.26.6,
including an imported-package advisory in `os`. The project therefore raised
the toolchain floor to Go 1.26.7, the current supported Go 1.26 patch on the
review date. `go.mod` records `toolchain go1.26.7`; the full test, race, vet,
static-analysis, and vulnerability gates are rerun with that resolved toolchain.

This correction does not change the `go 1.26` language/module compatibility
contract or the `x/sys` decision. It prevents a new datastore build from
standardizing on a toolchain with already-published fixes.
