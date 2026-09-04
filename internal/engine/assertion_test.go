package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

type assertionFailureFingerprint struct {
	records             string
	current             gapdb.Revision
	durable             gapdb.Revision
	lifecycle           gapdb.LifecycleState
	snapshotRevision    gapdb.Revision
	reservedRevisionEnd gapdb.Revision
	activeWALStart      gapdb.Revision
	expiries            []string
	expiryIndex         []string
	expiryKeys          int
	historyCommits      int
	historyEvents       int
	historyBytes        int
	historyWatermark    gapdb.Revision
	watchers            int
	activeWatches       int64
	allocatorCalls      int
	logFrames           string
	logBarriers         int
}

func captureAssertionFailureFingerprint(t *testing.T, state *DatabaseState, allocator *countingAllocator, log *fakeLog) assertionFailureFingerprint {
	t.Helper()
	fingerprint := assertionFailureFingerprint{}
	state.mu.RLock()
	records, err := json.Marshal(state.records)
	if err != nil {
		state.mu.RUnlock()
		t.Fatal(err)
	}
	fingerprint.records = string(records)
	fingerprint.current = state.current
	fingerprint.durable = state.durableThrough
	fingerprint.lifecycle = state.lifecycle
	fingerprint.snapshotRevision = state.snapshotRevision
	fingerprint.reservedRevisionEnd = state.reservedRevisionEnd
	fingerprint.activeWALStart = state.activeWALStart
	fingerprint.expiryKeys = len(state.expiryByKey)
	for _, candidate := range state.expiries {
		fingerprint.expiries = append(fingerprint.expiries, fmt.Sprintf("%s/%d/%s", candidate.key, candidate.revision, candidate.at.UTC().Format(time.RFC3339Nano)))
	}
	for key, candidate := range state.expiryByKey {
		fingerprint.expiryIndex = append(fingerprint.expiryIndex, fmt.Sprintf("%s/%s/%d/%s/%d", key, candidate.key, candidate.revision, candidate.at.UTC().Format(time.RFC3339Nano), candidate.index))
	}
	fingerprint.watchers = len(state.watchers)
	state.mu.RUnlock()
	fingerprint.activeWatches = state.activeWatches.Load()

	state.history.mu.RLock()
	fingerprint.historyCommits = len(state.history.commits)
	fingerprint.historyEvents = state.history.eventCount
	fingerprint.historyBytes = state.history.byteCount
	fingerprint.historyWatermark = state.history.compactedThrough
	state.history.mu.RUnlock()

	allocator.mu.Lock()
	fingerprint.allocatorCalls = allocator.calls
	allocator.mu.Unlock()
	log.mu.Lock()
	frames, err := json.Marshal(log.frames)
	if err != nil {
		log.mu.Unlock()
		t.Fatal(err)
	}
	fingerprint.logFrames = string(frames)
	fingerprint.logBarriers = log.barriers
	log.mu.Unlock()
	sort.Strings(fingerprint.expiries)
	sort.Strings(fingerprint.expiryIndex)
	return fingerprint
}

func TestAssertionFailureIsFirstOrderedPredicateAndHasZeroEffects(t *testing.T) {
	now := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	log := &fakeLog{durable: 4}
	allocator := &countingAllocator{next: 5}
	state := newTestState(t, testConfig{
		clock: clockAt(now), log: log, allocator: allocator, current: 4, durable: 4,
		records: []gapdb.Record{
			gapdb.NewRecord("guard/pass", []byte("authority-secret"), 2, nil),
			gapdb.NewRecord("guard/fail", []byte("other-secret"), 4, &expires),
			gapdb.NewRecord("mutation/fail", []byte("mutation-secret"), 3, nil),
		},
	})
	t.Cleanup(func() { closeState(t, state) })
	watch, err := state.Watch(t.Context(), "", state.CurrentRevision())
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	before := captureAssertionFailureFingerprint(t, state, allocator, log)

	batch := gapdb.Batch{
		Ack: gapdb.AckDurable,
		Assertions: []gapdb.Assertion{
			{Key: "guard/pass", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}},
			{Key: "guard/fail", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 3}},
		},
		Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("mutation/fail", []byte("changed"), gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}, nil),
		},
	}
	_, err = state.AtomicBatch(t.Context(), batch)
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.Code != gapdb.CodeConditionFailed || failure.AssertionIndex == nil || *failure.AssertionIndex != 1 || failure.MutationIndex != nil || failure.Key != "guard/fail" || failure.Condition != string(gapdb.ConditionRevision) || failure.ExpectedRevision == nil || *failure.ExpectedRevision != 3 || failure.ActualRevision == nil || *failure.ActualRevision != 4 || failure.OperationApplied {
		t.Fatalf("assertion failure = %#v", err)
	}
	after := captureAssertionFailureFingerprint(t, state, allocator, log)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed assertion changed state:\nbefore=%+v\nafter=%+v", before, after)
	}
	select {
	case event := <-watch.Events:
		t.Fatalf("failed assertion emitted watch event: %+v", event)
	default:
	}

	before = captureAssertionFailureFingerprint(t, state, allocator, log)
	_, err = state.AtomicBatch(t.Context(), gapdb.Batch{
		Ack:        gapdb.AckDurable,
		Assertions: []gapdb.Assertion{{Key: "guard/pass", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("mutation/fail", []byte("still-not-written"), gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}, nil)},
	})
	if !errors.As(err, &failure) || failure.AssertionIndex != nil || failure.MutationIndex == nil || *failure.MutationIndex != 0 || failure.OperationApplied {
		t.Fatalf("mutation predicate failure = %#v", err)
	}
	after = captureAssertionFailureFingerprint(t, state, allocator, log)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed mutation predicate changed state:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestSimultaneouslyFailingAssertionsHonorRequestOrderAndKillReverseIteration(t *testing.T) {
	log := &fakeLog{durable: 4}
	allocator := &countingAllocator{next: 5}
	state := newTestState(t, testConfig{
		log: log, allocator: allocator, current: 4, durable: 4,
		records: []gapdb.Record{
			gapdb.NewRecord("guard/first", []byte("first-secret"), 2, nil),
			gapdb.NewRecord("guard/second", []byte("second-secret"), 4, nil),
		},
	})
	t.Cleanup(func() { closeState(t, state) })
	first := gapdb.Assertion{Key: "guard/first", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}}
	second := gapdb.Assertion{Key: "guard/second", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 3}}
	for _, test := range []struct {
		name         string
		assertions   []gapdb.Assertion
		wantKey      string
		wantExpected gapdb.Revision
		wantActual   gapdb.Revision
	}{
		{"first_then_second", []gapdb.Assertion{first, second}, "guard/first", 1, 2},
		{"second_then_first", []gapdb.Assertion{second, first}, "guard/second", 3, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := state.AtomicBatch(t.Context(), gapdb.Batch{
				Ack:        gapdb.AckDurable,
				Assertions: test.assertions,
				Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("candidate", []byte("must-not-write"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
			})
			var failure *gapdb.Error
			if !errors.As(err, &failure) || failure.Code != gapdb.CodeConditionFailed || failure.AssertionIndex == nil || *failure.AssertionIndex != 0 || failure.Key != test.wantKey || failure.Condition != string(gapdb.ConditionRevision) || failure.ExpectedRevision == nil || *failure.ExpectedRevision != test.wantExpected || failure.ActualRevision == nil || *failure.ActualRevision != test.wantActual || failure.ActualState != "" || failure.MutationIndex != nil || failure.OperationApplied {
				t.Fatalf("ordered assertion failure = %#v", err)
			}
		})
	}
	if allocator.calls != 0 || len(log.frames) != 0 || log.barriers != 0 {
		t.Fatalf("ordered assertion refusals changed persistence: allocations=%d frames=%d barriers=%d", allocator.calls, len(log.frames), log.barriers)
	}
}

func TestExpiredPhysicalAssertionStateIsNeverLazilyCleaned(t *testing.T) {
	now := time.Date(2026, 9, 4, 8, 30, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Minute)
	log := &fakeLog{durable: 3}
	allocator := &countingAllocator{next: 4}
	state := newTestState(t, testConfig{
		clock: clockAt(now), log: log, allocator: allocator, current: 3, durable: 3,
		records: []gapdb.Record{
			gapdb.NewRecord("guard/expired", []byte("expired-secret"), 2, &expiredAt),
			gapdb.NewRecord("guard/fails-later", []byte("live-secret"), 3, nil),
		},
	})
	t.Cleanup(func() { closeState(t, state) })
	before := captureAssertionFailureFingerprint(t, state, allocator, log)
	physicalBefore := capturePhysicalExpiryEvidence(t, state, "guard/expired")

	_, err := state.AtomicBatch(t.Context(), gapdb.Batch{
		Ack: gapdb.AckDurable,
		Assertions: []gapdb.Assertion{
			{Key: "guard/expired", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}},
			{Key: "guard/fails-later", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}},
		},
		Mutations: []gapdb.Mutation{gapdb.NewPutMutation("candidate", []byte("must-not-write"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	})
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.AssertionIndex == nil || *failure.AssertionIndex != 1 || failure.Key != "guard/fails-later" {
		t.Fatalf("later assertion failure = %#v", err)
	}
	after := captureAssertionFailureFingerprint(t, state, allocator, log)
	physicalAfter := capturePhysicalExpiryEvidence(t, state, "guard/expired")
	if !reflect.DeepEqual(after, before) || !physicalExpiryEvidenceEqual(physicalAfter, physicalBefore) {
		t.Fatalf("expired asserted record was changed:\nbefore=%+v\nafter=%+v\nphysical before=%+v\nphysical after=%+v", before, after, physicalBefore, physicalAfter)
	}

	// Controlled lazy-cleanup mutants prove that each physical-state component
	// above is independently observed by the permanent failure fingerprint. Stop
	// the writer before applying test-only corruptions to its owned structures.
	closeState(t, state)
	mutantBaseline := captureAssertionFailureFingerprint(t, state, allocator, log)
	state.mu.Lock()
	candidate := state.expiryByKey["guard/expired"]
	delete(state.expiryByKey, "guard/expired")
	state.mu.Unlock()
	if reflect.DeepEqual(captureAssertionFailureFingerprint(t, state, allocator, log), mutantBaseline) {
		t.Fatal("expiry-by-key deletion mutant escaped the fingerprint")
	}
	state.mu.Lock()
	state.expiryByKey["guard/expired"] = candidate
	originalHeap := state.expiries
	state.expiries = nil
	state.mu.Unlock()
	if reflect.DeepEqual(captureAssertionFailureFingerprint(t, state, allocator, log), mutantBaseline) {
		t.Fatal("expiry-heap deletion mutant escaped the fingerprint")
	}
	state.mu.Lock()
	state.expiries = originalHeap
	record := state.records["guard/expired"]
	delete(state.records, "guard/expired")
	state.mu.Unlock()
	if reflect.DeepEqual(captureAssertionFailureFingerprint(t, state, allocator, log), mutantBaseline) {
		t.Fatal("physical-record deletion mutant escaped the fingerprint")
	}
	state.mu.Lock()
	state.records["guard/expired"] = record
	state.mu.Unlock()
}

type physicalExpiryEvidence struct {
	record          gapdb.Record
	candidate       *expiryCandidate
	candidateValue  expiryCandidate
	heapCandidate   *expiryCandidate
	heapLength      int
	expiryIndexSize int
}

func capturePhysicalExpiryEvidence(t *testing.T, state *DatabaseState, key string) physicalExpiryEvidence {
	t.Helper()
	state.mu.RLock()
	defer state.mu.RUnlock()
	record, exists := state.records[key]
	if !exists {
		t.Fatalf("physical record %q is missing", key)
	}
	candidate, exists := state.expiryByKey[key]
	if !exists || candidate.index < 0 || candidate.index >= len(state.expiries) {
		t.Fatalf("expiry candidate %q is missing or invalid: %+v", key, candidate)
	}
	return physicalExpiryEvidence{
		record:          record.Clone(),
		candidate:       candidate,
		candidateValue:  *candidate,
		heapCandidate:   state.expiries[candidate.index],
		heapLength:      len(state.expiries),
		expiryIndexSize: len(state.expiryByKey),
	}
}

func physicalExpiryEvidenceEqual(left, right physicalExpiryEvidence) bool {
	return reflect.DeepEqual(left.record, right.record) && left.candidate == right.candidate && left.candidateValue == right.candidateValue && left.heapCandidate == right.heapCandidate && left.heapLength == right.heapLength && left.expiryIndexSize == right.expiryIndexSize
}

func TestAssertionFailureEvidenceDistinguishesAbsentAndPresent(t *testing.T) {
	state := newTestState(t, testConfig{records: []gapdb.Record{gapdb.NewRecord("present", []byte("secret"), 2, nil)}, current: 2})
	t.Cleanup(func() { closeState(t, state) })
	for _, test := range []struct {
		name      string
		assertion gapdb.Assertion
		check     func(*gapdb.Error) bool
	}{
		{"revision missing", gapdb.Assertion{Key: "missing", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}}, func(err *gapdb.Error) bool {
			return err.ExpectedRevision != nil && *err.ExpectedRevision == 1 && err.ActualRevision == nil && err.ActualState == "absent"
		}},
		{"absence present", gapdb.Assertion{Key: "present", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}, func(err *gapdb.Error) bool {
			return err.ExpectedRevision == nil && err.ActualRevision != nil && *err.ActualRevision == 2 && err.ActualState == ""
		}},
		{"maximum key remains bounded", gapdb.Assertion{Key: strings.Repeat("k", gapdb.DefaultOptions().Limits.MaxKeyBytes), Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}}, func(err *gapdb.Error) bool {
			return len(err.Key) == 256 && err.ExpectedRevision != nil && *err.ExpectedRevision == 1 && err.ActualRevision == nil && err.ActualState == "absent"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := state.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory, Assertions: []gapdb.Assertion{test.assertion}, Mutations: []gapdb.Mutation{gapdb.NewPutMutation("new", []byte("value-secret"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)}})
			var failure *gapdb.Error
			if !errors.As(err, &failure) || failure.AssertionIndex == nil || *failure.AssertionIndex != 0 || failure.MutationIndex != nil || !test.check(failure) {
				t.Fatalf("failure = %#v", err)
			}
			encoded, marshalErr := json.Marshal(failure)
			if marshalErr != nil || len(encoded) >= 4096 || string(encoded) == "" || containsBytes(encoded, []byte("value-secret")) || containsBytes(encoded, []byte("secret")) {
				t.Fatalf("unsafe failure length=%d body=%s err=%v", len(encoded), encoded, marshalErr)
			}
		})
	}
}

func TestSuccessfulAssertionsWriteOnlyMutationsAndReportExactCounts(t *testing.T) {
	expires := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	log := &fakeLog{}
	state := newTestState(t, testConfig{log: log, records: []gapdb.Record{
		gapdb.NewRecord("guard", []byte("unchanged"), 2, &expires),
		gapdb.NewRecord("update", []byte("old"), 2, nil),
	}, current: 2})
	t.Cleanup(func() { closeState(t, state) })
	physicalBefore := capturePhysicalExpiryEvidence(t, state, "guard")
	watch, err := state.Watch(t.Context(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	batch := gapdb.Batch{Ack: gapdb.AckMemory,
		Assertions: []gapdb.Assertion{
			{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}},
			{Key: "vacant", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}},
		},
		Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("update", []byte("new"), gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 2}, nil),
			gapdb.NewPutMutation("created", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		},
	}
	result, err := state.AtomicBatch(t.Context(), batch)
	if err != nil || result.Revision != 3 || result.MutationCount != 2 || result.AssertionCount != 2 || len(result.Events) != 2 {
		t.Fatalf("AtomicBatch() = %+v, %v", result, err)
	}
	guard, err := state.Get("guard")
	if err != nil || guard.Revision != 2 || string(guard.Value) != "unchanged" {
		t.Fatalf("asserted record changed: %+v, %v", guard, err)
	}
	physicalAfter := capturePhysicalExpiryEvidence(t, state, "guard")
	if !physicalExpiryEvidenceEqual(physicalAfter, physicalBefore) {
		t.Fatalf("successful assertion changed expiring physical state:\nbefore=%+v\nafter=%+v", physicalBefore, physicalAfter)
	}
	if _, err := state.Get("vacant"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("absence assertion wrote a record: %v", err)
	}
	log.mu.Lock()
	if len(log.frames) != 1 || len(log.frames[0].Effects) != 2 || log.frames[0].Effects[0].Key != "update" || log.frames[0].Effects[1].Key != "created" {
		t.Fatalf("persisted frame = %+v", log.frames)
	}
	log.mu.Unlock()
	if state.history.eventCount != 2 || len(state.history.commits) != 1 {
		t.Fatalf("history commits=%d events=%d", len(state.history.commits), state.history.eventCount)
	}
	for index, key := range []string{"update", "created"} {
		event := receiveEvent(t, watch)
		if event.Key != key || event.Revision != 3 || event.Order != uint32(index) {
			t.Fatalf("watch event %d = %+v", index, event)
		}
	}
}

type sequenceClock struct {
	mu    sync.Mutex
	times []time.Time
	calls int
}

func (clock *sequenceClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	index := clock.calls
	clock.calls++
	if index >= len(clock.times) {
		return clock.times[len(clock.times)-1].Add(time.Duration(index-len(clock.times)+1) * time.Nanosecond)
	}
	return clock.times[index]
}

func TestAssertionsAndMutationConditionsShareOneWriterTimeAtExpiryBoundary(t *testing.T) {
	boundary := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	expires := boundary.Add(time.Nanosecond)
	scripted := &sequenceClock{times: []time.Time{boundary.Add(-2 * time.Nanosecond), boundary.Add(-time.Nanosecond), boundary, expires}}
	state := newTestState(t, testConfig{
		clock: scripted,
		records: []gapdb.Record{
			gapdb.NewRecord("guard", []byte("g"), 1, &expires),
			gapdb.NewRecord("update", []byte("u"), 1, &expires),
		},
		current: 1,
	})
	t.Cleanup(func() { closeState(t, state) })
	result, err := state.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("update", []byte("changed"), gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}, nil)},
	})
	if err != nil || result.Revision != 2 || result.AssertionCount != 1 {
		t.Fatalf("boundary batch = %+v, %v", result, err)
	}
	scripted.mu.Lock()
	calls := scripted.calls
	scripted.mu.Unlock()
	if calls < 3 {
		t.Fatalf("clock calls = %d, want initialization, preflight, and writer", calls)
	}
}

func TestAssertionAndMutationConditionUseExactExpiryBoundary(t *testing.T) {
	boundary := time.Date(2026, 9, 4, 9, 30, 0, 0, time.UTC)
	scripted := &sequenceClock{times: []time.Time{boundary.Add(-2 * time.Nanosecond), boundary.Add(-time.Nanosecond), boundary}}
	state := newTestState(t, testConfig{
		clock: scripted,
		records: []gapdb.Record{
			gapdb.NewRecord("guard", []byte("expired"), 1, &boundary),
			gapdb.NewRecord("update", []byte("expired"), 1, &boundary),
		},
		current: 1,
	})
	t.Cleanup(func() { closeState(t, state) })
	result, err := state.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("update", []byte("replaced"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	})
	if err != nil || result.Revision != 2 || result.AssertionCount != 1 || result.MutationCount != 1 {
		t.Fatalf("expiry-boundary batch = %+v, %v", result, err)
	}
}

func TestQueuedAssertionSeesEarlierSerializedCommitAndKillsPreflightMutant(t *testing.T) {
	runQueuedStaleAssertionTrial(t, 0, false)
}

func TestQueuedAbsenceAssertionSeesEarlierSerializedCommit(t *testing.T) {
	runQueuedStaleAssertionTrial(t, 0, true)
}

func TestQueuedStaleAssertionContentionQualification(t *testing.T) {
	for trial := 0; trial < 1000; trial++ {
		runQueuedStaleAssertionTrial(t, trial, false)
		runQueuedStaleAssertionTrial(t, trial, true)
	}
}

func runQueuedStaleAssertionTrial(t *testing.T, trial int, assertAbsent bool) {
	t.Helper()
	unblock := make(chan struct{})
	appended := make(chan struct{}, 1)
	log := &fakeLog{appendBlock: unblock, appended: appended}
	allocator := &countingAllocator{next: 2}
	records := []gapdb.Record{gapdb.NewRecord("baseline", []byte("old"), 1, nil)}
	assertion := gapdb.Assertion{Key: "authority", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}
	if !assertAbsent {
		records = append(records, gapdb.NewRecord("authority", []byte("old"), 1, nil))
		assertion.Condition = gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}
	}
	state := newTestState(t, testConfig{log: log, allocator: allocator, records: records, current: 1, queue: 4})
	defer closeState(t, state)
	first := make(chan error, 1)
	go func() {
		_, err := state.Put(t.Context(), "authority", []byte("new"), nil, gapdb.AckMemory)
		first <- err
	}()
	<-appended
	second := make(chan error, 1)
	go func() {
		_, err := state.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory,
			Assertions: []gapdb.Assertion{assertion},
			Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("candidate", []byte("must-not-write"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
		})
		second <- err
	}()
	for len(state.queue) != 1 {
		time.Sleep(time.Microsecond)
	}
	close(unblock)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	err := <-second
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.AssertionIndex == nil || failure.ActualRevision == nil || *failure.ActualRevision != 2 {
		t.Fatalf("trial %d queued stale assertion = %#v", trial, err)
	}
	if allocator.calls != 1 || len(log.frames) != 1 {
		t.Fatalf("trial %d stale batch allocated or persisted: allocations=%d frames=%d", trial, allocator.calls, len(log.frames))
	}
	if _, err := state.Get("candidate"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("trial %d stale batch wrote candidate: %v", trial, err)
	}
}

func TestPredicateBoundaryFaultsAreZeroEffectBeforeAllocation(t *testing.T) {
	injected := errors.New("predicate boundary fault with private provider detail")
	for _, test := range []struct {
		name  string
		point faultfs.Point
		phase faultfs.Phase
	}{
		{"before assertion evaluation", FaultPointAssertionEvaluation, faultfs.Before},
		{"after assertion evaluation", FaultPointAssertionEvaluation, faultfs.After},
		{"before mutation-condition evaluation", FaultPointMutationConditionEvaluation, faultfs.Before},
		{"after mutation-condition evaluation", FaultPointMutationConditionEvaluation, faultfs.After},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := &fakeLog{durable: 1}
			allocator := &countingAllocator{next: 2}
			state := newTestState(t, testConfig{
				log: log, allocator: allocator, current: 1, durable: 1,
				records: []gapdb.Record{gapdb.NewRecord("guard", []byte("unchanged"), 1, nil)},
			})
			t.Cleanup(func() { closeState(t, state) })
			state.faults = faultfs.NewOS(faultfs.NewInjector(41, faultfs.Rule{
				Point: test.point, Phase: test.phase, Occurrence: 1, Seed: 41, Err: injected,
			}))
			before := captureAssertionFailureFingerprint(t, state, allocator, log)

			_, err := state.AtomicBatch(t.Context(), gapdb.Batch{
				Ack:        gapdb.AckMemory,
				Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}}},
				Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("candidate", []byte("not-written"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
			})
			var failure *gapdb.Error
			if !errors.Is(err, injected) || !errors.As(err, &failure) || failure.Code != gapdb.CodeInternal || failure.OperationApplied {
				t.Fatalf("fault result = %#v", err)
			}
			after := captureAssertionFailureFingerprint(t, state, allocator, log)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("predicate fault changed state:\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}

func TestAssertionFaultBoundaryIsInactiveForAssertionFreeBatch(t *testing.T) {
	state := newTestState(t, testConfig{})
	t.Cleanup(func() { closeState(t, state) })
	state.faults = faultfs.NewOS(faultfs.NewInjector(42, faultfs.Rule{
		Point: FaultPointAssertionEvaluation, Phase: faultfs.Before, Occurrence: 1, Seed: 42,
		Err: errors.New("must remain dormant"),
	}))
	result, err := state.Put(t.Context(), "ordinary", []byte("value"), nil, gapdb.AckMemory)
	if err != nil || result.Revision != 1 || result.AssertionCount != 0 || result.MutationCount != 1 {
		t.Fatalf("assertion-free batch = %+v, %v", result, err)
	}
}

func TestAssertionBatchCancellationBeforeAndAfterAdmission(t *testing.T) {
	state := newTestState(t, testConfig{records: []gapdb.Record{gapdb.NewRecord("guard", []byte("old"), 1, nil)}, current: 1})
	t.Cleanup(func() { closeState(t, state) })
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := state.AtomicBatch(canceled, gapdb.Batch{
		Ack:        gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("precanceled", []byte("no"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled batch = %v", err)
	}
	if _, err := state.Get("precanceled"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("pre-canceled batch wrote: %v", err)
	}

	unblock := make(chan struct{})
	appended := make(chan struct{}, 1)
	blockingLog := &fakeLog{appendBlock: unblock, appended: appended}
	admitted := newTestState(t, testConfig{log: blockingLog, records: []gapdb.Record{gapdb.NewRecord("guard", []byte("old"), 1, nil)}, current: 1, queue: 4})
	t.Cleanup(func() { closeState(t, admitted) })
	first := make(chan error, 1)
	go func() {
		_, err := admitted.Put(t.Context(), "other", []byte("first"), nil, gapdb.AckMemory)
		first <- err
	}()
	<-appended
	ctx, cancelAdmitted := context.WithCancel(t.Context())
	second := make(chan commandResult, 1)
	go func() {
		result, err := admitted.AtomicBatch(ctx, gapdb.Batch{
			Ack:        gapdb.AckMemory,
			Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 1}}},
			Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("admitted", []byte("yes"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
		})
		second <- commandResult{result: result, err: err}
	}()
	for len(admitted.queue) != 1 {
		time.Sleep(time.Microsecond)
	}
	cancelAdmitted()
	close(unblock)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	completed := <-second
	if completed.err != nil || completed.result.AssertionCount != 1 || completed.result.MutationCount != 1 {
		t.Fatalf("admitted canceled batch = %+v, %v", completed.result, completed.err)
	}
}

type fixedClock time.Time

func clockAt(value time.Time) fixedClock { return fixedClock(value) }
func (clock fixedClock) Now() time.Time  { return time.Time(clock) }

func containsBytes(haystack, needle []byte) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if reflect.DeepEqual(haystack[index:index+len(needle)], needle) {
			return true
		}
	}
	return false
}
