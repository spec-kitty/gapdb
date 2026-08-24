package fuzz_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/admin"
	"github.com/spec-kitty/gapdb/internal/engine"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/persist"
	"github.com/spec-kitty/gapdb/internal/protocol"
)

func fuzzLimits() gapdb.Limits {
	limits := gapdb.DefaultOptions().Limits
	limits.MaxKeyBytes = 32
	limits.MaxValueBytes = 64
	limits.MaxFrameBytes = 1024
	limits.MaxBatchBytes = 1024
	limits.MaxBatchOperations = 8
	limits.MaxScanRecords = 8
	limits.MaxScanBytes = 1024
	limits.WatchBufferEvents = 4
	limits.MaxHistoryEvents = 8
	limits.MaxHistoryBytes = 1024
	return limits
}

func decoderSeeds(t testing.TB) [][]byte {
	t.Helper()
	limits := fuzzLimits()
	id := persist.DatabaseID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	identity, err := persist.EncodeIdentity(persist.Identity{DatabaseID: id, ReservedRevisionEnd: 32, Generation: 2})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, digest, err := persist.EncodeSnapshot(id, persist.SnapshotState{Revision: 1, Records: []gapdb.Record{gapdb.NewRecord("a", []byte{0, 1}, 1, nil)}}, time.Unix(123, 0).UTC(), limits)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := persist.EncodeManifest(persist.Manifest{DatabaseID: id, Generation: 2, SnapshotFile: "snapshot-00000000000000000001.gdb", SnapshotRevision: 1, SnapshotSHA256: hex.EncodeToString(digest[:]), WALFile: "wal-00000000000000000002.gdb", WALStartRevision: 2})
	if err != nil {
		t.Fatal(err)
	}
	header, err := persist.EncodeWALHeader(persist.WALHeader{DatabaseID: id, FirstRevision: 2, Generation: 2})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := persist.EncodeCommitFrame(persist.CommitFrame{Revision: 2, Effects: []persist.Effect{{Kind: gapdb.ChangePut, Key: "b", Value: []byte{2, 3}}}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.EncodeRequest(protocol.Request{SchemaVersion: 1, RequestID: "fuzz", Operation: protocol.OperationGet, Arguments: protocol.GetArguments{Key: "a"}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.EncodeResponse(protocol.Response{SchemaVersion: 1, OK: true, RequestID: "fuzz", DatabaseID: id.String(), Operation: protocol.OperationGet, Result: protocol.GetResult{Record: gapdb.NewRecord("a", []byte{0, 1}, 1, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	audit := auditSeed(id)
	return [][]byte{identity, manifest, header, commit, append(append([]byte{}, header...), commit...), snapshot, request, response, audit, []byte(`{"schema_version":1,"source_database_id":"` + id.String() + `","backup_id":"seed","durable_revision":1,"created_at":"1970-01-01T00:00:01Z","files":[],"tool_version":"fuzz"}\n`)}
}

func auditSeed(id persist.DatabaseID) []byte {
	base := fmt.Sprintf(`{"schema_version":1,"event_id":"seed","timestamp":"1970-01-01T00:00:01Z","database_id":"%s","operation":"verify","request_id":"fuzz","before_revision":1,"after_revision":1,"outcome":"applied","paths":[],"safe_actions":["verify"]}`, id.String())
	checksum := crc32.Checksum([]byte(base), crc32.MakeTable(crc32.Castagnoli))
	return []byte(strings.TrimSuffix(base, "}") + fmt.Sprintf(`,"crc32c":"%08x"}\n`, checksum))
}

func exerciseDecoders(data []byte) {
	limits := fuzzLimits()
	id := persist.DatabaseID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	var digest [32]byte
	copy(digest[:], data)
	_, _ = protocol.DecodeRequest(data, limits)
	_, _ = protocol.DecodeResponse(data, limits.MaxFrameBytes)
	_, _ = protocol.ReadFrame(bytes.NewReader(data), limits.MaxFrameBytes)
	_, _ = persist.DecodeIdentity(data)
	_, _ = persist.DecodeManifest(data, id, 1)
	_, _ = persist.DecodeWALHeader(data, id)
	_, _ = persist.DecodeCommitFrame(data, limits)
	_, _ = persist.ScanWAL(bytes.NewReader(data), int64(len(data)), id, 0, 64, limits)
	_, _ = persist.DecodeSnapshot(data, id, digest, limits)
	_ = admin.VerifyAudit(bytes.NewReader(data), 2048)
}

func TestSeededDecoderCorpus(t *testing.T) {
	for seedIndex, seed := range decoderSeeds(t) {
		for mutation := 0; mutation < 32; mutation++ {
			candidate := append([]byte(nil), seed...)
			if len(candidate) != 0 {
				position := (seedIndex*17 + mutation*31) % len(candidate)
				candidate[position] ^= byte(mutation + 1)
			}
			exerciseDecoders(candidate)
		}
	}

	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, persist.BackupMetadataFilename), []byte("{\"schema_version\":999}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := directoryDigest(t, directory)
	if _, err := persist.VerifyBackup(faultfs.NewOS(nil), directory, fuzzLimits()); err == nil {
		t.Fatal("hostile backup metadata accepted")
	}
	if after := directoryDigest(t, directory); after != before {
		t.Fatalf("rejected backup verification mutated input: %s -> %s", before, after)
	}
}

func FuzzWireAndStorageDecoders(f *testing.F) {
	for _, seed := range decoderSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		exerciseDecoders(data)
	})
}

func FuzzScanCursorDecoder(f *testing.F) {
	f.Add("")
	f.Add("R0FQQ1VSMDE")
	f.Add(strings.Repeat("A", 512))
	f.Fuzz(func(t *testing.T, cursor string) {
		if len(cursor) > 1024 {
			t.Skip()
		}
		id := persist.DatabaseID{1}
		state, err := engine.New(engine.Config{Limits: fuzzLimits(), Log: &fuzzLog{}, Allocator: &fuzzAllocator{}, DatabaseID: id, ReservedRevisionEnd: 32, ActiveWALStart: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = state.ScanPrefix("", 1, cursor)
		if err := state.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func FuzzBackupVerifierDoesNotMutate(f *testing.F) {
	f.Add([]byte("{}\n"))
	f.Add([]byte("not-json"))
	f.Fuzz(func(t *testing.T, metadata []byte) {
		if len(metadata) > 4096 {
			t.Skip()
		}
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, persist.BackupMetadataFilename), metadata, 0o600); err != nil {
			t.Fatal(err)
		}
		before := directoryDigest(t, directory)
		_, _ = persist.VerifyBackup(faultfs.NewOS(nil), directory, fuzzLimits())
		if after := directoryDigest(t, directory); after != before {
			t.Fatalf("backup verifier mutated rejected corpus: %s -> %s", before, after)
		}
	})
}

type fuzzLog struct{}

func (*fuzzLog) Append(persist.CommitFrame) error { return nil }
func (*fuzzLog) Barrier() error                   { return nil }
func (*fuzzLog) DurableThrough() gapdb.Revision   { return 0 }

type fuzzAllocator struct{ revision gapdb.Revision }

func (allocator *fuzzAllocator) Next() (gapdb.Revision, error) {
	allocator.revision++
	return allocator.revision, nil
}

func directoryDigest(t testing.TB, root string) string {
	t.Helper()
	var names []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			name, relativeErr := filepath.Rel(root, path)
			if relativeErr != nil {
				return relativeErr
			}
			names = append(names, name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		_, _ = io.WriteString(hash, name)
		value, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
