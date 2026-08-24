package persist

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
	"golang.org/x/sys/unix"
)

const (
	IdentityFilename = "IDENTITY"
	IdentitySize     = 64
	storageVersion   = uint16(1)
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

type DatabaseID [16]byte

func ParseDatabaseID(value string) (DatabaseID, error) {
	var result DatabaseID
	if len(value) != 32 || value != string(bytes.ToLower([]byte(value))) {
		return result, fmt.Errorf("database ID must be 32 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return result, fmt.Errorf("database ID: %w", err)
	}
	copy(result[:], decoded)
	return result, nil
}

func (id DatabaseID) String() string { return hex.EncodeToString(id[:]) }

func (id DatabaseID) IsZero() bool { return id == DatabaseID{} }

type Identity struct {
	DatabaseID          DatabaseID
	ReservedRevisionEnd gapdb.Revision
	Generation          uint64
}

type RevisionRange struct {
	First gapdb.Revision
	End   gapdb.Revision
}

func EncodeIdentity(identity Identity) ([]byte, error) {
	if identity.DatabaseID.IsZero() {
		return nil, corruptIdentity("database ID is zero", nil)
	}
	if identity.Generation == 0 {
		return nil, corruptIdentity("generation must be greater than zero", nil)
	}
	encoded := make([]byte, IdentitySize)
	copy(encoded[:8], "GAPID001")
	binary.BigEndian.PutUint16(encoded[8:10], storageVersion)
	binary.BigEndian.PutUint16(encoded[10:12], IdentitySize)
	copy(encoded[12:28], identity.DatabaseID[:])
	binary.BigEndian.PutUint64(encoded[28:36], uint64(identity.ReservedRevisionEnd))
	binary.BigEndian.PutUint64(encoded[36:44], identity.Generation)
	binary.BigEndian.PutUint32(encoded[60:64], crc32.Checksum(encoded[:60], castagnoli))
	return encoded, nil
}

func DecodeIdentity(encoded []byte) (Identity, error) {
	if len(encoded) != IdentitySize {
		return Identity{}, corruptIdentity(fmt.Sprintf("size is %d, want %d", len(encoded), IdentitySize), nil)
	}
	if string(encoded[:8]) != "GAPID001" {
		return Identity{}, corruptIdentity("magic does not match GAPID001", nil)
	}
	version := binary.BigEndian.Uint16(encoded[8:10])
	if version != storageVersion {
		return Identity{}, unknownStorageFormat(IdentityFilename, uint64(version))
	}
	if binary.BigEndian.Uint16(encoded[10:12]) != IdentitySize {
		return Identity{}, corruptIdentity("header length is not 64", nil)
	}
	if !allZero(encoded[44:60]) {
		return Identity{}, corruptIdentity("reserved bytes are nonzero", nil)
	}
	wantCRC := binary.BigEndian.Uint32(encoded[60:64])
	if got := crc32.Checksum(encoded[:60], castagnoli); got != wantCRC {
		return Identity{}, corruptIdentity("CRC32C mismatch", nil)
	}
	var id DatabaseID
	copy(id[:], encoded[12:28])
	identity := Identity{
		DatabaseID:          id,
		ReservedRevisionEnd: gapdb.Revision(binary.BigEndian.Uint64(encoded[28:36])),
		Generation:          binary.BigEndian.Uint64(encoded[36:44]),
	}
	if identity.DatabaseID.IsZero() || identity.Generation == 0 {
		return Identity{}, corruptIdentity("database ID and generation must be nonzero", nil)
	}
	return identity, nil
}

func ReadIdentity(fsys faultfs.FS, directory string) (Identity, error) {
	file, err := fsys.OpenFile(faultfs.PointOpen, filepath.Join(directory, IdentityFilename), os.O_RDONLY, 0)
	if err != nil {
		return Identity{}, ioFailure("open", IdentityFilename, err)
	}
	defer file.Close()
	encoded := make([]byte, IdentitySize+1)
	n, readErr := io.ReadFull(file, encoded)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return Identity{}, ioFailure("read", IdentityFilename, readErr)
	}
	if n != IdentitySize {
		return Identity{}, corruptIdentity(fmt.Sprintf("size is %d, want %d", n, IdentitySize), readErr)
	}
	var extra [1]byte
	if got, err := file.Read(extra[:]); err != io.EOF || got != 0 {
		if err != nil {
			return Identity{}, ioFailure("read", IdentityFilename, err)
		}
		return Identity{}, corruptIdentity("file has trailing bytes", nil)
	}
	return DecodeIdentity(encoded[:IdentitySize])
}

func LoadOrCreateIdentity(fsys faultfs.FS, directory string, randomness io.Reader) (Identity, bool, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	_, err := fsys.Stat(faultfs.PointStat, filepath.Join(directory, IdentityFilename))
	if err == nil {
		identity, readErr := ReadIdentity(fsys, directory)
		return identity, false, readErr
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Identity{}, false, ioFailure("stat", IdentityFilename, err)
	}
	var id DatabaseID
	if _, err := io.ReadFull(randomness, id[:]); err != nil {
		return Identity{}, false, ioFailure("generate_database_id", IdentityFilename, err)
	}
	if id.IsZero() {
		return Identity{}, false, corruptIdentity("generated database ID is zero", nil)
	}
	identity := Identity{DatabaseID: id, Generation: 1}
	encoded, err := EncodeIdentity(identity)
	if err != nil {
		return Identity{}, false, err
	}
	if err := installIdentity(fsys, directory, encoded, randomness); err != nil {
		return Identity{}, false, err
	}
	return identity, true, nil
}

func ReserveRevisions(fsys faultfs.FS, directory string, identity Identity, recovered gapdb.Revision, rangeSize uint64, randomness io.Reader) (Identity, RevisionRange, error) {
	if recovered > identity.ReservedRevisionEnd {
		return Identity{}, RevisionRange{}, revisionRangeError(identity.ReservedRevisionEnd, "recovered revision exceeds the durable reservation", nil)
	}
	if rangeSize == 0 {
		return Identity{}, RevisionRange{}, revisionRangeError(identity.ReservedRevisionEnd, "reservation size must be greater than zero", nil)
	}
	if uint64(identity.ReservedRevisionEnd) > math.MaxUint64-rangeSize {
		return Identity{}, RevisionRange{}, revisionRangeError(identity.ReservedRevisionEnd, "revision reservation would overflow", nil)
	}
	if identity.Generation == math.MaxUint64 {
		return Identity{}, RevisionRange{}, revisionRangeError(identity.ReservedRevisionEnd, "identity generation would overflow", nil)
	}
	if randomness == nil {
		randomness = rand.Reader
	}
	updated := identity
	updated.ReservedRevisionEnd = gapdb.Revision(uint64(identity.ReservedRevisionEnd) + rangeSize)
	updated.Generation++
	encoded, err := EncodeIdentity(updated)
	if err != nil {
		return Identity{}, RevisionRange{}, err
	}
	if err := installIdentity(fsys, directory, encoded, randomness); err != nil {
		return Identity{}, RevisionRange{}, err
	}
	return updated, RevisionRange{First: identity.ReservedRevisionEnd + 1, End: updated.ReservedRevisionEnd}, nil
}

func ValidateIdentity(identity Identity, minimumGeneration uint64, recovered gapdb.Revision) error {
	if identity.Generation < minimumGeneration {
		return corruptIdentity("identity generation regressed", nil)
	}
	if recovered > identity.ReservedRevisionEnd {
		return revisionRangeError(identity.ReservedRevisionEnd, "recovered revision exceeds the durable reservation", nil)
	}
	return nil
}

func installIdentity(fsys faultfs.FS, directory string, encoded []byte, randomness io.Reader) error {
	suffix, err := randomSuffix(randomness)
	if err != nil {
		return ioFailure("generate_temp_name", IdentityFilename, err)
	}
	temp := filepath.Join(directory, IdentityFilename+".tmp-"+suffix)
	file, err := fsys.OpenFile(faultfs.PointIdentityTempCreate, temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return ioFailure("identity_temp_create", IdentityFilename, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := writeFull(fsys, faultfs.PointIdentityTempWrite, file, encoded); err != nil {
		return ioFailure("identity_temp_write", IdentityFilename, err)
	}
	if err := fsys.Sync(faultfs.PointIdentityTempSync, file); err != nil {
		return ioFailure("identity_temp_sync", IdentityFilename, err)
	}
	if err := file.Close(); err != nil {
		return ioFailure("identity_temp_close", IdentityFilename, err)
	}
	closed = true
	if err := fsys.Rename(faultfs.PointIdentityRename, temp, filepath.Join(directory, IdentityFilename)); err != nil {
		if faultfs.FailedAfter(err) {
			return ioFailureApplied("identity_rename", IdentityFilename, err)
		}
		return ioFailure("identity_rename", IdentityFilename, err)
	}
	if err := fsys.SyncDir(faultfs.PointIdentityDirectorySync, directory); err != nil {
		return ioFailureApplied("identity_directory_sync", IdentityFilename, err)
	}
	return nil
}

type OwnerLock struct {
	mu            sync.Mutex
	file          faultfs.File
	directory     string
	directoryFD   int
	parentFD      int
	directoryName string
	claimed       bool
}

type OwnerLease struct {
	lock *OwnerLock
}

func AcquireOwner(fsys faultfs.FS, directory string) (*OwnerLock, error) {
	file, err := fsys.OpenFile(faultfs.PointLockOpen, filepath.Join(directory, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ioFailure("lock_open", "LOCK", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, &gapdb.Error{Code: gapdb.CodeOwnerExists, Message: "Another process owns the database.", Retry: gapdb.RetryAfterOperator, Path: filepath.Clean(directory), SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionWait, gapdb.ActionAbort}, Cause: err}
		}
		return nil, ioFailure("lock", "LOCK", err)
	}
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, ioFailure("lock_directory_open", "LOCK", err)
	}
	cleanDirectory := filepath.Clean(directory)
	parentFD, err := unix.Open(filepath.Dir(cleanDirectory), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = unix.Close(directoryFD)
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, ioFailure("lock_parent_open", "LOCK", err)
	}
	lock := &OwnerLock{file: file, directory: cleanDirectory, directoryFD: directoryFD, parentFD: parentFD, directoryName: filepath.Base(cleanDirectory)}
	if err := lock.validateDirectoryLocked(); err != nil {
		_ = lock.closeLocked()
		return nil, ioFailure("lock_directory_identity", "LOCK", err)
	}
	return lock, nil
}

// IsHeld reports live descriptor ownership, including ownership transferred to
// an active runtime.
func (lock *OwnerLock) IsHeld() bool {
	if lock == nil {
		return false
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	return lock.heldLocked()
}

// Claim atomically transfers close authority to one runtime after validating
// the live lock descriptor and its database directory.
func (lock *OwnerLock) Claim(directory string) (*OwnerLease, error) {
	if lock == nil {
		return nil, invalidOwnerLock("owner lock is required")
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.claimed {
		return nil, invalidOwnerLock("owner lock was already transferred")
	}
	if filepath.Clean(directory) != lock.directory {
		return nil, invalidOwnerLock("owner lock directory does not match")
	}
	if !lock.heldLocked() {
		return nil, invalidOwnerLock("owner lock is closed")
	}
	lock.claimed = true
	return &OwnerLease{lock: lock}, nil
}

func (lock *OwnerLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.claimed {
		return nil
	}
	return lock.closeLocked()
}

func (lease *OwnerLease) Close() error {
	if lease == nil || lease.lock == nil {
		return nil
	}
	lock := lease.lock
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if !lock.claimed {
		return nil
	}
	lock.claimed = false
	return lock.closeLocked()
}

// SecureHeldPath applies owner-only mode to the actual leased descriptor and
// proves the LOCK name in the acquisition-time directory still names it.
func (lease *OwnerLease) SecureHeldPath() error {
	if lease == nil || lease.lock == nil {
		return fmt.Errorf("owner lease is required")
	}
	lock := lease.lock
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if !lock.claimed || !lock.heldLocked() {
		return fmt.Errorf("owner lease is not held")
	}
	if err := lock.validateDirectoryLocked(); err != nil {
		return err
	}
	if err := unix.Fchmod(lock.directoryFD, 0o700); err != nil {
		return fmt.Errorf("chmod held database directory: %w", err)
	}
	var directoryStat unix.Stat_t
	if err := unix.Fstat(lock.directoryFD, &directoryStat); err != nil {
		return fmt.Errorf("verify held database directory mode: %w", err)
	}
	if directoryStat.Mode&0o777 != 0o700 {
		return fmt.Errorf("held database directory mode is %04o", directoryStat.Mode&0o777)
	}
	fd := int(lock.file.Fd())
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fmt.Errorf("chmod held LOCK: %w", err)
	}
	var held, named unix.Stat_t
	if err := unix.Fstat(fd, &held); err != nil {
		return fmt.Errorf("stat held LOCK: %w", err)
	}
	if err := unix.Fstatat(lock.directoryFD, "LOCK", &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat named LOCK: %w", err)
	}
	if held.Dev != named.Dev || held.Ino != named.Ino || held.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&unix.S_IFMT != unix.S_IFREG || held.Mode&0o777 != 0o600 || named.Mode&0o777 != 0o600 {
		return fmt.Errorf("held and named LOCK identity or mode differs")
	}
	return lock.validateDirectoryLocked()
}

func (lock *OwnerLock) validateDirectoryLocked() error {
	if lock.directoryFD < 0 || lock.parentFD < 0 || lock.directoryName == "" {
		return fmt.Errorf("database directory capability is closed")
	}
	var held, named unix.Stat_t
	if err := unix.Fstat(lock.directoryFD, &held); err != nil {
		return fmt.Errorf("stat held database directory: %w", err)
	}
	if err := unix.Fstatat(lock.parentFD, lock.directoryName, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat named database directory: %w", err)
	}
	if held.Dev != named.Dev || held.Ino != named.Ino || held.Mode&unix.S_IFMT != unix.S_IFDIR || named.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("held and named database directory identity differs")
	}
	return nil
}

func (lock *OwnerLock) heldLocked() bool {
	if lock.file == nil || lock.directoryFD < 0 || lock.parentFD < 0 {
		return false
	}
	var stat unix.Stat_t
	return unix.Fstat(int(lock.file.Fd()), &stat) == nil
}

func (lock *OwnerLock) closeLocked() error {
	if lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	directoryErr := unix.Close(lock.directoryFD)
	lock.directoryFD = -1
	parentErr := unix.Close(lock.parentFD)
	lock.parentFD = -1
	if unlockErr != nil {
		return unlockErr
	}
	return errors.Join(closeErr, directoryErr, parentErr)
}

func invalidOwnerLock(reason string) error {
	return &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed, Message: "The owner lock is not a live transferable capability.", Retry: gapdb.RetryAfterReconcile, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionAbort}}
}

func randomSuffix(randomness io.Reader) (string, error) {
	var value [8]byte
	if _, err := io.ReadFull(randomness, value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func writeFull(fsys faultfs.FS, point faultfs.Point, writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := fsys.Write(point, writer, value)
		if written < 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func corruptIdentity(reason string, cause error) error {
	return &gapdb.Error{Code: gapdb.CodeCorruptIdentity, Message: "Identity file is corrupt.", Retry: gapdb.RetryAfterOperator, Reason: reason, File: IdentityFilename, SafeActions: []gapdb.SafeAction{gapdb.ActionInspectOffline, gapdb.ActionRestoreBackup, gapdb.ActionAbort}, Cause: cause}
}

func unknownStorageFormat(file string, received uint64) error {
	return &gapdb.Error{Code: gapdb.CodeUnknownFormat, Message: "Storage format is not supported.", Retry: gapdb.RetryNever, ReceivedVersion: &received, SupportedVersions: []uint64{uint64(storageVersion)}, File: file, SafeActions: []gapdb.SafeAction{gapdb.ActionUpgradeGapdb, gapdb.ActionUseCompatibleBinary, gapdb.ActionAbort}}
}

func revisionRangeError(end gapdb.Revision, reason string, cause error) error {
	reserved := end
	return &gapdb.Error{Code: gapdb.CodeRevisionRangeExhausted, Message: "A safe revision range could not be reserved.", Retry: gapdb.RetryAfterRestart, Reason: reason, ReservedRevisionEnd: &reserved, SafeActions: []gapdb.SafeAction{gapdb.ActionVerifyStorage, gapdb.ActionRestartAfterRecovery, gapdb.ActionAbort}, Cause: cause}
}

func ioFailure(operation, file string, cause error) error {
	category := "other"
	switch {
	case errors.Is(cause, fs.ErrPermission):
		category = "permission"
	case errors.Is(cause, fs.ErrNotExist):
		category = "not_exist"
	case errors.Is(cause, fs.ErrExist):
		category = "already_exists"
	case errors.Is(cause, unix.ENOSPC):
		category = "no_space"
	case errors.Is(cause, unix.EROFS):
		category = "read_only"
	}
	return &gapdb.Error{Code: gapdb.CodeIOError, Message: "A storage operation failed.", Retry: gapdb.RetryAfterOperator, Path: file, Operation: operation, OSErrorCategory: category, SafeActions: []gapdb.SafeAction{gapdb.ActionCheckStorage, gapdb.ActionVerify, gapdb.ActionAbort}, Cause: cause}
}

func ioFailureApplied(operation, file string, cause error) error {
	err := ioFailure(operation, file, cause).(*gapdb.Error)
	err.OperationApplied = true
	return err
}
