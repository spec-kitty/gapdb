package engine

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/clock"
)

func TestSingleKeyConditionsAndFailedConditionsConsumeNothing(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	expiredAt := now
	log := &fakeLog{}
	allocator := &countingAllocator{next: 20}
	state := newTestState(t, testConfig{
		clock:     manual,
		log:       log,
		allocator: allocator,
		records: []gapdb.Record{
			gapdb.NewRecord("live", []byte("v"), 4, nil),
			gapdb.NewRecord("expired", []byte("old"), 5, &expiredAt),
		},
		current: 5,
	})
	t.Cleanup(func() { closeState(t, state) })

	if _, err := state.PutIfAbsent(t.Context(), "live", []byte("x"), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAlreadyExists}) {
		t.Fatalf("PutIfAbsent live error = %v", err)
	}
	if _, err := state.CompareAndSwap(t.Context(), "missing", 4, []byte("x"), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("CAS missing error = %v", err)
	}
	if _, err := state.CompareAndSwap(t.Context(), "expired", 5, []byte("x"), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("CAS expired error = %v", err)
	}
	if _, err := state.CompareAndSwap(t.Context(), "live", 3, []byte("x"), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionMismatch}) {
		t.Fatalf("CAS mismatch error = %v", err)
	}
	if _, err := state.DeleteIfRevision(t.Context(), "live", 3, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionMismatch}) {
		t.Fatalf("delete mismatch error = %v", err)
	}
	if _, err := state.DeleteIfRevision(t.Context(), "missing", 3, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("delete missing error = %v", err)
	}
	if _, err := state.DeleteIfRevision(t.Context(), "expired", 5, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("delete expired error = %v", err)
	}
	if allocator.calls != 0 || len(log.frames) != 0 || state.CurrentRevision() != 5 {
		t.Fatalf("failed conditions mutated: allocations=%d frames=%d current=%d", allocator.calls, len(log.frames), state.CurrentRevision())
	}

	result, err := state.PutIfAbsent(t.Context(), "expired", []byte("new"), nil, gapdb.AckMemory)
	if err != nil || result.Revision != 20 {
		t.Fatalf("PutIfAbsent expired = %+v, %v", result, err)
	}
	if _, err := state.Put(t.Context(), "live", []byte("v"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if state.CurrentRevision() != 21 {
		t.Fatalf("identical Put did not advance revision: %d", state.CurrentRevision())
	}
	if _, err := state.DeleteIfRevision(t.Context(), "live", 21, gapdb.AckMemory); err != nil {
		t.Fatalf("delete live: %v", err)
	}
	if _, err := state.Get("live"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("deleted key remains live: %v", err)
	}
}

func TestAtomicBatchUsesOnePreBatchViewAndPublishesOneRevision(t *testing.T) {
	state := newTestState(t, testConfig{records: []gapdb.Record{gapdb.NewRecord("a", []byte("old"), 2, nil)}, current: 2})
	t.Cleanup(func() { closeState(t, state) })
	batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("a", []byte("new"), gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}, nil),
		gapdb.NewPutMutation("b", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
	}}
	result, err := state.AtomicBatch(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 3 || result.MutationCount != 2 || len(result.Events) != 2 {
		t.Fatalf("batch result = %+v", result)
	}
	for index, event := range result.Events {
		if event.Revision != 3 || event.Order != uint32(index) {
			t.Fatalf("event %d = %+v", index, event)
		}
	}
	for _, key := range []string{"a", "b"} {
		record, err := state.Get(key)
		if err != nil || record.Revision != 3 {
			t.Fatalf("Get(%q) = %+v, %v", key, record, err)
		}
	}
}

func TestAtomicBatchFailureRollsBackAndIdentifiesMutation(t *testing.T) {
	log := &fakeLog{}
	allocator := &countingAllocator{next: 4}
	state := newTestState(t, testConfig{log: log, allocator: allocator, records: []gapdb.Record{gapdb.NewRecord("a", []byte("old"), 3, nil)}, current: 3})
	t.Cleanup(func() { closeState(t, state) })
	batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("a", []byte("new"), gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 3}, nil),
		gapdb.NewPutMutation("b", []byte("bad"), gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 3}, nil),
	}}
	_, err := state.AtomicBatch(t.Context(), batch)
	var apiErr *gapdb.Error
	if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeConditionFailed || apiErr.MutationIndex == nil || *apiErr.MutationIndex != 1 || apiErr.Key != "b" {
		t.Fatalf("batch condition error = %#v", err)
	}
	if allocator.calls != 0 || len(log.frames) != 0 || state.CurrentRevision() != 3 {
		t.Fatalf("failed batch mutated engine")
	}
	record, _ := state.Get("a")
	if string(record.Value) != "old" {
		t.Fatalf("failed batch partially applied: %q", record.Value)
	}
}

func TestAtomicBatchRejectsDuplicateAndEncodedSizeBeforeAllocation(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.MaxBatchBytes = 53
	limits.MaxFrameBytes = 53
	limits.MaxScanBytes = 53
	allocator := &countingAllocator{next: 1}
	state := newTestState(t, testConfig{limits: limits, allocator: allocator})
	t.Cleanup(func() { closeState(t, state) })
	duplicate := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("k", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		gapdb.NewPutMutation("k", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
	}}
	if _, err := state.AtomicBatch(t.Context(), duplicate); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeDuplicateKey}) {
		t.Fatalf("duplicate error = %v", err)
	}
	exact := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{gapdb.NewPutMutation("k", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil)}}
	if _, err := state.AtomicBatch(t.Context(), exact); err != nil {
		t.Fatalf("exact encoded-size boundary: %v", err)
	}
	tooLarge := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{gapdb.NewPutMutation("kk", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil)}}
	if _, err := state.AtomicBatch(t.Context(), tooLarge); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBatchTooLarge}) {
		t.Fatalf("encoded size error = %v", err)
	}
	if allocator.calls != 1 {
		t.Fatalf("static failures changed allocation count: %d", allocator.calls)
	}
}

func TestUnrepresentableExpiryFailsBeforeAllocation(t *testing.T) {
	allocator := &countingAllocator{next: 1}
	state := newTestState(t, testConfig{allocator: allocator})
	t.Cleanup(func() { closeState(t, state) })
	expires := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := state.Put(t.Context(), "key", nil, &expires, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("unrepresentable expiry error = %v", err)
	}
	if allocator.calls != 0 {
		t.Fatalf("unrepresentable expiry allocated a revision")
	}
}

func TestConditionalRacesHaveExactlyOneWinner(t *testing.T) {
	state := newTestState(t, testConfig{queue: 64})
	t.Cleanup(func() { closeState(t, state) })
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := state.PutIfAbsent(t.Context(), "winner", []byte("v"), nil, gapdb.AckMemory); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAlreadyExists}) {
				t.Errorf("PutIfAbsent error = %v", err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("PutIfAbsent winners = %d", winners.Load())
	}
	record, _ := state.Get("winner")
	winners.Store(0)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if _, err := state.CompareAndSwap(t.Context(), "winner", record.Revision, []byte{byte(index)}, nil, gapdb.AckMemory); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionMismatch}) {
				t.Errorf("CAS error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("CAS winners = %d", winners.Load())
	}
}

func TestBatchPublicationHasNoPartialMapVisibility(t *testing.T) {
	state := newTestState(t, testConfig{
		records: []gapdb.Record{
			gapdb.NewRecord("a", []byte{0}, 1, nil),
			gapdb.NewRecord("b", []byte{0}, 1, nil),
		},
		current: 1,
	})
	t.Cleanup(func() { closeState(t, state) })
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			state.mu.RLock()
			a, b := state.records["a"], state.records["b"]
			if a.Value[0] != b.Value[0] || a.Revision != b.Revision {
				t.Errorf("partial batch view: a=%+v b=%+v", a, b)
				state.mu.RUnlock()
				return
			}
			state.mu.RUnlock()
		}
	}()
	for index := 0; index < 200; index++ {
		value := []byte{byte(index & 1)}
		batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("a", value, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
			gapdb.NewPutMutation("b", value, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		}}
		if _, err := state.AtomicBatch(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	<-readerDone
}
