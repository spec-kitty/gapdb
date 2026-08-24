package persist

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
)

func TestIdentityFixedFormatAndStrictDecode(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	identity := Identity{DatabaseID: id, ReservedRevisionEnd: 99, Generation: 7}
	encoded, err := EncodeIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != IdentitySize {
		t.Fatalf("size = %d, want %d", len(encoded), IdentitySize)
	}
	if got := string(encoded[:8]); got != "GAPID001" {
		t.Fatalf("magic = %q", got)
	}
	if got := binary.BigEndian.Uint16(encoded[8:10]); got != 1 {
		t.Fatalf("version = %d", got)
	}
	if got := binary.BigEndian.Uint16(encoded[10:12]); got != IdentitySize {
		t.Fatalf("header length = %d", got)
	}
	if got, want := binary.BigEndian.Uint32(encoded[60:]), crc32.Checksum(encoded[:60], crc32.MakeTable(crc32.Castagnoli)); got != want {
		t.Fatalf("crc = %08x, want %08x", got, want)
	}
	decoded, err := DecodeIdentity(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != identity {
		t.Fatalf("decoded = %#v, want %#v", decoded, identity)
	}

	for _, mutate := range []func([]byte){
		func(value []byte) { value[44] = 1 },
		func(value []byte) { value[0] = 'X' },
		func(value []byte) { value[61] ^= 1 },
		func(value []byte) { binary.BigEndian.PutUint16(value[8:10], 2); resetCRC(value) },
	} {
		invalid := bytes.Clone(encoded)
		mutate(invalid)
		if _, err := DecodeIdentity(invalid); err == nil {
			t.Fatal("DecodeIdentity accepted invalid bytes")
		}
	}
}

func TestRevisionReservationInstallsBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	recorder := &faultfs.Recorder{}
	fsys := faultfs.NewOS(recorder)
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	initial := Identity{DatabaseID: id, ReservedRevisionEnd: 100, Generation: 3}
	writeIdentityDirect(t, dir, initial)

	updated, allocation, err := ReserveRevisions(fsys, dir, initial, 7, 10, bytes.NewReader(bytes.Repeat([]byte{0x4a}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if allocation.First != 101 || allocation.End != 110 || updated.ReservedRevisionEnd != 110 || updated.Generation != 4 {
		t.Fatalf("updated/allocation = %#v %#v", updated, allocation)
	}
	onDisk, err := ReadIdentity(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != updated {
		t.Fatalf("on disk = %#v, want %#v", onDisk, updated)
	}
	var points []faultfs.Point
	for _, event := range recorder.Events() {
		if event.Phase == faultfs.Before && event.Point != faultfs.PointOpen {
			points = append(points, event.Point)
		}
	}
	want := []faultfs.Point{
		faultfs.PointIdentityTempCreate,
		faultfs.PointIdentityTempWrite,
		faultfs.PointIdentityTempSync,
		faultfs.PointIdentityRename,
		faultfs.PointIdentityDirectorySync,
	}
	if !reflect.DeepEqual(points, want) {
		t.Fatalf("install points = %v, want %v", points, want)
	}
}

func TestRevisionReservationFaultsLeaveOldOrNewIdentity(t *testing.T) {
	points := []faultfs.Point{
		faultfs.PointIdentityTempCreate,
		faultfs.PointIdentityTempWrite,
		faultfs.PointIdentityTempSync,
		faultfs.PointIdentityRename,
		faultfs.PointIdentityDirectorySync,
	}
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	old := Identity{DatabaseID: id, ReservedRevisionEnd: 10, Generation: 2}
	wantNew := Identity{DatabaseID: id, ReservedRevisionEnd: 15, Generation: 3}
	for _, point := range points {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(string(point)+"/"+string(phase), func(t *testing.T) {
				dir := t.TempDir()
				writeIdentityDirect(t, dir, old)
				injected := errors.New("injected")
				fsys := faultfs.NewOS(faultfs.NewInjector(8, faultfs.Rule{Point: point, Phase: phase, Occurrence: 1, Seed: 8, Err: injected}))
				if _, _, err := ReserveRevisions(fsys, dir, old, 10, 5, bytes.NewReader(bytes.Repeat([]byte{0x3b}, 32))); err == nil {
					t.Fatal("ReserveRevisions succeeded across injected failure")
				}
				got, err := ReadIdentity(faultfs.NewOS(nil), dir)
				if err != nil {
					t.Fatalf("authoritative identity invalid: %v", err)
				}
				if got != old && got != wantNew {
					t.Fatalf("identity = %#v, want complete old or new", got)
				}
			})
		}
	}
}

func TestIdentityInstallReportsWhetherAuthorityMayHaveChanged(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	old := Identity{DatabaseID: id, ReservedRevisionEnd: 10, Generation: 2}
	updated := Identity{DatabaseID: id, ReservedRevisionEnd: 15, Generation: 3}
	for _, test := range []struct {
		name    string
		point   faultfs.Point
		phase   faultfs.Phase
		applied bool
	}{
		{"rename-before", faultfs.PointIdentityRename, faultfs.Before, false},
		{"rename-after", faultfs.PointIdentityRename, faultfs.After, true},
		{"directory-sync-before", faultfs.PointIdentityDirectorySync, faultfs.Before, true},
		{"directory-sync-after", faultfs.PointIdentityDirectorySync, faultfs.After, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeIdentityDirect(t, dir, old)
			injected := errors.New("injected")
			fsys := faultfs.NewOS(faultfs.NewInjector(81, faultfs.Rule{Point: test.point, Phase: test.phase, Occurrence: 1, Seed: 81, Err: injected}))
			_, _, err := ReserveRevisions(fsys, dir, old, 10, 5, bytes.NewReader(bytes.Repeat([]byte{0x3b}, 32)))
			var structured *gapdb.Error
			if !errors.As(err, &structured) || structured.Code != gapdb.CodeIOError || structured.OperationApplied != test.applied {
				t.Fatalf("ReserveRevisions error = %#v, want IO_ERROR operation_applied=%v", err, test.applied)
			}
			onDisk, readErr := ReadIdentity(faultfs.NewOS(nil), dir)
			want := old
			if test.applied {
				want = updated
			}
			if readErr != nil || onDisk != want {
				t.Fatalf("authoritative identity = %#v, %v, want %#v", onDisk, readErr, want)
			}
		})
	}
}

func TestRevisionReservationRejectsUnsafeRanges(t *testing.T) {
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	fsys := faultfs.NewOS(nil)
	for _, tt := range []struct {
		name      string
		identity  Identity
		recovered gapdb.Revision
		rangeSize uint64
	}{
		{"recovered-above-reserved", Identity{DatabaseID: id, ReservedRevisionEnd: 4, Generation: 1}, 5, 1},
		{"reserved-overflow", Identity{DatabaseID: id, ReservedRevisionEnd: gapdb.Revision(math.MaxUint64), Generation: 1}, 1, 1},
		{"generation-overflow", Identity{DatabaseID: id, ReservedRevisionEnd: 4, Generation: math.MaxUint64}, 1, 1},
		{"zero-range", Identity{DatabaseID: id, ReservedRevisionEnd: 4, Generation: 1}, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ReserveRevisions(fsys, t.TempDir(), tt.identity, tt.recovered, tt.rangeSize, bytes.NewReader(make([]byte, 32))); err == nil {
				t.Fatal("ReserveRevisions accepted unsafe range")
			}
		})
	}
}

func TestCreateIdentityOnceAndOwnerLock(t *testing.T) {
	dir := t.TempDir()
	fsys := faultfs.NewOS(nil)
	identity, created, err := LoadOrCreateIdentity(fsys, dir, bytes.NewReader(bytes.Repeat([]byte{0x11}, 64)))
	if err != nil || !created {
		t.Fatalf("LoadOrCreateIdentity = %#v, %v, want created", identity, err)
	}
	loaded, created, err := LoadOrCreateIdentity(fsys, dir, bytes.NewReader(bytes.Repeat([]byte{0x22}, 64)))
	if err != nil || created || loaded != identity {
		t.Fatalf("second LoadOrCreateIdentity = %#v, %v, created=%v", loaded, err, created)
	}
	info, err := os.Stat(filepath.Join(dir, IdentityFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %o", info.Mode().Perm())
	}

	owner, err := AcquireOwner(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := AcquireOwner(fsys, dir); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeOwnerExists}) {
		t.Fatalf("second AcquireOwner = %v, want OWNER_EXISTS", err)
	}
}

func mustDatabaseID(t *testing.T, value string) DatabaseID {
	t.Helper()
	id, err := ParseDatabaseID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func resetCRC(value []byte) {
	binary.BigEndian.PutUint32(value[len(value)-4:], crc32.Checksum(value[:len(value)-4], crc32.MakeTable(crc32.Castagnoli)))
}

func writeIdentityDirect(t *testing.T, dir string, identity Identity) {
	t.Helper()
	encoded, err := EncodeIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, IdentityFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
