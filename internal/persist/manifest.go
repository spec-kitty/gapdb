package persist

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
)

const (
	ManifestFilename  = "CURRENT"
	manifestHeaderLen = 16
	manifestTrailer   = 4
	maxManifestJSON   = 4096
)

type Manifest struct {
	DatabaseID       DatabaseID     `json:"-"`
	Generation       uint64         `json:"generation"`
	SnapshotFile     string         `json:"snapshot_file"`
	SnapshotRevision gapdb.Revision `json:"snapshot_revision"`
	SnapshotSHA256   string         `json:"snapshot_sha256"`
	WALFile          string         `json:"wal_file"`
	WALStartRevision gapdb.Revision `json:"wal_start_revision"`
}

type manifestJSON struct {
	DatabaseID       string         `json:"database_id"`
	Generation       uint64         `json:"generation"`
	SnapshotFile     string         `json:"snapshot_file"`
	SnapshotRevision gapdb.Revision `json:"snapshot_revision"`
	SnapshotSHA256   string         `json:"snapshot_sha256"`
	WALFile          string         `json:"wal_file"`
	WALStartRevision gapdb.Revision `json:"wal_start_revision"`
}

func EncodeManifest(manifest Manifest) ([]byte, error) {
	if err := validateManifest(manifest, 0, false); err != nil {
		return nil, err
	}
	wire := manifestJSON{
		DatabaseID:       manifest.DatabaseID.String(),
		Generation:       manifest.Generation,
		SnapshotFile:     manifest.SnapshotFile,
		SnapshotRevision: manifest.SnapshotRevision,
		SnapshotSHA256:   manifest.SnapshotSHA256,
		WALFile:          manifest.WALFile,
		WALStartRevision: manifest.WALStartRevision,
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, corruptManifest("JSON encoding failed", err)
	}
	if len(payload) > maxManifestJSON {
		return nil, corruptManifest("JSON payload exceeds the format bound", nil)
	}
	encoded := make([]byte, manifestHeaderLen+len(payload)+manifestTrailer)
	copy(encoded[:8], "GAPCUR01")
	binary.BigEndian.PutUint16(encoded[8:10], storageVersion)
	// bytes 10-11 are zero flags.
	binary.BigEndian.PutUint32(encoded[12:16], uint32(len(payload)))
	copy(encoded[16:], payload)
	binary.BigEndian.PutUint32(encoded[len(encoded)-4:], crc32.Checksum(encoded[:len(encoded)-4], castagnoli))
	return encoded, nil
}

func DecodeManifest(encoded []byte, expectedID DatabaseID, minimumGeneration uint64) (Manifest, error) {
	if len(encoded) < manifestHeaderLen+manifestTrailer {
		return Manifest{}, corruptManifest("frame is shorter than the fixed header and trailer", nil)
	}
	if string(encoded[:8]) != "GAPCUR01" {
		return Manifest{}, corruptManifest("magic does not match GAPCUR01", nil)
	}
	version := binary.BigEndian.Uint16(encoded[8:10])
	if version != storageVersion {
		return Manifest{}, unknownStorageFormat(ManifestFilename, uint64(version))
	}
	if binary.BigEndian.Uint16(encoded[10:12]) != 0 {
		return Manifest{}, corruptManifest("flags are nonzero", nil)
	}
	payloadLength := uint64(binary.BigEndian.Uint32(encoded[12:16]))
	if payloadLength > maxManifestJSON || payloadLength+manifestHeaderLen+manifestTrailer != uint64(len(encoded)) {
		return Manifest{}, corruptManifest("payload length does not match the frame", nil)
	}
	if got, want := crc32.Checksum(encoded[:len(encoded)-4], castagnoli), binary.BigEndian.Uint32(encoded[len(encoded)-4:]); got != want {
		return Manifest{}, corruptManifest("CRC32C mismatch", nil)
	}
	payload := encoded[16 : len(encoded)-4]
	var wire manifestJSON
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Manifest{}, corruptManifest("JSON payload is invalid", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Manifest{}, corruptManifest("JSON payload has trailing content", nil)
	}
	canonical, err := json.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, payload) {
		return Manifest{}, corruptManifest("JSON payload is not canonical field-ordered JSON", err)
	}
	id, err := ParseDatabaseID(wire.DatabaseID)
	if err != nil {
		return Manifest{}, corruptManifest("database ID is invalid", err)
	}
	manifest := Manifest{
		DatabaseID:       id,
		Generation:       wire.Generation,
		SnapshotFile:     wire.SnapshotFile,
		SnapshotRevision: wire.SnapshotRevision,
		SnapshotSHA256:   wire.SnapshotSHA256,
		WALFile:          wire.WALFile,
		WALStartRevision: wire.WALStartRevision,
	}
	if id != expectedID {
		return Manifest{}, databaseIDMismatch(expectedID, id, ManifestFilename)
	}
	if err := validateManifest(manifest, minimumGeneration, false); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ReadManifest(fsys faultfs.FS, directory string, expectedID DatabaseID, minimumGeneration uint64) (Manifest, error) {
	file, err := fsys.OpenFile(faultfs.PointOpen, filepath.Join(directory, ManifestFilename), os.O_RDONLY, 0)
	if err != nil {
		return Manifest{}, ioFailure("open", ManifestFilename, err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, manifestHeaderLen+maxManifestJSON+manifestTrailer+1))
	if err != nil {
		return Manifest{}, ioFailure("read", ManifestFilename, err)
	}
	if len(encoded) > manifestHeaderLen+maxManifestJSON+manifestTrailer {
		return Manifest{}, corruptManifest("frame exceeds the format bound", nil)
	}
	return DecodeManifest(encoded, expectedID, minimumGeneration)
}

func InstallManifest(fsys faultfs.FS, directory string, manifest Manifest, previousGeneration uint64, randomness io.Reader) error {
	if previousGeneration == math.MaxUint64 || manifest.Generation != previousGeneration+1 {
		return corruptManifest("generation must advance exactly once", nil)
	}
	if err := validateManifest(manifest, manifest.Generation, false); err != nil {
		return err
	}
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		return err
	}
	if randomness == nil {
		randomness = rand.Reader
	}
	suffix, err := randomSuffix(randomness)
	if err != nil {
		return ioFailure("generate_temp_name", ManifestFilename, err)
	}
	temp := filepath.Join(directory, ManifestFilename+".tmp-"+suffix)
	file, err := fsys.OpenFile(faultfs.PointManifestTempCreate, temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return ioFailure("manifest_temp_create", ManifestFilename, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := writeFull(fsys, faultfs.PointManifestTempWrite, file, encoded); err != nil {
		return ioFailure("manifest_temp_write", ManifestFilename, err)
	}
	if err := fsys.Sync(faultfs.PointManifestTempSync, file); err != nil {
		return ioFailure("manifest_temp_sync", ManifestFilename, err)
	}
	if err := file.Close(); err != nil {
		return ioFailure("manifest_temp_close", ManifestFilename, err)
	}
	closed = true
	if err := fsys.Rename(faultfs.PointManifestRename, temp, filepath.Join(directory, ManifestFilename)); err != nil {
		if faultfs.FailedAfter(err) {
			return ioFailureApplied("manifest_rename", ManifestFilename, err)
		}
		return ioFailure("manifest_rename", ManifestFilename, err)
	}
	if err := fsys.SyncDir(faultfs.PointManifestDirectorySync, directory); err != nil {
		return ioFailureApplied("manifest_directory_sync", ManifestFilename, err)
	}
	return nil
}

func validateManifest(manifest Manifest, minimumGeneration uint64, allowZeroGeneration bool) error {
	if manifest.DatabaseID.IsZero() {
		return corruptManifest("database ID is zero", nil)
	}
	if (!allowZeroGeneration && manifest.Generation == 0) || manifest.Generation < minimumGeneration {
		return corruptManifest("generation is zero or regressed", nil)
	}
	if uint64(manifest.SnapshotRevision) == math.MaxUint64 || manifest.WALStartRevision != manifest.SnapshotRevision+1 {
		return corruptManifest("WAL start revision must immediately follow the snapshot", nil)
	}
	if manifest.SnapshotFile != fmt.Sprintf("snapshot-%020d.gdb", manifest.SnapshotRevision) || filepath.Base(manifest.SnapshotFile) != manifest.SnapshotFile {
		return corruptManifest("snapshot file is not the canonical base name", nil)
	}
	if manifest.WALFile != fmt.Sprintf("wal-%020d.gdb", manifest.WALStartRevision) || filepath.Base(manifest.WALFile) != manifest.WALFile {
		return corruptManifest("WAL file is not the canonical base name", nil)
	}
	if len(manifest.SnapshotSHA256) != 64 || manifest.SnapshotSHA256 != string(bytes.ToLower([]byte(manifest.SnapshotSHA256))) {
		return corruptManifest("snapshot SHA-256 must be 64 lowercase hexadecimal characters", nil)
	}
	if _, err := hex.DecodeString(manifest.SnapshotSHA256); err != nil {
		return corruptManifest("snapshot SHA-256 is not hexadecimal", err)
	}
	return nil
}

func corruptManifest(reason string, cause error) error {
	return &gapdb.Error{Code: gapdb.CodeCorruptManifest, Message: "Manifest is corrupt.", Retry: gapdb.RetryAfterOperator, Reason: reason, File: ManifestFilename, SafeActions: []gapdb.SafeAction{gapdb.ActionInspectOffline, gapdb.ActionRecoverPropose, gapdb.ActionRestoreBackup, gapdb.ActionAbort}, Cause: cause}
}

func databaseIDMismatch(expected, actual DatabaseID, file string) error {
	return &gapdb.Error{Code: gapdb.CodeDatabaseIDMismatch, Message: "Database identity does not match.", Retry: gapdb.RetryNever, ExpectedDatabaseID: expected.String(), ActualDatabaseID: actual.String(), File: filepath.Base(file), SafeActions: []gapdb.SafeAction{gapdb.ActionSelectCorrectDatabase, gapdb.ActionAbort}}
}
