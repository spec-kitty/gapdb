package engine

import (
	"errors"
	"testing"

	"gapdb/gapdb"
	"gapdb/internal/persist"
)

func TestRangeAllocatorAcceptsGapsRefillsAndNeverReuses(t *testing.T) {
	var after []gapdb.Revision
	allocator, err := NewRangeAllocator(4, persist.RevisionRange{First: 10, End: 11}, func(previous gapdb.Revision) (persist.RevisionRange, error) {
		after = append(after, previous)
		return persist.RevisionRange{First: 20, End: 21}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []gapdb.Revision{10, 11, 20, 21} {
		got, err := allocator.Next()
		if err != nil || got != want {
			t.Fatalf("Next() = %d, %v; want %d, nil", got, err, want)
		}
	}
	if len(after) != 1 || after[0] != 11 {
		t.Fatalf("reservation requests = %v, want [11]", after)
	}
	if _, err := allocator.Next(); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionRangeExhausted}) {
		t.Fatalf("exhausted Next error = %v", err)
	}
}

func TestRangeAllocatorRejectsUnsafeRanges(t *testing.T) {
	tests := []persist.RevisionRange{
		{},
		{First: 5, End: 4},
		{First: 4, End: 10},
	}
	for _, allocation := range tests {
		if _, err := NewRangeAllocator(4, allocation, nil); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionRangeExhausted}) {
			t.Errorf("NewRangeAllocator(%+v) error = %v", allocation, err)
		}
	}
}

func TestRangeAllocatorDoesNotWrapAtMaximumRevision(t *testing.T) {
	maximum := gapdb.Revision(^uint64(0))
	allocator, err := NewRangeAllocator(maximum-1, persist.RevisionRange{First: maximum, End: maximum}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := allocator.Next(); err != nil || got != maximum {
		t.Fatalf("maximum Next = %d, %v", got, err)
	}
	if got, err := allocator.Next(); got != 0 || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionRangeExhausted}) {
		t.Fatalf("wrapped Next = %d, %v", got, err)
	}
}
