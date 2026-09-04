package performance_test

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
)

const (
	assertionPerformanceWindows      = 3
	assertionMemorySamplesPerWindow  = 512
	assertionDurableSamplesPerWindow = 64
	assertionDurableP95Limit         = 4 * time.Millisecond
	assertionMinimumMemoryAllowance  = 100 * time.Microsecond
	assertionRelativeMemoryAllowance = 0.15
)

type assertionPerformanceWindow struct {
	Window          int
	MemoryControl   time.Duration
	MemoryAsserted  time.Duration
	MemoryOverhead  time.Duration
	MemoryAllowance time.Duration
	DurableControl  time.Duration
	DurableAsserted time.Duration
}

func TestAssertionPerformancePairedSameHostWindows(t *testing.T) {
	if os.Getenv("GAPDB_ASSERTION_ACCEPTANCE") != "1" {
		t.Skip("set GAPDB_ASSERTION_ACCEPTANCE=1 to run the assertion qualification workload")
	}
	fixture := newSocketFixture(t, 1)
	defer fixture.close(t)
	client := fixture.clients[0]
	guard, err := client.Put(t.Context(), "assertion-performance/guard", []byte("authority"), nil, gapdb.AckDurable)
	if err != nil {
		t.Fatal(err)
	}

	// Warm both paths before collecting per-window evidence.
	for index := range 32 {
		measureAssertionMutation(t, client, guard.Revision, fmt.Sprintf("assertion-performance/warm/control/%03d", index), gapdb.AckMemory, false)
		measureAssertionMutation(t, client, guard.Revision, fmt.Sprintf("assertion-performance/warm/asserted/%03d", index), gapdb.AckMemory, true)
	}

	windows := make([]assertionPerformanceWindow, 0, assertionPerformanceWindows)
	for window := range assertionPerformanceWindows {
		memoryControl, memoryAsserted, _ := measurePairedAssertionWindow(t, client, guard.Revision, window, "memory", gapdb.AckMemory, assertionMemorySamplesPerWindow)
		durableControl, durableAsserted, _ := measurePairedAssertionWindow(t, client, guard.Revision, window, "durable", gapdb.AckDurable, assertionDurableSamplesPerWindow)
		allowance := max(time.Duration(float64(memoryControl)*assertionRelativeMemoryAllowance), assertionMinimumMemoryAllowance)
		result := assertionPerformanceWindow{
			Window: window, MemoryControl: memoryControl, MemoryAsserted: memoryAsserted, MemoryOverhead: memoryAsserted - memoryControl, MemoryAllowance: allowance,
			DurableControl: durableControl, DurableAsserted: durableAsserted,
		}
		windows = append(windows, result)
		if memoryAsserted > memoryControl+allowance {
			t.Fatalf("window %d memory assertion p95=%s control=%s allowance=%s", window, memoryAsserted, memoryControl, allowance)
		}
		if durableAsserted >= assertionDurableP95Limit {
			t.Fatalf("window %d durable assertion p95=%s, require <%s (control=%s)", window, durableAsserted, assertionDurableP95Limit, durableControl)
		}
	}
	t.Logf("ASSERTION_PERFORMANCE_WINDOWS=%+v", windows)
}

func measurePairedAssertionWindow(t *testing.T, client *gapdb.Client, guardRevision gapdb.Revision, window int, class string, ack gapdb.AckMode, samples int) (time.Duration, time.Duration, time.Duration) {
	t.Helper()
	control := make([]time.Duration, 0, samples)
	asserted := make([]time.Duration, 0, samples)
	overhead := make([]time.Duration, 0, samples)
	for index := range samples {
		// Alternate pair order so local drift cannot systematically favor either path.
		var controlSample, assertedSample time.Duration
		if index%2 == 0 {
			controlSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/control/%04d", window, class, index), ack, false)
			assertedSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/asserted/%04d", window, class, index), ack, true)
		} else {
			assertedSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/asserted/%04d", window, class, index), ack, true)
			controlSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/control/%04d", window, class, index), ack, false)
		}
		control = append(control, controlSample)
		asserted = append(asserted, assertedSample)
		overhead = append(overhead, assertedSample-controlSample)
	}
	sort.Slice(control, func(left, right int) bool { return control[left] < control[right] })
	sort.Slice(asserted, func(left, right int) bool { return asserted[left] < asserted[right] })
	sort.Slice(overhead, func(left, right int) bool { return overhead[left] < overhead[right] })
	return control[(95*len(control)-1)/100], asserted[(95*len(asserted)-1)/100], overhead[(95*len(overhead)-1)/100]
}

func measureAssertionMutation(t *testing.T, client *gapdb.Client, guardRevision gapdb.Revision, key string, ack gapdb.AckMode, asserted bool) time.Duration {
	t.Helper()
	batch := gapdb.Batch{
		Ack:       ack,
		Mutations: []gapdb.Mutation{gapdb.NewPutMutation(key, []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}
	if asserted {
		batch.Assertions = []gapdb.Assertion{{Key: "assertion-performance/guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: guardRevision}}}
	}
	started := time.Now()
	result, err := client.AtomicBatch(t.Context(), batch)
	duration := time.Since(started)
	if err != nil || result.MutationCount != 1 || result.AssertionCount != len(batch.Assertions) {
		t.Fatalf("%s result=%+v err=%v", key, result, err)
	}
	return duration
}
