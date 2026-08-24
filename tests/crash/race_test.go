package crash_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/server"
)

func TestFullStackRaceStress(t *testing.T) {
	options := gapdb.DefaultOptions()
	options.Limits.WatchBufferEvents = 8
	options.Limits.MaxHistoryEvents = 64
	directory := t.TempDir()
	instance, err := server.Open(server.Config{Directory: directory, Options: options, ToolVersion: "race-evidence", QueueCapacity: 16, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	writer := dialWithLimits(t, instance.SocketPath(), options.Limits)
	watch, err := writer.Watch(ctx, "stress/", 0)
	if err != nil {
		t.Fatal(err)
	}
	var lastWatch atomic.Uint64
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		for event := range watch.Events {
			previous := lastWatch.Swap(uint64(event.Revision))
			if previous > uint64(event.Revision) {
				t.Errorf("watch revision regressed: %d after %d", event.Revision, previous)
			}
		}
		<-watch.Ended
	}()

	errorsSeen := make(chan error, 64)
	var workers sync.WaitGroup
	for readerIndex := 0; readerIndex < 8; readerIndex++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: time.Second, Limits: options.Limits})
			if err != nil {
				errorsSeen <- err
				return
			}
			defer client.Close()
			for iteration := 0; iteration < 150; iteration++ {
				key := fmt.Sprintf("stress/%03d", (iteration+index)%120)
				_, err := client.Get(ctx, key)
				if err != nil && !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) && !expectedConcurrentExit(err) {
					errorsSeen <- fmt.Errorf("reader %d: %w", index, err)
					return
				}
			}
		}(readerIndex)
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: time.Second, Limits: options.Limits})
		if err != nil {
			errorsSeen <- err
			return
		}
		defer client.Close()
		for iteration := 0; iteration < 100; iteration++ {
			page, err := client.ScanPrefix(ctx, "stress/", 7, "")
			if err != nil && !expectedConcurrentExit(err) {
				errorsSeen <- fmt.Errorf("scan: %w", err)
				return
			}
			if err == nil {
				for index := 1; index < len(page.Records); index++ {
					if page.Records[index-1].Key >= page.Records[index].Key {
						errorsSeen <- fmt.Errorf("scan order is not strict")
						return
					}
				}
			}
		}
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: time.Second, Limits: options.Limits})
		if err != nil {
			errorsSeen <- err
			return
		}
		defer client.Close()
		for iteration := 0; iteration < 80; iteration++ {
			status, err := client.Status(ctx)
			if err != nil {
				if !expectedConcurrentExit(err) {
					errorsSeen <- fmt.Errorf("status: %w", err)
				}
				return
			}
			if iteration%20 == 0 {
				_, err = client.CreateSnapshot(ctx, status.DatabaseID, status.CurrentRevision)
				if err != nil && !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAdminPreconditionFailed}) && !errors.Is(err, &gapdb.Error{Code: gapdb.CodeSnapshotInProgress}) && !expectedConcurrentExit(err) {
					errorsSeen <- fmt.Errorf("snapshot: %w", err)
					return
				}
			}
		}
	}()

	var committed atomic.Uint64
	for iteration := 0; iteration < 120; iteration++ {
		ack := gapdb.AckMemory
		if iteration%3 == 0 {
			ack = gapdb.AckDurable
		}
		var expiry *time.Time
		if iteration%10 == 0 {
			value := time.Now().Add(20 * time.Millisecond).UTC()
			expiry = &value
		}
		result, err := writer.Put(ctx, fmt.Sprintf("stress/%03d", iteration), []byte{byte(iteration)}, expiry, ack)
		if err != nil {
			t.Fatalf("writer %d: %v", iteration, err)
		}
		if previous := committed.Swap(uint64(result.Revision)); previous >= uint64(result.Revision) {
			t.Fatalf("writer revision %d after %d", result.Revision, previous)
		}
	}
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}

	// A condition race has exactly one winner and consumes one revision.
	// Let the deliberately short expiries finish so this isolated oracle has
	// no unrelated cleanup revision racing its before/after observation.
	time.Sleep(100 * time.Millisecond)
	statusBefore, err := writer.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int64
	var conditionWorkers sync.WaitGroup
	for contender := 0; contender < 16; contender++ {
		conditionWorkers.Add(1)
		go func(index int) {
			defer conditionWorkers.Done()
			client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 2 * time.Second, Limits: options.Limits})
			if err != nil {
				t.Errorf("contender dial: %v", err)
				return
			}
			defer client.Close()
			if _, err := client.PutIfAbsent(ctx, "authority/single-winner", []byte{byte(index)}, nil, gapdb.AckDurable); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAlreadyExists}) {
				t.Errorf("contender: %v", err)
			}
		}(contender)
	}
	conditionWorkers.Wait()
	statusAfter, err := writer.Status(ctx)
	if err != nil || winners.Load() != 1 || statusAfter.CurrentRevision != statusBefore.CurrentRevision+1 {
		t.Fatalf("conditional oracle winners=%d before=%d after=%d err=%v", winners.Load(), statusBefore.CurrentRevision, statusAfter.CurrentRevision, err)
	}

	deadline, deadlineCancel := context.WithCancel(ctx)
	deadlineCancel()
	if _, err := writer.Get(deadline, "stress/001"); err == nil {
		t.Fatal("cancelled request unexpectedly succeeded")
	}
	watch.Close()
	select {
	case <-watchDone:
	case <-time.After(3 * time.Second):
		t.Fatal("watch cancellation leaked")
	}
	_ = writer.Close()
	if err := instance.Close(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("race_evidence readers=8 mutations=120 condition_contenders=16 last_watch_revision=%d", lastWatch.Load())
}

func TestFullStackLagDisconnectDeadlineShutdownMatrix(t *testing.T) {
	t.Run("exact-sequence-and-disconnect", func(t *testing.T) {
		options := gapdb.DefaultOptions()
		options.Limits.WatchBufferEvents = 8
		instance, err := server.Open(server.Config{Directory: t.TempDir(), Options: options, ToolVersion: "race-matrix", WriteTimeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer instance.Close(context.Background())
		writer := dialWithLimits(t, instance.SocketPath(), options.Limits)
		defer writer.Close()
		watchClient := dialWithLimits(t, instance.SocketPath(), options.Limits)
		status, err := writer.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		watch, err := watchClient.Watch(t.Context(), "ordered/", status.CurrentRevision)
		if err != nil {
			t.Fatal(err)
		}
		type expectedEvent struct {
			key      string
			revision gapdb.Revision
		}
		expected := make([]expectedEvent, 0, 3)
		for index := 0; index < 3; index++ {
			key := fmt.Sprintf("ordered/%d", index)
			result, err := writer.Put(t.Context(), key, []byte{byte(index)}, nil, gapdb.AckDurable)
			if err != nil {
				t.Fatal(err)
			}
			expected = append(expected, expectedEvent{key, result.Revision})
		}
		for index, want := range expected {
			select {
			case event := <-watch.Events:
				if event.Key != want.key || event.Revision != want.revision || event.Order != 0 {
					t.Fatalf("event %d=%+v want=%+v", index, event, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("event %d timed out", index)
			}
		}
		if err := watchClient.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case end := <-watch.Ended:
			if end.Reason != gapdb.WatchEndedByClient || end.LastDeliveredRevision != expected[len(expected)-1].revision {
				t.Fatalf("disconnect termination=%+v", end)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("disconnect did not terminate public watch")
		}
	})

	t.Run("watch-buffer-lag", func(t *testing.T) {
		options := gapdb.DefaultOptions()
		options.Limits.WatchBufferEvents = 2
		options.Limits.MaxHistoryEvents = 16
		instance, err := server.Open(server.Config{Directory: t.TempDir(), Options: options, ToolVersion: "race-matrix", WriteTimeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer instance.Close(context.Background())
		writer := dialWithLimits(t, instance.SocketPath(), options.Limits)
		defer writer.Close()
		exact, err := writer.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("lag/exact-a", []byte("a"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			gapdb.NewPutMutation("lag/exact-b", []byte("b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		}})
		if err != nil {
			t.Fatal(err)
		}
		exactClient := dialWithLimits(t, instance.SocketPath(), options.Limits)
		exactWatch, err := exactClient.Watch(t.Context(), "lag/exact-", 0)
		if err != nil {
			t.Fatal(err)
		}
		for order := uint32(0); order < 2; order++ {
			select {
			case event := <-exactWatch.Events:
				if event.Revision != exact.Revision || event.Order != order {
					t.Fatalf("watch buffer exact event=%+v order=%d", event, order)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("watch buffer exact delivery timed out")
			}
		}
		exactWatch.Close()
		_ = exactClient.Close()
		batch, err := writer.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("lag/a", []byte("a"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			gapdb.NewPutMutation("lag/b", []byte("b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			gapdb.NewPutMutation("lag/c", []byte("c"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		}})
		if err != nil {
			t.Fatal(err)
		}
		watchClient := dialWithLimits(t, instance.SocketPath(), options.Limits)
		defer watchClient.Close()
		watch, err := watchClient.Watch(t.Context(), "lag/", exact.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if event, ok := <-watch.Events; ok {
			t.Fatalf("partial event escaped an over-capacity commit: %+v", event)
		}
		end := <-watch.Ended
		if end.Reason != gapdb.WatchEndedByError || end.Error == nil || end.Error.Code != gapdb.CodeWatchLagged || end.LastDeliveredRevision != exact.Revision || end.Error.CurrentRevision == nil || *end.Error.CurrentRevision != batch.Revision {
			t.Fatalf("lag termination=%+v", end)
		}
	})

	t.Run("accepted-watch-deadline", func(t *testing.T) {
		options := gapdb.DefaultOptions()
		instance, err := server.Open(server.Config{Directory: t.TempDir(), Options: options, ToolVersion: "race-matrix"})
		if err != nil {
			t.Fatal(err)
		}
		defer instance.Close(context.Background())
		client := dialWithLimits(t, instance.SocketPath(), options.Limits)
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		watch, err := client.Watch(ctx, "deadline/", 0)
		if err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		select {
		case end := <-watch.Ended:
			deadlineApplied := end.Reason == gapdb.WatchEndedByClient || end.Reason == gapdb.WatchEndedByError && end.Error != nil && end.Error.Code == gapdb.CodeServerUnavailable
			if !deadlineApplied || end.LastDeliveredRevision != 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("deadline termination=%+v", end)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("accepted watch ignored its context deadline")
		}
	})

	t.Run("concurrent-shutdown-restart", func(t *testing.T) {
		options := gapdb.DefaultOptions()
		directory := t.TempDir()
		instance, err := server.Open(server.Config{Directory: directory, Options: options, ToolVersion: "race-matrix"})
		if err != nil {
			t.Fatal(err)
		}
		writer := dialWithLimits(t, instance.SocketPath(), options.Limits)
		watchClient := dialWithLimits(t, instance.SocketPath(), options.Limits)
		watch, err := watchClient.Watch(context.Background(), "shutdown/", 0)
		if err != nil {
			t.Fatal(err)
		}
		watchEnd := make(chan gapdb.WatchTermination, 1)
		go func() {
			for range watch.Events {
			}
			watchEnd <- <-watch.Ended
		}()
		type acknowledged struct {
			key      string
			value    []byte
			revision gapdb.Revision
		}
		var mu sync.Mutex
		var acknowledgements []acknowledged
		first := make(chan struct{})
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			for index := 0; index < 200; index++ {
				key := fmt.Sprintf("shutdown/%03d", index)
				value := []byte(fmt.Sprintf("value-%03d", index))
				result, err := writer.Put(context.Background(), key, value, nil, gapdb.AckDurable)
				if err != nil {
					if !expectedConcurrentExit(err) {
						t.Errorf("shutdown writer: %v", err)
					}
					return
				}
				mu.Lock()
				acknowledgements = append(acknowledgements, acknowledged{key, append([]byte(nil), value...), result.Revision})
				if len(acknowledgements) == 1 {
					close(first)
				}
				mu.Unlock()
			}
		}()
		select {
		case <-first:
		case <-time.After(2 * time.Second):
			t.Fatal("writer never produced an acknowledgement")
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := instance.Close(closeCtx); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		<-writerDone
		select {
		case end := <-watchEnd:
			if end.Reason != gapdb.WatchEndedByShutdown && end.Reason != gapdb.WatchEndedByError {
				t.Fatalf("shutdown watch termination=%+v", end)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("shutdown watch leaked")
		}
		_ = watchClient.Close()
		_ = writer.Close()

		restarted, err := server.Open(server.Config{Directory: directory, Options: options, ToolVersion: "race-matrix"})
		if err != nil {
			t.Fatal(err)
		}
		defer restarted.Close(context.Background())
		reconciler := dialWithLimits(t, restarted.SocketPath(), options.Limits)
		defer reconciler.Close()
		mu.Lock()
		stable := append([]acknowledged(nil), acknowledgements...)
		mu.Unlock()
		if len(stable) == 0 {
			t.Fatal("shutdown matrix recorded no acknowledged work")
		}
		for _, commit := range stable {
			record, err := reconciler.Get(t.Context(), commit.key)
			if err != nil || string(record.Value) != string(commit.value) || record.Revision != commit.revision {
				t.Fatalf("acknowledged work lost after shutdown: commit=%+v record=%+v err=%v", commit, record, err)
			}
		}
	})
}

func dialWithLimits(t *testing.T, socket string, limits gapdb.Limits) *gapdb.Client {
	t.Helper()
	client, err := gapdb.Dial(socket, gapdb.ClientOptions{Timeout: 2 * time.Second, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func expectedConcurrentExit(err error) bool {
	var transport *gapdb.TransportError
	return errors.As(err, &transport) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
