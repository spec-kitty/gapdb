package engine

import (
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spec-kitty/gapdb/gapdb"
)

const scanCursorFixedBytes = 52

var scanCRC = crc32.MakeTable(crc32.Castagnoli)

type scanCursor struct {
	prefix   string
	lastKey  string
	revision gapdb.Revision
	asOf     time.Time
}

func (state *DatabaseState) ScanPrefix(prefix string, limit int, encodedCursor string) (gapdb.ScanPage, error) {
	if err := validatePrefix(prefix, state.limits); err != nil {
		return gapdb.ScanPage{}, err
	}
	if limit <= 0 || limit > state.limits.MaxScanRecords {
		return gapdb.ScanPage{}, invalidField("limit", "must be between 1 and the configured scan-record limit")
	}
	var cursor scanCursor
	var err error
	if encodedCursor != "" {
		cursor, err = state.decodeScanCursor(encodedCursor)
		if err != nil {
			return gapdb.ScanPage{}, err
		}
		if cursor.prefix != prefix {
			return gapdb.ScanPage{}, invalidCursor("cursor prefix does not match request prefix")
		}
	}
	state.mu.RLock()
	asOf := state.clock.Now()
	observed := state.current
	if encodedCursor != "" {
		if cursor.revision != observed {
			state.mu.RUnlock()
			cursorRevision, current := cursor.revision, observed
			return gapdb.ScanPage{}, &gapdb.Error{Code: gapdb.CodeScanStale, Message: "The database changed after the scan page was created.", Retry: gapdb.RetryAfterRescan, CursorRevision: &cursorRevision, CurrentRevision: &current, SafeActions: []gapdb.SafeAction{gapdb.ActionRestartScan, gapdb.ActionAbort}}
		}
		asOf = cursor.asOf
	}
	page := gapdb.ScanPage{ObservedRevision: observed, AsOf: asOf.UTC()}
	used := 0
	start := sort.Search(len(state.orderedKeys), func(index int) bool {
		if encodedCursor != "" {
			return state.orderedKeys[index] > cursor.lastKey
		}
		return state.orderedKeys[index] >= prefix
	})
	for index := start; index < len(state.orderedKeys); index++ {
		key := state.orderedKeys[index]
		if !strings.HasPrefix(key, prefix) {
			break
		}
		record, exists := state.records[key]
		if !exists {
			state.mu.RUnlock()
			return gapdb.ScanPage{}, internalFailure("engine-ordered-key-index", false, nil)
		}
		if recordExpired(record, asOf) {
			continue
		}
		bytes := scanRecordBytes(record)
		if len(page.Records) == limit || used+bytes > state.limits.MaxScanBytes {
			if len(page.Records) == 0 {
				state.mu.RUnlock()
				return gapdb.ScanPage{}, &gapdb.Error{Code: gapdb.CodeFrameTooLarge, Message: "A scan record exceeds the configured page byte limit.", Retry: gapdb.RetryNever, ReceivedBytes: bytes, MaximumBytes: state.limits.MaxScanBytes, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceRequest, gapdb.ActionAbort}}
			}
			page.Truncated = true
			break
		}
		page.Records = append(page.Records, record.Clone())
		used += bytes
	}
	state.mu.RUnlock()
	if page.Truncated {
		page.Cursor = state.encodeScanCursor(scanCursor{prefix: prefix, lastKey: page.Records[len(page.Records)-1].Key, revision: observed, asOf: asOf})
	}
	return page.Clone(), nil
}

func scanRecordBytes(record gapdb.Record) int {
	bytes := 32 + len(record.Key) + len(record.Value)
	if record.ExpiresAt != nil {
		bytes += 8
	}
	return bytes
}

func validatePrefix(prefix string, limits gapdb.Limits) error {
	if !utf8.ValidString(prefix) {
		return invalidField("prefix", "must be valid UTF-8")
	}
	if len(prefix) > limits.MaxKeyBytes {
		return &gapdb.Error{Code: gapdb.CodeKeyTooLarge, Message: "Prefix exceeds the configured key limit.", Retry: gapdb.RetryNever, Field: "prefix", ReceivedBytes: len(prefix), MaximumBytes: limits.MaxKeyBytes, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceKey, gapdb.ActionAbort}}
	}
	return nil
}

func (state *DatabaseState) encodeScanCursor(cursor scanCursor) string {
	encoded := make([]byte, scanCursorFixedBytes+len(cursor.prefix)+len(cursor.lastKey))
	copy(encoded[:8], "GAPCUR01")
	copy(encoded[8:24], state.databaseID[:])
	binary.BigEndian.PutUint64(encoded[24:32], uint64(cursor.revision))
	binary.BigEndian.PutUint64(encoded[32:40], uint64(cursor.asOf.UTC().UnixNano()))
	binary.BigEndian.PutUint32(encoded[40:44], uint32(len(cursor.prefix)))
	binary.BigEndian.PutUint32(encoded[44:48], uint32(len(cursor.lastKey)))
	copy(encoded[48:], cursor.prefix)
	copy(encoded[48+len(cursor.prefix):], cursor.lastKey)
	binary.BigEndian.PutUint32(encoded[len(encoded)-4:], crc32.Checksum(encoded[:len(encoded)-4], scanCRC))
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func (state *DatabaseState) decodeScanCursor(value string) (scanCursor, error) {
	maximumDecoded := scanCursorFixedBytes + 2*state.limits.MaxKeyBytes
	if len(value) > base64.RawURLEncoding.EncodedLen(maximumDecoded) {
		return scanCursor{}, invalidCursor("cursor exceeds the configured bound")
	}
	encoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(encoded) < scanCursorFixedBytes || len(encoded) > maximumDecoded {
		return scanCursor{}, invalidCursor("cursor is not canonical base64url or has an invalid size")
	}
	if base64.RawURLEncoding.EncodeToString(encoded) != value {
		return scanCursor{}, invalidCursor("cursor is not canonical base64url or has an invalid size")
	}
	if string(encoded[:8]) != "GAPCUR01" || string(encoded[8:24]) != string(state.databaseID[:]) {
		return scanCursor{}, invalidCursor("cursor version or database ID does not match")
	}
	prefixBytes := uint64(binary.BigEndian.Uint32(encoded[40:44]))
	lastBytes := uint64(binary.BigEndian.Uint32(encoded[44:48]))
	if prefixBytes > uint64(state.limits.MaxKeyBytes) || lastBytes > uint64(state.limits.MaxKeyBytes) || uint64(scanCursorFixedBytes)+prefixBytes+lastBytes != uint64(len(encoded)) {
		return scanCursor{}, invalidCursor("cursor field lengths are invalid")
	}
	if crc32.Checksum(encoded[:len(encoded)-4], scanCRC) != binary.BigEndian.Uint32(encoded[len(encoded)-4:]) {
		return scanCursor{}, invalidCursor("cursor checksum does not match")
	}
	prefixEnd := 48 + int(prefixBytes)
	prefix, last := string(encoded[48:prefixEnd]), string(encoded[prefixEnd:len(encoded)-4])
	if !utf8.ValidString(prefix) || !utf8.ValidString(last) || last == "" || !strings.HasPrefix(last, prefix) {
		return scanCursor{}, invalidCursor("cursor key fields are invalid")
	}
	asOf := time.Unix(0, int64(binary.BigEndian.Uint64(encoded[32:40]))).UTC()
	if !expiryRepresentable(asOf) {
		return scanCursor{}, invalidCursor("cursor as-of time is invalid")
	}
	return scanCursor{prefix: prefix, lastKey: last, revision: gapdb.Revision(binary.BigEndian.Uint64(encoded[24:32])), asOf: asOf}, nil
}

func invalidCursor(reason string) error {
	return &gapdb.Error{Code: gapdb.CodeInvalidCursor, Message: "The scan cursor is invalid.", Retry: gapdb.RetryNever, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionRestartScan, gapdb.ActionAbort}}
}
