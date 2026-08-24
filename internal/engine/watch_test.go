package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/clock"
)

func receiveEvent(t *testing.T, watch *WatchSubscription) gapdb.ChangeEvent {
	t.Helper()
	select {
	case event, ok := <-watch.Events:
		if !ok {
			t.Fatal("watch events closed early")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watch event")
		return gapdb.ChangeEvent{}
	}
}

func receiveTermination(t *testing.T, watch *WatchSubscription) gapdb.WatchTermination {
	t.Helper()
	select {
	case termination := <-watch.Ended:
		return termination
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watch termination")
		return gapdb.WatchTermination{}
	}
}

func TestWatchBacklogThenLiveIsGapFreeAndOrdered(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	first, err := state.Put(t.Context(), "p/a", []byte("one"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Put(t.Context(), "other", nil, nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	watch, err := state.Watch(t.Context(), "p/", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watch.Close)
	if watch.RegistrationRevision != state.CurrentRevision() {
		t.Fatalf("registration revision = %d, current=%d", watch.RegistrationRevision, state.CurrentRevision())
	}
	batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("p/b", []byte("two"), gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		gapdb.NewPutMutation("p/c", []byte("three"), gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
	}}
	live, err := state.AtomicBatch(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	events := []gapdb.ChangeEvent{receiveEvent(t, watch), receiveEvent(t, watch), receiveEvent(t, watch)}
	if events[0].Revision != first.Revision || events[0].Key != "p/a" || events[1].Revision != live.Revision || events[1].Order != 0 || events[2].Order != 1 {
		t.Fatalf("watch sequence = %+v", events)
	}
}

func TestWatchRejectsAheadAndCompactedCursors(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.WatchBufferEvents = 1
	limits.MaxHistoryEvents = 1
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.Put(t.Context(), "a", nil, nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Put(t.Context(), "b", nil, nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Watch(t.Context(), "", 3); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionAhead}) {
		t.Fatalf("ahead cursor error = %v", err)
	}
	_, err := state.Watch(t.Context(), "", 0)
	var compacted *gapdb.Error
	if !errors.As(err, &compacted) || compacted.Code != gapdb.CodeRevisionCompacted || compacted.EarliestRevision == nil {
		t.Fatalf("compacted cursor error = %#v", err)
	}
}

func TestWatchLagIsExplicitAndNeverSplitsBatch(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.WatchBufferEvents = 1
	limits.MaxHistoryEvents = 4
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	baseline, err := state.Put(t.Context(), "other", nil, nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := state.Watch(t.Context(), "p/", baseline.Revision)
	if err != nil {
		t.Fatal(err)
	}
	batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("p/a", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		gapdb.NewPutMutation("p/b", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
	}}
	result, err := state.AtomicBatch(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	termination := receiveTermination(t, watch)
	if termination.Reason != gapdb.WatchEndedByError || termination.Error == nil || termination.Error.Code != gapdb.CodeWatchLagged || termination.Error.LastDeliveredRevision == nil || *termination.Error.LastDeliveredRevision != baseline.Revision || termination.Error.CurrentRevision == nil || *termination.Error.CurrentRevision != result.Revision {
		t.Fatalf("lag termination = %+v", termination)
	}
	select {
	case event, open := <-watch.Events:
		if open {
			t.Fatalf("partial lagged batch delivered: %+v", event)
		}
	default:
	}
}

func TestWatchBacklogPreflightNeverStartsAnOversizedCommit(t *testing.T) {
	for run := range 10 {
		limits := gapdb.DefaultOptions().Limits
		limits.WatchBufferEvents = 1
		limits.MaxHistoryEvents = 4
		now := time.Date(2026, 8, 23, 12, 0, run, 0, time.UTC)
		state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
		batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("p/a", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
			gapdb.NewPutMutation("p/b", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		}}
		result, err := state.AtomicBatch(t.Context(), batch)
		if err != nil {
			t.Fatal(err)
		}
		watch, err := state.Watch(t.Context(), "p/", 0)
		if err != nil {
			t.Fatal(err)
		}
		termination := receiveTermination(t, watch)
		if termination.Error == nil || termination.Error.Code != gapdb.CodeWatchLagged || termination.LastDeliveredRevision != 0 || termination.Error.CurrentRevision == nil || *termination.Error.CurrentRevision != result.Revision {
			t.Fatalf("run %d: backlog preflight termination = %+v", run, termination)
		}
		if event, open := <-watch.Events; open {
			t.Fatalf("run %d: oversized backlog began with event %+v", run, event)
		}
		closeState(t, state)
	}
}

func TestWatchBacklogPreflightBoundsCumulativeGroups(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.WatchBufferEvents = 1
	limits.MaxHistoryEvents = 4
	now := time.Date(2026, 8, 23, 12, 2, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	for _, key := range []string{"p/a", "p/b"} {
		if _, err := state.Put(t.Context(), key, nil, nil, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	watch, err := state.Watch(t.Context(), "p/", 0)
	if err != nil {
		t.Fatal(err)
	}
	termination := receiveTermination(t, watch)
	if termination.Error == nil || termination.Error.Code != gapdb.CodeWatchLagged || termination.LastDeliveredRevision != 0 || termination.Error.CurrentRevision == nil || *termination.Error.CurrentRevision != 2 {
		t.Fatalf("cumulative backlog termination = %+v", termination)
	}
	if event, open := <-watch.Events; open {
		t.Fatalf("over-budget backlog began with event %+v", event)
	}
}

func TestWatchLagWaitsForStartedBacklogCommit(t *testing.T) {
	for run := range 10 {
		limits := gapdb.DefaultOptions().Limits
		limits.WatchBufferEvents = 2
		limits.MaxHistoryEvents = 8
		now := time.Date(2026, 8, 23, 12, 1, run, 0, time.UTC)
		state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
		batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("p/a", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
			gapdb.NewPutMutation("p/b", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		}}
		backlog, err := state.AtomicBatch(t.Context(), batch)
		if err != nil {
			t.Fatal(err)
		}
		watch, err := state.Watch(t.Context(), "p/", 0)
		if err != nil {
			t.Fatal(err)
		}
		first := receiveEvent(t, watch)
		if first.Revision != backlog.Revision || first.Order != 0 {
			t.Fatalf("run %d: first backlog event = %+v", run, first)
		}
		live, err := state.Put(t.Context(), "p/c", nil, nil, gapdb.AckMemory)
		if err != nil {
			t.Fatal(err)
		}
		second := receiveEvent(t, watch)
		if second.Revision != backlog.Revision || second.Order != 1 {
			t.Fatalf("run %d: partial backlog commit = %+v", run, second)
		}
		termination := receiveTermination(t, watch)
		if termination.Error == nil || termination.Error.Code != gapdb.CodeWatchLagged || termination.LastDeliveredRevision != backlog.Revision || termination.Error.CurrentRevision == nil || *termination.Error.CurrentRevision != live.Revision {
			t.Fatalf("run %d: mid-backlog lag termination = %+v", run, termination)
		}
		closeState(t, state)
	}
}

func TestWatchCancellationAndOwnerShutdownTerminateCleanly(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	watch, err := state.Watch(t.Context(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	watch.Close()
	if termination := receiveTermination(t, watch); termination.Reason != gapdb.WatchEndedByClient {
		t.Fatalf("client termination = %+v", termination)
	}
	shutdownWatch, err := state.Watch(t.Context(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	closeState(t, state)
	if termination := receiveTermination(t, shutdownWatch); termination.Reason != gapdb.WatchEndedByShutdown {
		t.Fatalf("shutdown termination = %+v", termination)
	}
}

func TestScanPlusWatchEventsReconstructCurrentAuthority(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	expires := now.Add(time.Minute)
	put, err := state.Put(t.Context(), "p/a", []byte("old"), &expires, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	page, err := state.ScanPrefix("p/", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	watch, err := state.Watch(t.Context(), "p/", page.ObservedRevision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watch.Close)
	manual.Set(expires)
	expiry, err := state.ProcessDueExpiries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	newPut, err := state.Put(t.Context(), "p/b", []byte("new"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	events := []gapdb.ChangeEvent{receiveEvent(t, watch), receiveEvent(t, watch)}
	if events[0].Revision != expiry.Revision || events[0].Kind != gapdb.ChangeExpire || events[1].Revision != newPut.Revision || put.Revision != page.ObservedRevision {
		t.Fatalf("reconciliation events = %+v", events)
	}
	reconstructed := map[string][]byte{}
	for _, record := range page.Records {
		reconstructed[record.Key] = append([]byte(nil), record.Value...)
	}
	for _, event := range events {
		if event.Record == nil {
			delete(reconstructed, event.Key)
		} else {
			reconstructed[event.Key] = append([]byte(nil), event.Record.Value...)
		}
	}
	if len(reconstructed) != 1 || string(reconstructed["p/b"]) != "new" {
		t.Fatalf("reconstructed authority = %#v", reconstructed)
	}
}
