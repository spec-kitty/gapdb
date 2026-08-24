package owner_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/admin"
	"github.com/spec-kitty/gapdb/internal/engine"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/owner"
	"github.com/spec-kitty/gapdb/internal/persist"
)

type logStub struct {
	durable        gapdb.Revision
	barriers       atomic.Int64
	barrierBlock   <-chan struct{}
	barrierStarted chan<- struct{}
}

func (log *logStub) Append(frame persist.CommitFrame) error { return nil }
func (log *logStub) Barrier() error {
	log.barriers.Add(1)
	if log.barrierStarted != nil {
		select {
		case log.barrierStarted <- struct{}{}:
		default:
		}
	}
	if log.barrierBlock != nil {
		<-log.barrierBlock
	}
	log.durable = 0
	return nil
}
func (log *logStub) DurableThrough() gapdb.Revision { return log.durable }

type allocatorStub struct{ next gapdb.Revision }

func (allocator *allocatorStub) Next() (gapdb.Revision, error) {
	allocator.next++
	return allocator.next, nil
}

func TestOpenRoutesRecoveredOwnerStateToBoundedAdministrativeStatus(t *testing.T) {
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	fsys := faultfs.NewOS(nil)
	directory := t.TempDir()
	ownership, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := owner.Open(owner.OpenConfig{Engine: engine.Config{Log: &logStub{}, Allocator: &allocatorStub{}, DatabaseID: id}, Admin: admin.ControllerConfig{FS: fsys, Directory: directory, Manifest: persist.Manifest{DatabaseID: id, Generation: 1}, ToolVersion: "test"}, Ownership: ownership})
	if err != nil {
		t.Fatal(err)
	}
	view := runtime.Status()
	if view.SchemaVersion != 1 || view.Status.DatabaseID != id.String() || view.EffectiveConfig.Limits.MaxKeyBytes == 0 {
		t.Fatalf("composition status = %+v", view)
	}
	if _, err := persist.AcquireOwner(fsys, directory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeOwnerExists}) {
		t.Fatalf("runtime did not retain exclusive ownership: %v", err)
	}
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatalf("runtime close did not release ownership: %v", err)
	}
	_ = reopened.Close()
}

func TestOpenRejectsReleasedOwnerLockBeforeStartingEngine(t *testing.T) {
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	fsys := faultfs.NewOS(nil)
	directory := t.TempDir()
	ownership, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := ownership.Close(); err != nil || ownership.IsHeld() {
		t.Fatalf("closed ownership remains held: held=%v err=%v", ownership.IsHeld(), err)
	}
	if err := ownership.Close(); err != nil {
		t.Fatalf("second owner lock close: %v", err)
	}
	log := &logStub{}
	if runtime, err := owner.Open(openConfig(log, fsys, directory, id, ownership)); err == nil || runtime != nil || log.barriers.Load() != 0 {
		t.Fatalf("runtime=%v barriers=%d err=%v", runtime, log.barriers.Load(), err)
	}
	reopened, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatalf("closed ownership still blocks acquisition: %v", err)
	}
	_ = reopened.Close()
}

func TestOpenRejectsMismatchedAndDuplicateOwnershipTransfer(t *testing.T) {
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	fsys := faultfs.NewOS(nil)
	firstDirectory, secondDirectory := t.TempDir(), t.TempDir()
	mismatched, err := persist.AcquireOwner(fsys, firstDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if runtime, err := owner.Open(openConfig(&logStub{}, fsys, secondDirectory, id, mismatched)); err == nil || runtime != nil || mismatched.IsHeld() {
		t.Fatalf("mismatched runtime=%v held=%v err=%v", runtime, mismatched.IsHeld(), err)
	}
	reclaimed, err := persist.AcquireOwner(fsys, firstDirectory)
	if err != nil {
		t.Fatalf("mismatched transfer did not release original directory: %v", err)
	}
	_ = reclaimed.Close()

	ownership, err := persist.AcquireOwner(fsys, secondDirectory)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := owner.Open(openConfig(&logStub{}, fsys, secondDirectory, id, ownership))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, duplicateErr := owner.Open(openConfig(&logStub{}, fsys, secondDirectory, id, ownership)); duplicateErr == nil || duplicate != nil {
		t.Fatalf("duplicate runtime=%v err=%v", duplicate, duplicateErr)
	}
	if !ownership.IsHeld() {
		t.Fatal("duplicate transfer released active runtime ownership")
	}
	if _, err := persist.AcquireOwner(fsys, secondDirectory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeOwnerExists}) {
		t.Fatalf("second owner acquired during active runtime: %v", err)
	}
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ownership.IsHeld() {
		t.Fatal("runtime close retained duplicate-transfer ownership")
	}
}

func TestRuntimeConcurrentCloseIsIdempotentAndReleasesAfterDrain(t *testing.T) {
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	fsys := faultfs.NewOS(nil)
	directory := t.TempDir()
	ownership, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatal(err)
	}
	log := &logStub{}
	runtime, err := owner.Open(openConfig(log, fsys, directory, id, ownership))
	if err != nil {
		t.Fatal(err)
	}
	const callers = 16
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- runtime.Close(t.Context())
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if ownership.IsHeld() || log.barriers.Load() != 1 {
		t.Fatalf("held=%v close barriers=%d", ownership.IsHeld(), log.barriers.Load())
	}
	reopened, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatalf("ownership not released after drain: %v", err)
	}
	_ = reopened.Close()
}

func TestRuntimeRetainsOwnershipUntilBackgroundDrainCompletes(t *testing.T) {
	id, _ := persist.ParseDatabaseID("00112233445566778899aabbccddeeff")
	fsys := faultfs.NewOS(nil)
	directory := t.TempDir()
	ownership, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatal(err)
	}
	barrierBlock := make(chan struct{})
	barrierStarted := make(chan struct{}, 1)
	log := &logStub{barrierBlock: barrierBlock, barrierStarted: barrierStarted}
	runtime, err := owner.Open(openConfig(log, fsys, directory, id, ownership))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runtime.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled close = %v", err)
	}
	<-barrierStarted
	if _, err := persist.AcquireOwner(fsys, directory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeOwnerExists}) {
		t.Fatalf("ownership released before writer drain: %v", err)
	}
	close(barrierBlock)
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := persist.AcquireOwner(fsys, directory)
	if err != nil {
		t.Fatalf("ownership not released after writer drain: %v", err)
	}
	_ = reopened.Close()
}

func openConfig(log *logStub, fsys faultfs.FS, directory string, id persist.DatabaseID, ownership *persist.OwnerLock) owner.OpenConfig {
	return owner.OpenConfig{Engine: engine.Config{Log: log, Allocator: &allocatorStub{}, DatabaseID: id}, Admin: admin.ControllerConfig{FS: fsys, Directory: directory, Manifest: persist.Manifest{DatabaseID: id, Generation: 1}, ToolVersion: "test"}, Ownership: ownership}
}
