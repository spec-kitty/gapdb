package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/clock"
	"github.com/spec-kitty/gapdb/internal/persist"
)

func testDatabaseID(t *testing.T, value string) persist.DatabaseID {
	t.Helper()
	id, err := persist.ParseDatabaseID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestScanPrefixOrdersBoundsAndReusesCursorAsOf(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	expires := now.Add(time.Minute)
	for _, item := range []struct {
		key   string
		value string
	}{
		{key: "p/é", value: "3"},
		{key: "other", value: "x"},
		{key: "p/a", value: "1"},
		{key: "p/b", value: "2"},
	} {
		var expiry *time.Time
		if item.key == "p/b" {
			expiry = &expires
		}
		if _, err := state.Put(t.Context(), item.key, []byte(item.value), expiry, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	first, err := state.ScanPrefix("p/", 1, "")
	if err != nil || len(first.Records) != 1 || first.Records[0].Key != "p/a" || !first.Truncated || first.Cursor == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	manual.Set(expires.Add(time.Minute))
	second, err := state.ScanPrefix("p/", 10, first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 2 || second.Records[0].Key != "p/b" || second.Records[1].Key != "p/é" || second.AsOf != first.AsOf || second.ObservedRevision != first.ObservedRevision {
		t.Fatalf("continuation = %+v", second)
	}
}

func TestScanCursorRejectsTamperingBindingAndStaleness(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	for _, key := range []string{"p/a", "p/b"} {
		if _, err := state.Put(t.Context(), key, nil, nil, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	page, err := state.ScanPrefix("p/", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	tampered := []byte(page.Cursor)
	tampered[len(tampered)-1] ^= 1
	if _, err := state.ScanPrefix("p/", 1, string(tampered)); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidCursor}) {
		t.Fatalf("tampered cursor error = %v", err)
	}
	if _, err := state.ScanPrefix("q/", 1, page.Cursor); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidCursor}) {
		t.Fatalf("prefix-bound cursor error = %v", err)
	}
	if _, err := state.Put(t.Context(), "other", nil, nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	_, err = state.ScanPrefix("p/", 1, page.Cursor)
	var stale *gapdb.Error
	if !errors.As(err, &stale) || stale.Code != gapdb.CodeScanStale || stale.CursorRevision == nil || stale.CurrentRevision == nil {
		t.Fatalf("stale cursor error = %#v", err)
	}
}

func TestScanCursorRejectsNoncanonicalBase64URL(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.Put(t.Context(), "a", nil, nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	cursor := state.encodeScanCursor(scanCursor{lastKey: "a", revision: state.CurrentRevision(), asOf: now})
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := len(cursor) - 1
	index := byteIndex(alphabet, cursor[last])
	if index < 0 || index%4 == 3 {
		t.Fatalf("test cursor has unsuitable final base64 digit %q", cursor[last])
	}
	noncanonical := cursor[:last] + string(alphabet[index+1])
	if _, err := state.ScanPrefix("", 1, noncanonical); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidCursor}) {
		t.Fatalf("noncanonical cursor error = %v", err)
	}
}

func byteIndex(value string, target byte) int {
	for index := range len(value) {
		if value[index] == target {
			return index
		}
	}
	return -1
}

func TestScanCursorRejectsAnotherDatabaseAndHonorsByteLimit(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	limits := gapdb.DefaultOptions().Limits
	limits.MaxScanBytes = 38
	first, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, nil, 0)
	t.Cleanup(func() { closeState(t, first) })
	for _, key := range []string{"a", "b"} {
		if _, err := first.Put(t.Context(), key, []byte("1234"), nil, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	page, err := first.ScanPrefix("", 10, "")
	if err != nil || len(page.Records) != 1 || !page.Truncated {
		t.Fatalf("byte-bounded page = %+v, %v", page, err)
	}
	log := &fakeLog{}
	second, err := New(Config{Limits: limits, Clock: clock.NewManual(now), ExpiryTimer: newManualExpiryTimer(), Log: log, Allocator: &countingAllocator{next: 3}, CurrentRevision: page.ObservedRevision, SnapshotRevision: page.ObservedRevision, DatabaseID: testDatabaseID(t, "ffeeddccbbaa99887766554433221100")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeState(t, second) })
	if _, err := second.ScanPrefix("", 10, page.Cursor); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidCursor}) {
		t.Fatalf("database-bound cursor error = %v", err)
	}
}

func TestScanExcludesExpiryAtExactBoundary(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	expires := now.Add(time.Minute)
	state, _ := newObservationState(t, manual, newManualExpiryTimer(), gapdb.Limits{}, nil, 0)
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.Put(t.Context(), "expired", nil, &expires, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Put(t.Context(), "live", nil, nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	manual.Set(expires)
	page, err := state.ScanPrefix("", 10, "")
	if err != nil || len(page.Records) != 1 || page.Records[0].Key != "live" {
		t.Fatalf("boundary scan = %+v, %v", page, err)
	}
}
