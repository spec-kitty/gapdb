package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/clock"
)

type manualExpiryTimer struct {
	mu      sync.Mutex
	channel chan time.Time
	active  bool
	delay   time.Duration
}

func newManualExpiryTimer() *manualExpiryTimer {
	return &manualExpiryTimer{channel: make(chan time.Time, 1)}
}

func (timer *manualExpiryTimer) C() <-chan time.Time { return timer.channel }
func (timer *manualExpiryTimer) Reset(delay time.Duration) {
	timer.mu.Lock()
	timer.active, timer.delay = true, delay
	timer.mu.Unlock()
}
func (timer *manualExpiryTimer) Stop() {
	timer.mu.Lock()
	timer.active = false
	timer.mu.Unlock()
}
func (timer *manualExpiryTimer) Fire(at time.Time) {
	timer.mu.Lock()
	active := timer.active
	timer.active = false
	timer.mu.Unlock()
	if active {
		timer.channel <- at
	}
}

func newObservationState(t *testing.T, manual *clock.Manual, timer ExpiryTimer, limits gapdb.Limits, records []gapdb.Record, current gapdb.Revision) (*DatabaseState, *fakeLog) {
	t.Helper()
	if limits == (gapdb.Limits{}) {
		limits = gapdb.DefaultOptions().Limits
	}
	log := &fakeLog{}
	state, err := New(Config{
		Limits:           limits,
		QueueCapacity:    64,
		Clock:            manual,
		ExpiryTimer:      timer,
		Log:              log,
		Allocator:        &countingAllocator{next: current + 1},
		Records:          records,
		CurrentRevision:  current,
		SnapshotRevision: current,
		DatabaseID:       testDatabaseID(t, "00112233445566778899aabbccddeeff"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return state, log
}

func TestExpiryCleanupIsOneDurableSortedCommit(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	timer := newManualExpiryTimer()
	state, log := newObservationState(t, manual, timer, gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	expires := now.Add(time.Minute)
	for _, key := range []string{"z/key", "a/key"} {
		if _, err := state.Put(t.Context(), key, []byte(key), &expires, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	manual.Set(expires)
	if _, err := state.Get("a/key"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("logical expiry boundary = %v", err)
	}
	result, err := state.ProcessDueExpiries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 3 || result.Ack != gapdb.AckDurable || len(result.Events) != 2 || log.barriers != 1 {
		t.Fatalf("expiry result = %+v, barriers=%d", result, log.barriers)
	}
	if result.Events[0].Kind != gapdb.ChangeExpire || result.Events[0].Key != "a/key" || result.Events[1].Key != "z/key" {
		t.Fatalf("expiry event order = %+v", result.Events)
	}
	frame := log.frames[len(log.frames)-1]
	if len(frame.Effects) != 2 || frame.Effects[0].Kind != gapdb.ChangeExpire || frame.Effects[0].Key != "a/key" {
		t.Fatalf("expiry WAL frame = %+v", frame)
	}
}

func TestExpiryStaleCandidateCannotDeleteReplacement(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	expires := now.Add(time.Minute)
	if _, err := state.Put(t.Context(), "lease", []byte("old"), &expires, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	manual.Set(expires)
	if _, err := state.PutIfAbsent(t.Context(), "lease", []byte("new"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	before := state.CurrentRevision()
	result, err := state.ProcessDueExpiries(t.Context())
	if err != nil || result.Revision != 0 || state.CurrentRevision() != before {
		t.Fatalf("stale cleanup = %+v, %v, current=%d", result, err, state.CurrentRevision())
	}
	record, err := state.Get("lease")
	if err != nil || string(record.Value) != "new" {
		t.Fatalf("replacement = %+v, %v", record, err)
	}
	manual.Set(now)
	if record, err = state.Get("lease"); err != nil || string(record.Value) != "new" {
		t.Fatalf("clock rollback changed replacement = %+v, %v", record, err)
	}
}

func TestExpiryHeapRebuildsFromRecoveredRecordsAndTimer(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	expires := now.Add(-time.Second)
	manual := clock.NewManual(now)
	timer := newManualExpiryTimer()
	state, _ := newObservationState(t, manual, timer, gapdb.Limits{}, []gapdb.Record{gapdb.NewRecord("expired", []byte("v"), 5, &expires)}, 5)
	t.Cleanup(func() { closeState(t, state) })
	timer.Fire(now)
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for state.CurrentRevision() == 5 {
		select {
		case <-deadline.Done():
			t.Fatal("writer timer did not materialize recovered expiry")
		default:
		}
	}
	if _, err := state.Get("expired"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("expired recovered record remains: %v", err)
	}
}

func TestExpiryCleanupChunksAtBatchLimitWithoutLosingCandidates(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	limits := gapdb.DefaultOptions().Limits
	limits.MaxBatchOperations = 1
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	expires := now.Add(time.Minute)
	for _, key := range []string{"a", "b"} {
		if _, err := state.Put(t.Context(), key, nil, &expires, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	manual.Set(expires)
	first, err := state.ProcessDueExpiries(t.Context())
	if err != nil || len(first.Events) != 1 || first.Events[0].Key != "a" {
		t.Fatalf("first expiry chunk = %+v, %v", first, err)
	}
	second, err := state.ProcessDueExpiries(t.Context())
	if err != nil || len(second.Events) != 1 || second.Events[0].Key != "b" {
		t.Fatalf("second expiry chunk = %+v, %v", second, err)
	}
}

func TestNewRejectsRecoveredExpiryThatCannotFitCleanupFrame(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Minute)
	limits := gapdb.DefaultOptions().Limits
	limits.MaxFrameBytes = 52
	limits.MaxBatchBytes = 52
	limits.MaxScanBytes = 52
	timer := newManualExpiryTimer()
	_, err := New(Config{
		Limits:           limits,
		Clock:            clock.NewManual(now),
		ExpiryTimer:      timer,
		Log:              &fakeLog{},
		Allocator:        &countingAllocator{next: 2},
		Records:          []gapdb.Record{gapdb.NewRecord("a", nil, 1, &expires)},
		CurrentRevision:  1,
		SnapshotRevision: 1,
		DatabaseID:       testDatabaseID(t, "00112233445566778899aabbccddeeff"),
	})
	var protocolError *gapdb.Error
	if !errors.As(err, &protocolError) || protocolError.Code != gapdb.CodeBatchTooLarge || protocolError.ReceivedBytes != 53 || protocolError.MaximumBytes != 52 {
		t.Fatalf("New error = %#v, want BATCH_TOO_LARGE 53/52", err)
	}
	timer.mu.Lock()
	active := timer.active
	timer.mu.Unlock()
	if active {
		t.Fatal("rejected recovered expiry left its timer active")
	}
}

func TestExpirySchedulingRetainsOneCandidatePerLiveExpiringKey(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	expires := now.Add(10 * 365 * 24 * time.Hour)
	for revision := range 1_000 {
		if _, err := state.Put(t.Context(), "lease", []byte{byte(revision)}, &expires, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
		if len(state.expiries) != 1 || len(state.expiryByKey) != 1 {
			t.Fatalf("replacement %d retained heap=%d index=%d expiry candidates", revision, len(state.expiries), len(state.expiryByKey))
		}
	}
	if _, err := state.Put(t.Context(), "lease", nil, nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if len(state.expiries) != 0 || len(state.expiryByKey) != 0 {
		t.Fatalf("non-expiring replacement retained heap=%d index=%d candidates", len(state.expiries), len(state.expiryByKey))
	}
	for _, key := range []string{"lease", "other"} {
		if _, err := state.Put(t.Context(), key, nil, &expires, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	record, err := state.Get("lease")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.DeleteIfRevision(t.Context(), "lease", record.Revision, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if len(state.expiries) != 1 || len(state.expiryByKey) != 1 || state.expiries[0].key != "other" {
		t.Fatalf("delete retained stale expiry candidates: heap=%+v index=%+v", state.expiries, state.expiryByKey)
	}
	for cycle := range 100 {
		if _, err := state.Put(t.Context(), "temporary", []byte{byte(cycle)}, &expires, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
		temporary, err := state.Get("temporary")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.DeleteIfRevision(t.Context(), "temporary", temporary.Revision, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
		if len(state.expiries) != 1 || len(state.expiryByKey) != 1 {
			t.Fatalf("delete cycle %d retained heap=%d index=%d candidates", cycle, len(state.expiries), len(state.expiryByKey))
		}
	}
}

func TestExpiryReplacementMovesDeadlineWithoutStaleGenerationCleanup(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	firstExpiry := now.Add(time.Minute)
	secondExpiry := now.Add(2 * time.Minute)
	if _, err := state.Put(t.Context(), "lease", []byte("old"), &firstExpiry, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	replacement, err := state.Put(t.Context(), "lease", []byte("new"), &secondExpiry, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	manual.Set(firstExpiry)
	result, err := state.ProcessDueExpiries(t.Context())
	if err != nil || result.Revision != 0 {
		t.Fatalf("old generation cleanup = %+v, %v", result, err)
	}
	record, err := state.Get("lease")
	if err != nil || record.Revision != replacement.Revision || string(record.Value) != "new" {
		t.Fatalf("replacement at old deadline = %+v, %v", record, err)
	}
	manual.Set(secondExpiry)
	result, err = state.ProcessDueExpiries(t.Context())
	if err != nil || len(result.Events) != 1 || result.Events[0].Revision != result.Revision || result.Events[0].Kind != gapdb.ChangeExpire {
		t.Fatalf("replacement cleanup = %+v, %v", result, err)
	}
}
