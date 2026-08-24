package engine

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/clock"
	"gapdb/internal/persist"
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

// Compile-time coverage that the production WAL satisfies the engine seam.
var _ CommitLog = (*persist.WAL)(nil)
