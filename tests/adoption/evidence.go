package adoption

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type EvidenceKind string

const (
	EvidencePerformance EvidenceKind = "performance_results_v1"
	EvidenceRaw         EvidenceKind = "performance_raw_v1"
	EvidenceAdoption    EvidenceKind = "adoption_result_v1"
	EvidenceCrash       EvidenceKind = "crash_result_v1"
	EvidenceProtocolDoc EvidenceKind = "protocol_doc_v1"
	EvidenceStorageDoc  EvidenceKind = "storage_doc_v1"
)

type EvidenceAuthority struct {
	Kind          EvidenceKind
	SourceCommit  string
	ConfigSHA256  string
	Command       string
	ObservedCount int64
}

type CriterionAuthority struct {
	Command     string
	EvidenceIDs []string
}

// ReleaseAuthority is the immutable caller-side trust root. It is deliberately
// not decoded from release-manifest.json or any referenced evidence file.
type ReleaseAuthority struct {
	CodeUnderTestCommit string
	ConfigSHA256        string
	Evidence            map[string]EvidenceAuthority
	Criteria            map[string]CriterionAuthority
}

type semanticEvidence struct {
	SourceCommit  string
	ConfigSHA256  string
	Command       string
	ObservedCount int64
}

func validateSemanticEvidence(path string, reference EvidenceReference) (semanticEvidence, error) {
	switch reference.Kind {
	case EvidencePerformance:
		return validatePerformanceEvidence(path)
	case EvidenceRaw:
		return validateRawEvidence(path)
	case EvidenceAdoption:
		return validateAdoptionEvidence(path)
	case EvidenceCrash:
		return validateCrashEvidence(path)
	case EvidenceProtocolDoc:
		return validateDocumentEvidence(path, protocolAuthorityTokens, reference)
	case EvidenceStorageDoc:
		return validateDocumentEvidence(path, storageAuthorityTokens, reference)
	default:
		return semanticEvidence{}, fmt.Errorf("unknown evidence kind %q", reference.Kind)
	}
}

func decodeStrictFile(path string, destination any) error {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

type performanceMetricEvidence struct {
	Samples     int     `json:"samples"`
	P50MS       float64 `json:"p50_ms"`
	P95MS       float64 `json:"p95_ms"`
	P99MS       float64 `json:"p99_ms"`
	AllocsPerOp float64 `json:"allocs_per_op"`
	TargetP95MS float64 `json:"target_p95_ms"`
	Status      string  `json:"status"`
}

type performanceWindowEvidence struct {
	ReadersReady    int  `json:"readers_ready"`
	ReadersActive   int  `json:"readers_active"`
	WriterReady     bool `json:"writer_ready"`
	WriterActive    bool `json:"writer_active"`
	ReadOperations  int  `json:"read_operations"`
	WriteOperations int  `json:"write_operations"`
}

type performanceResultEvidence struct {
	SchemaVersion uint16 `json:"schema_version"`
	Profile       string `json:"profile"`
	CodeCommit    string `json:"code_commit"`
	ConfigSHA256  string `json:"config_sha256"`
	Environment   struct {
		CPUModel                string `json:"cpu_model"`
		PhysicalCores           int    `json:"physical_cores"`
		LogicalCPUs             int    `json:"logical_cpus"`
		MemoryKB                int64  `json:"memory_kb"`
		Architecture            string `json:"architecture"`
		Kernel                  string `json:"kernel"`
		Filesystem              string `json:"filesystem"`
		MountOptions            string `json:"mount_options"`
		StorageDevice           string `json:"storage_device"`
		StorageModel            string `json:"storage_model"`
		Go                      string `json:"go"`
		HostnameRecorded        bool   `json:"hostname_recorded"`
		UsernameRecorded        bool   `json:"username_recorded"`
		DeviceSerialRecorded    bool   `json:"device_serial_recorded"`
		FullMountLayoutRecorded bool   `json:"full_mount_layout_recorded"`
	} `json:"environment"`
	Command          string `json:"command"`
	BenchmarkCommand string `json:"benchmark_command"`
	Configuration    struct {
		LiveRecords     int    `json:"live_records"`
		ValueBytes      int    `json:"value_bytes"`
		Readers         int    `json:"readers"`
		Writers         int    `json:"writers"`
		GetSamples      int    `json:"get_samples"`
		MemorySamples   int    `json:"memory_samples"`
		DurableSamples  int    `json:"durable_samples"`
		LaterWALCommits int    `json:"later_wal_commits"`
		Seed            uint64 `json:"seed"`
		Transport       string `json:"transport"`
		Filesystem      string `json:"filesystem"`
	} `json:"configuration"`
	SocketMode                string                               `json:"socket_mode"`
	SuccessfulDurableBarriers int                                  `json:"successful_durable_barriers"`
	ObservedWALSyncs          int                                  `json:"observed_wal_syncs"`
	Metrics                   map[string]performanceMetricEvidence `json:"metrics"`
	Windows                   map[string]performanceWindowEvidence `json:"windows"`
	Recovery                  struct {
		SnapshotRevision        uint64  `json:"snapshot_revision"`
		LaterWALCommits         int     `json:"later_wal_commits"`
		ReadyMS                 float64 `json:"ready_ms"`
		TargetMS                float64 `json:"target_ms"`
		RecordCountAfterRestart int     `json:"record_count_after_restart"`
		Status                  string  `json:"status"`
	} `json:"recovery"`
	ReferenceAccepted bool     `json:"reference_accepted"`
	VolatileFields    []string `json:"volatile_fields"`
}

const (
	performanceCommand = "GAPDB_REFERENCE_ACCEPTANCE=1 go test ./tests/performance -run TestReferencePerformanceProfile -count=1 -v"
	benchmarkCommand   = "GAPDB_REFERENCE_ACCEPTANCE=1 go test ./tests/performance -run '^$' -bench '^BenchmarkReferenceUnixSocket$' -benchtime=100x -count=1 -benchmem"
)

func validatePerformanceEvidence(path string) (semanticEvidence, error) {
	var value performanceResultEvidence
	if err := decodeStrictFile(path, &value); err != nil {
		return semanticEvidence{}, err
	}
	if value.SchemaVersion != 1 || value.Profile != "reference_acceptance" || value.Command != performanceCommand || value.BenchmarkCommand != benchmarkCommand || value.Configuration.LiveRecords != 100_000 || value.Configuration.ValueBytes != 1_024 || value.Configuration.Readers != 8 || value.Configuration.Writers != 1 || value.Configuration.GetSamples != 2_048 || value.Configuration.MemorySamples != 512 || value.Configuration.DurableSamples != 128 || value.Configuration.LaterWALCommits != 10_000 || value.Configuration.Seed != 0x4741504442504552 || value.Configuration.Transport != "unix_socket" || value.Configuration.Filesystem != "os_fsync_enabled" || value.SocketMode != "0600" {
		return semanticEvidence{}, errors.New("performance workload identity drifted")
	}
	if value.Environment.HostnameRecorded || value.Environment.UsernameRecorded || value.Environment.DeviceSerialRecorded || value.Environment.FullMountLayoutRecorded || value.Environment.CPUModel == "" || value.Environment.PhysicalCores <= 0 || value.Environment.LogicalCPUs < 8 || value.Environment.MemoryKB <= 0 || value.Environment.Architecture == "" || value.Environment.Kernel == "" || value.Environment.Filesystem == "" || value.Environment.StorageDevice == "" || value.Environment.StorageModel == "" || value.Environment.Go == "" {
		return semanticEvidence{}, errors.New("performance environment is incomplete or contains private identifiers")
	}
	expectedMetrics := map[string]struct {
		samples int
		target  float64
		reads   int
		writes  int
	}{"get": {2_048, 2, 2_048, 256}, "memory_put": {512, 5, 4_096, 512}, "durable_put": {128, 50, 1_024, 128}}
	if len(value.Metrics) != len(expectedMetrics) || len(value.Windows) != len(expectedMetrics) {
		return semanticEvidence{}, errors.New("performance metric/window set is incomplete")
	}
	for name, expected := range expectedMetrics {
		metric, ok := value.Metrics[name]
		window, windowOK := value.Windows[name]
		if !ok || !windowOK || metric.Samples != expected.samples || metric.TargetP95MS != expected.target || metric.Status != "pass" || metric.P50MS < 0 || metric.P95MS < metric.P50MS || metric.P99MS < metric.P95MS || metric.P95MS > metric.TargetP95MS || metric.AllocsPerOp <= 0 || metric.AllocsPerOp > 1_000_000 || window.ReadersReady != 8 || window.ReadersActive != 8 || !window.WriterReady || !window.WriterActive || window.ReadOperations != expected.reads || window.WriteOperations != expected.writes {
			return semanticEvidence{}, fmt.Errorf("performance metric/window %q is not authoritative", name)
		}
	}
	if value.SuccessfulDurableBarriers != 151 || value.ObservedWALSyncs != 156 || value.Recovery.SnapshotRevision != 1329 || value.Recovery.LaterWALCommits != 10_000 || value.Recovery.RecordCountAfterRestart != 100_000 || value.Recovery.TargetMS != 5_000 || value.Recovery.ReadyMS < 0 || value.Recovery.ReadyMS > value.Recovery.TargetMS || value.Recovery.Status != "pass" || !value.ReferenceAccepted {
		return semanticEvidence{}, errors.New("performance durability/recovery outcome is not authoritative")
	}
	expectedVolatile := []string{"metrics.*.p50_ms", "metrics.*.p95_ms", "metrics.*.p99_ms", "recovery.ready_ms", "raw Go benchmark ns/op"}
	if strings.Join(value.VolatileFields, "\n") != strings.Join(expectedVolatile, "\n") {
		return semanticEvidence{}, errors.New("performance volatile-field policy drifted")
	}
	return semanticEvidence{SourceCommit: value.CodeCommit, ConfigSHA256: value.ConfigSHA256, Command: value.Command, ObservedCount: int64(value.Configuration.LiveRecords)}, nil
}

type rawReferenceResult struct {
	SchemaVersion             uint16                               `json:"schema_version"`
	Seed                      uint64                               `json:"seed"`
	LiveRecords               int                                  `json:"live_records"`
	ValueBytes                int                                  `json:"value_bytes"`
	Readers                   int                                  `json:"readers"`
	Writers                   int                                  `json:"writers"`
	Transport                 string                               `json:"transport"`
	Filesystem                string                               `json:"filesystem"`
	SocketMode                string                               `json:"socket_mode"`
	SuccessfulDurableBarriers int                                  `json:"successful_durable_barriers"`
	ObservedWALSyncs          int                                  `json:"observed_wal_syncs"`
	Metrics                   map[string]rawMetric                 `json:"metrics"`
	Windows                   map[string]performanceWindowEvidence `json:"windows"`
	Recovery                  rawRecovery                          `json:"recovery"`
	ReferenceAccepted         bool                                 `json:"reference_accepted"`
}

type rawMetric struct {
	Samples      int     `json:"samples"`
	P50MS        float64 `json:"p50_ms"`
	P95MS        float64 `json:"p95_ms"`
	P99MS        float64 `json:"p99_ms"`
	AllocsPerOp  float64 `json:"allocs_per_op"`
	TargetP95MS  float64 `json:"target_p95_ms"`
	TargetPassed bool    `json:"target_passed"`
}

type rawRecovery struct {
	SnapshotRevision uint64  `json:"snapshot_revision"`
	LaterWALCommits  int     `json:"later_wal_commits"`
	ReadyMS          float64 `json:"ready_ms"`
	TargetMS         float64 `json:"target_ms"`
	TargetPassed     bool    `json:"target_passed"`
}

func validateRawEvidence(path string) (semanticEvidence, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return semanticEvidence{}, err
	}
	lines := strings.Split(strings.TrimSuffix(string(encoded), "\n"), "\n")
	if len(lines) != 14 || !strings.HasPrefix(lines[0], "CODE_UNDER_TEST_COMMIT=") || !strings.HasPrefix(lines[1], "CONFIG_SHA256=") || lines[2] != "COMMAND="+performanceCommand || lines[4] != "COMMAND="+benchmarkCommand || lines[5] != "goos: linux" || lines[6] != "goarch: amd64" || lines[7] != "pkg: gapdb/tests/performance" || !strings.HasPrefix(lines[8], "cpu: ") || lines[12] != "PASS" || !strings.HasPrefix(lines[13], "ok gapdb/tests/performance ") {
		return semanticEvidence{}, errors.New("raw evidence authority header/commands drifted")
	}
	if !strings.HasPrefix(lines[3], "REFERENCE_RESULT=") {
		return semanticEvidence{}, errors.New("raw reference result is absent")
	}
	var result rawReferenceResult
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(lines[3], "REFERENCE_RESULT=")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return semanticEvidence{}, fmt.Errorf("raw reference result: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return semanticEvidence{}, fmt.Errorf("raw reference result has trailing JSON: %w", err)
	}
	if result.SchemaVersion != 1 || result.Seed != 0x4741504442504552 || result.LiveRecords != 100_000 || result.ValueBytes != 1_024 || result.Readers != 8 || result.Writers != 1 || result.Transport != "unix_socket" || result.Filesystem != "os_fsync_enabled" || result.SocketMode != "0600" || result.SuccessfulDurableBarriers != 151 || result.ObservedWALSyncs != 156 || result.Recovery.SnapshotRevision != 1329 || result.Recovery.LaterWALCommits != 10_000 || result.Recovery.TargetMS != 5_000 || result.Recovery.ReadyMS < 0 || result.Recovery.ReadyMS > result.Recovery.TargetMS || !result.Recovery.TargetPassed || !result.ReferenceAccepted {
		return semanticEvidence{}, errors.New("raw reference result semantics drifted")
	}
	expectedRaw := map[string]struct {
		samples, reads, writes int
		target                 float64
	}{"get": {2_048, 2_048, 256, 2}, "memory_put": {512, 4_096, 512, 5}, "durable_put": {128, 1_024, 128, 50}}
	if len(result.Metrics) != 3 || len(result.Windows) != 3 {
		return semanticEvidence{}, errors.New("raw metric/window set is incomplete")
	}
	for name, expected := range expectedRaw {
		metric, metricOK := result.Metrics[name]
		window, windowOK := result.Windows[name]
		if !metricOK || !windowOK || metric.Samples != expected.samples || metric.TargetP95MS != expected.target || !metric.TargetPassed || metric.P50MS < 0 || metric.P95MS < metric.P50MS || metric.P99MS < metric.P95MS || metric.P95MS > metric.TargetP95MS || metric.AllocsPerOp <= 0 || metric.AllocsPerOp > 1_000_000 || window.ReadersReady != 8 || window.ReadersActive != 8 || !window.WriterReady || !window.WriterActive || window.ReadOperations != expected.reads || window.WriteOperations != expected.writes {
			return semanticEvidence{}, fmt.Errorf("raw metric/window %q drifted", name)
		}
	}
	benchmarks := map[string]bool{"BenchmarkReferenceUnixSocket/Get-12": false, "BenchmarkReferenceUnixSocket/memory_put-12": false, "BenchmarkReferenceUnixSocket/durable_put-12": false}
	for _, line := range lines[5:] {
		for name := range benchmarks {
			if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"\t") {
				benchmarks[name] = strings.Contains(line, " ns/op") && strings.Contains(line, " B/op") && strings.Contains(line, " allocs/op")
			}
		}
	}
	for name, valid := range benchmarks {
		if !valid {
			return semanticEvidence{}, fmt.Errorf("raw benchmark %q absent or malformed", name)
		}
	}
	return semanticEvidence{SourceCommit: strings.TrimPrefix(lines[0], "CODE_UNDER_TEST_COMMIT="), ConfigSHA256: strings.TrimPrefix(lines[1], "CONFIG_SHA256="), Command: performanceCommand, ObservedCount: 3}, nil
}

type adoptionResultEvidence struct {
	SchemaVersion   uint16 `json:"schema_version"`
	ContractVersion string `json:"contract_version"`
	CodeCommit      string `json:"code_commit"`
	ConfigSHA256    string `json:"config_sha256"`
	Command         string `json:"command"`
	Gapdb           struct {
		Status     string   `json:"status"`
		Applicable int      `json:"applicable"`
		Scenarios  []string `json:"scenarios"`
	} `json:"gapdb"`
	SQLite struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"sqlite"`
	Safety            SafetyEvidence `json:"safety"`
	TechnicalGates    string         `json:"technical_gates"`
	ProductionBackend string         `json:"production_backend"`
	AdoptionStatus    string         `json:"adoption_status"`
	HumanApproval     HumanApproval  `json:"human_approval"`
}

const adoptionCommand = "go test ./tests/adoption -run TestGapdbAdapterPassesAllApplicableScenarios -count=1"

func validateAdoptionEvidence(path string) (semanticEvidence, error) {
	var value adoptionResultEvidence
	if err := decodeStrictFile(path, &value); err != nil {
		return semanticEvidence{}, err
	}
	expectedScenarios := ScenarioIDs()
	actual := append([]string(nil), value.Gapdb.Scenarios...)
	sort.Strings(actual)
	if value.SchemaVersion != 1 || value.ContractVersion != ContractVersion || value.Command != adoptionCommand || value.Gapdb.Status != "pass" || value.Gapdb.Applicable != ScenarioCount || strings.Join(actual, "\n") != strings.Join(expectedScenarios, "\n") || value.SQLite.Status != "pending" || value.SQLite.Reason == "" || !value.Safety.complete() || value.TechnicalGates != "incomplete" || value.ProductionBackend != "sqlite" || value.AdoptionStatus != "not_approved" || value.HumanApproval.Approved || value.HumanApproval.Source != "external_signed_record_required" {
		return semanticEvidence{}, errors.New("adoption authority is incomplete, vacuous, or unsafe")
	}
	return semanticEvidence{SourceCommit: value.CodeCommit, ConfigSHA256: value.ConfigSHA256, Command: value.Command, ObservedCount: int64(value.Gapdb.Applicable)}, nil
}

// Crash evidence is inherited from WP08 and remains bound to its own source
// commit. Every nested field is decoded strictly before the release gate uses it.
type crashResultEvidence struct {
	SchemaVersion       uint16 `json:"schema_version"`
	CodeUnderTestCommit string `json:"code_under_test_commit"`
	Profile             string `json:"profile"`
	Environment         struct {
		Go                      string `json:"go"`
		Kernel                  string `json:"kernel"`
		Filesystem              string `json:"filesystem"`
		LogicalCPUs             int    `json:"logical_cpus"`
		MemoryKB                int64  `json:"memory_kb"`
		HostIdentifiersRecorded bool   `json:"host_identifiers_recorded"`
	} `json:"environment"`
	FaultMatrix struct {
		Status                 string         `json:"status"`
		Command                string         `json:"command"`
		ScheduleCount          int            `json:"schedule_count"`
		PointPhasePairs        int            `json:"point_phase_pairs"`
		NamedPoints            int            `json:"named_points"`
		Categories             []string       `json:"categories"`
		AcknowledgementClasses []string       `json:"acknowledgement_classes"`
		ObservedClassCounts    map[string]int `json:"observed_class_counts"`
		InjectedClassCounts    map[string]int `json:"injected_operation_class_counts"`
		FirstSeed              uint64         `json:"first_seed"`
		LastSeed               uint64         `json:"last_seed"`
		DurationSeconds        float64        `json:"duration_seconds"`
		ObservationSource      string         `json:"observation_source"`
		AttemptedRevisionProbe struct {
			PreviousReservedEnd        uint64 `json:"previous_reserved_revision_end"`
			AttemptedRecoveredRevision uint64 `json:"actual_attempted_and_recovered_revision"`
			PostRecoveryRevision       uint64 `json:"post_recovery_revision"`
			ReuseRejected              bool   `json:"reuse_rejected"`
		} `json:"attempted_revision_probe"`
		PublicEffectRevisionMatch bool `json:"public_effect_revision_match_required"`
		SuccessfulResponseBinding struct {
			Expected              string `json:"expected"`
			ExactResponseRevision bool   `json:"exact_response_revision_preserved"`
			MismatchesRejected    bool   `json:"zero_lower_higher_and_overflow_mismatches_rejected"`
		} `json:"successful_response_revision_binding"`
		SmallestReproducerFormat string `json:"smallest_reproducer_format"`
	} `json:"fault_matrix"`
	Subprocess struct {
		Status                          string   `json:"status"`
		BinaryBuildTag                  string   `json:"binary_build_tag"`
		ReleaseBinaryHasHook            bool     `json:"release_binary_has_process_stop_hook"`
		ExternalLedgerSynced            bool     `json:"external_ledger_synced"`
		ExternalLedgerStrict            bool     `json:"external_ledger_strictly_decoded_into_oracle"`
		ExternalLedgerChain             bool     `json:"external_ledger_sha256_chain"`
		ExternalLedgerTerminalDigest    bool     `json:"external_ledger_terminal_digest_separately_synced"`
		TerminalDigestRetained          bool     `json:"terminal_digest_retained_outside_artifacts_by_parent"`
		RequestedAckRetained            bool     `json:"requested_acknowledgement_retained_outside_artifacts_by_parent"`
		BadAnchorsRejected              bool     `json:"absent_stale_and_wrong_retained_anchors_rejected"`
		CoherentRevisionRewriteRejected bool     `json:"coherent_revision_rewrite_rejected_by_public_record_revision"`
		CoherentAckRewriteRejected      bool     `json:"coherent_durable_to_memory_rewrite_rejected_by_requested_acknowledgement"`
		RechainedTamperRejected         bool     `json:"re_chained_tamper_rejected_by_terminal_digest"`
		PublicRecordRevisionReconciled  bool     `json:"public_record_revision_reconciled"`
		LedgerTamperRejected            bool     `json:"ledger_tamper_and_contradiction_rejected"`
		HookTerminationSignal           string   `json:"hook_termination_signal"`
		ExactSignalAsserted             bool     `json:"exact_signal_asserted"`
		Milestones                      []string `json:"milestones"`
	} `json:"subprocess"`
	RaceStress struct {
		Status                            string `json:"status"`
		Readers                           int    `json:"readers"`
		Mutations                         int    `json:"mutations"`
		ConditionContenders               int    `json:"condition_contenders"`
		ObservedWatchRevision             uint64 `json:"observed_watch_revision"`
		ExactWatchSequence                bool   `json:"exact_watch_sequence"`
		WatchLagForced                    bool   `json:"watch_lag_forced"`
		WatchLagTerminal                  bool   `json:"watch_lag_terminal_evidence_checked"`
		WriterAndWatchClientsSeparate     bool   `json:"writer_and_watch_clients_separate"`
		AcceptedWatchDeadline             bool   `json:"accepted_watch_deadline_checked"`
		DisconnectChecked                 bool   `json:"disconnect_checked"`
		ShutdownAcknowledgementsRecovered bool   `json:"concurrent_shutdown_acknowledgements_recovered"`
	} `json:"race_stress"`
	SafetyEnvelope struct {
		Status                      string   `json:"status"`
		UmaskCases                  int      `json:"umask_cases"`
		LimitsAtMaxAndMaxPlusOne    bool     `json:"configured_limits_at_max_and_max_plus_one"`
		LimitsExercised             []string `json:"configured_limits_exercised"`
		TotalRequestFrameBoundary   bool     `json:"total_request_frame_boundary_checked"`
		DiagnosticPathLimit         bool     `json:"diagnostic_path_limit_truncation_checked"`
		DegradedReadInspection      bool     `json:"degraded_read_and_inspection_evidence"`
		UnsupportedFormatNoMutation bool     `json:"unsupported_format_no_mutation"`
		OwnershipConflictNoMutation bool     `json:"ownership_conflict_no_mutation"`
	} `json:"safety_envelope"`
	FullTests struct {
		Normal gateResult `json:"normal"`
		Race   gateResult `json:"race"`
	} `json:"full_tests"`
	Fuzz []struct {
		Target          string  `json:"target"`
		Status          string  `json:"status"`
		DurationSeconds float64 `json:"duration_seconds"`
		Executions      int     `json:"executions"`
	} `json:"fuzz"`
	StaticGates struct {
		GoVet        string `json:"go_vet"`
		Staticcheck  string `json:"staticcheck"`
		Govulncheck  string `json:"govulncheck"`
		GoModVerify  string `json:"go_mod_verify"`
		Gofmt        string `json:"gofmt"`
		GitDiffCheck string `json:"git_diff_check"`
	} `json:"static_gates"`
	PlatformSkips []string `json:"platform_skips"`
	FlakyRetries  int      `json:"flaky_retries"`
}

type gateResult struct {
	Status  string `json:"status"`
	Command string `json:"command"`
}

const crashCommand = "go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1 -v"

func validateCrashEvidence(path string) (semanticEvidence, error) {
	var value crashResultEvidence
	if err := decodeStrictFile(path, &value); err != nil {
		return semanticEvidence{}, err
	}
	fault := value.FaultMatrix
	expectedCategories := []string{"ownership", "identity", "wal", "recovery", "snapshot", "manifest", "compaction", "backup", "audit", "apply", "response"}
	if value.SchemaVersion != 1 || value.Profile != "acceptance" || value.Environment.Go == "" || value.Environment.Kernel == "" || value.Environment.Filesystem == "" || value.Environment.LogicalCPUs < 8 || value.Environment.MemoryKB <= 0 || value.Environment.HostIdentifiersRecorded || fault.Status != "pass" || fault.Command != crashCommand || fault.ScheduleCount != 1_024 || fault.PointPhasePairs != 78 || fault.NamedPoints != 39 || !sameStringSet(fault.Categories, expectedCategories) || strings.Join(fault.AcknowledgementClasses, ",") != "durable,memory,unacknowledged" || len(fault.ObservedClassCounts) != 3 || len(fault.InjectedClassCounts) != 3 || fault.ObservedClassCounts["durable"] <= 0 || fault.ObservedClassCounts["memory"] <= 0 || fault.ObservedClassCounts["unacknowledged"] <= 0 || fault.InjectedClassCounts["durable"] <= 0 || fault.InjectedClassCounts["memory"] <= 0 || fault.InjectedClassCounts["unacknowledged"] <= 0 || fault.FirstSeed == 0 || fault.LastSeed == 0 || fault.DurationSeconds <= 0 || fault.ObservationSource == "" || !fault.AttemptedRevisionProbe.ReuseRejected || fault.AttemptedRevisionProbe.AttemptedRecoveredRevision != fault.AttemptedRevisionProbe.PreviousReservedEnd+1 || fault.AttemptedRevisionProbe.PostRecoveryRevision <= fault.AttemptedRevisionProbe.AttemptedRecoveredRevision || !fault.PublicEffectRevisionMatch || fault.SuccessfulResponseBinding.Expected != "reserved_revision_end + 1" || !fault.SuccessfulResponseBinding.ExactResponseRevision || !fault.SuccessfulResponseBinding.MismatchesRejected || fault.SmallestReproducerFormat == "" {
		return semanticEvidence{}, errors.New("crash schedule authority drifted")
	}
	subprocess := value.Subprocess
	expectedMilestones := []string{"wal_frame_write", "wal_buffer_flush", "wal_file_sync", "map_apply", "response_publish", "snapshot_rename", "manifest_rename", "stale_socket_recovery", "ownership_conflict", "graceful_shutdown_drain"}
	if subprocess.Status != "pass" || subprocess.BinaryBuildTag != "gapdb_crash_evidence" || subprocess.ReleaseBinaryHasHook || !subprocess.ExternalLedgerSynced || !subprocess.ExternalLedgerStrict || !subprocess.ExternalLedgerChain || !subprocess.ExternalLedgerTerminalDigest || !subprocess.TerminalDigestRetained || !subprocess.RequestedAckRetained || !subprocess.BadAnchorsRejected || !subprocess.CoherentRevisionRewriteRejected || !subprocess.CoherentAckRewriteRejected || !subprocess.RechainedTamperRejected || !subprocess.PublicRecordRevisionReconciled || !subprocess.LedgerTamperRejected || subprocess.HookTerminationSignal != "SIGKILL" || !subprocess.ExactSignalAsserted || !sameStringSet(subprocess.Milestones, expectedMilestones) {
		return semanticEvidence{}, errors.New("crash subprocess authority drifted")
	}
	race := value.RaceStress
	if race.Status != "pass" || race.Readers < 8 || race.Mutations <= 0 || race.ConditionContenders <= 1 || race.ObservedWatchRevision == 0 || !race.ExactWatchSequence || !race.WatchLagForced || !race.WatchLagTerminal || !race.WriterAndWatchClientsSeparate || !race.AcceptedWatchDeadline || !race.DisconnectChecked || !race.ShutdownAcknowledgementsRecovered {
		return semanticEvidence{}, errors.New("race stress authority drifted")
	}
	safety := value.SafetyEnvelope
	expectedLimits := []string{"max_key_bytes", "max_value_bytes", "max_frame_bytes", "max_batch_bytes", "max_batch_operations", "max_scan_records", "max_scan_bytes", "watch_buffer_events", "max_watch_clients", "max_concurrent_clients", "max_history_events", "max_history_bytes"}
	if safety.Status != "pass" || safety.UmaskCases != 4 || !safety.LimitsAtMaxAndMaxPlusOne || !sameStringSet(safety.LimitsExercised, expectedLimits) || !safety.TotalRequestFrameBoundary || !safety.DiagnosticPathLimit || !safety.DegradedReadInspection || !safety.UnsupportedFormatNoMutation || !safety.OwnershipConflictNoMutation {
		return semanticEvidence{}, errors.New("safety envelope authority drifted")
	}
	if value.FullTests.Normal != (gateResult{"pass", "go test -count=1 ./..."}) || value.FullTests.Race != (gateResult{"pass", "go test -race -count=1 ./..."}) || len(value.Fuzz) != 5 || value.StaticGates.GoVet != "pass" || value.StaticGates.Staticcheck != "pass" || value.StaticGates.Govulncheck != "pass_no_vulnerabilities" || value.StaticGates.GoModVerify != "pass" || value.StaticGates.Gofmt != "pass" || value.StaticGates.GitDiffCheck != "pass" || len(value.PlatformSkips) != 0 || value.FlakyRetries != 0 {
		return semanticEvidence{}, errors.New("crash gate authority drifted")
	}
	fuzzTargets := make([]string, 0, len(value.Fuzz))
	for _, fuzz := range value.Fuzz {
		if fuzz.Target == "" || fuzz.Status != "pass" || fuzz.DurationSeconds <= 0 || fuzz.Executions <= 0 {
			return semanticEvidence{}, errors.New("crash fuzz authority drifted")
		}
		fuzzTargets = append(fuzzTargets, fuzz.Target)
	}
	expectedFuzz := []string{"FuzzDecodeSnapshotNeverPanics", "FuzzStorageDecoders", "FuzzWireAndStorageDecoders", "FuzzScanCursorDecoder", "FuzzBackupVerifierDoesNotMutate"}
	if !sameStringSet(fuzzTargets, expectedFuzz) {
		return semanticEvidence{}, errors.New("crash fuzz target set drifted")
	}
	return semanticEvidence{SourceCommit: value.CodeUnderTestCommit, Command: fault.Command, ObservedCount: int64(fault.ScheduleCount)}, nil
}

func sameStringSet(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	left, right := append([]string(nil), actual...), append([]string(nil), expected...)
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\n") == strings.Join(right, "\n")
}

var protocolAuthorityTokens = []string{
	"../../internal/protocol", "../../gapdb/client.go", "requests.golden.jsonl", "success.golden.jsonl", "errors.golden.jsonl", "negative.golden.jsonl",
	"get", "get_many", "put", "put_if_absent", "compare_and_swap", "delete_if_revision", "atomic_batch", "scan_prefix", "watch",
	"status", "health", "stats", "describe_config", "verify", "create_snapshot", "compact", "backup",
	"offline_inspect", "offline_verify", "offline_recover_propose", "offline_recover_apply", "Memory acknowledgement", "not durable", "0600",
}

var storageAuthorityTokens = []string{
	"../../internal/persist", "../../tests/compatibility/storage/testdata/README.md", "GAPID001", "GAPCUR01", "GAPWAL01", "GAPSNAP1", "CRC32C", "SHA-256", "0700", "0600", "incomplete final frame",
}

func validateDocumentEvidence(path string, required []string, reference EvidenceReference) (semanticEvidence, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return semanticEvidence{}, err
	}
	text := string(encoded)
	for _, token := range required {
		if !strings.Contains(text, token) {
			return semanticEvidence{}, fmt.Errorf("document authority token %q is absent", token)
		}
	}
	return semanticEvidence{SourceCommit: reference.SourceCommit, ConfigSHA256: reference.ConfigSHA256, Command: reference.Command, ObservedCount: int64(len(required))}, nil
}
