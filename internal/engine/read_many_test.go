package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/clock"
)

func TestReadManyReturnsOneOrderedEntryFromOneRevision(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	expires := now.Add(time.Minute)
	first, err := state.Put(t.Context(), "first", []byte("one"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Put(t.Context(), "expired", []byte("old"), &expires, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	manual.Set(expires)

	result, err := state.ReadMany([]string{"missing", "first", "expired"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObservedRevision != state.CurrentRevision() || !result.AsOf.Equal(expires) || len(result.Entries) != 3 {
		t.Fatalf("ReadMany() = %+v", result)
	}
	if result.Entries[0].Key != "missing" || result.Entries[0].Found || result.Entries[0].Record != nil {
		t.Fatalf("missing entry = %+v", result.Entries[0])
	}
	if entry := result.Entries[1]; !entry.Found || entry.Record == nil || entry.Record.Key != "first" || entry.Record.Revision != first.Revision || string(entry.Record.Value) != "one" {
		t.Fatalf("found entry = %+v", entry)
	}
	if result.Entries[2].Key != "expired" || result.Entries[2].Found || result.Entries[2].Record != nil {
		t.Fatalf("expired entry = %+v", result.Entries[2])
	}
	result.Entries[1].Record.Value[0] = 'X'
	record, err := state.Get("first")
	if err != nil || string(record.Value) != "one" {
		t.Fatalf("caller mutated stored record: %+v %v", record, err)
	}
}

func TestReadManyRejectsDuplicateAndOversizedCensusWithoutPartialResult(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.MaxScanBytes = 100
	state, _ := newObservationState(t, clock.NewManual(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)), newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	for _, key := range []string{"a", "b"} {
		if _, err := state.Put(t.Context(), key, make([]byte, 60), nil, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := state.ReadMany([]string{"a", "a"}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeDuplicateKey}) || len(result.Entries) != 0 {
		t.Fatalf("duplicate result = %+v, %#v", result, err)
	}
	if result, err := state.ReadMany([]string{"a", "b"}); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeFrameTooLarge}) || len(result.Entries) != 0 {
		t.Fatalf("oversized result = %+v, %#v", result, err)
	}
}
