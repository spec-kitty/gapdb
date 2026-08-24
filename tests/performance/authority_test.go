package performance_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

func TestMixedWindowProvesAllParticipantsAndFixedBudgets(t *testing.T) {
	var reads, writes atomic.Int64
	evidence, err := runMixedWindow(t.Context(), mixedWindowConfig{
		ReaderOperations: 3,
		WriterOperations: 5,
		Read: func(context.Context, int, int) error {
			reads.Add(1)
			return nil
		},
		Write: func(context.Context, int) error {
			writes.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ReadersReady != referenceReaderCount || evidence.ReadersActive != referenceReaderCount || !evidence.WriterReady || !evidence.WriterActive {
		t.Fatalf("participant barriers not proved: %+v", evidence)
	}
	if evidence.ReadOperations != referenceReaderCount*3 || evidence.WriteOperations != 5 || int(reads.Load()) != evidence.ReadOperations || int(writes.Load()) != evidence.WriteOperations {
		t.Fatalf("operation budgets drifted: evidence=%+v reads=%d writes=%d", evidence, reads.Load(), writes.Load())
	}
}

func TestReferenceProvenanceRejectsNoSyncAndDirectSubstitutions(t *testing.T) {
	recorder := &faultfs.Recorder{}
	osfs := faultfs.NewOS(recorder)
	if err := validateFilesystemProvenance(osfs, osfs, recorder); err != nil {
		t.Fatalf("real observing OS filesystem rejected: %v", err)
	}
	if err := validateFilesystemProvenance(noSyncFS{FS: osfs}, osfs, recorder); err == nil {
		t.Fatal("no-sync filesystem substitution satisfied provenance")
	}
	if err := validateTransportProvenance("", nil, referenceReaderCount+referenceWriterCount); err == nil {
		t.Fatal("direct/in-process transport substitution satisfied provenance")
	}
}

func TestObservedSyncEvidenceIsFaultSensitive(t *testing.T) {
	injector := faultfs.NewInjector(7, faultfs.Rule{Point: faultfs.PointWALFileSync, Phase: faultfs.Before, Occurrence: 2, Seed: 7, Err: errors.New("sync boundary unavailable")})
	recorder := &faultfs.Recorder{}
	fsys := faultfs.NewOS(recordingFaultHook{recorder: recorder, injector: injector})
	fixture := newSocketFixtureWithFS(t, 1, fsys, nil)
	defer fixture.clients[0].Close()
	before := countSuccessfulWALSyncs(recorder.Events())
	if _, err := fixture.clients[0].Put(context.Background(), "fault-sensitive", []byte("value"), nil, gapdb.AckDurable); err == nil {
		t.Fatal("durable success survived an injected real WAL sync failure")
	}
	if got := countSuccessfulWALSyncs(recorder.Events()); got != before {
		t.Fatalf("failed sync counted as successful: before=%d after=%d", before, got)
	}
	_ = fixture.server.Close(context.Background())
	fixture.server = nil
}

type noSyncFS struct{ faultfs.FS }

func (noSyncFS) Sync(faultfs.Point, interface{ Sync() error }) error { return nil }
func (noSyncFS) SyncDir(faultfs.Point, string) error                 { return nil }

type recordingFaultHook struct {
	recorder *faultfs.Recorder
	injector *faultfs.Injector
}

func (hook recordingFaultHook) Visit(event faultfs.Event) error {
	_ = hook.recorder.Visit(event)
	return hook.injector.Visit(event)
}
