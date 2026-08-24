package persist

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

func TestWALHeaderFixedFormat(t *testing.T) {
	header := WALHeader{DatabaseID: mustDatabaseID(t, "00112233445566778899aabbccddeeff"), FirstRevision: 111, Generation: 8}
	encoded, err := EncodeWALHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != WALHeaderSize || string(encoded[:8]) != "GAPWAL01" {
		t.Fatalf("header size/magic = %d/%q", len(encoded), encoded[:8])
	}
	if got := binary.BigEndian.Uint64(encoded[28:36]); got != 111 {
		t.Fatalf("first revision = %d", got)
	}
	if got, want := binary.BigEndian.Uint32(encoded[60:]), crc32.Checksum(encoded[:60], castagnoli); got != want {
		t.Fatalf("CRC = %08x, want %08x", got, want)
	}
	decoded, err := DecodeWALHeader(encoded, header.DatabaseID)
	if err != nil || decoded != header {
		t.Fatalf("DecodeWALHeader = %#v, %v", decoded, err)
	}
	for _, index := range []int{0, 8, 10, 44, 63} {
		invalid := bytes.Clone(encoded)
		invalid[index] ^= 1
		if index != 63 {
			resetCRC(invalid)
		}
		if _, err := DecodeWALHeader(invalid, header.DatabaseID); err == nil {
			t.Fatalf("DecodeWALHeader accepted mutation at %d", index)
		}
	}
}

func TestCommitFrameRoundTripAndAtomicOrder(t *testing.T) {
	expires := time.Unix(0, 123456789).UTC()
	commit := CommitFrame{Revision: 113, Effects: []Effect{
		{Kind: gapdb.ChangePut, Key: "a", Value: []byte{}, ExpiresAt: &expires},
		{Kind: gapdb.ChangeDelete, Key: "b"},
		{Kind: gapdb.ChangeExpire, Key: "c"},
	}}
	encoded, err := EncodeCommitFrame(commit, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded[:4]) != "CMIT" || int(binary.BigEndian.Uint32(encoded[8:12])) != len(encoded) {
		t.Fatalf("frame header invalid")
	}
	if got, want := binary.BigEndian.Uint32(encoded[len(encoded)-4:]), crc32.Checksum(encoded[:len(encoded)-4], castagnoli); got != want {
		t.Fatalf("CRC = %08x, want %08x", got, want)
	}
	decoded, err := DecodeCommitFrame(encoded, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, commit) {
		t.Fatalf("decoded = %#v, want %#v", decoded, commit)
	}
}

func TestCommitFrameRejectsInvalidAndCorruptEffects(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	tests := []CommitFrame{
		{Revision: 0, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a", Value: []byte{}}}},
		{Revision: 1},
		{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a"}, {Kind: gapdb.ChangeDelete, Key: "a"}}},
		{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangeDelete, Key: "a", Value: []byte{1}}}},
		{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangeExpire, Key: "a", ExpiresAt: ptrTime(time.Unix(1, 0))}}},
		{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a", ExpiresAt: ptrTime(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC))}}},
		{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: strings.Repeat("k", limits.MaxKeyBytes+1)}}},
		{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a", Value: make([]byte, limits.MaxValueBytes+1)}}},
	}
	for index, commit := range tests {
		if _, err := EncodeCommitFrame(commit, limits); err == nil {
			t.Fatalf("case %d accepted", index)
		}
	}
	valid, err := EncodeCommitFrame(CommitFrame{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a", Value: []byte{1}}}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	badCRC := bytes.Clone(valid)
	badCRC[len(badCRC)-1] ^= 1
	badLength := bytes.Clone(valid)
	binary.BigEndian.PutUint32(badLength[8:12], uint32(len(badLength)+1))
	resetFrameCRC(badLength)
	badReserved := bytes.Clone(valid)
	badReserved[30] = 1
	resetFrameCRC(badReserved)
	for _, encoded := range [][]byte{badCRC, badLength, badReserved} {
		if _, err := DecodeCommitFrame(encoded, limits); err == nil {
			t.Fatal("DecodeCommitFrame accepted corruption")
		}
	}
}

func TestWALAppendBarrierCoversEarlierFrames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal-00000000000000000001.gdb")
	recorder := &faultfs.Recorder{}
	writer, err := CreateWAL(faultfs.NewOS(recorder), path, WALHeader{DatabaseID: mustDatabaseID(t, "00112233445566778899aabbccddeeff"), FirstRevision: 1, Generation: 1}, 4096, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, revision := range []gapdb.Revision{1, 3} {
		if err := writer.Append(CommitFrame{Revision: revision, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "k" + revision.String(), Value: []byte{byte(revision)}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if writer.DurableThrough() != 0 {
		t.Fatalf("durable through advanced before barrier")
	}
	if err := writer.Barrier(); err != nil {
		t.Fatal(err)
	}
	if writer.DurableThrough() != 3 {
		t.Fatalf("durable through = %d, want 3", writer.DurableThrough())
	}
	if err := writer.Append(CommitFrame{Revision: 3, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "x"}}}); err == nil {
		t.Fatal("duplicate revision accepted")
	}
	if err := writer.Append(CommitFrame{Revision: 2, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "x"}}}); err == nil {
		t.Fatal("revision regression accepted")
	}

	var barrier []faultfs.Event
	for _, event := range recorder.Events() {
		if event.Point == faultfs.PointWALBufferFlush || event.Point == faultfs.PointWALFileSync {
			barrier = append(barrier, event)
		}
	}
	want := []faultfs.Event{
		{Point: faultfs.PointWALFileSync, Phase: faultfs.Before},
		{Point: faultfs.PointWALFileSync, Phase: faultfs.After},
		{Point: faultfs.PointWALBufferFlush, Phase: faultfs.Before},
		{Point: faultfs.PointWALBufferFlush, Phase: faultfs.After},
		{Point: faultfs.PointWALFileSync, Phase: faultfs.Before},
		{Point: faultfs.PointWALFileSync, Phase: faultfs.After},
	}
	if !reflect.DeepEqual(barrier, want) {
		t.Fatalf("barrier events = %#v, want %#v", barrier, want)
	}
}

func TestWALBarrierFailureNeverAdvancesDurability(t *testing.T) {
	for _, point := range []faultfs.Point{faultfs.PointWALBufferFlush, faultfs.PointWALFileSync} {
		t.Run(string(point), func(t *testing.T) {
			dir := t.TempDir()
			// WAL creation uses one file sync, so target occurrence two for the barrier.
			occurrence := uint64(1)
			if point == faultfs.PointWALFileSync {
				occurrence = 2
			}
			injector := faultfs.NewInjector(5, faultfs.Rule{Point: point, Phase: faultfs.Before, Occurrence: occurrence, Seed: 5, Err: errors.New("fault")})
			writer, err := CreateWAL(faultfs.NewOS(injector), filepath.Join(dir, "wal-00000000000000000001.gdb"), WALHeader{DatabaseID: mustDatabaseID(t, "00112233445566778899aabbccddeeff"), FirstRevision: 1, Generation: 1}, 4096, gapdb.DefaultOptions().Limits)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if err := writer.Append(CommitFrame{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "k"}}}); err != nil {
				t.Fatal(err)
			}
			if err := writer.Barrier(); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeStorageDegraded}) {
				t.Fatalf("Barrier = %v, want STORAGE_DEGRADED", err)
			}
			if writer.DurableThrough() != 0 {
				t.Fatalf("durable through = %d after failure", writer.DurableThrough())
			}
		})
	}
}

func TestWALWriteAllHandlesShortWrites(t *testing.T) {
	dir := t.TempDir()
	base := faultfs.NewOS(nil)
	fsys := &shortWriteFS{FS: base, maximum: 7}
	writer, err := CreateWAL(fsys, filepath.Join(dir, "wal-00000000000000000001.gdb"), WALHeader{DatabaseID: mustDatabaseID(t, "00112233445566778899aabbccddeeff"), FirstRevision: 1, Generation: 1}, 32, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Append(CommitFrame{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "k", Value: []byte("value")}}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Barrier(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenWALForAppendContinuesRecoveredOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal-00000000000000000001.gdb")
	header := WALHeader{DatabaseID: mustDatabaseID(t, "00112233445566778899aabbccddeeff"), FirstRevision: 1, Generation: 1}
	created, err := CreateWAL(faultfs.NewOS(nil), path, header, 128, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Append(CommitFrame{Revision: 2, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if err := created.Barrier(); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWALForAppend(faultfs.NewOS(nil), path, header, 2, 2, 128, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Append(CommitFrame{Revision: 4, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Barrier(); err != nil || reopened.DurableThrough() != 4 {
		t.Fatalf("reopened barrier = %v, durable=%d", err, reopened.DurableThrough())
	}
}

func TestWALClosePerformsCleanShutdownBarrier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal-00000000000000000001.gdb")
	id := mustDatabaseID(t, "00112233445566778899aabbccddeeff")
	recorder := &faultfs.Recorder{}
	writer, err := CreateWAL(faultfs.NewOS(recorder), path, WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1}, 4096, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(CommitFrame{Revision: 1, Effects: []Effect{{Kind: gapdb.ChangePut, Key: "k", Value: []byte("v")}}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	replay, err := RecoverWALFile(faultfs.NewOS(nil), path, id, 0, 10, gapdb.DefaultOptions().Limits)
	if err != nil || len(replay.Commits) != 1 {
		t.Fatalf("clean close replay = %#v, %v", replay, err)
	}
	events := recorder.Events()
	if len(events) < 4 || events[len(events)-4].Point != faultfs.PointWALBufferFlush || events[len(events)-2].Point != faultfs.PointWALFileSync {
		t.Fatalf("clean close did not flush+sync: %v", events)
	}
}

type shortWriteFS struct {
	faultfs.FS
	maximum int
}

func (s *shortWriteFS) Write(_ faultfs.Point, writer io.Writer, value []byte) (int, error) {
	if len(value) > s.maximum {
		value = value[:s.maximum]
	}
	return writer.Write(value)
}

func ptrTime(value time.Time) *time.Time { return &value }

func resetFrameCRC(value []byte) {
	binary.BigEndian.PutUint32(value[len(value)-4:], crc32.Checksum(value[:len(value)-4], castagnoli))
}
