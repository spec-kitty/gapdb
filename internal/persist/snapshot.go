package persist

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
)

const (
	SnapshotHeaderSize = 64
	snapshotItemHeader = 40
	snapshotTrailer    = 32
	MaxSnapshotBytes   = 1 << 30
)

func EncodeSnapshot(databaseID DatabaseID, state SnapshotState, asOf time.Time, limits gapdb.Limits) ([]byte, [32]byte, error) {
	if databaseID.IsZero() || limits.MaxKeyBytes <= 0 || limits.MaxValueBytes <= 0 {
		return nil, [32]byte{}, corruptSnapshot("database ID and snapshot limits must be valid", "", 0, nil)
	}
	records := make([]gapdb.Record, 0, len(state.Records))
	for _, record := range state.Records {
		if record.ExpiresAt != nil && !record.ExpiresAt.After(asOf) {
			continue
		}
		records = append(records, record.Clone())
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	payloadBytes := uint64(0)
	for index, record := range records {
		if record.Key == "" || !utf8.ValidString(record.Key) || len(record.Key) > limits.MaxKeyBytes || len(record.Value) > limits.MaxValueBytes || record.Revision == 0 || record.Revision > state.Revision {
			return nil, [32]byte{}, corruptSnapshot("snapshot record fields are invalid", "", int64(index), nil)
		}
		if index != 0 && records[index-1].Key == record.Key {
			return nil, [32]byte{}, corruptSnapshot("snapshot contains duplicate keys", "", int64(index), nil)
		}
		if record.ExpiresAt != nil && !unixNanoRepresentable(*record.ExpiresAt) {
			return nil, [32]byte{}, corruptSnapshot("snapshot expiry is outside the signed Unix nanosecond range", "", int64(index), nil)
		}
		addition := uint64(snapshotItemHeader) + uint64(len(record.Key)) + uint64(len(record.Value))
		if payloadBytes > math.MaxUint64-addition {
			return nil, [32]byte{}, corruptSnapshot("snapshot payload length overflows", "", int64(index), nil)
		}
		payloadBytes += addition
	}
	total := uint64(SnapshotHeaderSize+snapshotTrailer) + payloadBytes
	if total > MaxSnapshotBytes || total > uint64(math.MaxInt) {
		return nil, [32]byte{}, corruptSnapshot("snapshot exceeds the format size bound", "", 0, nil)
	}
	encoded := make([]byte, int(total))
	copy(encoded[:8], "GAPSNAP1")
	binary.BigEndian.PutUint16(encoded[8:10], storageVersion)
	binary.BigEndian.PutUint16(encoded[10:12], SnapshotHeaderSize)
	copy(encoded[12:28], databaseID[:])
	binary.BigEndian.PutUint64(encoded[28:36], uint64(state.Revision))
	binary.BigEndian.PutUint64(encoded[36:44], uint64(len(records)))
	binary.BigEndian.PutUint64(encoded[44:52], payloadBytes)
	offset := SnapshotHeaderSize
	for _, record := range records {
		binary.BigEndian.PutUint32(encoded[offset:offset+4], uint32(len(record.Key)))
		binary.BigEndian.PutUint32(encoded[offset+4:offset+8], uint32(len(record.Value)))
		binary.BigEndian.PutUint64(encoded[offset+8:offset+16], uint64(record.Revision))
		if record.ExpiresAt != nil {
			encoded[offset+16] = 1
			binary.BigEndian.PutUint64(encoded[offset+24:offset+32], uint64(record.ExpiresAt.UTC().UnixNano()))
		}
		offset += snapshotItemHeader
		copy(encoded[offset:], record.Key)
		offset += len(record.Key)
		copy(encoded[offset:], record.Value)
		offset += len(record.Value)
	}
	digest := sha256.Sum256(encoded[:offset])
	copy(encoded[offset:], digest[:])
	return encoded, digest, nil
}

func DecodeSnapshot(encoded []byte, expectedID DatabaseID, expectedDigest [32]byte, limits gapdb.Limits) (SnapshotState, error) {
	if len(encoded) < SnapshotHeaderSize+snapshotTrailer || len(encoded) > MaxSnapshotBytes {
		return SnapshotState{}, corruptSnapshot("snapshot size is outside bounds", "", 0, nil)
	}
	if string(encoded[:8]) != "GAPSNAP1" {
		return SnapshotState{}, corruptSnapshot("magic does not match GAPSNAP1", "", 0, nil)
	}
	version := binary.BigEndian.Uint16(encoded[8:10])
	if version != storageVersion {
		return SnapshotState{}, unknownStorageFormat("snapshot", uint64(version))
	}
	if binary.BigEndian.Uint16(encoded[10:12]) != SnapshotHeaderSize || !allZero(encoded[52:64]) {
		return SnapshotState{}, corruptSnapshot("header length or reserved bytes are invalid", "", 0, nil)
	}
	var databaseID DatabaseID
	copy(databaseID[:], encoded[12:28])
	if databaseID != expectedID {
		return SnapshotState{}, databaseIDMismatch(expectedID, databaseID, "snapshot")
	}
	revision := gapdb.Revision(binary.BigEndian.Uint64(encoded[28:36]))
	count := binary.BigEndian.Uint64(encoded[36:44])
	payloadBytes := binary.BigEndian.Uint64(encoded[44:52])
	if payloadBytes+SnapshotHeaderSize+snapshotTrailer != uint64(len(encoded)) || count > payloadBytes/snapshotItemHeader {
		return SnapshotState{}, corruptSnapshot("revision, count, or payload length is invalid", "", 0, nil)
	}
	digest := sha256.Sum256(encoded[:len(encoded)-snapshotTrailer])
	if digest != expectedDigest || !bytes.Equal(digest[:], encoded[len(encoded)-snapshotTrailer:]) {
		return SnapshotState{}, corruptSnapshot("SHA-256 mismatch", "", int64(len(encoded)-snapshotTrailer), nil)
	}
	state := SnapshotState{Revision: revision, Records: make([]gapdb.Record, 0, int(count))}
	offset, payloadEnd := SnapshotHeaderSize, len(encoded)-snapshotTrailer
	for index := uint64(0); index < count; index++ {
		if offset > payloadEnd-snapshotItemHeader {
			return SnapshotState{}, corruptSnapshot("record header is truncated", "", int64(offset), nil)
		}
		keyBytes := uint64(binary.BigEndian.Uint32(encoded[offset : offset+4]))
		valueBytes := uint64(binary.BigEndian.Uint32(encoded[offset+4 : offset+8]))
		recordRevision := gapdb.Revision(binary.BigEndian.Uint64(encoded[offset+8 : offset+16]))
		flags := encoded[offset+16]
		expiryNanos := int64(binary.BigEndian.Uint64(encoded[offset+24 : offset+32]))
		if flags&^byte(1) != 0 || !allZero(encoded[offset+17:offset+24]) || !allZero(encoded[offset+32:offset+40]) {
			return SnapshotState{}, corruptSnapshot("record flags or reserved bytes are invalid", "", int64(offset), nil)
		}
		offset += snapshotItemHeader
		remaining := uint64(payloadEnd - offset)
		if keyBytes == 0 || keyBytes > uint64(limits.MaxKeyBytes) || valueBytes > uint64(limits.MaxValueBytes) || keyBytes > remaining || valueBytes > remaining-keyBytes {
			return SnapshotState{}, corruptSnapshot("record key/value lengths exceed bounds", "", int64(offset), nil)
		}
		keyEnd, valueEnd := offset+int(keyBytes), offset+int(keyBytes+valueBytes)
		key := string(encoded[offset:keyEnd])
		if !utf8.ValidString(key) || recordRevision == 0 || recordRevision > revision || (len(state.Records) != 0 && state.Records[len(state.Records)-1].Key >= key) {
			return SnapshotState{}, corruptSnapshot("records are invalid, duplicated, or not strictly sorted", "", int64(offset), nil)
		}
		var expiresAt *time.Time
		if flags == 1 {
			expires := time.Unix(0, expiryNanos).UTC()
			expiresAt = &expires
		} else if expiryNanos != 0 {
			return SnapshotState{}, corruptSnapshot("expiry is nonzero without its flag", "", int64(offset), nil)
		}
		state.Records = append(state.Records, gapdb.NewRecord(key, encoded[keyEnd:valueEnd], recordRevision, expiresAt))
		offset = valueEnd
	}
	if offset != payloadEnd {
		return SnapshotState{}, corruptSnapshot("snapshot payload has trailing bytes", "", int64(offset), nil)
	}
	return state, nil
}

type SnapshotFileStore struct{ Limits gapdb.Limits }

func (store SnapshotFileStore) Load(fsys faultfs.FS, directory string, manifest Manifest) (SnapshotState, error) {
	if store.Limits.MaxKeyBytes <= 0 || store.Limits.MaxValueBytes <= 0 {
		store.Limits = gapdb.DefaultOptions().Limits
	}
	file, err := fsys.OpenFile(faultfs.PointOpen, filepath.Join(directory, manifest.SnapshotFile), os.O_RDONLY, 0)
	if err != nil {
		return SnapshotState{}, ioFailure("snapshot_open", manifest.SnapshotFile, err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, MaxSnapshotBytes+1))
	if err != nil {
		return SnapshotState{}, ioFailure("snapshot_read", manifest.SnapshotFile, err)
	}
	if len(encoded) > MaxSnapshotBytes {
		return SnapshotState{}, corruptSnapshot("snapshot exceeds the format size bound", manifest.SnapshotFile, 0, nil)
	}
	digestBytes, err := hex.DecodeString(manifest.SnapshotSHA256)
	if err != nil || len(digestBytes) != sha256.Size {
		return SnapshotState{}, corruptSnapshot("manifest snapshot digest is invalid", manifest.SnapshotFile, 0, err)
	}
	var digest [32]byte
	copy(digest[:], digestBytes)
	state, err := DecodeSnapshot(encoded, manifest.DatabaseID, digest, store.Limits)
	if err != nil {
		return SnapshotState{}, withSnapshotFile(err, manifest.SnapshotFile)
	}
	return state, nil
}

type SnapshotInstallOptions struct {
	FS            faultfs.FS
	Directory     string
	Current       Manifest
	State         SnapshotState
	AsOf          time.Time
	Limits        gapdb.Limits
	Random        io.Reader
	WALBufferSize int
	Barrier       func() error
}

type SnapshotInstallResult struct {
	Manifest Manifest
	WAL      *WAL
	Duration time.Duration
}

func InstallSnapshotGeneration(options SnapshotInstallOptions) (SnapshotInstallResult, error) {
	started := time.Now()
	if options.FS == nil || options.Barrier == nil || options.Current.DatabaseID.IsZero() || options.Current.Generation == math.MaxUint64 || options.State.Revision < options.Current.SnapshotRevision || options.State.Revision == gapdb.Revision(math.MaxUint64) {
		return SnapshotInstallResult{}, corruptSnapshot("snapshot installation options are invalid", "", 0, nil)
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.Limits.MaxKeyBytes <= 0 || options.Limits.MaxValueBytes <= 0 {
		options.Limits = gapdb.DefaultOptions().Limits
	}
	if options.WALBufferSize <= 0 {
		options.WALBufferSize = 4096
	}
	if err := options.Barrier(); err != nil {
		return SnapshotInstallResult{}, err
	}
	encoded, digest, err := EncodeSnapshot(options.Current.DatabaseID, options.State, options.AsOf, options.Limits)
	if err != nil {
		return SnapshotInstallResult{}, err
	}
	snapshotName := "snapshot-" + leftPadRevision(options.State.Revision) + ".gdb"
	if err := installSnapshotFile(options.FS, options.Directory, snapshotName, encoded, options.Random); err != nil {
		return SnapshotInstallResult{}, err
	}
	nextRevision := options.State.Revision + 1
	walName := "wal-" + leftPadRevision(nextRevision) + ".gdb"
	header := WALHeader{DatabaseID: options.Current.DatabaseID, FirstRevision: nextRevision, Generation: options.Current.Generation + 1}
	if err := installNextWAL(options.FS, options.Directory, walName, header, options.Random); err != nil {
		return SnapshotInstallResult{}, err
	}
	nextWAL, err := OpenWALForAppend(options.FS, filepath.Join(options.Directory, walName), header, options.State.Revision, options.State.Revision, options.WALBufferSize, options.Limits)
	if err != nil {
		return SnapshotInstallResult{}, err
	}
	manifest := Manifest{DatabaseID: options.Current.DatabaseID, Generation: options.Current.Generation + 1, SnapshotFile: snapshotName, SnapshotRevision: options.State.Revision, SnapshotSHA256: hex.EncodeToString(digest[:]), WALFile: walName, WALStartRevision: nextRevision}
	if err := InstallManifest(options.FS, options.Directory, manifest, options.Current.Generation, options.Random); err != nil {
		_ = nextWAL.Close()
		return SnapshotInstallResult{Manifest: manifest, Duration: time.Since(started)}, err
	}
	return SnapshotInstallResult{Manifest: manifest, WAL: nextWAL, Duration: time.Since(started)}, nil
}

func installSnapshotFile(fsys faultfs.FS, directory, name string, encoded []byte, randomness io.Reader) error {
	final := filepath.Join(directory, name)
	if _, err := fsys.Stat(faultfs.PointStat, final); err == nil {
		return ioFailure("snapshot_destination_exists", name, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ioFailure("snapshot_stat", name, err)
	}
	suffix, err := randomSuffix(randomness)
	if err != nil {
		return ioFailure("snapshot_temp_name", name, err)
	}
	temp := final + ".tmp-" + suffix
	file, err := fsys.OpenFile(faultfs.PointSnapshotTempCreate, temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return ioFailure("snapshot_temp_create", name, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := writeFull(fsys, faultfs.PointSnapshotTempWrite, file, encoded); err != nil {
		return ioFailure("snapshot_temp_write", name, err)
	}
	if err := fsys.Sync(faultfs.PointSnapshotTempSync, file); err != nil {
		return ioFailure("snapshot_temp_sync", name, err)
	}
	if err := file.Close(); err != nil {
		return ioFailure("snapshot_temp_close", name, err)
	}
	closed = true
	if err := fsys.Rename(faultfs.PointSnapshotRename, temp, final); err != nil {
		if faultfs.FailedAfter(err) {
			return ioFailureApplied("snapshot_rename", name, err)
		}
		return ioFailure("snapshot_rename", name, err)
	}
	if err := fsys.SyncDir(faultfs.PointSnapshotDirectorySync, directory); err != nil {
		return ioFailureApplied("snapshot_directory_sync", name, err)
	}
	return nil
}

func installNextWAL(fsys faultfs.FS, directory, name string, header WALHeader, randomness io.Reader) error {
	final := filepath.Join(directory, name)
	if _, err := fsys.Stat(faultfs.PointStat, final); err == nil {
		return ioFailure("next_wal_destination_exists", name, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ioFailure("next_wal_stat", name, err)
	}
	encoded, err := EncodeWALHeader(header)
	if err != nil {
		return err
	}
	suffix, err := randomSuffix(randomness)
	if err != nil {
		return ioFailure("next_wal_temp_name", name, err)
	}
	temp := final + ".tmp-" + suffix
	file, err := fsys.OpenFile(faultfs.PointNextWALCreate, temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return ioFailure("next_wal_create", name, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if written, err := file.Write(encoded); err != nil {
		return ioFailure("next_wal_write", name, err)
	} else if written != len(encoded) {
		return ioFailure("next_wal_write", name, io.ErrShortWrite)
	}
	if err := fsys.Sync(faultfs.PointNextWALSync, file); err != nil {
		return ioFailure("next_wal_sync", name, err)
	}
	if err := file.Close(); err != nil {
		return ioFailure("next_wal_close", name, err)
	}
	closed = true
	if err := fsys.Rename(faultfs.PointNextWALRename, temp, final); err != nil {
		if faultfs.FailedAfter(err) {
			return ioFailureApplied("next_wal_rename", name, err)
		}
		return ioFailure("next_wal_rename", name, err)
	}
	if err := fsys.SyncDir(faultfs.PointNextWALSync, directory); err != nil {
		return ioFailureApplied("next_wal_directory_sync", name, err)
	}
	return nil
}

func unixNanoRepresentable(value time.Time) bool {
	value = value.UTC()
	return time.Unix(0, value.UnixNano()).UTC().Equal(value)
}

func withSnapshotFile(err error, file string) error {
	var structured *gapdb.Error
	if !errors.As(err, &structured) {
		return err
	}
	clone := structured.Clone()
	if clone.Code == gapdb.CodeCorruptSnapshot || clone.Code == gapdb.CodeUnknownFormat || clone.Code == gapdb.CodeDatabaseIDMismatch {
		clone.File = filepath.Base(file)
	}
	return clone
}

func leftPadRevision(revision gapdb.Revision) string {
	value := revision.String()
	return string(bytes.Repeat([]byte{'0'}, 20-len(value))) + value
}
