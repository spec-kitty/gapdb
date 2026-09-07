package engine

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/clock"
	"github.com/spec-kitty/gapdb/internal/persist"
)

func TestGetIsDirectExpiryAwareAndCopySafe(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	value := []byte("original")
	expires := now.Add(time.Minute)
	state := newTestState(t, testConfig{
		clock:   manual,
		records: []gapdb.Record{gapdb.NewRecord("key", value, 7, &expires)},
		current: 7,
	})
	t.Cleanup(func() { closeState(t, state) })

	value[0] = 'X'
	got, err := state.Get("key")
	if err != nil || !bytes.Equal(got.Value, []byte("original")) {
		t.Fatalf("Get before expiry = %+v, %v", got, err)
	}
	got.Value[0] = 'Y'
	again, err := state.Get("key")
	if err != nil || !bytes.Equal(again.Value, []byte("original")) {
		t.Fatalf("Get after output mutation = %+v, %v", again, err)
	}

	manual.Set(expires)
	_, err = state.Get("key")
	var apiErr *gapdb.Error
	if !errors.As(err, &apiErr) || apiErr.Code != gapdb.CodeNotFound || apiErr.CurrentRevision == nil || *apiErr.CurrentRevision != 7 {
		t.Fatalf("Get at expiry error = %#v", err)
	}
	manual.Set(now)
	if _, err := state.Get("key"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("clock rollback resurrected record: %v", err)
	}
}

func TestGetRemainsAvailableWhenWritesDegrade(t *testing.T) {
	log := &fakeLog{appendErr: errors.New("disk failed")}
	state := newTestState(t, testConfig{log: log, records: []gapdb.Record{gapdb.NewRecord("safe", []byte("v"), 1, nil)}, current: 1})
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.Put(t.Context(), "new", []byte("x"), nil, gapdb.AckMemory); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeStorageDegraded}) {
		t.Fatalf("Put error = %v", err)
	}
	if state.Lifecycle() != gapdb.LifecycleDegradedReadOnly {
		t.Fatalf("lifecycle = %s", state.Lifecycle())
	}
	if record, err := state.Get("safe"); err != nil || string(record.Value) != "v" {
		t.Fatalf("Get in degraded state = %+v, %v", record, err)
	}
}

func TestConcurrentDirectReadersAndWriter(t *testing.T) {
	state := newTestState(t, testConfig{})
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.Put(t.Context(), "key", []byte("0"), nil, gapdb.AckMemory); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				record, err := state.Get("key")
				if err == nil && len(record.Value) == 0 {
					t.Errorf("observed empty committed value")
				}
			}
		}()
	}
	close(start)
	for i := 0; i < 100; i++ {
		if _, err := state.Put(t.Context(), "key", []byte{byte(i)}, nil, gapdb.AckMemory); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestGetValidatesKey(t *testing.T) {
	state := newTestState(t, testConfig{})
	t.Cleanup(func() { closeState(t, state) })
	if _, err := state.Get(""); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("empty key error = %v", err)
	}
}

func TestReadRecoverySnapshotIsStableSortedBoundedAndCopySafe(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	manual := clock.NewManual(now)
	expiredAt := now
	state := newTestState(t, testConfig{
		clock: manual,
		records: []gapdb.Record{
			gapdb.NewRecord("spk/v2/z", []byte("z"), 9, nil),
			gapdb.NewRecord("foreign/a", []byte("foreign"), 7, nil),
			gapdb.NewRecord("spk/v2/a", []byte("a"), 8, nil),
			gapdb.NewRecord("spk/v2/expired", []byte("expired"), 6, &expiredAt),
		},
		current: 9,
	})
	t.Cleanup(func() { closeState(t, state) })
	request := gapdb.RecoverySnapshotRequest{Prefix: "spk/v2/", ExpectedRevision: 9, MaxRecords: 3, MaxBytes: 4096}

	result, err := state.ReadRecoverySnapshot(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ObservedRevision != 9 || !result.AsOf.Equal(now) || len(result.Records) != 2 || result.Records[0].Key != "spk/v2/a" || result.Records[1].Key != "spk/v2/z" {
		t.Fatalf("snapshot = %+v", result)
	}
	result.Records[0].Value[0] = 'X'
	again, err := state.ReadRecoverySnapshot(request)
	if err != nil || string(again.Records[0].Value) != "a" {
		t.Fatalf("copy-safe snapshot = %+v, %v", again, err)
	}

	stale := request
	stale.ExpectedRevision--
	if _, err := state.ReadRecoverySnapshot(stale); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeScanStale}) {
		t.Fatalf("stale snapshot = %v", err)
	}
	tooFew := request
	tooFew.MaxRecords = 1
	if _, err := state.ReadRecoverySnapshot(tooFew); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeFrameTooLarge}) {
		t.Fatalf("record bound = %v", err)
	}
	tooSmall := request
	tooSmall.MaxBytes = 80
	if _, err := state.ReadRecoverySnapshot(tooSmall); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeFrameTooLarge}) {
		t.Fatalf("byte bound = %v", err)
	}
}

// Compile-time coverage that the production WAL satisfies the engine seam.
var _ CommitLog = (*persist.WAL)(nil)
