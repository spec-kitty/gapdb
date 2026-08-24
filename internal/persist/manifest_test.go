package persist

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

func TestManifestOrderedChecksummedFrame(t *testing.T) {
	manifest := testManifest(t)
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded[:8]); got != "GAPCUR01" {
		t.Fatalf("magic = %q", got)
	}
	payloadLength := int(binary.BigEndian.Uint32(encoded[12:16]))
	if len(encoded) != 20+payloadLength {
		t.Fatalf("size = %d, payload=%d", len(encoded), payloadLength)
	}
	if got, want := binary.BigEndian.Uint32(encoded[len(encoded)-4:]), crc32.Checksum(encoded[:len(encoded)-4], castagnoli); got != want {
		t.Fatalf("CRC = %08x, want %08x", got, want)
	}
	wantPayload := `{"database_id":"00112233445566778899aabbccddeeff","generation":7,"snapshot_file":"snapshot-00000000000000000110.gdb","snapshot_revision":110,"snapshot_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","wal_file":"wal-00000000000000000111.gdb","wal_start_revision":111}`
	if got := string(encoded[16 : len(encoded)-4]); got != wantPayload {
		t.Fatalf("payload:\n got %s\nwant %s", got, wantPayload)
	}
	decoded, err := DecodeManifest(encoded, manifest.DatabaseID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != manifest {
		t.Fatalf("decoded = %#v, want %#v", decoded, manifest)
	}
}

func TestManifestStrictValidationBeforeWrites(t *testing.T) {
	valid := testManifest(t)
	for _, tt := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"generation-regression", func(value *Manifest) { value.Generation = 6 }},
		{"snapshot-traversal", func(value *Manifest) { value.SnapshotFile = "../snapshot.gdb" }},
		{"wal-separator", func(value *Manifest) { value.WALFile = "dir/wal.gdb" }},
		{"uppercase-hash", func(value *Manifest) { value.SnapshotSHA256 = strings.Repeat("A", 64) }},
		{"revision-relation", func(value *Manifest) { value.WALStartRevision = value.SnapshotRevision + 2 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manifest := valid
			tt.mutate(&manifest)
			recorder := &faultfs.Recorder{}
			if err := InstallManifest(faultfs.NewOS(recorder), t.TempDir(), manifest, 6, bytes.NewReader(make([]byte, 8))); err == nil {
				t.Fatal("InstallManifest accepted invalid manifest")
			}
			if len(recorder.Events()) != 0 {
				t.Fatalf("invalid manifest touched filesystem: %v", recorder.Events())
			}
		})
	}
}

func TestManifestRejectsNoncanonicalAndCorruptFrames(t *testing.T) {
	manifest := testManifest(t)
	valid, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	unknownPayload := bytes.Replace(valid[16:len(valid)-4], []byte(`"generation":7`), []byte(`"generation":7,"unknown":true`), 1)
	unknown := frameManifestPayload(unknownPayload)
	reorderedPayload := []byte(`{"generation":7,"database_id":"00112233445566778899aabbccddeeff","snapshot_file":"snapshot-00000000000000000110.gdb","snapshot_revision":110,"snapshot_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","wal_file":"wal-00000000000000000111.gdb","wal_start_revision":111}`)
	reordered := frameManifestPayload(reorderedPayload)
	badCRC := bytes.Clone(valid)
	badCRC[len(badCRC)-1] ^= 1
	badFlags := bytes.Clone(valid)
	badFlags[11] = 1
	resetManifestCRC(badFlags)
	for _, value := range [][]byte{unknown, reordered, badCRC, badFlags, valid[:len(valid)-1]} {
		if _, err := DecodeManifest(value, manifest.DatabaseID, 7); err == nil {
			t.Fatal("DecodeManifest accepted invalid frame")
		}
	}
}

func TestManifestInstallOrderAndAtomicFaultStates(t *testing.T) {
	manifest := testManifest(t)
	dir := t.TempDir()
	recorder := &faultfs.Recorder{}
	if err := InstallManifest(faultfs.NewOS(recorder), dir, manifest, 6, bytes.NewReader(bytes.Repeat([]byte{1}, 8))); err != nil {
		t.Fatal(err)
	}
	var points []faultfs.Point
	for _, event := range recorder.Events() {
		if event.Phase == faultfs.Before {
			points = append(points, event.Point)
		}
	}
	want := []faultfs.Point{faultfs.PointManifestTempCreate, faultfs.PointManifestTempWrite, faultfs.PointManifestTempSync, faultfs.PointManifestRename, faultfs.PointManifestDirectorySync}
	if !reflect.DeepEqual(points, want) {
		t.Fatalf("points = %v, want %v", points, want)
	}
	loaded, err := ReadManifest(faultfs.NewOS(nil), dir, manifest.DatabaseID, 7)
	if err != nil || loaded != manifest {
		t.Fatalf("ReadManifest = %#v, %v", loaded, err)
	}

	old := manifest
	old.Generation = 7
	updated := manifest
	updated.Generation = 8
	for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
		t.Run("rename-"+string(phase), func(t *testing.T) {
			faultDir := t.TempDir()
			oldBytes, _ := EncodeManifest(old)
			if err := os.WriteFile(filepath.Join(faultDir, ManifestFilename), oldBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			fsys := faultfs.NewOS(faultfs.NewInjector(4, faultfs.Rule{Point: faultfs.PointManifestRename, Phase: phase, Occurrence: 1, Seed: 4, Err: errors.New("stop")}))
			if err := InstallManifest(fsys, faultDir, updated, 7, bytes.NewReader(bytes.Repeat([]byte{2}, 8))); err == nil {
				t.Fatal("InstallManifest succeeded across fault")
			}
			loaded, err := ReadManifest(faultfs.NewOS(nil), faultDir, updated.DatabaseID, 7)
			if err != nil || (loaded != old && loaded != updated) {
				t.Fatalf("authoritative manifest = %#v, %v", loaded, err)
			}
		})
	}
}

func TestManifestInstallReportsWhetherAuthorityMayHaveChanged(t *testing.T) {
	old := testManifest(t)
	updated := old
	updated.Generation++
	for _, test := range []struct {
		name    string
		point   faultfs.Point
		phase   faultfs.Phase
		applied bool
	}{
		{"rename-before", faultfs.PointManifestRename, faultfs.Before, false},
		{"rename-after", faultfs.PointManifestRename, faultfs.After, true},
		{"directory-sync-before", faultfs.PointManifestDirectorySync, faultfs.Before, true},
		{"directory-sync-after", faultfs.PointManifestDirectorySync, faultfs.After, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			oldBytes, err := EncodeManifest(old)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ManifestFilename), oldBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected")
			fsys := faultfs.NewOS(faultfs.NewInjector(41, faultfs.Rule{Point: test.point, Phase: test.phase, Occurrence: 1, Seed: 41, Err: injected}))
			err = InstallManifest(fsys, dir, updated, old.Generation, bytes.NewReader(bytes.Repeat([]byte{2}, 8)))
			var structured *gapdb.Error
			if !errors.As(err, &structured) || structured.Code != gapdb.CodeIOError || structured.OperationApplied != test.applied {
				t.Fatalf("InstallManifest error = %#v, want IO_ERROR operation_applied=%v", err, test.applied)
			}
			onDisk, readErr := ReadManifest(faultfs.NewOS(nil), dir, old.DatabaseID, old.Generation)
			want := old
			if test.applied {
				want = updated
			}
			if readErr != nil || onDisk != want {
				t.Fatalf("authoritative manifest = %#v, %v, want %#v", onDisk, readErr, want)
			}
		})
	}
}

func testManifest(t *testing.T) Manifest {
	t.Helper()
	return Manifest{
		DatabaseID:       mustDatabaseID(t, "00112233445566778899aabbccddeeff"),
		Generation:       7,
		SnapshotFile:     "snapshot-00000000000000000110.gdb",
		SnapshotRevision: 110,
		SnapshotSHA256:   strings.Repeat("a", 64),
		WALFile:          "wal-00000000000000000111.gdb",
		WALStartRevision: 111,
	}
}

func frameManifestPayload(payload []byte) []byte {
	value := make([]byte, 20+len(payload))
	copy(value[:8], "GAPCUR01")
	binary.BigEndian.PutUint16(value[8:10], 1)
	binary.BigEndian.PutUint32(value[12:16], uint32(len(payload)))
	copy(value[16:], payload)
	resetManifestCRC(value)
	return value
}

func resetManifestCRC(value []byte) {
	binary.BigEndian.PutUint32(value[len(value)-4:], crc32.Checksum(value[:len(value)-4], castagnoli))
}
