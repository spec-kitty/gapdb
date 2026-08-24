package clock

import (
	"sync"
	"testing"
	"time"
)

func TestManualClockCanMoveBothDirections(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 23, 18, 0, 0, 123, time.FixedZone("offset", 3600))
	manual := NewManual(start)
	if got := manual.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
	manual.Advance(2 * time.Second)
	if got := manual.Now(); !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("advanced Now() = %v", got)
	}
	manual.Set(start.Add(-time.Hour))
	if got := manual.Now(); !got.Equal(start.Add(-time.Hour)) {
		t.Fatalf("backward Now() = %v", got)
	}
}

func TestNondecreasingClampsBackwardMovement(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	manual := NewManual(start)
	effective := NewNondecreasing(manual)
	if got := effective.Now(); !got.Equal(start) {
		t.Fatalf("first Now() = %v", got)
	}
	manual.Set(start.Add(-time.Hour))
	if got := effective.Now(); !got.Equal(start) {
		t.Fatalf("clamped Now() = %v, want %v", got, start)
	}
	manual.Set(start.Add(time.Hour))
	if got := effective.Now(); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("forward Now() = %v", got)
	}
}

func TestExpiryBoundaryAndUTCFormatting(t *testing.T) {
	t.Parallel()

	expiry := time.Date(2026, 8, 23, 18, 30, 0, 123456789, time.FixedZone("offset", -5*60*60))
	if IsExpired(expiry, expiry.Add(-time.Nanosecond)) {
		t.Fatal("record expired before boundary")
	}
	if !IsExpired(expiry, expiry) {
		t.Fatal("record must be expired exactly at boundary")
	}
	if got, want := FormatUTC(expiry), "2026-08-23T23:30:00.123456789Z"; got != want {
		t.Fatalf("FormatUTC() = %q, want %q", got, want)
	}
}

func TestManualAndNondecreasingAreConcurrentSafe(t *testing.T) {
	t.Parallel()

	start := time.Unix(1, 0).UTC()
	manual := NewManual(start)
	effective := NewNondecreasing(manual)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for step := range 1000 {
				manual.Set(start.Add(time.Duration(worker*1000+step) * time.Nanosecond))
				_ = manual.Now()
				_ = effective.Now()
			}
		}(worker)
	}
	wg.Wait()
}
