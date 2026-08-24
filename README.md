# Gapdb

Gapdb is a small local coordination store: one Go map in RAM, one mutation
owner goroutine, conditional writes, an append-only WAL, occasional snapshots,
and an owner-only Unix socket. It provides Get, Put, PutIfAbsent, CAS,
DeleteIfRevision, AtomicBatch, ScanPrefix, and Watch without SQL, indexes,
vectors, networking, consensus, or general-database aspirations.

Memory acknowledgements are visible in RAM but **are not durable** and may be
lost on abrupt failure. Leases, reviews, workflow state, and integration
authority require durable acknowledgement after the real WAL sync barrier.

Start with:

```sh
go test ./...
go run ./cmd/gapdbd --db /srv/gapdb --json
go run ./cmd/gapctl --socket /srv/gapdb/gapdb.sock status
```

The [protocol v1](docs/formats/protocol-v1.md), [storage v1](docs/formats/storage-v1.md),
[configuration guide](docs/operations/configuration.md), and
[recovery runbook](docs/operations/recovery.md) are tied to committed codecs and
goldens. Reproducible [crash](docs/evidence/crash/README.md) and
[performance](docs/evidence/performance/README.md) evidence remains separate
from adoption authority.

SQLite remains the production backend. Gapdb's in-repository portable Phase 1
adapter passes, but the originating application's real SQLite/Gapdb dual-backend
run is external and currently pending. No automated result can approve or alter
the production switch; it requires a separate signed human decision.
