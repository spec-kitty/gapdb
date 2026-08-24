package performance_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/server"
)

const (
	referenceSeed             = uint64(0x4741504442504552)
	referenceRecordCount      = 100_000
	referenceValueBytes       = 1 << 10
	referenceReaderCount      = 8
	referenceWriterCount      = 1
	referenceLaterWALCommits  = 10_000
	referenceGetWriterOps     = 256
	referenceAllocationRuns   = 20
	referenceSnapshotRevision = gapdb.Revision((referenceRecordCount+255)/256 + referenceGetWriterOps + 512 + (referenceAllocationRuns + 1) + 128 + (referenceAllocationRuns + 1))
)

type latencySummary struct {
	Samples      int     `json:"samples"`
	P50Millis    float64 `json:"p50_ms"`
	P95Millis    float64 `json:"p95_ms"`
	P99Millis    float64 `json:"p99_ms"`
	AllocsPerOp  float64 `json:"allocs_per_op"`
	TargetP95MS  float64 `json:"target_p95_ms"`
	TargetPassed bool    `json:"target_passed"`
}

type recoverySummary struct {
	SnapshotRevision gapdb.Revision `json:"snapshot_revision"`
	LaterWALCommits  int            `json:"later_wal_commits"`
	ReadyMillis      float64        `json:"ready_ms"`
	TargetMillis     float64        `json:"target_ms"`
	TargetPassed     bool           `json:"target_passed"`
}

type referenceSummary struct {
	SchemaVersion             uint16                    `json:"schema_version"`
	Seed                      uint64                    `json:"seed"`
	LiveRecords               int                       `json:"live_records"`
	ValueBytes                int                       `json:"value_bytes"`
	Readers                   int                       `json:"readers"`
	Writers                   int                       `json:"writers"`
	Transport                 string                    `json:"transport"`
	Filesystem                string                    `json:"filesystem"`
	SocketMode                string                    `json:"socket_mode"`
	SuccessfulDurableBarriers int                       `json:"successful_durable_barriers"`
	ObservedWALSyncs          int                       `json:"observed_wal_syncs"`
	Metrics                   map[string]latencySummary `json:"metrics"`
	Windows                   map[string]windowEvidence `json:"windows"`
	Recovery                  recoverySummary           `json:"recovery"`
	ReferenceAccepted         bool                      `json:"reference_accepted"`
}

func TestReferencePerformanceProfile(t *testing.T) {
	if os.Getenv("GAPDB_REFERENCE_ACCEPTANCE") != "1" {
		t.Skip("set GAPDB_REFERENCE_ACCEPTANCE=1 to run the documented reference workload")
	}
	summary := runReferenceProfile(t)
	validateReferenceSummary(t, summary)
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("REFERENCE_RESULT=%s", encoded)
}

type socketFixture struct {
	directory        string
	server           *server.Server
	clients          []*gapdb.Client
	fs               faultfs.FS
	observedOS       *faultfs.OS
	recorder         *faultfs.Recorder
	durableSuccesses atomic.Int64
}

func newSocketFixture(t testing.TB, clients int) *socketFixture {
	t.Helper()
	recorder := &faultfs.Recorder{}
	osfs := faultfs.NewOS(recorder)
	return newSocketFixtureWithFS(t, clients, osfs, recorder)
}

func newSocketFixtureWithFS(t testing.TB, clients int, fsys faultfs.FS, recorder *faultfs.Recorder) *socketFixture {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "database")
	instance, err := server.Open(server.Config{
		Directory:    directory,
		Options:      gapdb.DefaultOptions(),
		ToolVersion:  "performance-evidence",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
		FS:           fsys,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &socketFixture{directory: directory, server: instance, fs: fsys, recorder: recorder}
	if observed, ok := fsys.(*faultfs.OS); ok && recorder != nil {
		fixture.observedOS = observed
	}
	for range clients {
		client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 30 * time.Second})
		if err != nil {
			fixture.close(t)
			t.Fatal(err)
		}
		fixture.clients = append(fixture.clients, client)
	}
	if err := validateTransportProvenance(instance.SocketPath(), fixture.clients, clients); err != nil {
		fixture.close(t)
		t.Fatalf("performance transport provenance: %v", err)
	}
	if recorder != nil {
		if err := validateFilesystemProvenance(fsys, fixture.observedOS, recorder); err != nil {
			fixture.close(t)
			t.Fatalf("performance filesystem provenance: %v", err)
		}
	}
	return fixture
}

func validateFilesystemProvenance(configured faultfs.FS, observed *faultfs.OS, recorder *faultfs.Recorder) error {
	if configured == nil || observed == nil || recorder == nil || configured != observed {
		return errors.New("filesystem is not the exact pass-through observing OS implementation")
	}
	return nil
}

func validateTransportProvenance(socketPath string, clients []*gapdb.Client, expectedClients int) error {
	if socketPath == "" || len(clients) != expectedClients || expectedClients <= 0 {
		return errors.New("public Unix client set is incomplete")
	}
	info, err := os.Lstat(socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("path is not a 0600 Unix socket: mode=%v err=%w", info, err)
	}
	for index, client := range clients {
		if client == nil || client.SocketPath() != socketPath {
			return fmt.Errorf("client %d is not bound to the public socket", index)
		}
	}
	return nil
}

func countSuccessfulWALSyncs(events []faultfs.Event) int {
	count := 0
	for _, event := range events {
		if event.Point == faultfs.PointWALFileSync && event.Phase == faultfs.After {
			count++
		}
	}
	return count
}

func (fixture *socketFixture) close(t testing.TB) {
	t.Helper()
	for _, client := range fixture.clients {
		_ = client.Close()
	}
	if fixture.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := fixture.server.Close(ctx); err != nil {
			t.Errorf("close performance server: %v", err)
		}
		fixture.server = nil
	}
}

func deterministicValue(index int) []byte {
	var seed [16]byte
	binary.BigEndian.PutUint64(seed[:8], referenceSeed)
	binary.BigEndian.PutUint64(seed[8:], uint64(index))
	digest := sha256.Sum256(seed[:])
	value := make([]byte, referenceValueBytes)
	for offset := 0; offset < len(value); offset += len(digest) {
		copy(value[offset:], digest[:])
	}
	return value
}

func recordKey(index int) string { return fmt.Sprintf("record/%06d", index) }

func populateFixture(t testing.TB, fixture *socketFixture, count int) gapdb.Revision {
	t.Helper()
	client := fixture.clients[0]
	const batchSize = 256
	var last gapdb.MutationResult
	for offset := 0; offset < count; offset += batchSize {
		end := min(offset+batchSize, count)
		mutations := make([]gapdb.Mutation, 0, end-offset)
		for index := offset; index < end; index++ {
			mutations = append(mutations, gapdb.NewPutMutation(recordKey(index), deterministicValue(index), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil))
		}
		ack := gapdb.AckMemory
		if end == count {
			ack = gapdb.AckDurable
		}
		var err error
		last, err = client.AtomicBatch(context.Background(), gapdb.Batch{Ack: ack, Mutations: mutations})
		if err != nil {
			t.Fatalf("populate records %d..%d: %v", offset, end, err)
		}
		fixture.recordDurable(last, err)
	}
	status, err := client.Status(context.Background())
	if err != nil || status.RecordCount != count || status.DurableThroughRevision < last.Revision {
		t.Fatalf("fixture status=%+v last=%+v err=%v", status, last, err)
	}
	return status.CurrentRevision
}

func (fixture *socketFixture) recordDurable(result gapdb.MutationResult, err error) {
	if err == nil && result.Ack == gapdb.AckDurable && result.Revision != 0 && result.DurableThroughRevision >= result.Revision {
		fixture.durableSuccesses.Add(1)
	}
}

func (fixture *socketFixture) put(ctx context.Context, client *gapdb.Client, key string, value []byte, ack gapdb.AckMode) (gapdb.MutationResult, error) {
	result, err := client.Put(ctx, key, value, nil, ack)
	fixture.recordDurable(result, err)
	return result, err
}

func runReferenceProfile(t *testing.T) referenceSummary {
	fixture := newSocketFixture(t, referenceReaderCount+referenceWriterCount)
	populateFixture(t, fixture, referenceRecordCount)
	getMetric, getWindow := measureGets(t, fixture, 2_048, 2)
	memoryMetric, memoryWindow := measureWrites(t, fixture, gapdb.AckMemory, 512, 5)
	durableMetric, durableWindow := measureWrites(t, fixture, gapdb.AckDurable, 128, 50)
	metrics := map[string]latencySummary{"get": getMetric, "memory_put": memoryMetric, "durable_put": durableMetric}
	windows := map[string]windowEvidence{"get": getWindow, "memory_put": memoryWindow, "durable_put": durableWindow}
	statusAfter, err := fixture.clients[0].Status(t.Context())
	if err != nil || statusAfter.DurableThroughRevision != statusAfter.CurrentRevision || statusAfter.CurrentRevision != referenceSnapshotRevision {
		fixture.close(t)
		t.Fatalf("durable benchmark revision/durability drift: status=%+v expected_snapshot=%d err=%v", statusAfter, referenceSnapshotRevision, err)
	}
	verified, err := fixture.clients[0].Verify(t.Context(), "full")
	if err != nil || !verified.Verified || verified.CurrentRevision != statusAfter.CurrentRevision {
		fixture.close(t)
		t.Fatalf("durable benchmark verification=%+v status=%+v err=%v", verified, statusAfter, err)
	}
	recovery := measureReferenceRecovery(t, fixture)
	observedSyncs := countSuccessfulWALSyncs(fixture.recorder.Events())
	durableSuccesses := int(fixture.durableSuccesses.Load())
	if durableSuccesses == 0 || observedSyncs < durableSuccesses {
		t.Fatalf("durable operation/sync provenance mismatch: operations=%d observed_wal_syncs=%d", durableSuccesses, observedSyncs)
	}
	summary := referenceSummary{
		SchemaVersion:             1,
		Seed:                      referenceSeed,
		LiveRecords:               referenceRecordCount,
		ValueBytes:                referenceValueBytes,
		Readers:                   referenceReaderCount,
		Writers:                   referenceWriterCount,
		Transport:                 "unix_socket",
		Filesystem:                "os_fsync_enabled",
		SocketMode:                "0600",
		SuccessfulDurableBarriers: durableSuccesses,
		ObservedWALSyncs:          observedSyncs,
		Metrics:                   metrics,
		Windows:                   windows,
		Recovery:                  recovery,
	}
	summary.ReferenceAccepted = allTargetsPass(summary)
	return summary
}

type windowEvidence struct {
	ReadersReady    int  `json:"readers_ready"`
	ReadersActive   int  `json:"readers_active"`
	WriterReady     bool `json:"writer_ready"`
	WriterActive    bool `json:"writer_active"`
	ReadOperations  int  `json:"read_operations"`
	WriteOperations int  `json:"write_operations"`
}

type mixedWindowConfig struct {
	ReaderOperations int
	WriterOperations int
	Read             func(context.Context, int, int) error
	Write            func(context.Context, int) error
}

func runMixedWindow(ctx context.Context, config mixedWindowConfig) (windowEvidence, error) {
	if config.ReaderOperations <= 0 || config.WriterOperations <= 0 || config.Read == nil || config.Write == nil {
		return windowEvidence{}, errors.New("mixed window requires positive fixed operation budgets")
	}
	ready := sync.WaitGroup{}
	active := sync.WaitGroup{}
	workers := sync.WaitGroup{}
	ready.Add(referenceReaderCount + referenceWriterCount)
	active.Add(referenceReaderCount + referenceWriterCount)
	startActive := make(chan struct{})
	startWork := make(chan struct{})
	errorsFound := make(chan error, referenceReaderCount+referenceWriterCount)
	var readersReady, readersActive, readOperations, writeOperations atomic.Int64
	var writerReady, writerActive atomic.Bool
	for reader := range referenceReaderCount {
		workers.Add(1)
		go func(reader int) {
			defer workers.Done()
			readersReady.Add(1)
			ready.Done()
			select {
			case <-startActive:
			case <-ctx.Done():
				errorsFound <- ctx.Err()
				return
			}
			readersActive.Add(1)
			active.Done()
			select {
			case <-startWork:
			case <-ctx.Done():
				errorsFound <- ctx.Err()
				return
			}
			for operation := range config.ReaderOperations {
				if err := config.Read(ctx, reader, operation); err != nil {
					errorsFound <- err
					return
				}
				readOperations.Add(1)
			}
		}(reader)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		writerReady.Store(true)
		ready.Done()
		select {
		case <-startActive:
		case <-ctx.Done():
			errorsFound <- ctx.Err()
			return
		}
		writerActive.Store(true)
		active.Done()
		select {
		case <-startWork:
		case <-ctx.Done():
			errorsFound <- ctx.Err()
			return
		}
		for operation := range config.WriterOperations {
			if err := config.Write(ctx, operation); err != nil {
				errorsFound <- err
				return
			}
			writeOperations.Add(1)
		}
	}()
	ready.Wait()
	close(startActive)
	active.Wait()
	close(startWork)
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			return windowEvidence{}, err
		}
	}
	evidence := windowEvidence{
		ReadersReady: int(readersReady.Load()), ReadersActive: int(readersActive.Load()),
		WriterReady: writerReady.Load(), WriterActive: writerActive.Load(),
		ReadOperations: int(readOperations.Load()), WriteOperations: int(writeOperations.Load()),
	}
	if evidence.ReadersReady != referenceReaderCount || evidence.ReadersActive != referenceReaderCount || !evidence.WriterReady || !evidence.WriterActive || evidence.ReadOperations != referenceReaderCount*config.ReaderOperations || evidence.WriteOperations != config.WriterOperations {
		return evidence, fmt.Errorf("mixed window barrier or budget mismatch: %+v", evidence)
	}
	return evidence, nil
}

func measureGets(t testing.TB, fixture *socketFixture, samples int, targetMS float64) (latencySummary, windowEvidence) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	durations := make(chan time.Duration, samples)
	perReader := samples / referenceReaderCount
	evidence, err := runMixedWindow(ctx, mixedWindowConfig{
		ReaderOperations: perReader,
		WriterOperations: referenceGetWriterOps,
		Read: func(ctx context.Context, reader, operation int) error {
			client := fixture.clients[reader]
			key := recordKey((reader*perReader + operation) * 7919 % referenceRecordCount)
			began := time.Now()
			record, err := client.Get(ctx, key)
			durations <- time.Since(began)
			if err != nil || len(record.Value) != referenceValueBytes {
				return fmt.Errorf("reader %d get %q: bytes=%d err=%w", reader, key, len(record.Value), err)
			}
			return nil
		},
		Write: func(ctx context.Context, operation int) error {
			keyIndex := operation % 64
			if _, err := fixture.put(ctx, fixture.clients[referenceReaderCount], recordKey(keyIndex), deterministicValue(keyIndex+operation+1), gapdb.AckMemory); err != nil {
				return fmt.Errorf("concurrent writer: %w", err)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	close(durations)
	values := make([]time.Duration, 0, samples)
	for duration := range durations {
		values = append(values, duration)
	}
	var allocationErr error
	allocations := testing.AllocsPerRun(20, func() {
		_, allocationErr = fixture.clients[0].Get(context.Background(), recordKey(100))
	})
	if allocationErr != nil {
		t.Fatal(allocationErr)
	}
	return summarizeLatency(t, values, allocations, targetMS), evidence
}

func measureWrites(t testing.TB, fixture *socketFixture, ack gapdb.AckMode, samples int, targetMS float64) (latencySummary, windowEvidence) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	writer := fixture.clients[referenceReaderCount]
	durations := make([]time.Duration, 0, samples)
	var durationMu sync.Mutex
	evidence, err := runMixedWindow(ctx, mixedWindowConfig{
		ReaderOperations: samples,
		WriterOperations: samples,
		Read: func(ctx context.Context, reader, operation int) error {
			client := fixture.clients[reader]
			if _, err := client.Get(ctx, recordKey((reader*997+operation)%referenceRecordCount)); err != nil {
				return fmt.Errorf("background reader %d: %w", reader, err)
			}
			return nil
		},
		Write: func(ctx context.Context, operation int) error {
			index := 128 + operation%128
			began := time.Now()
			result, err := fixture.put(ctx, writer, recordKey(index), deterministicValue(index+operation+17), ack)
			duration := time.Since(began)
			durationMu.Lock()
			durations = append(durations, duration)
			durationMu.Unlock()
			if err != nil || result.Ack != ack || (ack == gapdb.AckDurable && result.DurableThroughRevision < result.Revision) {
				return fmt.Errorf("%s writer result=%+v err=%w", ack, result, err)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var allocationErr error
	allocationIndex := 512
	allocations := testing.AllocsPerRun(referenceAllocationRuns, func() {
		_, allocationErr = fixture.put(context.Background(), writer, recordKey(allocationIndex), deterministicValue(allocationIndex), ack)
	})
	if allocationErr != nil {
		t.Fatal(allocationErr)
	}
	return summarizeLatency(t, durations, allocations, targetMS), evidence
}

func summarizeLatency(t testing.TB, durations []time.Duration, allocations, targetMS float64) latencySummary {
	t.Helper()
	if len(durations) == 0 {
		t.Fatal("latency sample is empty")
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	percentile := func(percent int) float64 {
		index := int(math.Ceil(float64(percent)*float64(len(durations))/100)) - 1
		return float64(durations[max(index, 0)]) / float64(time.Millisecond)
	}
	result := latencySummary{
		Samples:     len(durations),
		P50Millis:   percentile(50),
		P95Millis:   percentile(95),
		P99Millis:   percentile(99),
		AllocsPerOp: allocations,
		TargetP95MS: targetMS,
	}
	result.TargetPassed = result.P95Millis <= targetMS
	return result
}

func allTargetsPass(summary referenceSummary) bool {
	if !summary.Recovery.TargetPassed {
		return false
	}
	for _, metric := range summary.Metrics {
		if !metric.TargetPassed {
			return false
		}
	}
	return true
}

func validateReferenceSummary(t testing.TB, summary referenceSummary) {
	t.Helper()
	if summary.SchemaVersion != 1 || summary.Seed != referenceSeed || summary.LiveRecords != referenceRecordCount || summary.ValueBytes != referenceValueBytes || summary.Readers != 8 || summary.Writers != 1 {
		t.Fatalf("reference workload drifted: %+v", summary)
	}
	if summary.Transport != "unix_socket" || summary.Filesystem != "os_fsync_enabled" || summary.SocketMode != "0600" || summary.SuccessfulDurableBarriers == 0 || summary.ObservedWALSyncs < summary.SuccessfulDurableBarriers {
		t.Fatalf("reference durability/transport evidence is incomplete: %+v", summary)
	}
	if summary.Recovery.SnapshotRevision != referenceSnapshotRevision || summary.Recovery.LaterWALCommits != referenceLaterWALCommits {
		t.Fatalf("reference recovery revision/count drifted: %+v", summary.Recovery)
	}
	for name, window := range summary.Windows {
		if window.ReadersReady != referenceReaderCount || window.ReadersActive != referenceReaderCount || !window.WriterReady || !window.WriterActive || window.ReadOperations <= 0 || window.WriteOperations <= 0 {
			t.Fatalf("%s did not prove the 8-reader/1-writer mixed window: %+v", name, window)
		}
	}
	if len(summary.Windows) != 3 {
		t.Fatalf("mixed window evidence is incomplete: %+v", summary.Windows)
	}
	if !summary.ReferenceAccepted || !allTargetsPass(summary) {
		t.Fatalf("reference performance thresholds failed: %+v", summary)
	}
}

func BenchmarkReferenceUnixSocket(b *testing.B) {
	count := 1_000
	if os.Getenv("GAPDB_REFERENCE_ACCEPTANCE") == "1" {
		count = referenceRecordCount
	}
	fixture := newSocketFixture(b, referenceReaderCount+referenceWriterCount)
	defer fixture.close(b)
	populateFixture(b, fixture, count)
	b.Run("Get", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		var next atomic.Uint64
		_, err := runMixedWindow(context.Background(), mixedWindowConfig{
			ReaderOperations: max(1, (b.N+referenceReaderCount-1)/referenceReaderCount),
			WriterOperations: max(1, b.N/referenceReaderCount),
			Read: func(ctx context.Context, reader, _ int) error {
				index := int(next.Add(1) % uint64(count))
				_, err := fixture.clients[reader].Get(ctx, recordKey(index))
				return err
			},
			Write: func(ctx context.Context, operation int) error {
				index := operation % min(count, 128)
				_, err := fixture.put(ctx, fixture.clients[referenceReaderCount], recordKey(index), deterministicValue(index+operation), gapdb.AckMemory)
				return err
			},
		})
		if err != nil {
			b.Fatal(err)
		}
	})
	for _, ack := range []gapdb.AckMode{gapdb.AckMemory, gapdb.AckDurable} {
		b.Run(string(ack)+"_put", func(b *testing.B) {
			writer := fixture.clients[referenceReaderCount]
			b.ReportAllocs()
			b.ResetTimer()
			_, err := runMixedWindow(context.Background(), mixedWindowConfig{
				ReaderOperations: max(1, b.N/referenceReaderCount),
				WriterOperations: max(1, b.N),
				Read: func(ctx context.Context, reader, operation int) error {
					_, err := fixture.clients[reader].Get(ctx, recordKey((reader+operation)%count))
					return err
				},
				Write: func(ctx context.Context, operation int) error {
					index := operation % min(count, 128)
					_, err := fixture.put(ctx, writer, recordKey(index), deterministicValue(index+operation), ack)
					return err
				},
			})
			if err != nil {
				b.Fatal(err)
			}
		})
	}
}
