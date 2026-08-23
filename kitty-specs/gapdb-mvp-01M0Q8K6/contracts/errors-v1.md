# Gapdb Error Contract v1

## Rules

- Every failed socket or JSON CLI operation returns the common v1 error envelope.
- Error `code`, `retry`, field names, and `safe_actions` tokens are stable within
  protocol v1. Human `message` text may improve without a protocol version change.
- Errors never use a zero process exit status.
- Errors include only fields relevant to the failure; absent evidence is omitted,
  not represented by invented zero values.
- If an operation may have applied before a later failure, the envelope includes
  `operation_applied: true`. Callers must inspect/reconcile and never blindly retry.

## Retry classifications

| Value | Meaning |
|---|---|
| `never` | The same request cannot succeed unchanged. |
| `immediate` | A bounded retry is safe without reading new state. |
| `after_reconcile` | Read authoritative state before deciding whether to retry. |
| `after_rescan` | Rebuild a point-in-time view and resume from its revision. |
| `after_restart` | Retry only after owner restart or health recovery. |
| `after_operator` | Human-authorized inspection or recovery is required. |

## Validation and protocol errors

| Code | Retry | Required evidence | Safe actions |
|---|---|---|---|
| `INVALID_REQUEST` | never | field/path and reason | `fix_request`, `abort` |
| `UNSUPPORTED_VERSION` | never | received and supported versions | `use_supported_version`, `upgrade_client`, `abort` |
| `FRAME_TOO_LARGE` | never | received and maximum bytes | `reduce_request`, `abort` |
| `KEY_TOO_LARGE` | never | key bytes and maximum | `reduce_key`, `abort` |
| `VALUE_TOO_LARGE` | never | value bytes and maximum | `reduce_value`, `abort` |
| `BATCH_TOO_LARGE` | never | operations/bytes and maxima | `split_batch`, `abort` |
| `DUPLICATE_KEY` | never | key and mutation indexes | `deduplicate_batch`, `abort` |
| `EXPIRY_NOT_FUTURE` | after_reconcile | supplied expiry and effective time | `choose_future_expiry`, `abort` |
| `INVALID_CURSOR` | never | reason | `restart_scan`, `abort` |

## Conditional-state errors

| Code | Retry | Required evidence | Safe actions |
|---|---|---|---|
| `NOT_FOUND` | after_reconcile | key, current revision | `get`, `put_if_absent`, `abort` |
| `ALREADY_EXISTS` | after_reconcile | key, actual revision | `get`, `compare_and_swap`, `abort` |
| `REVISION_MISMATCH` | after_reconcile | key, expected and actual revision | `get`, `retry_with_new_condition`, `abort` |
| `CONDITION_FAILED` | after_reconcile | mutation index, key, condition, actual state/revision | `get`, `rebuild_batch`, `abort` |

## Scan and watch errors

| Code | Retry | Required evidence | Safe actions |
|---|---|---|---|
| `SCAN_STALE` | after_rescan | cursor revision and current revision | `restart_scan`, `abort` |
| `REVISION_AHEAD` | after_rescan | requested and current revision | `scan_prefix`, `restart_watch`, `abort` |
| `REVISION_COMPACTED` | after_rescan | requested and earliest available revision | `scan_prefix`, `restart_watch`, `abort` |
| `WATCH_LAGGED` | after_rescan | last delivered and current revision | `scan_prefix`, `restart_watch`, `abort` |

## Ownership and availability errors

| Code | Retry | Required evidence | Safe actions |
|---|---|---|---|
| `OWNER_EXISTS` | after_operator | database path and diagnostic owner metadata when safe | `status`, `wait`, `abort` |
| `SERVER_BUSY` | immediate | active and maximum clients/queue depth | `retry_with_backoff`, `abort` |
| `SERVER_SHUTTING_DOWN` | after_restart | lifecycle state | `wait_for_restart`, `abort` |
| `DEADLINE_EXCEEDED` | after_reconcile | operation and deadline | `status`, `reconcile`, `abort` |
| `SERVER_UNAVAILABLE` | after_restart | socket path and connection reason | `status_offline`, `start_server`, `abort` |
| `PERMISSION_DENIED` | after_operator | safe path and effective operation | `fix_permissions`, `abort` |

## Storage and compatibility errors

| Code | Retry | Required evidence | Safe actions |
|---|---|---|---|
| `STORAGE_DEGRADED` | after_restart | failed stage, current and durable-through revisions | `status`, `verify`, `restart_after_recovery`, `abort` |
| `CORRUPT_IDENTITY` | after_operator | file, offset/reason, expected database ID if known | `inspect_offline`, `restore_backup`, `abort` |
| `CORRUPT_MANIFEST` | after_operator | file and checksum/framing reason | `inspect_offline`, `recover_propose`, `restore_backup`, `abort` |
| `CORRUPT_SNAPSHOT` | after_operator | file, offset/record, checksum/reason | `inspect_offline`, `recover_propose`, `restore_backup`, `abort` |
| `CORRUPT_WAL` | after_operator | file, offset, revision if decoded, reason | `inspect_offline`, `recover_propose`, `restore_backup`, `abort` |
| `UNKNOWN_FORMAT` | never | file, received and supported versions | `upgrade_gapdb`, `use_compatible_binary`, `abort` |
| `DATABASE_ID_MISMATCH` | never | expected/actual IDs and file/request | `select_correct_database`, `abort` |
| `REVISION_RANGE_EXHAUSTED` | after_restart | reserved end and reservation failure | `verify_storage`, `restart_after_recovery`, `abort` |
| `IO_ERROR` | after_operator | operation, safe path, OS error category | `check_storage`, `verify`, `abort` |

An incomplete final WAL frame recovered during startup is a successful recovery
diagnostic (`TAIL_TRUNCATED`), not `CORRUPT_WAL`. The diagnostic includes file,
old/new sizes, and last complete revision.

## Administrative errors

| Code | Retry | Required evidence | Safe actions |
|---|---|---|---|
| `ADMIN_PRECONDITION_FAILED` | after_reconcile | expected/actual database ID, revision, or manifest generation | `status`, `rebuild_request`, `abort` |
| `SNAPSHOT_IN_PROGRESS` | immediate | operation ID and start time | `status`, `wait`, `abort` |
| `COMPACTION_NOT_SAFE` | after_reconcile | through revision and active snapshot revision | `status`, `create_snapshot`, `abort` |
| `BACKUP_DESTINATION_EXISTS` | never | destination | `choose_new_destination`, `abort` |
| `BACKUP_INVALID` | after_operator | backup path and failing file/check | `verify_backup`, `choose_other_backup`, `abort` |
| `RECOVERY_REQUIRED` | after_operator | detected corruption and proposal availability | `recover_propose`, `restore_backup`, `abort` |
| `RECOVERY_ACTION_MISMATCH` | after_reconcile | expected/actual database ID, generation, and proposal ID | `recover_propose`, `abort` |
| `AUDIT_FAILED_AFTER_APPLY` | after_reconcile | applied operation result plus audit failure | `status`, `verify`, `repair_audit_storage`, `abort` |

## Internal safety error

`INTERNAL` means an invariant failed without a more specific safe classification.
It uses retry `after_operator`, sets health to degraded or terminates before
serving more mutations, includes a correlation ID, and offers only `status`,
`verify`, and `abort`. It never exposes stack traces or raw memory in the public
envelope.

## CLI exit codes

| Exit | Class |
|---:|---|
| 0 | Success, including healthy inspection and applied recovery |
| 2 | Request, validation, or unsupported protocol error |
| 3 | Conditional state, stale scan, or watch cursor error |
| 4 | Owner, availability, deadline, or permission error |
| 5 | Storage integrity, compatibility, backup, or recovery error |
| 6 | Internal invariant or post-apply audit failure |

With `--output=json`, stdout contains exactly one error object for unary commands;
stderr remains empty unless the process cannot initialize the JSON encoder itself.
Watch streams emit the terminal error frame on stdout and exit with the mapped
nonzero status.
