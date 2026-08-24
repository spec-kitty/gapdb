package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"gapdb/gapdb"
)

type snapshotInstallerFunc func(SnapshotCut) (CommitLog, error)

func (function snapshotInstallerFunc) Install(cut SnapshotCut) (CommitLog, error) {
	return function(cut)
}

func TestSnapshotRunsOnWriterWithCopySafeCutAndReadableState(t *testing.T) {
	oldLog := &fakeLog{}
	state := newTestState(t, testConfig{log: oldLog})
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	nextLog := &fakeLog{durable: 1}
	done := make(chan error, 1)
	go func() {
		_, err := state.Snapshot(context.Background(), snapshotInstallerFunc(func(cut SnapshotCut) (CommitLog, error) {
			close(entered)
			if cut.Revision != 1 || len(cut.Records) != 1 || string(cut.Records[0].Value) != "value" {
				t.Errorf("cut=%+v", cut)
			}
			cut.Records[0].Value[0] = 'X'
			<-release
			return nextLog, nil
		}))
		done <- err
	}()
	<-entered
	if record, err := state.Get("key"); err != nil || string(record.Value) != "value" {
		t.Fatalf("read during snapshot=%+v,%v", record, err)
	}
	mutationDone := make(chan struct{})
	go func() {
		defer close(mutationDone)
		_, _ = state.Put(context.Background(), "after", []byte("2"), nil, gapdb.AckMemory)
	}()
	select {
	case <-mutationDone:
		t.Fatal("mutation passed snapshot writer barrier")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-mutationDone
	if len(nextLog.frames) != 1 || nextLog.frames[0].Revision != 2 {
		t.Fatalf("new WAL frames=%+v", nextLog.frames)
	}
}

type snapshotPanicBarrierLog struct {
	fakeLog
	panicked bool
}

func (log *snapshotPanicBarrierLog) Barrier() error {
	if !log.panicked {
		log.panicked = true
		panic("barrier panic")
	}
	return log.fakeLog.Barrier()
}

func TestSnapshotBarrierPanicDoesNotClaimAppliedAuthority(t *testing.T) {
	state := newTestState(t, testConfig{log: &snapshotPanicBarrierLog{}})
	t.Cleanup(func() { closeState(t, state) })
	called := false
	_, err := state.Snapshot(t.Context(), SnapshotInstallerFunc(func(SnapshotCut) (CommitLog, error) { called = true; return nil, nil }))
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeInternal || structured.OperationApplied || called {
		t.Fatalf("panic evidence=%#v, installer called=%v", err, called)
	}
}

func TestSnapshotInstallerPanicConservativelyClaimsPossibleApply(t *testing.T) {
	state := newTestState(t, testConfig{})
	t.Cleanup(func() { closeState(t, state) })
	_, err := state.Snapshot(t.Context(), SnapshotInstallerFunc(func(SnapshotCut) (CommitLog, error) { panic("after install began") }))
	var structured *gapdb.Error
	if !errors.As(err, &structured) || structured.Code != gapdb.CodeInternal || !structured.OperationApplied || state.Lifecycle() != gapdb.LifecycleDegradedReadOnly {
		t.Fatalf("panic evidence=%#v lifecycle=%s", err, state.Lifecycle())
	}
}
