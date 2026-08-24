package persist

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

func TestSnapshotRoundTripSortsCopiesAndFiltersExpiry(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	asOf := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	expired, liveExpiry := asOf, asOf.Add(time.Hour)
	value := []byte{0, 1, 0xff}
	state := SnapshotState{Revision: 7, Records: []gapdb.Record{
		gapdb.NewRecord("z", value, 7, &liveExpiry),
		gapdb.NewRecord("expired", []byte("gone"), 6, &expired),
		gapdb.NewRecord("a", nil, 2, nil),
	}}
	encoded, digest, err := EncodeSnapshot(id, state, asOf, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	value[0] = 9
	if string(encoded[:8]) != "GAPSNAP1" || binary.BigEndian.Uint64(encoded[36:44]) != 2 {
		t.Fatalf("snapshot header = %x", encoded[:64])
	}
	if got := sha256.Sum256(encoded[:len(encoded)-32]); got != digest || !bytes.Equal(got[:], encoded[len(encoded)-32:]) {
		t.Fatal("snapshot digest does not cover header and payload")
	}
	decoded, err := DecodeSnapshot(encoded, id, digest, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Revision != 7 || len(decoded.Records) != 2 || decoded.Records[0].Key != "a" || decoded.Records[1].Key != "z" || !bytes.Equal(decoded.Records[1].Value, []byte{0, 1, 0xff}) || decoded.Records[1].ExpiresAt == nil || !decoded.Records[1].ExpiresAt.Equal(liveExpiry) {
		t.Fatalf("decoded snapshot = %+v", decoded)
	}
}

func TestSnapshotDecodeFailsClosedOnStructureAndIntegrity(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	state := SnapshotState{Revision: 2, Records: []gapdb.Record{gapdb.NewRecord("a", []byte("v"), 1, nil)}}
	encoded, digest, err := EncodeSnapshot(id, state, time.Unix(0, 0), gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "magic", mutate: func(value []byte) { value[0] = 'X' }},
		{name: "version", mutate: func(value []byte) { binary.BigEndian.PutUint16(value[8:10], 2) }},
		{name: "reserved", mutate: func(value []byte) { value[52] = 1 }},
		{name: "payload-length", mutate: func(value []byte) { binary.BigEndian.PutUint64(value[44:52], 1) }},
		{name: "checksum", mutate: func(value []byte) { value[len(value)-1] ^= 1 }},
		{name: "record-revision", mutate: func(value []byte) { binary.BigEndian.PutUint64(value[72:80], 3) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := bytes.Clone(encoded)
			test.mutate(bad)
			if _, err := DecodeSnapshot(bad, id, digest, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeCorruptSnapshot}) && !errors.Is(err, &gapdb.Error{Code: gapdb.CodeUnknownFormat}) {
				t.Fatalf("DecodeSnapshot = %v", err)
			}
		})
	}
}

func TestInstallSnapshotGenerationInstallsCurrentLast(t *testing.T) {
	dir := t.TempDir()
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	old := Manifest{DatabaseID: id, Generation: 1, SnapshotFile: "snapshot-00000000000000000000.gdb", SnapshotRevision: 0, SnapshotSHA256: string(bytes.Repeat([]byte{'0'}, 64)), WALFile: "wal-00000000000000000001.gdb", WALStartRevision: 1}
	writeManifestDirect(t, dir, old)
	barrierCalled := false
	result, err := InstallSnapshotGeneration(SnapshotInstallOptions{
		FS:            faultfs.NewOS(nil),
		Directory:     dir,
		Current:       old,
		State:         SnapshotState{Revision: 2, Records: []gapdb.Record{gapdb.NewRecord("key", []byte("opaque"), 2, nil)}},
		AsOf:          time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		Limits:        gapdb.DefaultOptions().Limits,
		Random:        bytes.NewReader(bytes.Repeat([]byte{0x5a}, 64)),
		WALBufferSize: 4096,
		Barrier: func() error {
			barrierCalled = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.WAL.Close()
	if !barrierCalled || result.Manifest.Generation != 2 || result.Manifest.SnapshotRevision != 2 || result.Manifest.WALStartRevision != 3 {
		t.Fatalf("install result = %+v, barrier=%v", result, barrierCalled)
	}
	onDisk, err := ReadManifest(faultfs.NewOS(nil), dir, id, 2)
	if err != nil || onDisk != result.Manifest {
		t.Fatalf("CURRENT = %+v, %v", onDisk, err)
	}
	loaded, err := (SnapshotFileStore{Limits: gapdb.DefaultOptions().Limits}).Load(faultfs.NewOS(nil), dir, onDisk)
	if err != nil || loaded.Revision != 2 || len(loaded.Records) != 1 || string(loaded.Records[0].Value) != "opaque" {
		t.Fatalf("loaded snapshot = %+v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(dir, old.SnapshotFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test unexpectedly created old snapshot: %v", err)
	}
}

func TestSnapshotInstallFaultMatrixKeepsCompleteOldOrNewAuthority(t *testing.T) {
	points := []faultfs.Point{
		faultfs.PointSnapshotTempCreate, faultfs.PointSnapshotTempWrite, faultfs.PointSnapshotTempSync,
		faultfs.PointSnapshotRename, faultfs.PointSnapshotDirectorySync, faultfs.PointNextWALCreate,
		faultfs.PointNextWALSync, faultfs.PointNextWALRename, faultfs.PointManifestTempCreate,
		faultfs.PointManifestTempWrite, faultfs.PointManifestTempSync, faultfs.PointManifestRename,
		faultfs.PointManifestDirectorySync,
	}
	for _, point := range points {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(string(point)+"-"+string(phase), func(t *testing.T) {
				dir := t.TempDir()
				id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
				old := Manifest{DatabaseID: id, Generation: 1, SnapshotFile: "snapshot-00000000000000000000.gdb", SnapshotRevision: 0, SnapshotSHA256: string(bytes.Repeat([]byte{'0'}, 64)), WALFile: "wal-00000000000000000001.gdb", WALStartRevision: 1}
				writeManifestDirect(t, dir, old)
				injector := faultfs.NewInjector(11, faultfs.Rule{Point: point, Phase: phase, Occurrence: 1, Seed: 11, Err: os.ErrPermission})
				result, err := InstallSnapshotGeneration(SnapshotInstallOptions{FS: faultfs.NewOS(injector), Directory: dir, Current: old, State: SnapshotState{Revision: 2, Records: []gapdb.Record{gapdb.NewRecord("k", []byte("v"), 2, nil)}}, AsOf: time.Unix(1, 0), Limits: gapdb.DefaultOptions().Limits, Random: bytes.NewReader(bytes.Repeat([]byte{3}, 64)), Barrier: func() error { return nil }})
				if result.WAL != nil {
					_ = result.WAL.Close()
				}
				if err == nil {
					t.Fatal("fault unexpectedly succeeded")
				}
				onDisk, readErr := ReadManifest(faultfs.NewOS(nil), dir, id, 1)
				if readErr != nil {
					t.Fatalf("CURRENT became invalid: %v", readErr)
				}
				newAuthority := point == faultfs.PointManifestDirectorySync || (point == faultfs.PointManifestRename && phase == faultfs.After)
				if newAuthority {
					if onDisk.Generation != 2 {
						t.Fatalf("post-rename authority = %+v", onDisk)
					}
					if _, loadErr := (SnapshotFileStore{Limits: gapdb.DefaultOptions().Limits}).Load(faultfs.NewOS(nil), dir, onDisk); loadErr != nil {
						t.Fatalf("new authority snapshot incomplete: %v", loadErr)
					}
				} else if onDisk != old {
					t.Fatalf("authority changed before successful CURRENT install: %+v", onDisk)
				}
			})
		}
	}
}

func FuzzDecodeSnapshotNeverPanics(f *testing.F) {
	id, err := ParseDatabaseID("00112233445566778899aabbccddeeff")
	if err != nil {
		f.Fatal(err)
	}
	encoded, digest, _ := EncodeSnapshot(id, SnapshotState{}, time.Unix(0, 0), gapdb.DefaultOptions().Limits)
	f.Add(encoded)
	f.Fuzz(func(t *testing.T, value []byte) {
		_, _ = DecodeSnapshot(value, id, digest, gapdb.DefaultOptions().Limits)
	})
}

func writeManifestDirect(t *testing.T, directory string, manifest Manifest) {
	t.Helper()
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ManifestFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
