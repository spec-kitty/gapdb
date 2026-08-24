package engine

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/clock"
	"gapdb/internal/persist"
)

func TestHistoryEvictsWholeCommitsAndOwnsValues(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.WatchBufferEvents = 1
	limits.MaxHistoryEvents = 3
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	firstValue := []byte("one")
	first := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("p/a", firstValue, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		gapdb.NewPutMutation("p/b", []byte("two"), gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
	}}
	firstResult, err := state.AtomicBatch(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	firstValue[0] = 'X'
	second := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("p/c", []byte("three"), gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		gapdb.NewPutMutation("p/d", []byte("four"), gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
	}}
	secondResult, err := state.AtomicBatch(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReplayHistory("p/", 0); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionCompacted}) {
		t.Fatalf("evicted cursor error = %v", err)
	}
	events, err := state.ReplayHistory("p/", firstResult.Revision)
	if err != nil || len(events) != 2 || events[0].Revision != secondResult.Revision || events[0].Order != 0 || events[1].Order != 1 {
		t.Fatalf("retained replay = %+v, %v", events, err)
	}
	events[0].Record.Value[0] = 'Y'
	again, _ := state.ReplayHistory("p/", firstResult.Revision)
	if string(again[0].Record.Value) != "three" {
		t.Fatalf("history output aliases retained value: %q", again[0].Record.Value)
	}
}

func TestHistoryRebuildsFromWALAboveSnapshotBoundary(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.WatchBufferEvents = 2
	limits.MaxHistoryEvents = 4
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	log := &fakeLog{}
	state, err := New(Config{
		Limits:           limits,
		Clock:            clock.NewManual(now),
		ExpiryTimer:      newManualExpiryTimer(),
		Log:              log,
		Allocator:        &countingAllocator{next: 8},
		CurrentRevision:  7,
		DatabaseID:       testDatabaseID(t, "00112233445566778899aabbccddeeff"),
		SnapshotRevision: 5,
		RecoveredCommits: []persist.CommitFrame{
			{Revision: 6, Effects: []persist.Effect{{Kind: gapdb.ChangePut, Key: "a", Value: []byte("v")}}},
			{Revision: 7, Effects: []persist.Effect{{Kind: gapdb.ChangeDelete, Key: "b"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.ReplayHistory("", 4); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionCompacted}) {
		t.Fatalf("pre-snapshot replay error = %v", err)
	}
	events, err := state.ReplayHistory("", 5)
	if err != nil || len(events) != 2 || events[0].Revision != 6 || events[1].Revision != 7 {
		t.Fatalf("rebuilt replay = %+v, %v", events, err)
	}
}

func TestHistoryByteLimitEvictsACompleteRevision(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.WatchBufferEvents = 1
	limits.MaxHistoryEvents = 10
	limits.MaxHistoryBytes = 60
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	first, err := state.Put(t.Context(), "a", []byte("1234567890"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Put(t.Context(), "b", []byte("abcdefghij"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReplayHistory("", 0); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionCompacted}) {
		t.Fatalf("byte-evicted cursor error = %v", err)
	}
	events, err := state.ReplayHistory("", first.Revision)
	if err != nil || len(events) != 1 || events[0].Key != "b" {
		t.Fatalf("byte-retained replay = %+v, %v", events, err)
	}
}

func TestHistoryFailsClosedOnIncompleteRecoveredWAL(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	state, err := New(Config{
		Limits:           limits,
		Clock:            clock.NewManual(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)),
		ExpiryTimer:      newManualExpiryTimer(),
		Log:              &fakeLog{},
		Allocator:        &countingAllocator{next: 8},
		CurrentRevision:  7,
		SnapshotRevision: 5,
		RecoveredCommits: []persist.CommitFrame{{Revision: 6, Effects: []persist.Effect{{Kind: gapdb.ChangePut, Key: "a"}}}},
	})
	if state != nil || err == nil {
		if state != nil {
			closeState(t, state)
		}
		t.Fatalf("New = %v, %v; want incomplete recovery rejection", state, err)
	}
}

func TestHistoryRecoveryRequiresValidSnapshotBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		snapshot  gapdb.Revision
		current   gapdb.Revision
		commits   []persist.CommitFrame
		wantError bool
	}{
		{name: "nil-empty-database", snapshot: 0, current: 0},
		{name: "empty-at-snapshot", snapshot: 5, current: 5, commits: []persist.CommitFrame{}},
		{name: "nil-post-snapshot-gap", snapshot: 5, current: 7, wantError: true},
		{name: "empty-post-snapshot-gap", snapshot: 5, current: 7, commits: []persist.CommitFrame{}, wantError: true},
		{name: "single-complete", snapshot: 5, current: 6, commits: []persist.CommitFrame{{Revision: 6}}},
		{name: "single-burned-start-gap", snapshot: 5, current: 7, commits: []persist.CommitFrame{{Revision: 7}}},
		{name: "first-at-snapshot", snapshot: 5, current: 7, commits: []persist.CommitFrame{{Revision: 5}, {Revision: 7}}, wantError: true},
		{name: "first-below-snapshot", snapshot: 5, current: 7, commits: []persist.CommitFrame{{Revision: 4}, {Revision: 7}}, wantError: true},
		{name: "multi-complete", snapshot: 5, current: 8, commits: []persist.CommitFrame{{Revision: 6}, {Revision: 7}, {Revision: 8}}},
		{name: "multi-burned-middle-gap", snapshot: 5, current: 8, commits: []persist.CommitFrame{{Revision: 6}, {Revision: 8}}},
		{name: "duplicate", snapshot: 5, current: 8, commits: []persist.CommitFrame{{Revision: 6}, {Revision: 6}, {Revision: 8}}, wantError: true},
		{name: "regression", snapshot: 5, current: 8, commits: []persist.CommitFrame{{Revision: 7}, {Revision: 6}, {Revision: 8}}, wantError: true},
		{name: "multi-tail-gap", snapshot: 5, current: 8, commits: []persist.CommitFrame{{Revision: 6}, {Revision: 7}}, wantError: true},
		{name: "current-overshoot", snapshot: 5, current: 8, commits: []persist.CommitFrame{{Revision: 6}, {Revision: 9}}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := New(Config{
				Limits:           gapdb.DefaultOptions().Limits,
				Clock:            clock.NewManual(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)),
				ExpiryTimer:      newManualExpiryTimer(),
				Log:              &fakeLog{},
				Allocator:        &countingAllocator{next: test.current + 1},
				CurrentRevision:  test.current,
				SnapshotRevision: test.snapshot,
				RecoveredCommits: test.commits,
			})
			if test.wantError {
				if state != nil || err == nil {
					if state != nil {
						closeState(t, state)
					}
					t.Fatalf("New = %v, %v; want recovery coverage error", state, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			closeState(t, state)
		})
	}
}

func TestReviewerWP04RecoveryAcceptsDurablyBurnedRevisionGaps(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	id := testDatabaseID(t, "00112233445566778899aabbccddeeff")
	header, err := persist.EncodeWALHeader(persist.WALHeader{DatabaseID: id, FirstRevision: 2, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	frame2, err := persist.EncodeCommitFrame(persist.CommitFrame{Revision: 2, Effects: []persist.Effect{{Kind: gapdb.ChangePut, Key: "a", Value: []byte("two")}}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	frame4, err := persist.EncodeCommitFrame(persist.CommitFrame{Revision: 4, Effects: []persist.Effect{{Kind: gapdb.ChangePut, Key: "b", Value: []byte("four")}}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	wal := append(append(bytes.Clone(header), frame2...), frame4...)
	replay, err := persist.ScanWAL(bytes.NewReader(wal), int64(len(wal)), id, 1, 4, limits)
	if err != nil || replay.RecoveredRevision != 4 || len(replay.Commits) != 2 {
		t.Fatalf("ScanWAL = %+v, %v", replay, err)
	}
	state, err := New(Config{
		Limits:           limits,
		Clock:            clock.NewManual(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)),
		ExpiryTimer:      newManualExpiryTimer(),
		Log:              &fakeLog{},
		Allocator:        &countingAllocator{next: 5},
		CurrentRevision:  replay.RecoveredRevision,
		SnapshotRevision: 1,
		RecoveredCommits: replay.Commits,
	})
	if err != nil {
		t.Fatalf("New from production WAL replay = %v", err)
	}
	t.Cleanup(func() { closeState(t, state) })
	events, err := state.ReplayHistory("", 1)
	if err != nil || len(events) != 2 || events[0].Revision != 2 || events[1].Revision != 4 {
		t.Fatalf("replayed history = %+v, %v", events, err)
	}
	events, err = state.ReplayHistory("", 3)
	if err != nil || len(events) != 1 || events[0].Revision != 4 {
		t.Fatalf("history after burned revision = %+v, %v", events, err)
	}
}
