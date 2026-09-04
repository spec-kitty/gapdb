package performance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
)

const (
	assertionPerformanceRuns          = 11
	assertionPerformanceWindowsPerRun = 3
	assertionMemorySamplesPerWindow   = 2048
	assertionDurableSamplesPerWindow  = 1024
	assertionWarmupPairs              = 256
	assertionDurableP95Limit          = 4 * time.Millisecond
	assertionMinimumMemoryAllowance   = 100 * time.Microsecond
	assertionRelativeMemoryAllowance  = 0.15
	assertionPerformanceSchemaVersion = 2
)

type assertionRawWindow struct {
	Run                  int     `json:"run"`
	Window               int     `json:"window"`
	MemoryControlNS      []int64 `json:"memory_control_ns"`
	MemoryAssertedNS     []int64 `json:"memory_asserted_ns"`
	DurableControlNS     []int64 `json:"durable_control_ns"`
	DurableAssertedNS    []int64 `json:"durable_asserted_ns"`
	MemoryControlP95NS   int64   `json:"memory_control_p95_ns"`
	MemoryAssertedP95NS  int64   `json:"memory_asserted_p95_ns"`
	MemoryAllowanceNS    int64   `json:"memory_allowance_ns"`
	DurableControlP95NS  int64   `json:"durable_control_p95_ns"`
	DurableAssertedP95NS int64   `json:"durable_asserted_p95_ns"`
	Passed               bool    `json:"passed"`
}

type assertionRawRun struct {
	Run     int                  `json:"run"`
	Passed  bool                 `json:"passed"`
	Windows []assertionRawWindow `json:"windows"`
}

type assertionRawEvidence struct {
	SchemaVersion uint16 `json:"schema_version"`
	Configuration struct {
		Runs                 int     `json:"runs"`
		WindowsPerRun        int     `json:"windows_per_run"`
		WarmupPairs          int     `json:"warmup_pairs_per_run"`
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
		RetryPolicy          string  `json:"retry_policy"`
	} `json:"configuration"`
	ReferenceHost struct {
		RuntimeVersion    string `json:"runtime_version"`
		OS                string `json:"os"`
		Arch              string `json:"arch"`
		LogicalCPUs       int    `json:"logical_cpus"`
		GOMAXPROCS        int    `json:"gomaxprocs"`
		LoadAverageStart  string `json:"load_average_start"`
		LoadAverageFinish string `json:"load_average_finish"`
		LoadPolicy        string `json:"load_policy"`
	} `json:"reference_host"`
	AttemptedRuns int               `json:"attempted_runs"`
	PassedRuns    int               `json:"passed_runs"`
	Runs          []assertionRawRun `json:"runs"`
}

func TestAssertionPerformancePairedSameHostWindows(t *testing.T) {
	if os.Getenv("GAPDB_ASSERTION_ACCEPTANCE") != "1" {
		t.Skip("set GAPDB_ASSERTION_ACCEPTANCE=1 to run the complete assertion qualification campaign")
	}
	raw := newAssertionRawEvidence()
	raw.ReferenceHost.LoadAverageStart = readLoadAverage()
	allPassed := true
	for run := range assertionPerformanceRuns {
		result := runAssertionPerformanceTrial(t, run)
		raw.AttemptedRuns++
		raw.Runs = append(raw.Runs, result)
		if result.Passed {
			raw.PassedRuns++
		} else {
			allPassed = false
		}
	}
	raw.ReferenceHost.LoadAverageFinish = readLoadAverage()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("GAPDB_ASSERTION_RAW_OUT"); path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("ASSERTION_PERFORMANCE_CAMPAIGN attempted=%d passed=%d windows=%d durable_samples_per_window=%d", raw.AttemptedRuns, raw.PassedRuns, raw.AttemptedRuns*assertionPerformanceWindowsPerRun, assertionDurableSamplesPerWindow)
	for _, run := range raw.Runs {
		maxDurable := int64(0)
		for _, window := range run.Windows {
			maxDurable = max(maxDurable, window.DurableAssertedP95NS)
		}
		t.Logf("ASSERTION_PERFORMANCE_RUN run=%d passed=%t max_durable_asserted_p95=%s", run.Run, run.Passed, time.Duration(maxDurable))
	}
	if raw.AttemptedRuns != assertionPerformanceRuns || raw.PassedRuns != assertionPerformanceRuns || !allPassed {
		t.Fatalf("fixed assertion performance campaign attempted=%d passed=%d", raw.AttemptedRuns, raw.PassedRuns)
	}
}

func newAssertionRawEvidence() assertionRawEvidence {
	raw := assertionRawEvidence{SchemaVersion: assertionPerformanceSchemaVersion}
	raw.Configuration.Runs = assertionPerformanceRuns
	raw.Configuration.WindowsPerRun = assertionPerformanceWindowsPerRun
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
	raw.Configuration.RetryPolicy = "none_no_omissions"
	raw.ReferenceHost.RuntimeVersion = runtime.Version()
	raw.ReferenceHost.OS = runtime.GOOS
	raw.ReferenceHost.Arch = runtime.GOARCH
	raw.ReferenceHost.LogicalCPUs = runtime.NumCPU()
	raw.ReferenceHost.GOMAXPROCS = runtime.GOMAXPROCS(0)
	raw.ReferenceHost.LoadPolicy = "single_fixed_campaign_no_parallel_gapdb_qualification"
	return raw
}

func runAssertionPerformanceTrial(t *testing.T, run int) assertionRawRun {
	t.Helper()
	fixture := newSocketFixture(t, 1)
	defer fixture.close(t)
	client := fixture.clients[0]
	guardKey := fmt.Sprintf("assertion-performance/%02d/guard", run)
	guard, err := client.Put(t.Context(), guardKey, []byte("authority"), nil, gapdb.AckDurable)
	if err != nil {
		t.Fatal(err)
	}
	for index := range assertionWarmupPairs {
		measureAssertionMutation(t, client, guardKey, guard.Revision, fmt.Sprintf("assertion-performance/%02d/warm/controlx/%04d", run, index), gapdb.AckMemory, false)
		measureAssertionMutation(t, client, guardKey, guard.Revision, fmt.Sprintf("assertion-performance/%02d/warm/asserted/%04d", run, index), gapdb.AckMemory, true)
	}
	result := assertionRawRun{Run: run, Passed: true, Windows: make([]assertionRawWindow, 0, assertionPerformanceWindowsPerRun)}
	for window := range assertionPerformanceWindowsPerRun {
		memoryControlRaw, memoryAssertedRaw := measurePairedAssertionWindow(t, client, guardKey, guard.Revision, run, window, "memory", gapdb.AckMemory, assertionMemorySamplesPerWindow)
		durableControlRaw, durableAssertedRaw := measurePairedAssertionWindow(t, client, guardKey, guard.Revision, run, window, "durable", gapdb.AckDurable, assertionDurableSamplesPerWindow)
		memoryControl := nearestRankP95(memoryControlRaw)
		memoryAsserted := nearestRankP95(memoryAssertedRaw)
		durableControl := nearestRankP95(durableControlRaw)
		durableAsserted := nearestRankP95(durableAssertedRaw)
		allowance := max(time.Duration(float64(memoryControl)*assertionRelativeMemoryAllowance), assertionMinimumMemoryAllowance)
		passed := memoryAsserted <= memoryControl+allowance && durableAsserted < assertionDurableP95Limit
		result.Passed = result.Passed && passed
		result.Windows = append(result.Windows, assertionRawWindow{
			Run: run, Window: window,
			MemoryControlNS: memoryControlRaw, MemoryAssertedNS: memoryAssertedRaw,
			DurableControlNS: durableControlRaw, DurableAssertedNS: durableAssertedRaw,
			MemoryControlP95NS: memoryControl.Nanoseconds(), MemoryAssertedP95NS: memoryAsserted.Nanoseconds(), MemoryAllowanceNS: allowance.Nanoseconds(),
			DurableControlP95NS: durableControl.Nanoseconds(), DurableAssertedP95NS: durableAsserted.Nanoseconds(), Passed: passed,
		})
	}
	return result
}

func measurePairedAssertionWindow(t *testing.T, client *gapdb.Client, guardKey string, guardRevision gapdb.Revision, run, window int, class string, ack gapdb.AckMode, samples int) ([]int64, []int64) {
	t.Helper()
	control := make([]int64, 0, samples)
	asserted := make([]int64, 0, samples)
	for index := range samples {
		controlKey := fmt.Sprintf("assertion-performance/%02d/%02d/%s/controlx/%04d", run, window, class, index)
		assertedKey := fmt.Sprintf("assertion-performance/%02d/%02d/%s/asserted/%04d", run, window, class, index)
		var controlSample, assertedSample time.Duration
		if index%2 == 0 {
			controlSample = measureAssertionMutation(t, client, guardKey, guardRevision, controlKey, ack, false)
			assertedSample = measureAssertionMutation(t, client, guardKey, guardRevision, assertedKey, ack, true)
		} else {
			assertedSample = measureAssertionMutation(t, client, guardKey, guardRevision, assertedKey, ack, true)
			controlSample = measureAssertionMutation(t, client, guardKey, guardRevision, controlKey, ack, false)
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

func measureAssertionMutation(t *testing.T, client *gapdb.Client, guardKey string, guardRevision gapdb.Revision, key string, ack gapdb.AckMode, asserted bool) time.Duration {
	t.Helper()
	batch := gapdb.Batch{Ack: ack, Mutations: []gapdb.Mutation{gapdb.NewPutMutation(key, []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)}}
	if asserted {
		batch.Assertions = []gapdb.Assertion{{Key: guardKey, Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: guardRevision}}}
	}
	started := time.Now()
	result, err := client.AtomicBatch(t.Context(), batch)
	duration := time.Since(started)
	if err != nil || result.MutationCount != 1 || result.AssertionCount != len(batch.Assertions) {
		t.Fatalf("%s result=%+v err=%v", key, result, err)
	}
	return duration
}

func readLoadAverage() string {
	contents, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable"
	}
	fields := strings.Fields(string(contents))
	if len(fields) < 3 {
		return "unavailable"
	}
	return strings.Join(fields[:3], " ")
}
