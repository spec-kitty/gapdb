package performance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
)

const (
	assertionPerformanceWindows      = 3
	assertionMemorySamplesPerWindow  = 2048
	assertionDurableSamplesPerWindow = 256
	assertionWarmupPairs             = 256
	assertionDurableP95Limit         = 4 * time.Millisecond
	assertionMinimumMemoryAllowance  = 100 * time.Microsecond
	assertionRelativeMemoryAllowance = 0.15
)

type assertionPerformanceWindow struct {
	Window          int           `json:"window"`
	MemoryControl   time.Duration `json:"-"`
	MemoryAsserted  time.Duration `json:"-"`
	MemoryOverhead  time.Duration `json:"-"`
	MemoryAllowance time.Duration `json:"-"`
	DurableControl  time.Duration `json:"-"`
	DurableAsserted time.Duration `json:"-"`
}

type assertionRawWindow struct {
	Window            int     `json:"window"`
	MemoryControlNS   []int64 `json:"memory_control_ns"`
	MemoryAssertedNS  []int64 `json:"memory_asserted_ns"`
	DurableControlNS  []int64 `json:"durable_control_ns"`
	DurableAssertedNS []int64 `json:"durable_asserted_ns"`
}

type assertionRawEvidence struct {
	SchemaVersion uint16 `json:"schema_version"`
	Configuration struct {
		Windows              int     `json:"windows"`
		WarmupPairs          int     `json:"warmup_pairs"`
		MemorySamples        int     `json:"memory_samples_per_window"`
		DurableSamples       int     `json:"durable_samples_per_window"`
		ValueBytes           int     `json:"value_bytes"`
		ControlTokenBytes    int     `json:"control_token_bytes"`
		AssertedTokenBytes   int     `json:"asserted_token_bytes"`
		MemoryRelativeLimit  float64 `json:"memory_relative_limit"`
		MemoryMinimumNanos   int64   `json:"memory_minimum_ns"`
		DurableMaximumNanos  int64   `json:"durable_maximum_ns"`
		Quantile             string  `json:"quantile"`
		PairOrder            string  `json:"pair_order"`
		Transport            string  `json:"transport"`
		AcknowledgementModes string  `json:"acknowledgement_modes"`
	} `json:"configuration"`
	Windows []assertionRawWindow `json:"windows"`
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
	for index := range assertionWarmupPairs {
		measureAssertionMutation(t, client, guard.Revision, fmt.Sprintf("assertion-performance/warm/controlx/%04d", index), gapdb.AckMemory, false)
		measureAssertionMutation(t, client, guard.Revision, fmt.Sprintf("assertion-performance/warm/asserted/%04d", index), gapdb.AckMemory, true)
	}

	windows := make([]assertionPerformanceWindow, 0, assertionPerformanceWindows)
	raw := assertionRawEvidence{SchemaVersion: 1}
	raw.Configuration.Windows = assertionPerformanceWindows
	raw.Configuration.WarmupPairs = assertionWarmupPairs
	raw.Configuration.MemorySamples = assertionMemorySamplesPerWindow
	raw.Configuration.DurableSamples = assertionDurableSamplesPerWindow
	raw.Configuration.ValueBytes = len("value")
	raw.Configuration.ControlTokenBytes = len("controlx")
	raw.Configuration.AssertedTokenBytes = len("asserted")
	raw.Configuration.MemoryRelativeLimit = assertionRelativeMemoryAllowance
	raw.Configuration.MemoryMinimumNanos = assertionMinimumMemoryAllowance.Nanoseconds()
	raw.Configuration.DurableMaximumNanos = assertionDurableP95Limit.Nanoseconds()
	raw.Configuration.Quantile = "nearest_rank_p95"
	raw.Configuration.PairOrder = "alternating_control_first_asserted_first"
	raw.Configuration.Transport = "public_unix_socket"
	raw.Configuration.AcknowledgementModes = "memory_and_durable"
	for window := range assertionPerformanceWindows {
		memoryControlRaw, memoryAssertedRaw := measurePairedAssertionWindow(t, client, guard.Revision, window, "memory", gapdb.AckMemory, assertionMemorySamplesPerWindow)
		durableControlRaw, durableAssertedRaw := measurePairedAssertionWindow(t, client, guard.Revision, window, "durable", gapdb.AckDurable, assertionDurableSamplesPerWindow)
		memoryControl, memoryAsserted := nearestRankP95(memoryControlRaw), nearestRankP95(memoryAssertedRaw)
		durableControl, durableAsserted := nearestRankP95(durableControlRaw), nearestRankP95(durableAssertedRaw)
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
		raw.Windows = append(raw.Windows, assertionRawWindow{
			Window: window, MemoryControlNS: memoryControlRaw, MemoryAssertedNS: memoryAssertedRaw,
			DurableControlNS: durableControlRaw, DurableAssertedNS: durableAssertedRaw,
		})
	}
	t.Logf("ASSERTION_PERFORMANCE_WINDOWS=%+v", windows)
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ASSERTION_PERFORMANCE_RAW=%s", encoded)
	if path := os.Getenv("GAPDB_ASSERTION_RAW_OUT"); path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func measurePairedAssertionWindow(t *testing.T, client *gapdb.Client, guardRevision gapdb.Revision, window int, class string, ack gapdb.AckMode, samples int) ([]int64, []int64) {
	t.Helper()
	control := make([]int64, 0, samples)
	asserted := make([]int64, 0, samples)
	for index := range samples {
		// Alternate pair order so local drift cannot systematically favor either path.
		var controlSample, assertedSample time.Duration
		if index%2 == 0 {
			controlSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/controlx/%04d", window, class, index), ack, false)
			assertedSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/asserted/%04d", window, class, index), ack, true)
		} else {
			assertedSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/asserted/%04d", window, class, index), ack, true)
			controlSample = measureAssertionMutation(t, client, guardRevision, fmt.Sprintf("assertion-performance/%02d/%s/controlx/%04d", window, class, index), ack, false)
		}
		control = append(control, controlSample.Nanoseconds())
		asserted = append(asserted, assertedSample.Nanoseconds())
	}
	return control, asserted
}

func nearestRankP95(raw []int64) time.Duration {
	ordered := append([]int64(nil), raw...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	return time.Duration(ordered[(95*len(ordered)-1)/100])
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
