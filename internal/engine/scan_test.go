package engine

import (
	"errors"
	"fmt"
	"sort"
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

func TestScanPrefixOrderedIndexTracksRecoveryInsertUpdateAndDelete(t *testing.T) {
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	records := []gapdb.Record{
		gapdb.NewRecord("p/z", []byte("z"), 1, nil),
		gapdb.NewRecord("p/b", []byte("b"), 1, nil),
	}
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), gapdb.Limits{}, records, 1)
	t.Cleanup(func() { closeState(t, state) })
	initial, err := state.ScanPrefix("p/", 10, "")
	if err != nil || len(initial.Records) != 2 || initial.Records[0].Key != "p/b" || initial.Records[1].Key != "p/z" {
		t.Fatalf("recovered index scan = %+v, %v", initial, err)
	}
	if _, err := state.Put(t.Context(), "p/a", []byte("a"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Put(t.Context(), "p/z", []byte("new-z"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := state.DeleteIfRevision(t.Context(), "p/b", 1, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	page, err := state.ScanPrefix("p/", 10, "")
	if err != nil || len(page.Records) != 2 || page.Records[0].Key != "p/a" || page.Records[1].Key != "p/z" || string(page.Records[1].Value) != "new-z" {
		t.Fatalf("mutated index scan = %+v, %v", page, err)
	}
}

func TestScanPrefixFirstPageAllocationsAreBoundedByPageNotDatabase(t *testing.T) {
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	records := make([]gapdb.Record, 10_000)
	for index := range records {
		records[index] = gapdb.NewRecord(fmt.Sprintf("p/%05d", index), []byte("value"), 1, nil)
	}
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), gapdb.Limits{}, records, 1)
	t.Cleanup(func() { closeState(t, state) })
	var scanErr error
	allocations := testing.AllocsPerRun(5, func() {
		_, scanErr = state.ScanPrefix("p/", 500, "")
	})
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	if allocations > 2_000 {
		t.Fatalf("first-page allocations = %.0f, want page-bounded work", allocations)
	}
}

func TestOrderedKeyDeltaLargeAdversarialBatchIsBounded(t *testing.T) {
	base := make([]string, 100_000)
	for index := range base {
		base[index] = fmt.Sprintf("m/%06d", index)
	}
	mutations := make([]gapdb.Mutation, 0, 10_000)
	for index := 3_332; index >= 0; index-- {
		mutations = append(mutations, gapdb.NewPutMutation(fmt.Sprintf("a/%05d", index), nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil))
	}
	for index := 3_332; index >= 0; index-- {
		mutations = append(mutations, gapdb.NewPutMutation(fmt.Sprintf("m/%06d/x", index*20+1), nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil))
	}
	for index := 0; index < 3_334; index++ {
		mutations = append(mutations, gapdb.Mutation{Kind: gapdb.MutationDelete, Key: fmt.Sprintf("m/%06d", index*20)})
	}
	started := time.Now()
	result := mergeOrderedKeyDelta(base, mutations)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("100k-key/10k-delta merge took %s", elapsed)
	}
	if len(result) != len(base)+6_666-3_334 || !sort.StringsAreSorted(result) {
		t.Fatalf("merged key census length/sort = %d/%v", len(result), sort.StringsAreSorted(result))
	}
	for index := 1; index < len(result); index++ {
		if result[index-1] == result[index] {
			t.Fatalf("duplicate ordered key %q", result[index])
		}
	}
}

func TestOverwriteOnlyBatchDoesNotCopyOrReplaceOrderedIndex(t *testing.T) {
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	records := make([]gapdb.Record, 100_000)
	for index := range records {
		records[index] = gapdb.NewRecord(fmt.Sprintf("m/%06d", index), nil, 1, nil)
	}
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), gapdb.Limits{}, records, 1)
	t.Cleanup(func() { closeState(t, state) })
	before := &state.orderedKeys[0]
	mutations := make([]gapdb.Mutation, 128)
	for index := range mutations {
		mutations[index] = gapdb.NewPutMutation(fmt.Sprintf("m/%06d", index*500), []byte("updated"), gapdb.Condition{Kind: gapdb.ConditionAny}, nil)
	}
	started := time.Now()
	if _, err := state.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory, Mutations: mutations}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("128-key overwrite acknowledgement took %s", elapsed)
	}
	if before != &state.orderedKeys[0] {
		t.Fatal("overwrite-only batch replaced the ordered membership index")
	}
}

func TestLargeDescendingMembershipBatchesStayWithinAcknowledgementBudget(t *testing.T) {
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	records := make([]gapdb.Record, 100_000)
	for index := range records {
		records[index] = gapdb.NewRecord(fmt.Sprintf("m/%06d", index), nil, 1, nil)
	}
	limits := gapdb.DefaultOptions().Limits
	limits.MaxBatchOperations = 10_000
	state, _ := newObservationState(t, clock.NewManual(now), newManualExpiryTimer(), limits, records, 1)
	t.Cleanup(func() { closeState(t, state) })
	stopReader := make(chan struct{})
	readerReady := make(chan struct{})
	readerResult := make(chan error, 1)
	go func() {
		if _, err := state.Get("m/050000"); err != nil {
			readerResult <- err
			return
		}
		close(readerReady)
		for {
			select {
			case <-stopReader:
				readerResult <- nil
				return
			default:
				if _, err := state.Get("m/050000"); err != nil {
					readerResult <- err
					return
				}
			}
		}
	}()
	<-readerReady

	insertions := make([]gapdb.Mutation, 0, 10_000)
	for index := 9_999; index >= 0; index-- {
		insertions = append(insertions, gapdb.NewPutMutation(fmt.Sprintf("a/%05d", index), nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil))
	}
	started := time.Now()
	inserted, err := state.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory, Mutations: insertions})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("descending 10k-key insertion acknowledgement took %s", elapsed)
	}

	deletions := make([]gapdb.Mutation, 0, 10_000)
	for index := 9_999; index >= 0; index-- {
		deletions = append(deletions, gapdb.NewDeleteMutation(fmt.Sprintf("a/%05d", index), inserted.Revision))
	}
	started = time.Now()
	if _, err := state.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory, Mutations: deletions}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("descending 10k-key deletion acknowledgement took %s", elapsed)
	}
	if len(state.orderedKeys) != len(records) {
		t.Fatalf("ordered key census after insert/delete = %d", len(state.orderedKeys))
	}
	close(stopReader)
	if err := <-readerResult; err != nil {
		t.Fatalf("concurrent exact reader: %v", err)
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
