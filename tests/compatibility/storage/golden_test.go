package storage_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gapdb/gapdb"
	"gapdb/internal/persist"
)

var updateGoldens = flag.Bool("update-storage-goldens", false, "rewrite storage fixtures from the independent contract encoders")

func TestStorageGoldenFixtures(t *testing.T) {
	id := contractDatabaseID()
	fixtures := map[string][]byte{
		"identity-v1.hex": contractIdentity(id),
		"manifest-v1.hex": contractManifest(id),
		"wal-v1.hex":      contractWAL(id),
		"wal-tail-v1.hex": contractWALTail(id),
	}
	if *updateGoldens {
		for name, value := range fixtures {
			if err := os.WriteFile(filepath.Join("testdata", name), []byte(hex.EncodeToString(value)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, want := range fixtures {
		encoded, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs from the independent contract encoding", name)
		}
	}

	identityBytes := mustFixture(t, "identity-v1.hex")
	if len(identityBytes) != 64 {
		t.Fatalf("identity size = %d", len(identityBytes))
	}
	identity, err := persist.DecodeIdentity(identityBytes)
	if err != nil || identity.DatabaseID != id || identity.ReservedRevisionEnd != 100 || identity.Generation != 3 {
		t.Fatalf("identity = %#v, %v", identity, err)
	}

	manifestBytes := mustFixture(t, "manifest-v1.hex")
	manifest, err := persist.DecodeManifest(manifestBytes, id, 3)
	if err != nil || manifest.Generation != 3 || manifest.SnapshotRevision != 0 || manifest.WALStartRevision != 1 {
		t.Fatalf("manifest = %#v, %v", manifest, err)
	}
	payloadLength := int(binary.BigEndian.Uint32(manifestBytes[12:16]))
	if len(manifestBytes) != 20+payloadLength {
		t.Fatalf("manifest size does not match payload")
	}

	walBytes := mustFixture(t, "wal-v1.hex")
	if len(walBytes) <= persist.WALHeaderSize {
		t.Fatal("WAL fixture has no commit")
	}
	header, err := persist.DecodeWALHeader(walBytes[:persist.WALHeaderSize], id)
	if err != nil || header.FirstRevision != 1 || header.Generation != 3 {
		t.Fatalf("WAL header = %#v, %v", header, err)
	}
	commit, err := persist.DecodeCommitFrame(walBytes[persist.WALHeaderSize:], gapdb.DefaultOptions().Limits)
	if err != nil || commit.Revision != 2 || len(commit.Effects) != 1 || commit.Effects[0].Key != "k" || !bytes.Equal(commit.Effects[0].Value, []byte{0, 1}) {
		t.Fatalf("commit = %#v, %v", commit, err)
	}

	tailBytes := mustFixture(t, "wal-tail-v1.hex")
	replay, err := persist.ScanWAL(bytes.NewReader(tailBytes), int64(len(tailBytes)), id, 0, 100, gapdb.DefaultOptions().Limits)
	if err != nil || replay.Tail == nil || replay.Tail.NewSize != int64(len(walBytes)) || len(replay.Commits) != 1 {
		t.Fatalf("tail replay = %#v, %v", replay, err)
	}
}

func FuzzStorageDecoders(f *testing.F) {
	for _, name := range []string{"identity-v1.hex", "manifest-v1.hex", "wal-v1.hex", "wal-tail-v1.hex"} {
		encoded, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		value, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(value)
	}
	id := contractDatabaseID()
	limits := gapdb.DefaultOptions().Limits
	limits.MaxFrameBytes = 4096
	limits.MaxBatchBytes = 4096
	limits.MaxValueBytes = 2048
	limits.MaxKeyBytes = 256
	limits.MaxBatchOperations = 32
	f.Fuzz(func(t *testing.T, value []byte) {
		if len(value) > 8192 {
			t.Skip()
		}
		_, _ = persist.DecodeIdentity(value)
		_, _ = persist.DecodeManifest(value, id, 0)
		_, _ = persist.DecodeWALHeader(value, id)
		_, _ = persist.DecodeCommitFrame(value, limits)
		_, _ = persist.ScanWAL(bytes.NewReader(value), int64(len(value)), id, 0, 100, limits)
	})
}

func contractDatabaseID() persist.DatabaseID {
	var id persist.DatabaseID
	copy(id[:], []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	return id
}

func contractIdentity(id persist.DatabaseID) []byte {
	value := make([]byte, 64)
	copy(value[:8], "GAPID001")
	binary.BigEndian.PutUint16(value[8:10], 1)
	binary.BigEndian.PutUint16(value[10:12], 64)
	copy(value[12:28], id[:])
	binary.BigEndian.PutUint64(value[28:36], 100)
	binary.BigEndian.PutUint64(value[36:44], 3)
	putCRC(value)
	return value
}

func contractManifest(id persist.DatabaseID) []byte {
	payload, err := json.Marshal(struct {
		DatabaseID       string `json:"database_id"`
		Generation       uint64 `json:"generation"`
		SnapshotFile     string `json:"snapshot_file"`
		SnapshotRevision uint64 `json:"snapshot_revision"`
		SnapshotSHA256   string `json:"snapshot_sha256"`
		WALFile          string `json:"wal_file"`
		WALStartRevision uint64 `json:"wal_start_revision"`
	}{id.String(), 3, "snapshot-00000000000000000000.gdb", 0, strings.Repeat("a", 64), "wal-00000000000000000001.gdb", 1})
	if err != nil {
		panic(err)
	}
	value := make([]byte, 20+len(payload))
	copy(value[:8], "GAPCUR01")
	binary.BigEndian.PutUint16(value[8:10], 1)
	binary.BigEndian.PutUint32(value[12:16], uint32(len(payload)))
	copy(value[16:], payload)
	putCRC(value)
	return value
}

func contractWAL(id persist.DatabaseID) []byte {
	header := make([]byte, 64)
	copy(header[:8], "GAPWAL01")
	binary.BigEndian.PutUint16(header[8:10], 1)
	binary.BigEndian.PutUint16(header[10:12], 64)
	copy(header[12:28], id[:])
	binary.BigEndian.PutUint64(header[28:36], 1)
	binary.BigEndian.PutUint64(header[36:44], 3)
	putCRC(header)
	payload := make([]byte, 23)
	payload[0] = 1
	binary.BigEndian.PutUint32(payload[4:8], 1)
	binary.BigEndian.PutUint32(payload[8:12], 2)
	copy(payload[20:], []byte{'k', 0, 1})
	frame := make([]byte, 32+len(payload))
	copy(frame[:4], "CMIT")
	binary.BigEndian.PutUint16(frame[4:6], 1)
	binary.BigEndian.PutUint32(frame[8:12], uint32(len(frame)))
	binary.BigEndian.PutUint64(frame[12:20], 2)
	binary.BigEndian.PutUint32(frame[20:24], 1)
	binary.BigEndian.PutUint32(frame[24:28], uint32(len(payload)))
	copy(frame[28:], payload)
	putCRC(frame)
	return append(header, frame...)
}

func contractWALTail(id persist.DatabaseID) []byte {
	complete := contractWAL(id)
	second := make([]byte, 32+21)
	copy(second[:4], "CMIT")
	binary.BigEndian.PutUint16(second[4:6], 1)
	binary.BigEndian.PutUint32(second[8:12], uint32(len(second)))
	binary.BigEndian.PutUint64(second[12:20], 4)
	binary.BigEndian.PutUint32(second[20:24], 1)
	binary.BigEndian.PutUint32(second[24:28], 21)
	return append(complete, second[:17]...)
}

func putCRC(value []byte) {
	table := crc32.MakeTable(crc32.Castagnoli)
	binary.BigEndian.PutUint32(value[len(value)-4:], crc32.Checksum(value[:len(value)-4], table))
}

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
