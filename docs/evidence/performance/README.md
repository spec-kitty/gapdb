# Reference performance and adoption evidence

[`results.json`](results.json) is the deterministic machine summary and
[`raw.txt`](raw.txt) preserves the exact Go test/benchmark output. Both bind to
Gapdb code-under-test commit `3a98149c005031a77a85c42c04caae7073b80dad` and configuration
SHA-256 `8651e5d4d9d1d3ae8804045cda5d8fda97f3d51523a82a23e4c9dfe923166fc4`.
The code-under-test anchor is the first repair commit containing the benchmark
and portable-adapter execution paths. Evidence, semantic validators, and their
compiled pins are committed afterward, so no evidence file needs to name or
authenticate the commit that contains itself.

The reference run created exactly 100,000 live 1,024-byte values with seed
`5134473304279631186`, used eight concurrent socket readers and one socket
writer, and measured fixed windows after setup. The server used the production
production OS filesystem implementation, an effective `0600` Unix socket, and real durable
barriers. Each durable response covered its revision; the harness then required
`durable_through_revision == current_revision`, full public verification, clean
close/reopen, and exact recovered reads. No fake filesystem, disabled sync,
direct engine call, or in-process data API is accepted.

Every measured window uses fixed budgets and a ready/active/start barrier for
all eight readers and the writer. The recorded snapshot revision is therefore
exactly 1,329. A pass-through `faultfs.OS` observer delegates to real operating
system sync calls and recorded 156 successful WAL-sync after-events for 151
successful durable operations; an injected sync failure cannot increment either
success authority. Substituted no-sync filesystems and non-socket transports are
deletion-sensitively rejected.

After the latency windows, the harness installed a verified snapshot and made
exactly 10,000 later WAL commits while retaining 100,000 live records. Readiness
includes production recovery, listener creation, public client dial, status, and
a recovered read.

## Reproduction

Reference acceptance:

```sh
GAPDB_REFERENCE_ACCEPTANCE=1 go test ./tests/performance -run TestReferencePerformanceProfile -count=1 -v
GAPDB_REFERENCE_ACCEPTANCE=1 go test ./tests/performance -run '^$' -bench '^BenchmarkReferenceUnixSocket$' -benchtime=100x -count=1 -benchmem
```

Developer smoke (does not claim the numerical reference thresholds):

```sh
go test ./tests/performance ./tests/adoption -count=1
go test ./tests/performance -run '^$' -bench '^BenchmarkReferenceUnixSocket$' -benchtime=10x -count=1
```

The recorded host is Linux/amd64 7.0.0-28-generic with Go 1.26.7, an AMD Ryzen
5 5600X (six physical/twelve logical cores), 65,721,852 kB RAM, and ext4 on a
WD_BLACK SN7100 NVMe device. It records no hostname, username, serial number, or
full mount layout.

Timing fields are volatile and meaningful only for the recorded profile.
Repeat a candidate run at least three times, compare p95 values and recovery to
the fixed thresholds, and investigate variance above 15%; do not claim precise
cross-machine regression percentages. Tests never silently change thresholds.

## Adoption state

[`adoption.json`](adoption.json) records the passing 14-scenario in-repository
Gapdb adapter. Its response-loss case writes a canonical durable request over a
raw Unix connection, closes the response direction without decoding a result,
returns only portable ambiguity, restarts independently, and reconciles the
exact public record revision and durable authority. The originating
application's real SQLite adapter is unavailable,
so its result is explicitly `pending`; this is not a pass. The recommendation
remains `production_backend:"sqlite"` and `adoption_status:"not_approved"`.
Automation has no human-approval input. A production switch requires a separate
signed human decision retained by the originating application after both real
adapters and all authority evidence pass.

The bounded [`release-manifest.json`](release-manifest.json) separates MVP
completion from adoption. Its validator hashes every referenced evidence file,
then strictly decodes each evidence kind against a separately compiled trust
root for source commit, configuration, command, count, result, and allowable
criterion relevance. It accounts for SC-001–SC-009 and NFR-001–NFR-012, and
fails on deletion, coherent semantic rewrites, staleness, mismatch,
duplicate/missing criteria, or an out-of-bounds file/count.
