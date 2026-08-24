package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/clock"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/persist"
)

type fakeLog struct {
	mu          sync.Mutex
	frames      []persist.CommitFrame
	durable     gapdb.Revision
	appendErr   error
	barrierErr  error
	appendBlock <-chan struct{}
	appended    chan struct{}
	barriers    int
}

func (log *fakeLog) Append(frame persist.CommitFrame) error {
	if log.appended != nil {
		select {
		case log.appended <- struct{}{}:
		default:
		}
	}
	if log.appendBlock != nil {
		<-log.appendBlock
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.appendErr != nil {
		return log.appendErr
	}
	clone := persist.CommitFrame{Revision: frame.Revision, Effects: append([]persist.Effect(nil), frame.Effects...)}
	log.frames = append(log.frames, clone)
	return nil
}

func (log *fakeLog) Barrier() error {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.barriers++
	if log.barrierErr != nil {
		return log.barrierErr
	}
	if len(log.frames) > 0 {
		log.durable = log.frames[len(log.frames)-1].Revision
	}
	return nil
}

func (log *fakeLog) DurableThrough() gapdb.Revision {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.durable
}

type countingAllocator struct {
	mu         sync.Mutex
	next       gapdb.Revision
	calls      int
	err        error
	panicValue any
}

func (a *countingAllocator) Next() (gapdb.Revision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.panicValue != nil {
		panic(a.panicValue)
	}
	if a.err != nil {
		return 0, a.err
	}
	r := a.next
	a.next++
	return r, nil
}

type testConfig struct {
	clock     clock.Clock
	log       CommitLog
	allocator RevisionAllocator
	records   []gapdb.Record
	current   gapdb.Revision
	durable   gapdb.Revision
	limits    gapdb.Limits
	queue     int
}

func newTestState(t *testing.T, config testConfig) *DatabaseState {
	t.Helper()
	if config.clock == nil {
		config.clock = clock.NewManual(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	}
	if config.log == nil {
		config.log = &fakeLog{durable: config.durable}
	}
	if config.allocator == nil {
		config.allocator = &countingAllocator{next: config.current + 1}
	}
	if config.limits == (gapdb.Limits{}) {
		config.limits = gapdb.DefaultOptions().Limits
	}
	if config.queue == 0 {
		config.queue = 16
	}
	state, err := New(Config{Limits: config.limits, QueueCapacity: config.queue, Clock: config.clock, Log: config.log, Allocator: config.allocator, Records: config.records, CurrentRevision: config.current, DurableThroughRevision: config.durable, SnapshotRevision: config.current})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func closeState(t *testing.T, state *DatabaseState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := state.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestMemoryAndDurableAcknowledgementsTellTheTruth(t *testing.T) {
	log := &fakeLog{}
	state := newTestState(t, testConfig{log: log})
	t.Cleanup(func() { closeState(t, state) })
	memory, err := state.Put(t.Context(), "a", []byte("1"), nil, gapdb.AckMemory)
	if err != nil || memory.Revision != 1 || memory.DurableThroughRevision != 0 || log.barriers != 0 {
		t.Fatalf("memory result = %+v, err=%v, barriers=%d", memory, err, log.barriers)
	}
	durable, err := state.Put(t.Context(), "b", []byte("2"), nil, gapdb.AckDurable)
	if err != nil || durable.Revision != 2 || durable.DurableThroughRevision != 2 || log.barriers != 1 {
		t.Fatalf("durable result = %+v, err=%v, barriers=%d", durable, err, log.barriers)
	}
	if state.DurableThroughRevision() != 2 {
		t.Fatalf("state durable through = %d", state.DurableThroughRevision())
	}
}

func TestPersistenceFailuresDoNotPublishAndDisableWrites(t *testing.T) {
	for _, test := range []struct {
		name                  string
		appendErr, barrierErr error
		ack                   gapdb.AckMode
	}{
		{name: "append", appendErr: errors.New("append"), ack: gapdb.AckMemory},
		{name: "barrier", barrierErr: errors.New("sync"), ack: gapdb.AckDurable},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := &fakeLog{appendErr: test.appendErr, barrierErr: test.barrierErr}
			state := newTestState(t, testConfig{log: log})
			t.Cleanup(func() { _ = state.Close(t.Context()) })
			_, err := state.Put(t.Context(), "key", []byte("value"), nil, test.ack)
			var apiErr *gapdb.Error
			if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeStorageDegraded || apiErr.FailedStage == "" {
				t.Fatalf("Put error = %#v", err)
			}
			if _, err := state.Get("key"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
				t.Fatalf("failed commit published: %v", err)
			}
			if _, err := state.Put(t.Context(), "other", nil, nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeStorageDegraded}) {
				t.Fatalf("write after degradation = %v", err)
			}
		})
	}
}

func TestImpossibleDurabilityEvidenceDoesNotPublish(t *testing.T) {
	log := &fakeLog{durable: 99}
	state := newTestState(t, testConfig{log: log})
	t.Cleanup(func() { _ = state.Close(t.Context()) })
	if result, err := state.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeStorageDegraded}) {
		t.Fatalf("Put = %+v, %v; want STORAGE_DEGRADED", result, err)
	}
	if _, err := state.Get("key"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("commit with impossible durability evidence published: %v", err)
	}
}

func TestBoundedQueueCancellationAndCloseDrain(t *testing.T) {
	unblock := make(chan struct{})
	appended := make(chan struct{}, 1)
	log := &fakeLog{appendBlock: unblock, appended: appended}
	state := newTestState(t, testConfig{log: log, queue: 1})
	firstDone := make(chan error, 1)
	go func() {
		_, err := state.Put(context.Background(), "first", nil, nil, gapdb.AckMemory)
		firstDone <- err
	}()
	<-appended
	secondDone := make(chan error, 1)
	go func() {
		_, err := state.Put(context.Background(), "second", nil, nil, gapdb.AckMemory)
		secondDone <- err
	}()
	for len(state.queue) != 1 {
	}
	if _, err := state.Put(t.Context(), "third", nil, nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeServerBusy}) {
		t.Fatalf("full queue error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := state.Put(canceled, "canceled", nil, nil, gapdb.AckMemory); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-admission cancellation = %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- state.Close(context.Background()) }()
	close(unblock)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := state.Get("second"); err != nil {
		t.Fatalf("accepted command was not drained: %v", err)
	}
	if _, err := state.Put(t.Context(), "late", nil, nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeServerShuttingDown}) {
		t.Fatalf("post-close mutation error = %v", err)
	}
}

func TestCancellationAfterAdmissionDoesNotCancelCommit(t *testing.T) {
	unblock := make(chan struct{})
	appended := make(chan struct{}, 1)
	state := newTestState(t, testConfig{log: &fakeLog{appendBlock: unblock, appended: appended}})
	t.Cleanup(func() { closeState(t, state) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := state.Put(ctx, "key", []byte("v"), nil, gapdb.AckMemory); done <- err }()
	<-appended
	cancel()
	close(unblock)
	if err := <-done; err != nil {
		t.Fatalf("admitted command error = %v", err)
	}
	if _, err := state.Get("key"); err != nil {
		t.Fatalf("admitted command not applied: %v", err)
	}
}

func TestAdmittedMutationOwnsItsInputBytes(t *testing.T) {
	unblock := make(chan struct{})
	appended := make(chan struct{}, 1)
	state := newTestState(t, testConfig{log: &fakeLog{appendBlock: unblock, appended: appended}})
	t.Cleanup(func() { closeState(t, state) })
	value := []byte("original")
	done := make(chan error, 1)
	go func() { _, err := state.Put(context.Background(), "key", value, nil, gapdb.AckMemory); done <- err }()
	<-appended
	value[0] = 'X'
	close(unblock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	record, err := state.Get("key")
	if err != nil || string(record.Value) != "original" {
		t.Fatalf("stored record = %+v, %v", record, err)
	}
}

func TestWriterPanicDegradesInsteadOfContinuing(t *testing.T) {
	state := newTestState(t, testConfig{allocator: &countingAllocator{next: 1, panicValue: "boom"}})
	t.Cleanup(func() { _ = state.Close(t.Context()) })
	if _, err := state.Put(t.Context(), "key", nil, nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInternal}) {
		t.Fatalf("panic error = %v", err)
	}
	if _, err := state.Put(t.Context(), "other", nil, nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeStorageDegraded}) {
		t.Fatalf("post-panic mutation error = %v", err)
	}
}

func TestReviewerWP03RejectsInvalidAllocatorRevisions(t *testing.T) {
	maximum := gapdb.Revision(^uint64(0))
	tests := []struct {
		name    string
		current gapdb.Revision
		next    gapdb.Revision
	}{
		{name: "zero", current: 5, next: 0},
		{name: "equal", current: 5, next: 5},
		{name: "regressing", current: 5, next: 4},
		{name: "maximum-boundary", current: maximum, next: maximum},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := &fakeLog{}
			state := newTestState(t, testConfig{log: log, allocator: &countingAllocator{next: test.next}, current: test.current})
			t.Cleanup(func() { _ = state.Close(t.Context()) })
			result, err := state.Put(t.Context(), "new-key", []byte("value"), nil, gapdb.AckMemory)
			var apiErr *gapdb.Error
			if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeInternal || apiErr.CorrelationID == "" || apiErr.OperationApplied {
				t.Fatalf("Put = %+v, %#v; want unapplied INTERNAL", result, err)
			}
			if len(log.frames) != 0 || state.CurrentRevision() != test.current || state.Lifecycle() != gapdb.LifecycleDegradedReadOnly {
				t.Fatalf("invalid revision escaped: frames=%d current=%d lifecycle=%s", len(log.frames), state.CurrentRevision(), state.Lifecycle())
			}
			if _, err := state.Get("new-key"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
				t.Fatalf("invalid revision published: %v", err)
			}
		})
	}
}

func TestReviewerWP03MaximumRevisionCommitsOnceThenFailsClosed(t *testing.T) {
	maximum := gapdb.Revision(^uint64(0))
	log := &fakeLog{}
	state := newTestState(t, testConfig{log: log, allocator: &countingAllocator{next: maximum}, current: maximum - 1})
	t.Cleanup(func() { _ = state.Close(t.Context()) })
	first, err := state.Put(t.Context(), "last", nil, nil, gapdb.AckMemory)
	if err != nil || first.Revision != maximum {
		t.Fatalf("maximum commit = %+v, %v", first, err)
	}
	_, err = state.Put(t.Context(), "wrapped", nil, nil, gapdb.AckMemory)
	var apiErr *gapdb.Error
	if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeInternal || apiErr.OperationApplied {
		t.Fatalf("wrapped commit error = %#v", err)
	}
	if len(log.frames) != 1 || state.CurrentRevision() != maximum {
		t.Fatalf("wrapped commit escaped: frames=%d current=%d", len(log.frames), state.CurrentRevision())
	}
	if _, err := state.Get("wrapped"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("wrapped revision published: %v", err)
	}
}

func TestReviewerWP03RealWALWriteAfterErrorCarriesReplayEvidence(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "wal-00000000000000000001.gdb")
	id, err := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	injector := faultfs.NewInjector(31, faultfs.Rule{Point: faultfs.PointWALFrameWrite, Phase: faultfs.After, Occurrence: 1, Seed: 31, Err: errors.New("write-after fault")})
	wal, err := persist.CreateWAL(faultfs.NewOS(injector), path, persist.WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1}, 1, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	state := newTestState(t, testConfig{log: wal, allocator: &countingAllocator{next: 1}})
	result, mutationErr := state.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckMemory)
	var apiErr *gapdb.Error
	if !errors.As(mutationErr, &apiErr) || apiErr.Code != gapdb.CodeStorageDegraded || !apiErr.OperationApplied {
		t.Fatalf("Put = %+v, %#v; want applied-possible STORAGE_DEGRADED", result, mutationErr)
	}
	if _, err := state.Get("key"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("failed append published in RAM: %v", err)
	}
	_ = state.Close(t.Context())
	_ = wal.Close()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := persist.ScanWAL(file, info.Size(), id, 0, 10, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Tail != nil || replay.RecoveredRevision != 1 || len(replay.Commits) != 1 || replay.Commits[0].Effects[0].Key != "key" {
		t.Fatalf("restart replay = %+v", replay)
	}
}

func TestReviewerWP03RealWALWriteBeforeErrorRemainsUnapplied(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "wal-00000000000000000001.gdb")
	id, err := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	injector := faultfs.NewInjector(32, faultfs.Rule{Point: faultfs.PointWALFrameWrite, Phase: faultfs.Before, Occurrence: 1, Seed: 32, Err: errors.New("write-before fault")})
	wal, err := persist.CreateWAL(faultfs.NewOS(injector), path, persist.WALHeader{DatabaseID: id, FirstRevision: 1, Generation: 1}, 1, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	state := newTestState(t, testConfig{log: wal, allocator: &countingAllocator{next: 1}})
	_, mutationErr := state.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckMemory)
	var apiErr *gapdb.Error
	if !errors.As(mutationErr, &apiErr) || apiErr.Code != gapdb.CodeStorageDegraded || apiErr.OperationApplied {
		t.Fatalf("Put error = %#v; want unapplied STORAGE_DEGRADED", mutationErr)
	}
	_ = state.Close(t.Context())
	_ = wal.Close()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	replay, err := persist.ScanWAL(file, info.Size(), id, 0, 10, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Tail != nil || replay.RecoveredRevision != 0 || len(replay.Commits) != 0 || info.Size() != persist.WALHeaderSize {
		t.Fatalf("restart replay = %+v, size=%d", replay, info.Size())
	}
}

type panicAfterAppendLog struct {
	frames []persist.CommitFrame
}

func (log *panicAfterAppendLog) Append(frame persist.CommitFrame) error {
	log.frames = append(log.frames, frame)
	return nil
}

func (*panicAfterAppendLog) Barrier() error { return nil }

func (*panicAfterAppendLog) DurableThrough() gapdb.Revision { panic("durability panic") }

type panicBarrierLog struct {
	frames   []persist.CommitFrame
	panicked bool
}

func (log *panicBarrierLog) Append(frame persist.CommitFrame) error {
	log.frames = append(log.frames, frame)
	return nil
}

func (log *panicBarrierLog) Barrier() error {
	if !log.panicked {
		log.panicked = true
		panic("barrier panic")
	}
	return nil
}

func (*panicBarrierLog) DurableThrough() gapdb.Revision { return 0 }

func TestReviewerWP03PanicEvidenceTracksAppendPhase(t *testing.T) {
	t.Run("allocator-panic-before-append", func(t *testing.T) {
		state := newTestState(t, testConfig{allocator: &countingAllocator{panicValue: "allocator panic"}})
		t.Cleanup(func() { _ = state.Close(t.Context()) })
		_, err := state.Put(t.Context(), "key", nil, nil, gapdb.AckMemory)
		var apiErr *gapdb.Error
		if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeInternal || apiErr.OperationApplied {
			t.Fatalf("allocator panic error = %#v", err)
		}
	})
	t.Run("log-panic-after-append", func(t *testing.T) {
		log := &panicAfterAppendLog{}
		state := newTestState(t, testConfig{log: log})
		t.Cleanup(func() { _ = state.Close(t.Context()) })
		_, err := state.Put(t.Context(), "key", nil, nil, gapdb.AckMemory)
		var apiErr *gapdb.Error
		if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeInternal || !apiErr.OperationApplied {
			t.Fatalf("post-append panic error = %#v", err)
		}
		if len(log.frames) != 1 {
			t.Fatalf("accepted frames = %d", len(log.frames))
		}
	})
	t.Run("barrier-panic-after-append", func(t *testing.T) {
		log := &panicBarrierLog{}
		state := newTestState(t, testConfig{log: log})
		t.Cleanup(func() { _ = state.Close(t.Context()) })
		_, err := state.Put(t.Context(), "key", nil, nil, gapdb.AckDurable)
		var apiErr *gapdb.Error
		if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeInternal || !apiErr.OperationApplied {
			t.Fatalf("post-append barrier panic error = %#v", err)
		}
		if len(log.frames) != 1 {
			t.Fatalf("accepted frames = %d", len(log.frames))
		}
	})
}

func TestReviewerWP03PersistenceFailurePreservesAppliedEvidence(t *testing.T) {
	cause := &gapdb.Error{
		Code:             gapdb.CodeStorageDegraded,
		Message:          "storage failed",
		Retry:            gapdb.RetryAfterRestart,
		FailedStage:      "wal_frame_write",
		OperationApplied: true,
		SafeActions:      []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionRestartAfterRecovery, gapdb.ActionAbort},
	}
	var got *gapdb.Error
	if err := persistenceFailure("wal_append", 4, 3, false, cause); !errors.As(err, &got) || !got.OperationApplied {
		t.Fatalf("persistenceFailure = %#v", err)
	}
}
