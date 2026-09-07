# Recovery snapshot candidate extension v1

This document defines a technical candidate extension. It does not amend the
adopted Gapdb protocol-v1 authority or claim release adoption.

`read_recovery_snapshot` is a read-only stable-revision export for bounded
recovery engines. Its request and every failure use the ordinary canonical JSON
envelope. A success uses the `GDBREC1` length-prefixed binary frame and binds the
database ID, request ID, observed revision, UTC observation time, sorted UTF-8
prefix membership, record revisions and expiry, and a trailing SHA-256 digest.

The request declares non-empty UTF-8 `prefix`, exact `expected_revision`,
positive `max_records` no greater than 200,000, and positive `max_bytes` no
greater than 256 MiB. The complete frame, including an empty result header and
digest, must fit `max_bytes`. There is no partial result. Record keys use a
32-bit length so the existing 65,536-byte hard key bound remains representable.
Values use a 32-bit length and remain bounded by the complete frame budget.

The owner permits one recovery snapshot construction at a time. Saturation
returns typed server backpressure before cloning records or acquiring the
database read lock. The operation never mutates, replays a mutation, returns a
storage handle, or replaces caller-side canonical and semantic validation.

The public decoder returns defensive value copies. The public client may retain
private frame storage internally, but each returned value has capacity equal to
its length so it cannot reslice or append into an adjacent record.
