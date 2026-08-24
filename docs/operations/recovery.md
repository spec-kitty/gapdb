# Gapdb inspection, backup, and recovery runbook

Gapdb never repairs corruption automatically. Online administration operates
through the owner socket. Offline inspection and recovery acquire the same
exclusive owner lock as the server, so a live owner produces `OWNER_EXISTS`
without reading or changing authority files.

## Routine guarded administration

Read `status` first and copy stable fields, never the advisory message:

```sh
gapctl --socket /srv/gapdb/gapdb.sock status
gapctl --socket /srv/gapdb/gapdb.sock --request-id snapshot-42 create-snapshot \
  --expected-database-id DATABASE_ID --expected-revision REVISION
gapctl --socket /srv/gapdb/gapdb.sock --request-id compact-42 compact \
  --expected-database-id DATABASE_ID --through-revision REVISION
gapctl --socket /srv/gapdb/gapdb.sock --request-id backup-42 backup \
  --expected-database-id DATABASE_ID --expected-revision REVISION \
  --destination /srv/gapdb-backups/backup-42
```

Backup and quarantine destinations are clean absolute new locations outside the
database directory. Publication is no-overwrite and verified by the storage
layer. A precondition failure means obtain new status and rebuild the request;
never substitute a guessed identifier or revision.

## Offline inspection and proposal

Stop the owner and prove it has exited before offline work:

```sh
gapctl --db /srv/gapdb inspect
gapctl --db /srv/gapdb verify --mode full
gapctl --db /srv/gapdb recover propose
```

Inspection is read-only apart from acquiring the existing ownership lock. A
failure object contains stable `error.code`, `safe_actions`, and an `evidence`
inspection object. `recover propose` hashes the complete damaged artifact and
returns a non-mutating proposal containing:

- proposal/action ID;
- database ID and manifest generation;
- action and damaged filename;
- the exact canonical allowed-action set for the finding;
- complete-file SHA-256, size, device, and inode evidence.

If the safe actions do not contain `recover_propose`, stop. Unknown formats,
identity uncertainty, absent evidence, or a live owner do not grant recovery
authority.

## Explicit apply

Persist only the `result.proposal` JSON object to a bounded proposal file. Apply
requires every guard and an explicit quarantine location:

```sh
gapctl --db /srv/gapdb --request-id recovery-42 recover apply \
  --proposal-file /run/gapdb/proposal-42.json \
  --action-id PROPOSAL_ID \
  --expected-database-id DATABASE_ID \
  --expected-manifest-generation MANIFEST_GENERATION \
  --destination /srv/gapdb-quarantine/recovery-42
```

Before ownership or mutation, `gapctl` strictly decodes the proposal and checks
the action ID, database ID, manifest generation, and clean absolute destination.
After taking ownership, the recovery layer revalidates the current identity,
manifest generation, complete-file hash/size/device/inode, and no-overwrite
destination. It also repeats the inspection while holding ownership and
requires the exact same finding code, artifact evidence, and canonical allowed
actions. Any mismatch makes no change and directs the caller to run
`recover propose` again. `operation_applied: true` means authority may already
have changed; run offline verify and reconcile before any retry.

Machine envelopes always use the locked protocol-v1 operation names
`offline_inspect`, `offline_verify`, `offline_recover_propose`, and
`offline_recover_apply`, including validation and storage errors.

After a watch returns `WATCH_LAGGED`, `REVISION_COMPACTED`, or `SCAN_STALE`, run
a fresh bounded `scan-prefix`, use its `observed_revision` as the next exclusive
watch revision, and resume. No other resume procedure is authoritative.
