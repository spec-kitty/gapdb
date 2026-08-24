package persist

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

const (
	WALHeaderSize   = 64
	frameHeaderSize = 28
	frameTrailer    = 4
	effectHeader    = 20
)

type WALHeader struct {
	DatabaseID    DatabaseID
	FirstRevision gapdb.Revision
	Generation    uint64
}

type Effect struct {
	Kind      gapdb.ChangeKind
	Key       string
	Value     []byte
	ExpiresAt *time.Time
}

type CommitFrame struct {
	Revision gapdb.Revision
	Effects  []Effect
}

func EncodeWALHeader(header WALHeader) ([]byte, error) {
	if header.DatabaseID.IsZero() || header.FirstRevision == 0 || header.Generation == 0 {
		return nil, corruptWAL("WAL header fields must be nonzero", "", 0, nil)
	}
	encoded := make([]byte, WALHeaderSize)
	copy(encoded[:8], "GAPWAL01")
	binary.BigEndian.PutUint16(encoded[8:10], storageVersion)
	binary.BigEndian.PutUint16(encoded[10:12], WALHeaderSize)
	copy(encoded[12:28], header.DatabaseID[:])
	binary.BigEndian.PutUint64(encoded[28:36], uint64(header.FirstRevision))
	binary.BigEndian.PutUint64(encoded[36:44], header.Generation)
	binary.BigEndian.PutUint32(encoded[60:64], crc32.Checksum(encoded[:60], castagnoli))
	return encoded, nil
}

func DecodeWALHeader(encoded []byte, expectedID DatabaseID) (WALHeader, error) {
	if len(encoded) != WALHeaderSize {
		return WALHeader{}, corruptWAL("WAL header size is not 64", "", 0, nil)
	}
	if string(encoded[:8]) != "GAPWAL01" {
		return WALHeader{}, corruptWAL("WAL magic does not match GAPWAL01", "", 0, nil)
	}
	version := binary.BigEndian.Uint16(encoded[8:10])
	if version != storageVersion {
		return WALHeader{}, unknownStorageFormat("WAL", uint64(version))
	}
	if binary.BigEndian.Uint16(encoded[10:12]) != WALHeaderSize || !allZero(encoded[44:60]) {
		return WALHeader{}, corruptWAL("WAL header length or reserved bytes are invalid", "", 0, nil)
	}
	if got, want := crc32.Checksum(encoded[:60], castagnoli), binary.BigEndian.Uint32(encoded[60:64]); got != want {
		return WALHeader{}, corruptWAL("WAL header CRC32C mismatch", "", 0, nil)
	}
	var id DatabaseID
	copy(id[:], encoded[12:28])
	if id != expectedID {
		return WALHeader{}, databaseIDMismatch(expectedID, id, "WAL")
	}
	header := WALHeader{DatabaseID: id, FirstRevision: gapdb.Revision(binary.BigEndian.Uint64(encoded[28:36])), Generation: binary.BigEndian.Uint64(encoded[36:44])}
	if header.FirstRevision == 0 || header.Generation == 0 {
		return WALHeader{}, corruptWAL("WAL first revision and generation must be nonzero", "", 0, nil)
	}
	return header, nil
}

func EncodeCommitFrame(commit CommitFrame, limits gapdb.Limits) ([]byte, error) {
	if err := validateCommit(commit, limits); err != nil {
		return nil, err
	}
	payloadLength, err := effectPayloadLength(commit.Effects)
	if err != nil {
		return nil, err
	}
	total := uint64(frameHeaderSize+frameTrailer) + payloadLength
	maximum := limits.MaxBatchBytes
	if limits.MaxFrameBytes < maximum {
		maximum = limits.MaxFrameBytes
	}
	if total > uint64(maximum) || total > math.MaxUint32 {
		return nil, &gapdb.Error{Code: gapdb.CodeBatchTooLarge, Message: "WAL commit frame exceeds the configured byte limit.", Retry: gapdb.RetryNever, ReceivedBytes: saturatedInt(total), MaximumBytes: maximum, SafeActions: []gapdb.SafeAction{gapdb.ActionSplitBatch, gapdb.ActionAbort}}
	}
	encoded := make([]byte, int(total))
	copy(encoded[:4], "CMIT")
	binary.BigEndian.PutUint16(encoded[4:6], storageVersion)
	// bytes 6-7 are zero flags.
	binary.BigEndian.PutUint32(encoded[8:12], uint32(total))
	binary.BigEndian.PutUint64(encoded[12:20], uint64(commit.Revision))
	binary.BigEndian.PutUint32(encoded[20:24], uint32(len(commit.Effects)))
	binary.BigEndian.PutUint32(encoded[24:28], uint32(payloadLength))
	offset := frameHeaderSize
	for _, effect := range commit.Effects {
		kind := byte(0)
		switch effect.Kind {
		case gapdb.ChangePut:
			kind = 1
		case gapdb.ChangeDelete:
			kind = 2
		case gapdb.ChangeExpire:
			kind = 3
		}
		encoded[offset] = kind
		if effect.ExpiresAt != nil {
			encoded[offset+1] = 1
		}
		binary.BigEndian.PutUint32(encoded[offset+4:offset+8], uint32(len(effect.Key)))
		binary.BigEndian.PutUint32(encoded[offset+8:offset+12], uint32(len(effect.Value)))
		if effect.ExpiresAt != nil {
			binary.BigEndian.PutUint64(encoded[offset+12:offset+20], uint64(effect.ExpiresAt.UTC().UnixNano()))
		}
		offset += effectHeader
		copy(encoded[offset:], effect.Key)
		offset += len(effect.Key)
		copy(encoded[offset:], effect.Value)
		offset += len(effect.Value)
	}
	binary.BigEndian.PutUint32(encoded[len(encoded)-4:], crc32.Checksum(encoded[:len(encoded)-4], castagnoli))
	return encoded, nil
}

func DecodeCommitFrame(encoded []byte, limits gapdb.Limits) (CommitFrame, error) {
	if len(encoded) < frameHeaderSize+frameTrailer {
		return CommitFrame{}, corruptWAL("commit frame is shorter than its fixed fields", "", 0, nil)
	}
	if string(encoded[:4]) != "CMIT" {
		return CommitFrame{}, corruptWAL("commit magic does not match CMIT", "", 0, nil)
	}
	version := binary.BigEndian.Uint16(encoded[4:6])
	if version != storageVersion {
		return CommitFrame{}, unknownStorageFormat("WAL", uint64(version))
	}
	if binary.BigEndian.Uint16(encoded[6:8]) != 0 {
		return CommitFrame{}, corruptWAL("commit flags are nonzero", "", 0, nil)
	}
	total := uint64(binary.BigEndian.Uint32(encoded[8:12]))
	payloadLength := uint64(binary.BigEndian.Uint32(encoded[24:28]))
	maximum := limits.MaxBatchBytes
	if limits.MaxFrameBytes < maximum {
		maximum = limits.MaxFrameBytes
	}
	if total != uint64(len(encoded)) || total != uint64(frameHeaderSize+frameTrailer)+payloadLength || total > uint64(maximum) {
		return CommitFrame{}, corruptWAL("commit frame lengths are inconsistent or exceed limits", "", 0, nil)
	}
	count := uint64(binary.BigEndian.Uint32(encoded[20:24]))
	if count == 0 || count > uint64(limits.MaxBatchOperations) || count > payloadLength/effectHeader {
		return CommitFrame{}, corruptWAL("mutation count is outside limits or impossible for payload", "", 0, nil)
	}
	if got, want := crc32.Checksum(encoded[:len(encoded)-4], castagnoli), binary.BigEndian.Uint32(encoded[len(encoded)-4:]); got != want {
		return CommitFrame{}, corruptWAL("commit frame CRC32C mismatch", "", 0, nil)
	}
	commit := CommitFrame{Revision: gapdb.Revision(binary.BigEndian.Uint64(encoded[12:20])), Effects: make([]Effect, 0, int(count))}
	payloadEnd := len(encoded) - frameTrailer
	offset := frameHeaderSize
	for index := uint64(0); index < count; index++ {
		if offset > payloadEnd-effectHeader {
			return CommitFrame{}, corruptWAL("mutation header exceeds the payload", "", int64(offset), nil)
		}
		kind := encoded[offset]
		flags := encoded[offset+1]
		if flags&^byte(1) != 0 || !allZero(encoded[offset+2:offset+4]) {
			return CommitFrame{}, corruptWAL("mutation flags or reserved bytes are invalid", "", int64(offset), nil)
		}
		keyLength := uint64(binary.BigEndian.Uint32(encoded[offset+4 : offset+8]))
		valueLength := uint64(binary.BigEndian.Uint32(encoded[offset+8 : offset+12]))
		expiryNanos := int64(binary.BigEndian.Uint64(encoded[offset+12 : offset+20]))
		offset += effectHeader
		remaining := uint64(payloadEnd - offset)
		if keyLength > uint64(limits.MaxKeyBytes) || valueLength > uint64(limits.MaxValueBytes) || keyLength > remaining || valueLength > remaining-keyLength {
			return CommitFrame{}, corruptWAL("mutation key/value lengths exceed bounds", "", int64(offset), nil)
		}
		keyEnd := offset + int(keyLength)
		valueEnd := keyEnd + int(valueLength)
		key := string(encoded[offset:keyEnd])
		if key == "" || !utf8.ValidString(key) {
			return CommitFrame{}, corruptWAL("mutation key is empty or invalid UTF-8", "", int64(offset), nil)
		}
		effect := Effect{Key: key}
		switch kind {
		case 1:
			effect.Kind = gapdb.ChangePut
			effect.Value = append([]byte{}, encoded[keyEnd:valueEnd]...)
			if flags&1 != 0 {
				expires := time.Unix(0, expiryNanos).UTC()
				effect.ExpiresAt = &expires
			} else if expiryNanos != 0 {
				return CommitFrame{}, corruptWAL("put expiry value is nonzero without its flag", "", int64(offset), nil)
			}
		case 2, 3:
			if valueLength != 0 || flags != 0 || expiryNanos != 0 {
				return CommitFrame{}, corruptWAL("delete/expire carries a value or expiry", "", int64(offset), nil)
			}
			if kind == 2 {
				effect.Kind = gapdb.ChangeDelete
			} else {
				effect.Kind = gapdb.ChangeExpire
			}
		default:
			return CommitFrame{}, corruptWAL("mutation kind is unknown", "", int64(offset), nil)
		}
		commit.Effects = append(commit.Effects, effect)
		offset = valueEnd
	}
	if offset != payloadEnd {
		return CommitFrame{}, corruptWAL("mutation payload has trailing bytes", "", int64(offset), nil)
	}
	if err := validateCommit(commit, limits); err != nil {
		return CommitFrame{}, corruptWAL("decoded commit violates mutation invariants", "", 0, err)
	}
	return commit, nil
}

type WAL struct {
	fs             faultfs.FS
	file           faultfs.File
	buffer         *bufio.Writer
	path           string
	header         WALHeader
	limits         gapdb.Limits
	lastRevision   gapdb.Revision
	durableThrough gapdb.Revision
	pending        bool
	failed         bool
}

func CreateWAL(fsys faultfs.FS, path string, header WALHeader, bufferSize int, limits gapdb.Limits) (*WAL, error) {
	if bufferSize <= 0 {
		return nil, corruptWAL("WAL buffer size must be positive", filepath.Base(path), 0, nil)
	}
	wantName := fmt.Sprintf("wal-%020d.gdb", header.FirstRevision)
	if filepath.Base(path) != wantName {
		return nil, corruptWAL("WAL filename does not match its first revision", filepath.Base(path), 0, nil)
	}
	encoded, err := EncodeWALHeader(header)
	if err != nil {
		return nil, err
	}
	file, err := fsys.OpenFile(faultfs.PointWALHeaderCreate, path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ioFailure("wal_header_create", filepath.Base(path), err)
	}
	if err := writeFull(fsys, faultfs.PointWALHeaderWrite, file, encoded); err != nil {
		_ = file.Close()
		return nil, ioFailure("wal_header_write", filepath.Base(path), err)
	}
	if err := fsys.Sync(faultfs.PointWALFileSync, file); err != nil {
		_ = file.Close()
		return nil, ioFailure("wal_header_sync", filepath.Base(path), err)
	}
	return &WAL{fs: fsys, file: file, buffer: bufio.NewWriterSize(file, bufferSize), path: filepath.Base(path), header: header, limits: limits, lastRevision: header.FirstRevision - 1}, nil
}

func OpenWALForAppend(fsys faultfs.FS, path string, expected WALHeader, lastRevision, durableThrough gapdb.Revision, bufferSize int, limits gapdb.Limits) (*WAL, error) {
	if bufferSize <= 0 || lastRevision < expected.FirstRevision-1 || durableThrough > lastRevision {
		return nil, corruptWAL("recovered WAL append state is invalid", filepath.Base(path), 0, nil)
	}
	file, err := fsys.OpenFile(faultfs.PointOpen, path, os.O_RDWR, 0)
	if err != nil {
		return nil, ioFailure("wal_open_append", filepath.Base(path), err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, ioFailure("wal_stat_append", filepath.Base(path), err)
	}
	replay, err := ScanWAL(file, info.Size(), expected.DatabaseID, expected.FirstRevision-1, lastRevision, limits)
	if err != nil {
		return nil, withWALContext(err, filepath.Base(path), 0)
	}
	if replay.Tail != nil || replay.Header != expected || replay.RecoveredRevision != lastRevision {
		return nil, corruptWAL("WAL append state does not match recovered authority", filepath.Base(path), 0, nil)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, ioFailure("wal_seek_append", filepath.Base(path), err)
	}
	closeOnError = false
	return &WAL{fs: fsys, file: file, buffer: bufio.NewWriterSize(file, bufferSize), path: filepath.Base(path), header: expected, limits: limits, lastRevision: lastRevision, durableThrough: durableThrough}, nil
}

func (wal *WAL) Append(commit CommitFrame) error {
	if wal.failed {
		return storageDegraded("wal_unavailable", wal.lastRevision, wal.durableThrough, nil)
	}
	if commit.Revision < wal.header.FirstRevision || commit.Revision <= wal.lastRevision {
		return corruptWAL("commit revision is below the WAL start or not increasing", wal.path, 0, nil)
	}
	encoded, err := EncodeCommitFrame(commit, wal.limits)
	if err != nil {
		return err
	}
	if err := writeFull(wal.fs, faultfs.PointWALFrameWrite, wal.buffer, encoded); err != nil {
		wal.failed = true
		return storageDegraded("wal_frame_write", wal.lastRevision, wal.durableThrough, err)
	}
	wal.lastRevision = commit.Revision
	wal.pending = true
	return nil
}

func (wal *WAL) Barrier() error {
	if wal.failed {
		return storageDegraded("wal_unavailable", wal.lastRevision, wal.durableThrough, nil)
	}
	if err := wal.fs.Flush(faultfs.PointWALBufferFlush, wal.buffer); err != nil {
		wal.failed = true
		return storageDegraded("wal_buffer_flush", wal.lastRevision, wal.durableThrough, err)
	}
	if err := wal.fs.Sync(faultfs.PointWALFileSync, wal.file); err != nil {
		wal.failed = true
		return storageDegraded("wal_file_sync", wal.lastRevision, wal.durableThrough, err)
	}
	wal.durableThrough = wal.lastRevision
	wal.pending = false
	return nil
}

func (wal *WAL) DurableThrough() gapdb.Revision { return wal.durableThrough }

func (wal *WAL) LastRevision() gapdb.Revision { return wal.lastRevision }

func (wal *WAL) Close() error {
	if wal == nil || wal.file == nil {
		return nil
	}
	var barrierErr error
	if wal.pending || wal.failed {
		barrierErr = wal.Barrier()
	}
	closeErr := wal.file.Close()
	wal.file = nil
	if barrierErr != nil {
		return barrierErr
	}
	return closeErr
}

func validateCommit(commit CommitFrame, limits gapdb.Limits) error {
	if commit.Revision == 0 {
		return corruptWAL("commit revision must be nonzero", "", 0, nil)
	}
	if len(commit.Effects) == 0 {
		return corruptWAL("commit must contain at least one mutation", "", 0, nil)
	}
	if len(commit.Effects) > limits.MaxBatchOperations {
		return &gapdb.Error{Code: gapdb.CodeBatchTooLarge, Message: "WAL commit exceeds the configured operation limit.", Retry: gapdb.RetryNever, ReceivedOperations: len(commit.Effects), MaximumOperations: limits.MaxBatchOperations, SafeActions: []gapdb.SafeAction{gapdb.ActionSplitBatch, gapdb.ActionAbort}}
	}
	seen := make(map[string]int, len(commit.Effects))
	for index, effect := range commit.Effects {
		if effect.Key == "" || !utf8.ValidString(effect.Key) {
			return corruptWAL("effect key is empty or invalid UTF-8", "", 0, nil)
		}
		if len(effect.Key) > limits.MaxKeyBytes {
			return &gapdb.Error{Code: gapdb.CodeKeyTooLarge, Message: "Key exceeds the configured limit.", Retry: gapdb.RetryNever, Field: "key", ReceivedBytes: len(effect.Key), MaximumBytes: limits.MaxKeyBytes, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceKey, gapdb.ActionAbort}}
		}
		if previous, ok := seen[effect.Key]; ok {
			return &gapdb.Error{Code: gapdb.CodeDuplicateKey, Message: "A key occurs more than once in the commit.", Retry: gapdb.RetryNever, Key: effect.Key, MutationIndexes: []int{previous, index}, SafeActions: []gapdb.SafeAction{gapdb.ActionDeduplicateBatch, gapdb.ActionAbort}}
		}
		seen[effect.Key] = index
		switch effect.Kind {
		case gapdb.ChangePut:
			if len(effect.Value) > limits.MaxValueBytes {
				return &gapdb.Error{Code: gapdb.CodeValueTooLarge, Message: "Value exceeds the configured limit.", Retry: gapdb.RetryNever, Field: "value_base64", ReceivedBytes: len(effect.Value), MaximumBytes: limits.MaxValueBytes, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceValue, gapdb.ActionAbort}}
			}
			if effect.ExpiresAt != nil {
				expires := effect.ExpiresAt.UTC()
				if !time.Unix(0, expires.UnixNano()).UTC().Equal(expires) {
					return corruptWAL("put expiry cannot be represented as Unix nanoseconds", "", 0, nil)
				}
			}
		case gapdb.ChangeDelete, gapdb.ChangeExpire:
			if len(effect.Value) != 0 || effect.ExpiresAt != nil {
				return corruptWAL("delete/expire cannot carry value or expiry", "", 0, nil)
			}
		default:
			return corruptWAL("effect kind is unknown", "", 0, nil)
		}
	}
	return nil
}

func effectPayloadLength(effects []Effect) (uint64, error) {
	var total uint64
	for _, effect := range effects {
		item := uint64(effectHeader) + uint64(len(effect.Key)) + uint64(len(effect.Value))
		if total > math.MaxUint64-item {
			return 0, corruptWAL("mutation payload length overflows", "", 0, nil)
		}
		total += item
	}
	return total, nil
}

func corruptWAL(reason, file string, offset int64, cause error) error {
	if file == "" {
		file = "WAL"
	}
	offsetCopy := offset
	return &gapdb.Error{Code: gapdb.CodeCorruptWAL, Message: "WAL is corrupt.", Retry: gapdb.RetryAfterOperator, Reason: reason, Offset: &offsetCopy, File: filepath.Base(file), SafeActions: []gapdb.SafeAction{gapdb.ActionInspectOffline, gapdb.ActionRecoverPropose, gapdb.ActionRestoreBackup, gapdb.ActionAbort}, Cause: cause}
}

func storageDegraded(stage string, current, durable gapdb.Revision, cause error) error {
	currentCopy, durableCopy := current, durable
	return &gapdb.Error{Code: gapdb.CodeStorageDegraded, Message: "Storage can no longer accept mutations safely.", Retry: gapdb.RetryAfterRestart, CurrentRevision: &currentCopy, DurableThroughRevision: &durableCopy, FailedStage: stage, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionRestartAfterRecovery, gapdb.ActionAbort}, Cause: cause}
}

func saturatedInt(value uint64) int {
	if value > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(value)
}
