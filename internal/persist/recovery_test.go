package persist

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	wire "github.com/spec-kitty/gapdb/internal/protocol"
)

func TestRecoverWALTornTailAtEveryByte(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
	first, _ := EncodeCommitFrame(CommitFrame{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "first", Value: []byte("one")}}}, gapdb.DefaultOptions().Limits)
	second, _ := EncodeCommitFrame(CommitFrame{Revision: 3, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a", Value: []byte("A")}, {Kind: gapdb.ChangePut, Key: "b", Value: []byte("B")}}}, gapdb.DefaultOptions().Limits)
	completeOffset := int64(len(header) + len(first))

	for cut := 1; cut < len(second); cut++ {
		t.Run(strconv.Itoa(cut), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wal-00000000000000000001.gdb")
			value := append(append(bytes.Clone(header), first...), second[:cut]...)
			if err := os.WriteFile(path, value, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := RecoverWALFile(faultfs.NewOS(nil), path, id, 0, 100, gapdb.DefaultOptions().Limits)
			if err != nil {
				t.Fatalf("RecoverWALFile(cut=%d) = %v", cut, err)
			}
			if len(result.Commits) != 1 || result.Commits[0].Revision != 1 {
				t.Fatalf("commits = %#v; a proper subset of the second batch must not appear", result.Commits)
			}
			if result.Tail == nil || result.Tail.NewSize != completeOffset || result.Tail.OldSize != int64(len(value)) {
				t.Fatalf("tail = %#v, want old=%d new=%d", result.Tail, len(value), completeOffset)
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() != completeOffset {
				t.Fatalf("truncated size = %v, %v", info, err)
			}
		})
	}
}

func TestRecoverWALCompleteCorruptionFailsWithoutTruncation(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
	frame, _ := EncodeCommitFrame(CommitFrame{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a", Value: []byte("A")}}}, gapdb.DefaultOptions().Limits)
	mutations := []func([]byte){
		func(v []byte) { v[0] = 'X'; resetFrameCRC(v) },
		func(v []byte) { binary.BigEndian.PutUint16(v[4:6], 2); resetFrameCRC(v) },
		func(v []byte) { v[7] = 1; resetFrameCRC(v) },
		func(v []byte) { binary.BigEndian.PutUint32(v[8:12], uint32(len(v)-1)); resetFrameCRC(v) },
		func(v []byte) { binary.BigEndian.PutUint64(v[12:20], 0); resetFrameCRC(v) },
		func(v []byte) { binary.BigEndian.PutUint32(v[20:24], 0); resetFrameCRC(v) },
		func(v []byte) { binary.BigEndian.PutUint32(v[24:28], 1); resetFrameCRC(v) },
		func(v []byte) { v[28] = 9; resetFrameCRC(v) },
		func(v []byte) { v[30] = 1; resetFrameCRC(v) },
		func(v []byte) { v[len(v)-1] ^= 1 },
	}
	for index, mutate := range mutations {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wal-00000000000000000001.gdb")
			bad := bytes.Clone(frame)
			mutate(bad)
			value := append(bytes.Clone(header), bad...)
			if err := os.WriteFile(path, value, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := RecoverWALFile(faultfs.NewOS(nil), path, id, 0, 100, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptWAL}) && !errors.Is(err, &gapdb.Error{Code: gapdb.CodeUnknownFormat}) {
				t.Fatalf("RecoverWALFile = %v, want fail-closed storage error", err)
			}
			info, statErr := os.Stat(path)
			if statErr != nil || info.Size() != int64(len(value)) {
				t.Fatalf("corruption mutated file size: %v, %v", info, statErr)
			}
		})
	}
}

func TestRecoverWALRevisionRules(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 2, Generation: 1})
	frame2, _ := EncodeCommitFrame(CommitFrame{Revision: 2, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a"}}}, gapdb.DefaultOptions().Limits)
	frame4, _ := EncodeCommitFrame(CommitFrame{Revision: 4, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "b"}}}, gapdb.DefaultOptions().Limits)
	valid := append(append(bytes.Clone(header), frame2...), frame4...)
	result, err := ScanWAL(bytes.NewReader(valid), int64(len(valid)), id, 1, 4, gapdb.DefaultOptions().Limits)
	if err != nil || len(result.Commits) != 2 || result.RecoveredRevision != 4 {
		t.Fatalf("ScanWAL gaps = %#v, %v", result, err)
	}
	if _, err := ScanWAL(bytes.NewReader(valid), int64(len(valid)), id, 1, 3, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionRangeExhausted}) {
		t.Fatalf("reserved bound = %v", err)
	}
	regressed := append(append(bytes.Clone(header), frame4...), frame2...)
	if _, err := ScanWAL(bytes.NewReader(regressed), int64(len(regressed)), id, 1, 10, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptWAL}) {
		t.Fatalf("revision regression = %v", err)
	}
}

func TestRecoverWALRejectsInvalidFieldsInAnIncompletePrefix(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
	prefix := make([]byte, 24)
	copy(prefix[:4], "CMIT")
	binary.BigEndian.PutUint16(prefix[4:6], 1)
	binary.BigEndian.PutUint32(prefix[8:12], 64)
	// Revision zero is fully present and invalid even though the physical frame is incomplete.
	binary.BigEndian.PutUint32(prefix[20:24], 1)
	value := append(header, prefix...)
	if _, err := ScanWAL(bytes.NewReader(value), int64(len(value)), id, 0, 100, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptWAL}) {
		t.Fatalf("zero revision incomplete prefix = %v, want CORRUPT_WAL", err)
	}

	limits := gapdb.DefaultOptions().Limits
	binary.BigEndian.PutUint64(prefix[12:20], 1)
	binary.BigEndian.PutUint32(prefix[20:24], uint32(limits.MaxBatchOperations+1))
	value = append(bytes.Clone(header), prefix...)
	if _, err := ScanWAL(bytes.NewReader(value), int64(len(value)), id, 0, 100, limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptWAL}) {
		t.Fatalf("oversized count incomplete prefix = %v, want CORRUPT_WAL", err)
	}
}

func TestScanWALRejectsNonEOFFailuresWhileReadingFramePrefix(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
	deviceFailure := errors.New("device read failure")
	for _, prefix := range [][]byte{nil, {'C'}} {
		name := "zero-byte"
		if len(prefix) != 0 {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) {
			reader := io.MultiReader(bytes.NewReader(header), bytes.NewReader(prefix), iotest.ErrReader(deviceFailure))
			result, err := ScanWAL(reader, int64(len(header)+len(prefix)+1), id, 0, 100, gapdb.DefaultOptions().Limits)
			var structured *gapdb.Error
			if !errors.As(err, &structured) || structured.Code != gapdb.CodeIOError || !errors.Is(err, deviceFailure) {
				t.Fatalf("ScanWAL = %#v, %v, want IO_ERROR wrapping device failure", result, err)
			}
			if result.Tail != nil {
				t.Fatalf("non-EOF read failure proposed truncation: %#v", result.Tail)
			}
		})
	}
}

func TestTailMutationFailureReportsPossibleApplication(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
	frame, _ := EncodeCommitFrame(CommitFrame{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a"}}}, gapdb.DefaultOptions().Limits)
	dir := t.TempDir()
	path := filepath.Join(dir, "wal-00000000000000000001.gdb")
	if err := os.WriteFile(path, append(append(header, frame...), []byte{'C'}...), 0o600); err != nil {
		t.Fatal(err)
	}
	injector := faultfs.NewInjector(3, faultfs.Rule{Point: faultfs.PointWALTailTruncate, Phase: faultfs.After, Occurrence: 1, Seed: 3, Err: errors.New("after truncate")})
	_, err := RecoverWALFile(faultfs.NewOS(injector), path, id, 0, 100, gapdb.DefaultOptions().Limits)
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeIOError || !structured.OperationApplied {
		t.Fatalf("RecoverWALFile = %#v, want IO_ERROR with operation_applied", err)
	}
}

func TestRecoverDatabaseValidatesAuthorityBeforeReservation(t *testing.T) {
	dir := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	identity := Identity{DatabaseID: id, ReservedRevisionEnd: 100, Generation: 1}
	writeIdentityDirect(t, dir, identity)
	manifest := Manifest{DatabaseID: id, Generation: 1, SnapshotFile: "snapshot-00000000000000000000.gdb", SnapshotRevision: 0, SnapshotSHA256: strings64("a"), WALFile: "wal-00000000000000000001.gdb", WALStartRevision: 1}
	manifestBytes, _ := EncodeManifest(manifest)
	if err := os.WriteFile(filepath.Join(dir, ManifestFilename), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
	frame, _ := EncodeCommitFrame(CommitFrame{Revision: 2, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "k", Value: []byte("v")}}}, gapdb.DefaultOptions().Limits)
	if err := os.WriteFile(filepath.Join(dir, manifest.WALFile), append(header, frame...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wal-00000000000000000099.gdb"), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := SnapshotLoaderFunc(func(_ faultfs.FS, _ string, got Manifest) (SnapshotState, error) {
		if got != manifest {
			t.Fatalf("snapshot loader manifest = %#v", got)
		}
		return SnapshotState{Revision: 0}, nil
	})
	result, err := RecoverDatabase(faultfs.NewOS(nil), dir, RecoveryOptions{Limits: gapdb.DefaultOptions().Limits, ReservationSize: 10, Random: bytes.NewReader(bytes.Repeat([]byte{7}, 16)), SnapshotLoader: loader})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Owner.Close()
	if result.CurrentRevision != 2 || result.DurableThroughRevision != 2 || result.Allocation.First != 101 || result.Allocation.End != 110 {
		t.Fatalf("recovery result = %#v", result)
	}
	if len(result.PostSnapshotCommits) != 1 || len(result.PostSnapshotCommits[0].Effects) != 1 {
		t.Fatalf("post-snapshot commits = %#v", result.PostSnapshotCommits)
	}
	if len(result.CleanupCandidates) != 1 || result.CleanupCandidates[0] != "wal-00000000000000000099.gdb" {
		t.Fatalf("cleanup candidates = %v", result.CleanupCandidates)
	}
}

func TestRecoverDatabaseValidatesWALAuthorityBeforeTailMutation(t *testing.T) {
	dir := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	writeIdentityDirect(t, dir, Identity{DatabaseID: id, ReservedRevisionEnd: 100, Generation: 1})
	manifest := Manifest{DatabaseID: id, Generation: 1, SnapshotFile: "snapshot-00000000000000000000.gdb", SnapshotRevision: 0, SnapshotSHA256: strings64("a"), WALFile: "wal-00000000000000000001.gdb", WALStartRevision: 1}
	manifestBytes, _ := EncodeManifest(manifest)
	if err := os.WriteFile(filepath.Join(dir, ManifestFilename), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 2})
	walPath := filepath.Join(dir, manifest.WALFile)
	before := append(header, 'C')
	if err := os.WriteFile(walPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &faultfs.Recorder{}
	auditCalls := 0
	_, err := RecoverDatabase(faultfs.NewOS(recorder), dir, RecoveryOptions{
		Limits:          gapdb.DefaultOptions().Limits,
		ReservationSize: 10,
		SnapshotLoader: SnapshotLoaderFunc(func(faultfs.FS, string, Manifest) (SnapshotState, error) {
			return SnapshotState{Revision: 0}, nil
		}),
		AuditTail: func(TailTruncation) error {
			auditCalls++
			return nil
		},
	})
	if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptWAL}) {
		t.Fatalf("RecoverDatabase = %v, want CORRUPT_WAL", err)
	}
	after, readErr := os.ReadFile(walPath)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("authority mismatch mutated WAL: got %x, want %x, read error %v", after, before, readErr)
	}
	if auditCalls != 0 {
		t.Fatalf("authority mismatch invoked tail audit %d time(s)", auditCalls)
	}
	for _, event := range recorder.Events() {
		if event.Point == faultfs.PointWALTailTruncate || event.Point == faultfs.PointWALTailSync {
			t.Fatalf("authority mismatch reached tail mutation: %v", recorder.Events())
		}
	}
}

func TestRecoveryReportsEarlierTailApplyWhenLaterStageFails(t *testing.T) {
	type failureCase struct {
		name            string
		point           faultfs.Point
		phase           faultfs.Phase
		reservationSize uint64
		wantCode        gapdb.ErrorCode
		identityApplied bool
	}
	cases := []failureCase{
		{name: "cleanup-read-directory", point: faultfs.PointReadDir, phase: faultfs.Before, reservationSize: 10, wantCode: gapdb.CodeIOError},
		{name: "reservation-validation", reservationSize: 0, wantCode: gapdb.CodeRevisionRangeExhausted},
	}
	for _, point := range []faultfs.Point{
		faultfs.PointIdentityTempCreate,
		faultfs.PointIdentityTempWrite,
		faultfs.PointIdentityTempSync,
		faultfs.PointIdentityRename,
		faultfs.PointIdentityDirectorySync,
	} {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			identityApplied := point == faultfs.PointIdentityRename && phase == faultfs.After || point == faultfs.PointIdentityDirectorySync
			cases = append(cases, failureCase{
				name:            string(point) + "/" + string(phase),
				point:           point,
				phase:           phase,
				reservationSize: 10,
				wantCode:        gapdb.CodeIOError,
				identityApplied: identityApplied,
			})
		}
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
			identity := Identity{DatabaseID: id, ReservedRevisionEnd: 100, Generation: 1}
			updatedIdentity := Identity{DatabaseID: id, ReservedRevisionEnd: 110, Generation: 2}
			writeIdentityDirect(t, dir, identity)
			manifest := Manifest{DatabaseID: id, Generation: 1, SnapshotFile: "snapshot-00000000000000000000.gdb", SnapshotRevision: 0, SnapshotSHA256: strings64("a"), WALFile: "wal-00000000000000000001.gdb", WALStartRevision: 1}
			manifestBytes, _ := EncodeManifest(manifest)
			if err := os.WriteFile(filepath.Join(dir, ManifestFilename), manifestBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
			walPath := filepath.Join(dir, manifest.WALFile)
			if err := os.WriteFile(walPath, append(bytes.Clone(header), 'C'), 0o600); err != nil {
				t.Fatal(err)
			}

			var hook faultfs.Hook
			if test.point != "" {
				hook = faultfs.NewInjector(93, faultfs.Rule{Point: test.point, Phase: test.phase, Occurrence: 1, Seed: 93, Err: errors.New("injected later failure")})
			}
			auditCalls := 0
			_, err := RecoverDatabase(faultfs.NewOS(hook), dir, RecoveryOptions{
				Limits:          gapdb.DefaultOptions().Limits,
				ReservationSize: test.reservationSize,
				Random:          bytes.NewReader(bytes.Repeat([]byte{0x5a}, 16)),
				SnapshotLoader: SnapshotLoaderFunc(func(faultfs.FS, string, Manifest) (SnapshotState, error) {
					return SnapshotState{Revision: 0}, nil
				}),
				AuditTail: func(TailTruncation) error {
					auditCalls++
					return nil
				},
			})
			var structured *gapdb.Error
			if !errors.As(err, &structured) || structured.Code != test.wantCode || !structured.OperationApplied {
				t.Fatalf("RecoverDatabase error = %#v, want %s operation_applied=true", err, test.wantCode)
			}
			if auditCalls != 1 {
				t.Fatalf("tail audit calls = %d, want 1", auditCalls)
			}
			walBytes, readErr := os.ReadFile(walPath)
			if readErr != nil || !bytes.Equal(walBytes, header) {
				t.Fatalf("repaired WAL = %x, %v, want header %x", walBytes, readErr, header)
			}
			identityBytes, readErr := os.ReadFile(filepath.Join(dir, IdentityFilename))
			wantIdentity := identity
			if test.identityApplied {
				wantIdentity = updatedIdentity
			}
			wantIdentityBytes, encodeErr := EncodeIdentity(wantIdentity)
			if readErr != nil || encodeErr != nil || !bytes.Equal(identityBytes, wantIdentityBytes) {
				t.Fatalf("authoritative identity = %x, read=%v encode=%v, want %x", identityBytes, readErr, encodeErr, wantIdentityBytes)
			}
		})
	}
}

func TestRecoveryAppliedRevisionRangeErrorRoundTripsProtocolV1(t *testing.T) {
	dir := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	identity := Identity{DatabaseID: id, ReservedRevisionEnd: 100, Generation: 1}
	writeIdentityDirect(t, dir, identity)
	manifest := Manifest{DatabaseID: id, Generation: 1, SnapshotFile: "snapshot-00000000000000000000.gdb", SnapshotRevision: 0, SnapshotSHA256: strings64("a"), WALFile: "wal-00000000000000000001.gdb", WALStartRevision: 1}
	manifestBytes, _ := EncodeManifest(manifest)
	if err := os.WriteFile(filepath.Join(dir, ManifestFilename), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	header, _ := EncodeWALHeader(WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1})
	if err := os.WriteFile(filepath.Join(dir, manifest.WALFile), append(bytes.Clone(header), 'C'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, recoveryErr := RecoverDatabase(faultfs.NewOS(nil), dir, RecoveryOptions{
		Limits:          gapdb.DefaultOptions().Limits,
		ReservationSize: 0,
		SnapshotLoader: SnapshotLoaderFunc(func(faultfs.FS, string, Manifest) (SnapshotState, error) {
			return SnapshotState{Revision: 0}, nil
		}),
		AuditTail: func(TailTruncation) error { return nil },
	})
	var recoveryEvidence *gapdb.Error
	if !errors.As(recoveryErr, &recoveryEvidence) || recoveryEvidence.Code != gapdb.CodeRevisionRangeExhausted || !recoveryEvidence.OperationApplied {
		t.Fatalf("RecoverDatabase error = %#v, want applied REVISION_RANGE_EXHAUSTED", recoveryErr)
	}
	encoded, err := wire.EncodeResponse(wire.Response{
		SchemaVersion: wire.SchemaVersion,
		OK:            false,
		DatabaseID:    id.String(),
		Operation:     wire.OperationGet,
		Error:         recoveryEvidence,
	})
	if err != nil {
		t.Fatalf("EncodeResponse(recovery error) = %v", err)
	}
	decoded, err := wire.DecodeResponse(encoded, gapdb.DefaultMaxFrameBytes)
	if err != nil {
		t.Fatalf("DecodeResponse(recovery error) = %v", err)
	}
	if decoded.Error == nil || decoded.Error.Code != gapdb.CodeRevisionRangeExhausted || !decoded.Error.OperationApplied || decoded.Error.ReservedRevisionEnd == nil || *decoded.Error.ReservedRevisionEnd != identity.ReservedRevisionEnd {
		t.Fatalf("decoded recovery evidence = %#v", decoded.Error)
	}
}

func TestPersistenceErrorsRespectProtocolV1EvidenceSchemas(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	other := mustDatabaseID(t, "ffeeddccbbaa99887766554433221100")
	errorsToEncode := []error{
		corruptIdentity("bad identity", errors.New("cause")),
		corruptManifest("bad manifest", errors.New("cause")),
		corruptWAL("bad WAL", "wal.gdb", 2, errors.New("cause")),
		unknownStorageFormat("CURRENT", 2),
		databaseIDMismatch(id, other, "CURRENT"),
		revisionRangeError(100, "exhausted", errors.New("cause")),
		ioFailureApplied("truncate", "wal.gdb", errors.New("cause")),
		storageDegraded("sync", 3, 2, errors.New("cause")),
		corruptSnapshot("bad snapshot", "snapshot.gdb", 4, errors.New("cause")),
		auditAfterApplyError("tail_truncate", "audit failed", 3, errors.New("cause")),
	}
	for _, failure := range errorsToEncode {
		var structured *gapdb.Error
		if !errors.As(failure, &structured) {
			t.Fatalf("failure %T is not structured", failure)
		}
		if _, err := wire.EncodeResponse(wire.Response{SchemaVersion: wire.SchemaVersion, OK: false, DatabaseID: id.String(), Operation: wire.OperationGet, Error: structured}); err != nil {
			t.Errorf("EncodeResponse(%s) = %v", structured.Code, err)
		}
	}
}

func TestSnapshotStateMustRemainStrictlyKeySorted(t *testing.T) {
	manifest := testManifest(t)
	manifest.SnapshotRevision = 2
	manifest.SnapshotFile = "snapshot-00000000000000000002.gdb"
	manifest.WALStartRevision = 3
	manifest.WALFile = "wal-00000000000000000003.gdb"
	snapshot := SnapshotState{Revision: 2, Records: []gapdb.Record{
		gapdb.NewRecord("b", nil, 1, nil),
		gapdb.NewRecord("a", nil, 2, nil),
	}}
	if err := validateSnapshotState(snapshot, manifest); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptSnapshot}) {
		t.Fatalf("validateSnapshotState = %v, want CORRUPT_SNAPSHOT", err)
	}
}

func strings64(value string) string { return strings.Repeat(value, 64) }
