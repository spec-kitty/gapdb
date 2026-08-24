package persist

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
)

const BackupMetadataFilename = "backup.json"

type BackupFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type BackupMetadata struct {
	SchemaVersion    uint16         `json:"schema_version"`
	SourceDatabaseID string         `json:"source_database_id"`
	BackupID         string         `json:"backup_id"`
	DurableRevision  gapdb.Revision `json:"durable_revision"`
	CreatedAt        string         `json:"created_at"`
	Files            []BackupFile   `json:"files"`
	ToolVersion      string         `json:"tool_version"`
	Destination      string         `json:"-"`
}

type BackupOptions struct {
	FS                      faultfs.FS
	SourceDirectory         string
	Destination             string
	ExpectedDatabaseID      DatabaseID
	ExpectedDurableRevision gapdb.Revision
	CreatedAt               time.Time
	BackupID                string
	ToolVersion             string
	Limits                  gapdb.Limits
	Barrier                 func() error
}

func CreateBackup(options BackupOptions) (BackupMetadata, error) {
	if err := validateSeparateNewDestination(options.SourceDirectory, options.Destination); err != nil {
		return BackupMetadata{}, err
	}
	if options.CreatedAt.IsZero() || options.BackupID == "" || options.ToolVersion == "" {
		return BackupMetadata{}, adminPrecondition("backup ID, creation time, and tool version are required")
	}
	if options.FS == nil || options.ExpectedDatabaseID.IsZero() || options.Barrier == nil {
		return BackupMetadata{}, adminPrecondition("backup identity and durability barrier are required")
	}
	if err := options.Barrier(); err != nil {
		return BackupMetadata{}, err
	}
	identity, manifest, revision, err := verifyGeneration(options.FS, options.SourceDirectory, options.ExpectedDatabaseID, options.Limits)
	if err != nil {
		return BackupMetadata{}, err
	}
	if identity.DatabaseID != options.ExpectedDatabaseID || revision != options.ExpectedDurableRevision {
		return BackupMetadata{}, adminPrecondition("backup database ID or durable revision is stale")
	}
	temp, err := exclusiveSiblingTemp(options.Destination)
	if err != nil {
		return BackupMetadata{}, ioFailure("backup_temp_create", filepath.Base(options.Destination), err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(temp)
		}
	}()
	names := []string{IdentityFilename, ManifestFilename, manifest.SnapshotFile, manifest.WALFile}
	metadata := BackupMetadata{SchemaVersion: 1, SourceDatabaseID: identity.DatabaseID.String(), BackupID: options.BackupID, DurableRevision: revision, CreatedAt: options.CreatedAt.UTC().Format(time.RFC3339Nano), ToolVersion: options.ToolVersion}
	for _, name := range names {
		entry, copyErr := copyAndSync(options.FS, filepath.Join(options.SourceDirectory, name), filepath.Join(temp, name))
		if copyErr != nil {
			return BackupMetadata{}, copyErr
		}
		entry.Name = name
		metadata.Files = append(metadata.Files, entry)
	}
	sort.Slice(metadata.Files, func(i, j int) bool { return metadata.Files[i].Name < metadata.Files[j].Name })
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return BackupMetadata{}, backupInvalid("metadata encoding failed", err)
	}
	encoded = append(encoded, '\n')
	if err := writeSyncedFile(options.FS, filepath.Join(temp, BackupMetadataFilename), encoded); err != nil {
		return BackupMetadata{}, err
	}
	if _, err := VerifyBackup(options.FS, temp, options.Limits); err != nil {
		return BackupMetadata{}, err
	}
	if err := options.FS.SyncDir(faultfs.PointBackupFileSync, temp); err != nil {
		return BackupMetadata{}, ioFailure("backup_directory_sync", filepath.Base(temp), err)
	}
	tempIdentity, err := pathIdentity(temp)
	if err != nil {
		return BackupMetadata{}, ioFailure("backup_temp_identity", filepath.Base(temp), err)
	}
	publication, err := faultfs.PublishNoReplaceAnchored(options.FS, faultfs.PointBackupRename, faultfs.PointPublicationDirectorySync, filepath.Dir(temp), filepath.Base(temp), filepath.Dir(options.Destination), filepath.Base(options.Destination), tempIdentity)
	if publication.DestinationPath != "" {
		metadata.Destination = publication.DestinationPath
	}
	if err != nil {
		if errors.Is(err, faultfs.ErrIdentityChanged) {
			// The path used for cleanup is no longer trusted. Leave the verified
			// temp on its pinned filesystem for explicit operator reconciliation.
			keep = true
		}
		if errors.Is(err, os.ErrExist) {
			return BackupMetadata{}, backupDestinationExists(options.Destination)
		}
		if faultfs.FailedAfter(err) {
			keep = true
			return metadata, ioFailureApplied("backup_rename", filepath.Base(options.Destination), err)
		}
		return BackupMetadata{}, ioFailure("backup_rename", filepath.Base(options.Destination), err)
	}
	keep = true
	if metadata.Destination == "" {
		metadata.Destination = options.Destination
	}
	return metadata, nil
}

func VerifyBackup(fsys faultfs.FS, directory string, limits gapdb.Limits) (BackupMetadata, error) {
	encoded, err := readSmallFile(fsys, filepath.Join(directory, BackupMetadataFilename), 64<<10)
	if err != nil {
		return BackupMetadata{}, backupInvalid("backup metadata cannot be read", err)
	}
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return BackupMetadata{}, backupInvalid("backup metadata is not newline-terminated canonical JSON", nil)
	}
	var metadata BackupMetadata
	decoder := json.NewDecoder(bytes.NewReader(encoded[:len(encoded)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return BackupMetadata{}, backupInvalid("backup metadata JSON is invalid", err)
	}
	canonical, _ := json.Marshal(metadata)
	createdAt, timeErr := time.Parse(time.RFC3339Nano, metadata.CreatedAt)
	if !bytes.Equal(canonical, encoded[:len(encoded)-1]) || metadata.SchemaVersion != 1 || metadata.BackupID == "" || metadata.ToolVersion == "" || timeErr != nil || createdAt.Format(time.RFC3339Nano) != metadata.CreatedAt || len(metadata.Files) != 4 {
		return BackupMetadata{}, backupInvalid("backup metadata is noncanonical or incomplete", nil)
	}
	id, err := ParseDatabaseID(metadata.SourceDatabaseID)
	if err != nil {
		return BackupMetadata{}, backupInvalid("source database ID is invalid", err)
	}
	entries, err := fsys.ReadDir(faultfs.PointReadDir, directory)
	if err != nil {
		return BackupMetadata{}, backupInvalid("backup directory cannot be listed", err)
	}
	allowed := map[string]bool{BackupMetadataFilename: true}
	for _, file := range metadata.Files {
		if filepath.Base(file.Name) != file.Name || allowed[file.Name] {
			return BackupMetadata{}, backupInvalid("backup file list is unsafe or duplicated", nil)
		}
		allowed[file.Name] = true
		size, digest, readErr := hashFile(fsys, filepath.Join(directory, file.Name))
		if readErr != nil {
			return BackupMetadata{}, backupInvalid("backup file cannot be read", readErr)
		}
		if size != file.Size || digest != file.SHA256 {
			return BackupMetadata{}, backupInvalid("backup file hash or size mismatch", nil)
		}
	}
	for _, entry := range entries {
		if entry.IsDir() || !allowed[entry.Name()] || len(entries) != 5 {
			return BackupMetadata{}, backupInvalid("backup contains unexpected entries", nil)
		}
	}
	_, _, revision, err := verifyGeneration(fsys, directory, id, limits)
	if err != nil {
		return BackupMetadata{}, backupInvalid("backup generation verification failed", err)
	}
	if revision != metadata.DurableRevision {
		return BackupMetadata{}, backupInvalid("durable revision does not match backup metadata", nil)
	}
	metadata.Destination = directory
	return metadata, nil
}

func RestoreBackup(fsys faultfs.FS, backupDirectory, destination string, limits gapdb.Limits) (BackupMetadata, error) {
	if err := validateSeparateNewDestination(backupDirectory, destination); err != nil {
		return BackupMetadata{}, err
	}
	metadata, err := VerifyBackup(fsys, backupDirectory, limits)
	if err != nil {
		return BackupMetadata{}, err
	}
	temp, err := exclusiveSiblingTemp(destination)
	if err != nil {
		return BackupMetadata{}, ioFailure("restore_temp_create", filepath.Base(destination), err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(temp)
		}
	}()
	for _, name := range append(fileNames(metadata.Files), BackupMetadataFilename) {
		if _, err := copyAndSync(fsys, filepath.Join(backupDirectory, name), filepath.Join(temp, name)); err != nil {
			return BackupMetadata{}, err
		}
	}
	if _, err := VerifyBackup(fsys, temp, limits); err != nil {
		return BackupMetadata{}, err
	}
	if err := fsys.SyncDir(faultfs.PointBackupFileSync, temp); err != nil {
		return BackupMetadata{}, ioFailure("restore_directory_sync", filepath.Base(temp), err)
	}
	tempIdentity, err := pathIdentity(temp)
	if err != nil {
		return BackupMetadata{}, ioFailure("restore_temp_identity", filepath.Base(temp), err)
	}
	publication, err := faultfs.PublishNoReplaceAnchored(fsys, faultfs.PointBackupRename, faultfs.PointPublicationDirectorySync, filepath.Dir(temp), filepath.Base(temp), filepath.Dir(destination), filepath.Base(destination), tempIdentity)
	if publication.DestinationPath != "" {
		metadata.Destination = publication.DestinationPath
	}
	if err != nil {
		if errors.Is(err, faultfs.ErrIdentityChanged) {
			keep = true
		}
		if errors.Is(err, os.ErrExist) {
			return BackupMetadata{}, backupDestinationExists(destination)
		}
		if faultfs.FailedAfter(err) {
			keep = true
			return metadata, ioFailureApplied("restore_rename", filepath.Base(destination), err)
		}
		return BackupMetadata{}, ioFailure("restore_rename", filepath.Base(destination), err)
	}
	keep = true
	if metadata.Destination == "" {
		metadata.Destination = destination
	}
	return metadata, nil
}

func pathIdentity(path string) (faultfs.FileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return faultfs.FileIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return faultfs.FileIdentity{}, os.ErrInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return faultfs.FileIdentity{}, faultfs.ErrIdentityChanged
	}
	return faultfs.FileIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func verifyGeneration(fsys faultfs.FS, directory string, expectedID DatabaseID, limits gapdb.Limits) (Identity, Manifest, gapdb.Revision, error) {
	if limits.MaxKeyBytes <= 0 {
		limits = gapdb.DefaultOptions().Limits
	}
	identity, err := ReadIdentity(fsys, directory)
	if err != nil {
		return Identity{}, Manifest{}, 0, err
	}
	if identity.DatabaseID != expectedID {
		return Identity{}, Manifest{}, 0, databaseIDMismatch(expectedID, identity.DatabaseID, IdentityFilename)
	}
	manifest, err := ReadManifest(fsys, directory, expectedID, 1)
	if err != nil {
		return Identity{}, Manifest{}, 0, err
	}
	if _, err := (SnapshotFileStore{Limits: limits}).Load(fsys, directory, manifest); err != nil {
		return Identity{}, Manifest{}, 0, err
	}
	path := filepath.Join(directory, manifest.WALFile)
	file, err := fsys.OpenFile(faultfs.PointOpen, path, os.O_RDONLY, 0)
	if err != nil {
		return Identity{}, Manifest{}, 0, ioFailure("backup_wal_open", manifest.WALFile, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Identity{}, Manifest{}, 0, ioFailure("backup_wal_stat", manifest.WALFile, err)
	}
	replay, err := ScanWAL(file, info.Size(), expectedID, manifest.SnapshotRevision, identity.ReservedRevisionEnd, limits)
	if err != nil {
		return Identity{}, Manifest{}, 0, withWALContext(err, manifest.WALFile, 0)
	}
	if replay.Header.Generation != manifest.Generation || replay.Header.FirstRevision != manifest.WALStartRevision || replay.Tail != nil {
		return Identity{}, Manifest{}, 0, corruptWAL("backup WAL is not a complete authoritative generation", manifest.WALFile, replay.CompleteOffset, nil)
	}
	return identity, manifest, replay.RecoveredRevision, nil
}

// VerifyGeneration validates the complete immutable authority referenced by
// CURRENT without repairing a WAL tail or changing any file.
func VerifyGeneration(fsys faultfs.FS, directory string, expectedID DatabaseID, limits gapdb.Limits) (Identity, Manifest, gapdb.Revision, error) {
	return verifyGeneration(fsys, directory, expectedID, limits)
}

func copyAndSync(fsys faultfs.FS, source, destination string) (BackupFile, error) {
	input, err := fsys.OpenFile(faultfs.PointOpen, source, os.O_RDONLY, 0)
	if err != nil {
		return BackupFile{}, ioFailure("backup_source_read", filepath.Base(source), err)
	}
	defer input.Close()
	output, err := fsys.OpenFile(faultfs.PointBackupFileCopy, destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return BackupFile{}, ioFailure("backup_file_create", filepath.Base(destination), err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = output.Close()
		}
	}()
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var size int64
	for {
		n, readErr := input.Read(buffer)
		if n > 0 {
			if _, hashErr := hash.Write(buffer[:n]); hashErr != nil {
				return BackupFile{}, hashErr
			}
			if writeErr := writeFull(fsys, faultfs.PointBackupFileCopy, output, buffer[:n]); writeErr != nil {
				return BackupFile{}, ioFailure("backup_file_copy", filepath.Base(destination), writeErr)
			}
			size += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return BackupFile{}, ioFailure("backup_source_read", filepath.Base(source), readErr)
		}
		if n == 0 {
			return BackupFile{}, ioFailure("backup_source_read", filepath.Base(source), io.ErrNoProgress)
		}
	}
	if err := fsys.Sync(faultfs.PointBackupFileSync, output); err != nil {
		return BackupFile{}, ioFailure("backup_file_sync", filepath.Base(destination), err)
	}
	if err := output.Close(); err != nil {
		return BackupFile{}, ioFailure("backup_file_close", filepath.Base(destination), err)
	}
	closed = true
	return BackupFile{Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func hashFile(fsys faultfs.FS, path string) (int64, string, error) {
	file, err := fsys.OpenFile(faultfs.PointOpen, path, os.O_RDONLY, 0)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.CopyBuffer(hash, file, make([]byte, 64<<10))
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func writeSyncedFile(fsys faultfs.FS, path string, value []byte) error {
	file, err := fsys.OpenFile(faultfs.PointBackupFileCopy, path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return ioFailure("backup_file_create", filepath.Base(path), err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := writeFull(fsys, faultfs.PointBackupFileCopy, file, value); err != nil {
		return ioFailure("backup_file_copy", filepath.Base(path), err)
	}
	if err := fsys.Sync(faultfs.PointBackupFileSync, file); err != nil {
		return ioFailure("backup_file_sync", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return ioFailure("backup_file_close", filepath.Base(path), err)
	}
	closed = true
	return nil
}

func readSmallFile(fsys faultfs.FS, path string, maximum int) ([]byte, error) {
	file, err := fsys.OpenFile(faultfs.PointOpen, path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(value) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return value, nil
}
func fileNames(files []BackupFile) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return names
}
func validateSeparateNewDestination(source, destination string) error {
	err := ValidateNewDestination(source, destination)
	if errors.Is(err, ErrDestinationExists) {
		return backupDestinationExists(destination)
	}
	if err != nil {
		return adminPrecondition(err.Error())
	}
	return nil
}

func backupDestinationExists(destination string) error {
	return &gapdb.Error{Code: gapdb.CodeBackupDestinationExists, Message: "Backup destination already exists.", Retry: gapdb.RetryNever, Path: filepath.Base(destination), SafeActions: []gapdb.SafeAction{gapdb.ActionChooseNewDestination, gapdb.ActionAbort}}
}
func exclusiveSiblingTemp(destination string) (string, error) {
	suffix := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, suffix); err != nil {
		return "", err
	}
	temp := destination + ".tmp-" + hex.EncodeToString(suffix)
	if err := os.Mkdir(temp, 0o700); err != nil {
		return "", err
	}
	return temp, nil
}
func backupInvalid(reason string, cause error) error {
	return &gapdb.Error{Code: gapdb.CodeBackupInvalid, Message: "Backup verification failed.", Retry: gapdb.RetryAfterOperator, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionVerifyBackup, gapdb.ActionChooseOtherBackup, gapdb.ActionAbort}, Cause: cause}
}
