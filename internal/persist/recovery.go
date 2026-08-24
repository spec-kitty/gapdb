package persist

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

type TailTruncation struct {
	Code                 string
	File                 string
	OldSize              int64
	NewSize              int64
	LastCompleteRevision gapdb.Revision
}

type WALReplay struct {
	Header            WALHeader
	Commits           []CommitFrame
	RecoveredRevision gapdb.Revision
	CompleteOffset    int64
	Tail              *TailTruncation
}

func ScanWAL(reader io.Reader, physicalSize int64, expectedID DatabaseID, afterRevision, reservedEnd gapdb.Revision, limits gapdb.Limits) (WALReplay, error) {
	headerBytes := make([]byte, WALHeaderSize)
	if _, err := io.ReadFull(reader, headerBytes); err != nil {
		return WALReplay{}, corruptWAL("WAL header is incomplete", "WAL", 0, err)
	}
	header, err := DecodeWALHeader(headerBytes, expectedID)
	if err != nil {
		return WALReplay{}, err
	}
	if afterRevision == gapdb.Revision(^uint64(0)) || header.FirstRevision != afterRevision+1 {
		return WALReplay{}, corruptWAL("WAL first revision does not immediately follow the snapshot", "WAL", 0, nil)
	}
	result := WALReplay{Header: header, RecoveredRevision: afterRevision, CompleteOffset: WALHeaderSize}
	offset := int64(WALHeaderSize)
	maximum := limits.MaxBatchBytes
	if limits.MaxFrameBytes < maximum {
		maximum = limits.MaxFrameBytes
	}
	for {
		prefix := make([]byte, frameHeaderSize)
		n, readErr := io.ReadFull(reader, prefix)
		if n == 0 && errors.Is(readErr, io.EOF) {
			return result, nil
		}
		if n < frameHeaderSize {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				return WALReplay{}, ioFailure("wal_read_frame_header", "WAL", readErr)
			}
			if err := validateFramePrefix(prefix[:n], maximum, limits.MaxBatchOperations); err != nil {
				return WALReplay{}, withWALContext(err, "WAL", offset)
			}
			result.Tail = &TailTruncation{Code: "TAIL_TRUNCATED", File: "WAL", OldSize: physicalSize, NewSize: offset, LastCompleteRevision: result.RecoveredRevision}
			return result, nil
		}
		if readErr != nil {
			return WALReplay{}, ioFailure("wal_read_frame_header", "WAL", readErr)
		}
		if err := validateFramePrefix(prefix, maximum, limits.MaxBatchOperations); err != nil {
			return WALReplay{}, withWALContext(err, "WAL", offset)
		}
		total := int(binary.BigEndian.Uint32(prefix[8:12]))
		frame := make([]byte, total)
		copy(frame, prefix)
		remaining := total - frameHeaderSize
		n, readErr = io.ReadFull(reader, frame[frameHeaderSize:])
		if n < remaining {
			if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				return WALReplay{}, ioFailure("wal_read_frame", "WAL", readErr)
			}
			result.Tail = &TailTruncation{Code: "TAIL_TRUNCATED", File: "WAL", OldSize: physicalSize, NewSize: offset, LastCompleteRevision: result.RecoveredRevision}
			return result, nil
		}
		commit, err := DecodeCommitFrame(frame, limits)
		if err != nil {
			return WALReplay{}, withWALContext(err, "WAL", offset)
		}
		if commit.Revision <= result.RecoveredRevision {
			return WALReplay{}, corruptWAL("commit revisions are not strictly increasing", "WAL", offset, nil)
		}
		if commit.Revision > reservedEnd {
			return WALReplay{}, revisionRangeError(reservedEnd, "WAL commit exceeds the durable revision reservation", nil)
		}
		result.Commits = append(result.Commits, cloneCommit(commit))
		result.RecoveredRevision = commit.Revision
		offset += int64(total)
		result.CompleteOffset = offset
	}
}

func RecoverWALFile(fsys faultfs.FS, path string, expectedID DatabaseID, afterRevision, reservedEnd gapdb.Revision, limits gapdb.Limits) (WALReplay, error) {
	file, result, err := scanWALFile(fsys, path, expectedID, afterRevision, reservedEnd, limits)
	if err != nil {
		return WALReplay{}, err
	}
	defer file.Close()
	if err := applyWALTail(fsys, file, filepath.Base(path), result.Tail); err != nil {
		return WALReplay{}, err
	}
	return result, nil
}

func scanWALFile(fsys faultfs.FS, path string, expectedID DatabaseID, afterRevision, reservedEnd gapdb.Revision, limits gapdb.Limits) (faultfs.File, WALReplay, error) {
	base := filepath.Base(path)
	file, err := fsys.OpenFile(faultfs.PointOpen, path, os.O_RDWR, 0)
	if err != nil {
		return nil, WALReplay{}, ioFailure("wal_open", base, err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, WALReplay{}, ioFailure("wal_stat", base, err)
	}
	result, err := ScanWAL(file, info.Size(), expectedID, afterRevision, reservedEnd, limits)
	if err != nil {
		return nil, WALReplay{}, withWALContext(err, base, 0)
	}
	if result.Tail != nil {
		result.Tail.File = base
	}
	closeOnError = false
	return file, result, nil
}

func applyWALTail(fsys faultfs.FS, file faultfs.File, base string, tail *TailTruncation) error {
	if tail == nil {
		return nil
	}
	if err := fsys.Truncate(faultfs.PointWALTailTruncate, file, tail.NewSize); err != nil {
		return ioFailureApplied("wal_tail_truncate", base, err)
	}
	if err := fsys.Sync(faultfs.PointWALTailSync, file); err != nil {
		return ioFailureApplied("wal_tail_sync", base, err)
	}
	return nil
}

func validateFramePrefix(prefix []byte, maximum, maximumOperations int) error {
	if len(prefix) >= 4 && string(prefix[:4]) != "CMIT" {
		return corruptWAL("commit magic does not match CMIT", "WAL", 0, nil)
	}
	if len(prefix) >= 6 {
		version := binary.BigEndian.Uint16(prefix[4:6])
		if version != storageVersion {
			return unknownStorageFormat("WAL", uint64(version))
		}
	}
	if len(prefix) >= 8 && binary.BigEndian.Uint16(prefix[6:8]) != 0 {
		return corruptWAL("commit flags are nonzero", "WAL", 0, nil)
	}
	if len(prefix) >= 12 {
		total := uint64(binary.BigEndian.Uint32(prefix[8:12]))
		if total < frameHeaderSize+frameTrailer || total > uint64(maximum) {
			return corruptWAL("declared commit frame size is outside bounds", "WAL", 0, nil)
		}
	}
	if len(prefix) >= 24 {
		if binary.BigEndian.Uint64(prefix[12:20]) == 0 {
			return corruptWAL("commit revision is zero", "WAL", 0, nil)
		}
		count := uint64(binary.BigEndian.Uint32(prefix[20:24]))
		if count == 0 || count > uint64(maximumOperations) {
			return corruptWAL("declared mutation count is outside configured bounds", "WAL", 0, nil)
		}
	}
	if len(prefix) >= 28 {
		total := uint64(binary.BigEndian.Uint32(prefix[8:12]))
		payload := uint64(binary.BigEndian.Uint32(prefix[24:28]))
		if total != frameHeaderSize+frameTrailer+payload {
			return corruptWAL("declared commit and payload lengths disagree", "WAL", 0, nil)
		}
		count := uint64(binary.BigEndian.Uint32(prefix[20:24]))
		if count > payload/effectHeader {
			return corruptWAL("mutation count cannot fit in declared payload", "WAL", 0, nil)
		}
	}
	return nil
}

type SnapshotState struct {
	Revision gapdb.Revision
	Records  []gapdb.Record
}

type SnapshotLoader interface {
	Load(faultfs.FS, string, Manifest) (SnapshotState, error)
}

type SnapshotLoaderFunc func(faultfs.FS, string, Manifest) (SnapshotState, error)

func (function SnapshotLoaderFunc) Load(fsys faultfs.FS, directory string, manifest Manifest) (SnapshotState, error) {
	return function(fsys, directory, manifest)
}

type RecoveryOptions struct {
	Limits          gapdb.Limits
	ReservationSize uint64
	Random          io.Reader
	SnapshotLoader  SnapshotLoader
	AuditTail       func(TailTruncation) error
}

type RecoveryResult struct {
	Owner                  *OwnerLock
	Identity               Identity
	Manifest               Manifest
	Snapshot               SnapshotState
	PostSnapshotCommits    []CommitFrame
	CurrentRevision        gapdb.Revision
	DurableThroughRevision gapdb.Revision
	Allocation             RevisionRange
	Tail                   *TailTruncation
	CleanupCandidates      []string
}

func RecoverDatabase(fsys faultfs.FS, directory string, options RecoveryOptions) (RecoveryResult, error) {
	owner, err := AcquireOwner(fsys, directory)
	if err != nil {
		return RecoveryResult{}, err
	}
	success := false
	defer func() {
		if !success {
			_ = owner.Close()
		}
	}()
	identity, err := ReadIdentity(fsys, directory)
	if err != nil {
		return RecoveryResult{}, err
	}
	manifest, err := ReadManifest(fsys, directory, identity.DatabaseID, 1)
	if err != nil {
		return RecoveryResult{}, err
	}
	if options.SnapshotLoader == nil {
		return RecoveryResult{}, corruptSnapshot("no snapshot validator is configured", manifest.SnapshotFile, 0, nil)
	}
	snapshot, err := options.SnapshotLoader.Load(fsys, directory, manifest)
	if err != nil {
		return RecoveryResult{}, err
	}
	if err := validateSnapshotState(snapshot, manifest); err != nil {
		return RecoveryResult{}, err
	}
	walPath := filepath.Join(directory, manifest.WALFile)
	walFile, replay, err := scanWALFile(fsys, walPath, identity.DatabaseID, manifest.SnapshotRevision, identity.ReservedRevisionEnd, options.Limits)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer walFile.Close()
	if replay.Header.Generation != manifest.Generation || replay.Header.FirstRevision != manifest.WALStartRevision {
		return RecoveryResult{}, corruptWAL("WAL generation or start does not match CURRENT", manifest.WALFile, 0, nil)
	}
	if err := applyWALTail(fsys, walFile, manifest.WALFile, replay.Tail); err != nil {
		return RecoveryResult{}, err
	}
	operationApplied := replay.Tail != nil
	if replay.Tail != nil {
		if options.AuditTail == nil {
			return RecoveryResult{}, withRecoveryAppliedState(auditAfterApplyError("startup_tail_truncate", "tail audit callback is required", replay.RecoveredRevision, nil), operationApplied)
		}
		if err := options.AuditTail(*replay.Tail); err != nil {
			return RecoveryResult{}, withRecoveryAppliedState(auditAfterApplyError("startup_tail_truncate", "tail audit failed", replay.RecoveredRevision, err), operationApplied)
		}
	}
	if options.ReservationSize == 0 {
		return RecoveryResult{}, withRecoveryAppliedState(revisionRangeError(identity.ReservedRevisionEnd, "reservation size must be greater than zero", nil), operationApplied)
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	candidates, err := cleanupCandidates(fsys, directory, manifest)
	if err != nil {
		return RecoveryResult{}, withRecoveryAppliedState(err, operationApplied)
	}
	updatedIdentity, allocation, err := ReserveRevisions(fsys, directory, identity, replay.RecoveredRevision, options.ReservationSize, options.Random)
	if err != nil {
		return RecoveryResult{}, withRecoveryAppliedState(err, operationApplied)
	}
	result := RecoveryResult{
		Owner:                  owner,
		Identity:               updatedIdentity,
		Manifest:               manifest,
		Snapshot:               cloneSnapshot(snapshot),
		PostSnapshotCommits:    cloneCommits(replay.Commits),
		CurrentRevision:        replay.RecoveredRevision,
		DurableThroughRevision: replay.RecoveredRevision,
		Allocation:             allocation,
		Tail:                   replay.Tail,
		CleanupCandidates:      candidates,
	}
	success = true
	return result, nil
}

func withRecoveryAppliedState(err error, operationApplied bool) error {
	if err == nil || !operationApplied {
		return err
	}
	var structured *gapdb.Error
	if !errors.As(err, &structured) {
		return err
	}
	clone := structured.Clone()
	clone.OperationApplied = true
	return clone
}

func validateSnapshotState(snapshot SnapshotState, manifest Manifest) error {
	if snapshot.Revision != manifest.SnapshotRevision {
		return corruptSnapshot("snapshot revision does not match CURRENT", manifest.SnapshotFile, 0, nil)
	}
	seen := make(map[string]struct{}, len(snapshot.Records))
	for index, record := range snapshot.Records {
		if record.Key == "" || !utf8.ValidString(record.Key) || record.Revision == 0 || record.Revision > snapshot.Revision {
			return corruptSnapshot("snapshot record fields are invalid", manifest.SnapshotFile, int64(index), nil)
		}
		if index > 0 && snapshot.Records[index-1].Key >= record.Key {
			return corruptSnapshot("snapshot records are not strictly key-sorted", manifest.SnapshotFile, int64(index), nil)
		}
		if _, exists := seen[record.Key]; exists {
			return corruptSnapshot("snapshot contains duplicate keys", manifest.SnapshotFile, int64(index), nil)
		}
		seen[record.Key] = struct{}{}
	}
	return nil
}

func cleanupCandidates(fsys faultfs.FS, directory string, manifest Manifest) ([]string, error) {
	entries, err := fsys.ReadDir(faultfs.PointReadDir, directory)
	if err != nil {
		return nil, ioFailure("read_directory", filepath.Base(directory), err)
	}
	var candidates []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == manifest.SnapshotFile || name == manifest.WALFile {
			continue
		}
		if (strings.HasPrefix(name, "snapshot-") || strings.HasPrefix(name, "wal-")) && strings.HasSuffix(name, ".gdb") {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	return candidates, nil
}

func withWALContext(err error, file string, baseOffset int64) error {
	var structured *gapdb.Error
	if !errors.As(err, &structured) {
		return err
	}
	clone := structured.Clone()
	if clone.Code == gapdb.CodeCorruptWAL {
		clone.File = filepath.Base(file)
		if clone.Offset != nil {
			offset := baseOffset + *clone.Offset
			clone.Offset = &offset
		}
	}
	if clone.Code == gapdb.CodeUnknownFormat {
		clone.File = filepath.Base(file)
	}
	return clone
}

func cloneCommit(commit CommitFrame) CommitFrame {
	clone := CommitFrame{Revision: commit.Revision, Effects: make([]Effect, len(commit.Effects))}
	for index, effect := range commit.Effects {
		clone.Effects[index] = effect
		clone.Effects[index].Value = append([]byte(nil), effect.Value...)
		if effect.ExpiresAt != nil {
			value := effect.ExpiresAt.UTC()
			clone.Effects[index].ExpiresAt = &value
		}
	}
	return clone
}

func cloneCommits(commits []CommitFrame) []CommitFrame {
	result := make([]CommitFrame, len(commits))
	for index, commit := range commits {
		result[index] = cloneCommit(commit)
	}
	return result
}

func cloneSnapshot(snapshot SnapshotState) SnapshotState {
	result := SnapshotState{Revision: snapshot.Revision, Records: make([]gapdb.Record, len(snapshot.Records))}
	for index, record := range snapshot.Records {
		result.Records[index] = record.Clone()
	}
	return result
}

func corruptSnapshot(reason, file string, offset int64, cause error) error {
	offsetCopy := offset
	return &gapdb.Error{Code: gapdb.CodeCorruptSnapshot, Message: "Snapshot is corrupt.", Retry: gapdb.RetryAfterOperator, Reason: reason, Offset: &offsetCopy, File: filepath.Base(file), SafeActions: []gapdb.SafeAction{gapdb.ActionInspectOffline, gapdb.ActionRecoverPropose, gapdb.ActionRestoreBackup, gapdb.ActionAbort}, Cause: cause}
}

func auditAfterApplyError(operation, reason string, current gapdb.Revision, cause error) error {
	currentCopy := current
	return &gapdb.Error{Code: gapdb.CodeAuditFailedAfterApply, Message: "Audit failed after a storage change was applied.", Retry: gapdb.RetryAfterReconcile, Reason: reason, Operation: operation, CurrentRevision: &currentCopy, OperationApplied: true, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionRepairAuditStorage, gapdb.ActionAbort}, Cause: cause}
}
