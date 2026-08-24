package crash_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/faultfs"
	"gapdb/internal/persist"
	"gapdb/internal/protocol"
	"gapdb/internal/server"
)

func boundedOptions() gapdb.Options {
	options := gapdb.DefaultOptions()
	options.Limits.MaxKeyBytes = 16
	options.Limits.MaxValueBytes = 64
	options.Limits.MaxFrameBytes = 2048
	options.Limits.MaxBatchBytes = 1024
	options.Limits.MaxBatchOperations = 4
	options.Limits.MaxScanRecords = 4
	options.Limits.MaxScanBytes = 1024
	options.Limits.WatchBufferEvents = 2
	options.Limits.MaxWatchClients = 2
	options.Limits.MaxConcurrentClients = 8
	options.Limits.MaxHistoryEvents = 4
	options.Limits.MaxHistoryBytes = 2048
	return options
}

func TestPermissionsAcrossUmasksRemainOwnerOnly(t *testing.T) {
	for _, mask := range []int{0, 0o022, 0o077, 0o777} {
		t.Run(strings.TrimPrefix(strings.TrimPrefix(filepath.Base(time.Unix(int64(mask), 0).Format("150405")), ""), ""), func(t *testing.T) {
			parent := t.TempDir()
			previous := syscall.Umask(mask)
			defer syscall.Umask(previous)
			directory := filepath.Join(parent, "database")
			instance, err := server.Open(server.Config{Directory: directory, Options: boundedOptions(), ToolVersion: "permission-evidence"})
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close(context.Background())
			for _, name := range []string{"LOCK", persist.IdentityFilename, persist.ManifestFilename, "snapshot-00000000000000000000.gdb", "wal-00000000000000000001.gdb", "gapdb.sock"} {
				info, err := os.Stat(filepath.Join(directory, name))
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if info.Mode().Perm()&0o077 != 0 {
					t.Fatalf("umask %03o made %s non-owner-only: %04o", mask, name, info.Mode().Perm())
				}
			}
		})
	}
}

func TestEveryConfiguredBoundaryAtMaximumAndMaximumPlusOne(t *testing.T) {
	options := boundedOptions()
	directory := t.TempDir()
	instance, err := server.Open(server.Config{Directory: directory, Options: options, ToolVersion: "limit-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close(context.Background())
	client := dialWithLimits(t, instance.SocketPath(), options.Limits)
	defer client.Close()

	maxKey := strings.Repeat("k", options.Limits.MaxKeyBytes)
	if _, err := client.Put(t.Context(), maxKey, bytes.Repeat([]byte("v"), options.Limits.MaxValueBytes), nil, gapdb.AckDurable); err != nil {
		t.Fatalf("key/value maximum rejected: %v", err)
	}
	if _, err := client.Put(t.Context(), maxKey+"x", nil, nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeKeyTooLarge}) {
		t.Fatalf("key maximum+1=%v", err)
	}
	if _, err := client.Put(t.Context(), "value-plus", bytes.Repeat([]byte("v"), options.Limits.MaxValueBytes+1), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeValueTooLarge}) {
		t.Fatalf("value maximum+1=%v", err)
	}

	mutations := make([]gapdb.Mutation, options.Limits.MaxBatchOperations)
	for index := range mutations {
		mutations[index] = gapdb.NewPutMutation(string(rune('a'+index)), []byte{byte(index)}, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)
	}
	if _, err := client.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: mutations}); err != nil {
		t.Fatalf("batch operation maximum rejected: %v", err)
	}
	tooMany := append(append([]gapdb.Mutation(nil), mutations...), gapdb.NewPutMutation("z", nil, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil))
	if _, err := client.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory, Mutations: tooMany}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBatchTooLarge}) {
		t.Fatalf("batch operation maximum+1=%v", err)
	}
	testBatchByteBoundary(t)
	if _, err := client.ScanPrefix(t.Context(), "", options.Limits.MaxScanRecords, ""); err != nil {
		t.Fatalf("scan maximum rejected: %v", err)
	}
	if _, err := client.ScanPrefix(t.Context(), "", options.Limits.MaxScanRecords+1, ""); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("scan maximum+1=%v", err)
	}
	testScanByteBoundary(t)

	watchStatus, err := client.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	watches := make([]*gapdb.Watch, options.Limits.MaxWatchClients)
	for index := range watches {
		watches[index], err = client.Watch(t.Context(), "watch/", watchStatus.CurrentRevision)
		if err != nil {
			t.Fatalf("watch maximum %d: %v", index, err)
		}
		defer watches[index].Close()
	}
	if extra, err := client.Watch(t.Context(), "watch/", watchStatus.CurrentRevision); extra != nil || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeServerBusy}) {
		if extra != nil {
			extra.Close()
		}
		t.Fatalf("watch maximum+1=%v, %v", extra, err)
	}
	for _, watch := range watches {
		watch.Close()
	}
	testConcurrentClientBoundary(t)

	if err := protocol.WriteFrame(&bytes.Buffer{}, bytes.Repeat([]byte("x"), options.Limits.MaxFrameBytes), options.Limits.MaxFrameBytes); err != nil {
		t.Fatalf("frame maximum rejected: %v", err)
	}
	if err := protocol.WriteFrame(&bytes.Buffer{}, bytes.Repeat([]byte("x"), options.Limits.MaxFrameBytes+1), options.Limits.MaxFrameBytes); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeFrameTooLarge}) {
		t.Fatalf("frame maximum+1=%v", err)
	}

	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], uint32(options.Limits.MaxFrameBytes+1))
	if _, err := protocol.ReadFrame(bytes.NewReader(oversized[:]), options.Limits.MaxFrameBytes); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeFrameTooLarge}) {
		t.Fatalf("received frame maximum+1=%v", err)
	}
	testServerFrameBoundary(t)
	testHistoryBoundaries(t)
	testBoundedCompactionDiagnostics(t)
}

func openLimitServer(t *testing.T, options gapdb.Options) (*server.Server, *gapdb.Client) {
	t.Helper()
	instance, err := server.Open(server.Config{Directory: t.TempDir(), Options: options, ToolVersion: "limit-evidence", ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client := dialWithLimits(t, instance.SocketPath(), options.Limits)
	t.Cleanup(func() {
		_ = client.Close()
		_ = instance.Close(context.Background())
	})
	return instance, client
}

func testBatchByteBoundary(t *testing.T) {
	t.Helper()
	options := boundedOptions()
	exact := gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("b", bytes.Repeat([]byte("v"), options.Limits.MaxValueBytes), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
	}}
	exactArguments, err := json.Marshal(protocol.BatchArguments{Ack: exact.Ack, Mutations: exact.Mutations})
	if err != nil {
		t.Fatal(err)
	}
	options.Limits.MaxBatchBytes = len(exactArguments)
	_, client := openLimitServer(t, options)
	if _, err := client.AtomicBatch(t.Context(), exact); err != nil {
		t.Fatalf("batch byte maximum rejected: %v", err)
	}
	status, err := client.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	plusOne := gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("bb", bytes.Repeat([]byte("v"), options.Limits.MaxValueBytes), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
	}}
	plusArguments, err := json.Marshal(protocol.BatchArguments{Ack: plusOne.Ack, Mutations: plusOne.Mutations})
	if err != nil || len(plusArguments) != options.Limits.MaxBatchBytes+1 {
		t.Fatalf("batch wire calibration exact=%d plus=%d err=%v", options.Limits.MaxBatchBytes, len(plusArguments), err)
	}
	request, err := json.Marshal(struct {
		SchemaVersion uint64          `json:"schema_version"`
		RequestID     string          `json:"request_id"`
		Operation     string          `json:"operation"`
		Arguments     json.RawMessage `json:"arguments"`
	}{1, "batch-plus-one", "atomic_batch", plusArguments})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.DecodeRequest(request, options.Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBatchTooLarge}) {
		t.Fatalf("batch byte maximum+1=%v", err)
	}
	after, err := client.Status(t.Context())
	if err != nil || after.CurrentRevision != status.CurrentRevision {
		t.Fatalf("oversized batch mutated authority: before=%d after=%d err=%v", status.CurrentRevision, after.CurrentRevision, err)
	}
}

func testScanByteBoundary(t *testing.T) {
	t.Helper()
	options := boundedOptions()
	options.Limits.MaxScanBytes = 32 + options.Limits.MaxKeyBytes + 56 + 8
	_, client := openLimitServer(t, options)
	expires := time.Now().Add(time.Hour).UTC()
	exactKey := "scan/" + strings.Repeat("a", options.Limits.MaxKeyBytes-len("scan/"))
	tooLargeKey := "scan/" + strings.Repeat("b", options.Limits.MaxKeyBytes-len("scan/"))
	if _, err := client.Put(t.Context(), exactKey, bytes.Repeat([]byte("e"), 56), &expires, gapdb.AckDurable); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Put(t.Context(), tooLargeKey, bytes.Repeat([]byte("o"), 57), &expires, gapdb.AckDurable); err != nil {
		t.Fatal(err)
	}
	page, err := client.ScanPrefix(t.Context(), exactKey, 1, "")
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("scan byte maximum=%+v err=%v", page, err)
	}
	if _, err := client.ScanPrefix(t.Context(), tooLargeKey, 1, ""); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeFrameTooLarge}) {
		t.Fatalf("scan byte maximum+1=%v", err)
	}
}

func testConcurrentClientBoundary(t *testing.T) {
	t.Helper()
	options := boundedOptions()
	options.Limits.MaxConcurrentClients = 3
	options.Limits.MaxWatchClients = 3
	options.Limits.MaxHistoryEvents = 4
	instance, _ := openLimitServer(t, options)
	connections := make([]net.Conn, 0, options.Limits.MaxConcurrentClients)
	for index := 0; index < options.Limits.MaxConcurrentClients; index++ {
		conn, err := net.Dial("unix", instance.SocketPath())
		if err != nil {
			t.Fatal(err)
		}
		request, err := protocol.EncodeRequest(protocol.Request{SchemaVersion: protocol.SchemaVersion, RequestID: fmt.Sprintf("held-%d", index), Operation: protocol.OperationWatch, Arguments: protocol.WatchArguments{Prefix: "held/", AfterRevision: 0}}, options.Limits)
		if err != nil {
			t.Fatal(err)
		}
		if err := protocol.WriteFrame(conn, request, options.Limits.MaxFrameBytes); err != nil {
			t.Fatal(err)
		}
		payload, err := protocol.ReadFrame(conn, options.Limits.MaxFrameBytes)
		if err != nil {
			t.Fatal(err)
		}
		response, err := protocol.DecodeResponse(payload, options.Limits.MaxFrameBytes)
		if err != nil || response.Stream != protocol.StreamStarted {
			t.Fatalf("client maximum %d was not admitted: %+v err=%v", index, response, err)
		}
		connections = append(connections, conn)
	}
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	extra, err := net.Dial("unix", instance.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(time.Second))
	payload, err := protocol.ReadFrame(extra, options.Limits.MaxFrameBytes)
	if err != nil {
		t.Fatalf("client maximum+1 response: %v", err)
	}
	response, err := protocol.DecodeResponse(payload, options.Limits.MaxFrameBytes)
	if err != nil || response.Error == nil || response.Error.Code != gapdb.CodeServerBusy || response.Error.ActiveClients == nil || *response.Error.ActiveClients != options.Limits.MaxConcurrentClients {
		t.Fatalf("client maximum+1=%+v err=%v", response, err)
	}
}

func testServerFrameBoundary(t *testing.T) {
	t.Helper()
	options := boundedOptions()
	instance, _ := openLimitServer(t, options)
	conn, err := net.Dial("unix", instance.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(conn, bytes.Repeat([]byte(" "), options.Limits.MaxFrameBytes), options.Limits.MaxFrameBytes); err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.ReadFrame(conn, options.Limits.MaxFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeResponse(payload, options.Limits.MaxFrameBytes)
	if err != nil || response.Error == nil || response.Error.Code != gapdb.CodeInvalidRequest {
		t.Fatalf("server frame maximum=%+v err=%v", response, err)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(options.Limits.MaxFrameBytes+1))
	if _, err := conn.Write(prefix[:]); err != nil {
		t.Fatal(err)
	}
	payload, err = protocol.ReadFrame(conn, options.Limits.MaxFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	response, err = protocol.DecodeResponse(payload, options.Limits.MaxFrameBytes)
	if err != nil || response.Error == nil || response.Error.Code != gapdb.CodeFrameTooLarge {
		t.Fatalf("server frame maximum+1=%+v err=%v", response, err)
	}
}

func testHistoryBoundaries(t *testing.T) {
	t.Helper()
	for _, test := range []struct {
		name      string
		configure func(*gapdb.Options)
		keyPrefix string
		value     []byte
	}{
		{"events", func(options *gapdb.Options) {
			options.Limits.WatchBufferEvents = 1
			options.Limits.MaxHistoryEvents = 2
		}, "he/", []byte("v")},
		{"bytes", func(options *gapdb.Options) {
			options.Limits.WatchBufferEvents = 1
			options.Limits.MaxHistoryEvents = 10
			options.Limits.MaxHistoryBytes = 64
		}, "hb/", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := boundedOptions()
			test.configure(&options)
			_, client := openLimitServer(t, options)
			results := make([]gapdb.MutationResult, 3)
			for index := range results {
				key := fmt.Sprintf("%s%d", test.keyPrefix, index)
				value := test.value
				if test.name == "bytes" {
					// 24 + 2*len("hb/N") = 32 bytes per event: two
					// commits fit 64 only after using the exact configured bound.
					value = nil
				}
				var err error
				results[index], err = client.Put(t.Context(), key, value, nil, gapdb.AckMemory)
				if err != nil {
					t.Fatal(err)
				}
			}
			if watch, err := client.Watch(t.Context(), test.keyPrefix, results[0].Revision); err != nil {
				t.Fatalf("history %s maximum rejected: %v", test.name, err)
			} else {
				watch.Close()
			}
			watch, err := client.Watch(t.Context(), test.keyPrefix, 0)
			if watch != nil {
				watch.Close()
			}
			var structured *gapdb.Error
			if !errors.As(err, &structured) || structured.Code != gapdb.CodeRevisionCompacted || structured.EarliestRevision == nil {
				t.Fatalf("history maximum+1=%v", err)
			}
		})
	}
}

func testBoundedCompactionDiagnostics(t *testing.T) {
	t.Helper()
	options := boundedOptions()
	options.Limits.MaxHistoryEvents = 128
	options.Limits.MaxFrameBytes = 16 << 10
	instance, client := openLimitServer(t, options)
	for index := 0; index < 70; index++ {
		if _, err := client.Put(t.Context(), fmt.Sprintf("c/%02d", index), nil, nil, gapdb.AckDurable); err != nil {
			t.Fatal(err)
		}
	}
	status, err := client.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.CreateSnapshot(t.Context(), status.DatabaseID, status.CurrentRevision)
	if err != nil {
		t.Fatal(err)
	}
	for revision := 1; revision <= 70; revision++ {
		name := fmt.Sprintf("wal-%020d.gdb", revision)
		if name == fmt.Sprintf("wal-%020d.gdb", snapshot.NewWALStart) {
			continue
		}
		if err := os.WriteFile(filepath.Join(instanceDirectory(instance), name), []byte("obsolete"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	compaction, err := client.Compact(t.Context(), status.DatabaseID, snapshot.Revision)
	if err != nil || compaction.RemovedCount <= len(compaction.Removed) || len(compaction.Removed) != 64 || !compaction.PathsTruncated {
		t.Fatalf("bounded compaction diagnostics=%+v err=%v", compaction, err)
	}
}

func instanceDirectory(instance *server.Server) string {
	return filepath.Dir(instance.SocketPath())
}

func TestDegradedAdmissionKeepsBoundedReadAndInspectionEvidence(t *testing.T) {
	directory := t.TempDir()
	injector := faultfs.NewInjector(83, faultfs.Rule{Point: faultfs.PointWALFrameWrite, Phase: faultfs.Before, Occurrence: 1, Seed: 83, Err: errors.New("disk offline")})
	instance, err := server.Open(server.Config{Directory: directory, Options: boundedOptions(), ToolVersion: "degraded-evidence", FS: faultfs.NewOS(injector)})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close(context.Background())
	client := dialWithLimits(t, instance.SocketPath(), boundedOptions().Limits)
	defer client.Close()
	if _, err := client.Put(t.Context(), "first", []byte("value"), nil, gapdb.AckDurable); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeStorageDegraded}) {
		t.Fatalf("append fault=%v", err)
	}
	if _, err := client.Put(t.Context(), "second", []byte("value"), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeStorageDegraded}) {
		t.Fatalf("degraded admission=%v", err)
	}
	if _, err := client.Get(t.Context(), "first"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("degraded Get evidence=%v", err)
	}
	status, err := client.Status(t.Context())
	if err != nil || status.Lifecycle != gapdb.LifecycleDegradedReadOnly || status.CurrentRevision != 0 {
		t.Fatalf("degraded status=%+v %v", status, err)
	}
	health, err := client.Health(t.Context())
	if err != nil || health.Healthy || len(health.FailingSubsystems) == 0 || len(health.FailingSubsystems) > 8 || len(health.SafeActions) > 8 {
		t.Fatalf("bounded health=%+v %v", health, err)
	}
}

func TestUnsupportedFormatAndOwnershipConflictDoNotMutateAuthority(t *testing.T) {
	directory := t.TempDir()
	instance, err := server.Open(server.Config{Directory: directory, Options: boundedOptions(), ToolVersion: "conflict-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	beforeOwner, err := treeDigest(directory)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := server.Open(server.Config{Directory: directory, Options: boundedOptions(), ToolVersion: "conflict-evidence"}); second != nil || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeOwnerExists}) {
		if second != nil {
			_ = second.Close(context.Background())
		}
		t.Fatalf("second owner=%v, %v", second, err)
	}
	if afterOwner, err := treeDigest(directory); err != nil || afterOwner != beforeOwner {
		t.Fatalf("ownership conflict mutated tree: %s -> %s (%v)", beforeOwner, afterOwner, err)
	}
	if err := instance.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	identityPath := filepath.Join(directory, persist.IdentityFilename)
	identity, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(identity[8:10], 999)
	if err := os.WriteFile(identityPath, identity, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeFormat, err := treeDigest(directory)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := server.Open(server.Config{Directory: directory, Options: boundedOptions(), ToolVersion: "conflict-evidence"}); opened != nil || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeUnknownFormat}) {
		if opened != nil {
			_ = opened.Close(context.Background())
		}
		t.Fatalf("unknown identity format=%v, %v", opened, err)
	}
	if afterFormat, err := treeDigest(directory); err != nil || afterFormat != beforeFormat {
		t.Fatalf("unsupported format mutated tree: %s -> %s (%v)", beforeFormat, afterFormat, err)
	}
}
