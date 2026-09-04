package adoption_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	qualifiedAssertionCommit    = "8de0f7f549791d0299e84a15bbe6b256c8c7afdd"
	qualifiedAssertionTree      = "b61245808fda7d621f5d037c5e97d83fb2c82886"
	assertionSignerPublicKeyB64 = "om7MgdYehWovo5FHX/lOtwzxoUWBLt2zaavqnnpbmrg="
	assertionMaxEvidenceBytes   = int64(1 << 20)
)

var assertionEvidenceFiles = []string{
	"external-consumption.json",
	"gates.json",
	"manifest.json",
	"manifest.sig",
	"performance.json",
	"qualification.json",
}

type assertionManifestFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type assertionEvidenceManifest struct {
	SchemaVersion   uint16                  `json:"schema_version"`
	CandidateCommit string                  `json:"candidate_commit"`
	CandidateTree   string                  `json:"candidate_tree"`
	MaxFileBytes    int64                   `json:"max_file_bytes"`
	Files           []assertionManifestFile `json:"files"`
}

type assertionQualificationEvidence struct {
	SchemaVersion   uint16 `json:"schema_version"`
	CandidateCommit string `json:"candidate_commit"`
	CandidateTree   string `json:"candidate_tree"`
	FrozenAdoption  struct {
		ContractVersion string `json:"contract_version"`
		ScenarioCount   int    `json:"scenario_count"`
		ScenariosPassed int    `json:"scenarios_passed"`
		SQLiteDecision  string `json:"sqlite_decision"`
	} `json:"frozen_adoption"`
	Contention struct {
		Trials         int      `json:"trials"`
		RevisionTrials int      `json:"revision_trials"`
		AbsenceTrials  int      `json:"absence_trials"`
		GuardedFirst   int      `json:"guarded_first"`
		AuthorityFirst int      `json:"authority_first"`
		StaleEffects   int      `json:"stale_effects"`
		MutantsKilled  []string `json:"mutants_killed"`
		RaceEnabled    bool     `json:"race_enabled"`
		RetryCount     int      `json:"retry_count"`
	} `json:"contention"`
	Crash struct {
		PredicateSIGKILLSchedules          int  `json:"predicate_sigkill_schedules"`
		DurableSIGKILLSchedules            int  `json:"durable_sigkill_schedules"`
		NormalRestartRecovery              bool `json:"normal_restart_recovery"`
		RawResponseLossRecovered           bool `json:"raw_response_loss_recovered"`
		FailedPredicateZeroEffect          bool `json:"failed_predicate_zero_effect"`
		WatchLiveMutationOnly              bool `json:"watch_live_mutation_only"`
		WatchReplayMutationOnly            bool `json:"watch_replay_mutation_only"`
		TrailingAssertionEventMutantKilled bool `json:"trailing_assertion_event_mutant_killed"`
	} `json:"crash"`
	Bounds struct {
		PublicOperations         bool `json:"public_operations_l_minus_1_l_l_plus_1"`
		PublicBytes              bool `json:"public_bytes_l_minus_1_l_l_plus_1"`
		ProtocolJSON             bool `json:"protocol_json_l_minus_1_l_l_plus_1"`
		ZeroEffectBeforeRestart  bool `json:"rejection_zero_effect_before_restart"`
		ZeroEffectAfterRestart   bool `json:"rejection_zero_effect_after_restart"`
		WatchHistoryZeroEffect   bool `json:"watch_history_zero_effect"`
		ExpiryMetadataZeroEffect bool `json:"expiry_metadata_zero_effect"`
		AfterEffectMutantKilled  bool `json:"after_effect_mutant_killed"`
		OffByOneMutantKilled     bool `json:"off_by_one_mutant_killed"`
	} `json:"bounds"`
	Performance struct {
		RawArtifact     string `json:"raw_artifact"`
		StabilityRuns   int    `json:"stability_runs"`
		StabilityPassed int    `json:"stability_passed"`
		RetryCount      int    `json:"retry_count"`
		Windows         []struct {
			Window               int   `json:"window"`
			MemoryControlP95NS   int64 `json:"memory_control_p95_ns"`
			MemoryAssertedP95NS  int64 `json:"memory_asserted_p95_ns"`
			MemoryAllowanceNS    int64 `json:"memory_allowance_ns"`
			DurableControlP95NS  int64 `json:"durable_control_p95_ns"`
			DurableAssertedP95NS int64 `json:"durable_asserted_p95_ns"`
		} `json:"windows"`
	} `json:"performance"`
}

type assertionPerformanceEvidence struct {
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
	Windows []struct {
		Window            int     `json:"window"`
		MemoryControlNS   []int64 `json:"memory_control_ns"`
		MemoryAssertedNS  []int64 `json:"memory_asserted_ns"`
		DurableControlNS  []int64 `json:"durable_control_ns"`
		DurableAssertedNS []int64 `json:"durable_asserted_ns"`
	} `json:"windows"`
}

type assertionExternalEvidence struct {
	SchemaVersion        uint16 `json:"schema_version"`
	CandidateCommit      string `json:"candidate_commit"`
	CandidateTree        string `json:"candidate_tree"`
	RemoteRef            string `json:"remote_ref"`
	RemoteSHA            string `json:"remote_sha"`
	Module               string `json:"module"`
	ResolvedVersion      string `json:"resolved_version"`
	ModuleSum            string `json:"module_sum"`
	GoModSum             string `json:"go_mod_sum"`
	GoWork               string `json:"gowork"`
	GoProxy              string `json:"goproxy"`
	FreshModuleCache     bool   `json:"fresh_module_cache"`
	FreshBuildCache      bool   `json:"fresh_build_cache"`
	FreshBinaryDirectory bool   `json:"fresh_binary_directory"`
	Replace              bool   `json:"replace"`
	InstalledDaemon      bool   `json:"installed_daemon"`
	PassingBatch         bool   `json:"passing_batch"`
	StaleBatchRefused    bool   `json:"stale_batch_refused"`
	StaleErrorCode       string `json:"stale_error_code"`
	StaleTargetAbsent    bool   `json:"stale_target_absent"`
	ProgramOutput        string `json:"program_output"`
	RetryCount           int    `json:"retry_count"`
}

type assertionGatesEvidence struct {
	SchemaVersion   uint16 `json:"schema_version"`
	CandidateCommit string `json:"candidate_commit"`
	CandidateTree   string `json:"candidate_tree"`
	GoVersion       string `json:"go_version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	Kernel          string `json:"kernel"`
	Gates           []struct {
		Command string `json:"command"`
		Status  string `json:"status"`
		Output  string `json:"output,omitempty"`
	} `json:"gates"`
}

func TestAssertionQualificationEvidenceUsesCallerPinnedTrustAndClosedArtifacts(t *testing.T) {
	directory := assertionEvidenceDirectory(t)
	if err := validateAssertionEvidenceBundle(directory); err != nil {
		t.Fatal(err)
	}
}

func TestAssertionEvidenceBundleRejectsComposedTrustAndFilesystemMutants(t *testing.T) {
	source := assertionEvidenceDirectory(t)
	mutants := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"deleted_artifact", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, "gates.json")) }},
		{"extra_artifact", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, "extra.json"), []byte("{}\n")) }},
		{"substituted_filename", func(t *testing.T, root string) {
			mustRename(t, filepath.Join(root, "gates.json"), filepath.Join(root, "renamed.json"))
		}},
		{"symlinked_artifact", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "performance.json"))
			if err := os.Symlink("qualification.json", filepath.Join(root, "performance.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"altered_and_rehashed", func(t *testing.T, root string) {
			path := filepath.Join(root, "gates.json")
			mustWrite(t, path, append(mustRead(t, path), ' '))
			rehashManifestFile(t, root, "gates.json")
		}},
		{"signature_substitution", func(t *testing.T, root string) {
			path := filepath.Join(root, "manifest.sig")
			signature := mustRead(t, path)
			signature[0] = 'A'
			mustWrite(t, path, signature)
		}},
		{"replacement_key_and_signature", func(t *testing.T, root string) { resignManifestWithReplacementKey(t, root) }},
		{"coherently_resigned_tamper", func(t *testing.T, root string) {
			path := filepath.Join(root, "external-consumption.json")
			var value assertionExternalEvidence
			decodeStrictBytes(mustRead(t, path), &value)
			value.Replace = true
			mustWriteJSON(t, path, value)
			rehashManifestFile(t, root, "external-consumption.json")
			resignManifestWithReplacementKey(t, root)
		}},
	}
	for _, mutant := range mutants {
		t.Run(mutant.name, func(t *testing.T) {
			root := copyAssertionEvidenceDirectory(t, source)
			mutant.mutate(t, root)
			if err := validateAssertionEvidenceBundle(root); err == nil {
				t.Fatal("evidence mutant survived")
			}
		})
	}
}

func TestAssertionQualificationSemanticMutantsAreRejected(t *testing.T) {
	root := assertionEvidenceDirectory(t)
	var valid assertionQualificationEvidence
	decodeStrictBytes(mustRead(t, filepath.Join(root, "qualification.json")), &valid)
	mutants := []struct {
		name   string
		mutate func(*assertionQualificationEvidence)
	}{
		{"trials_999", func(value *assertionQualificationEvidence) { value.Contention.Trials = 999 }},
		{"one_stale_effect", func(value *assertionQualificationEvidence) { value.Contention.StaleEffects = 1 }},
		{"mutant_not_killed", func(value *assertionQualificationEvidence) {
			value.Contention.MutantsKilled = value.Contention.MutantsKilled[:2]
		}},
		{"bad_history_count", func(value *assertionQualificationEvidence) { value.Contention.GuardedFirst++ }},
		{"bad_p95_math", func(value *assertionQualificationEvidence) { value.Performance.Windows[0].MemoryControlP95NS++ }},
		{"bad_p95_threshold", func(value *assertionQualificationEvidence) {
			value.Performance.Windows[0].MemoryAssertedP95NS += 200000
		}},
		{"stale_commit", func(value *assertionQualificationEvidence) { value.CandidateCommit = strings.Repeat("0", 40) }},
		{"stale_tree", func(value *assertionQualificationEvidence) { value.CandidateTree = strings.Repeat("0", 40) }},
		{"retry_count", func(value *assertionQualificationEvidence) { value.Performance.RetryCount = 1 }},
	}
	var performance assertionPerformanceEvidence
	decodeStrictBytes(mustRead(t, filepath.Join(root, "performance.json")), &performance)
	for _, mutant := range mutants {
		t.Run(mutant.name, func(t *testing.T) {
			candidate := cloneJSON(t, valid)
			mutant.mutate(&candidate)
			if err := validateAssertionQualification(candidate, performance); err == nil {
				t.Fatal("semantic evidence mutant survived")
			}
		})
	}
}

func validateAssertionEvidenceBundle(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if !slices.Equal(names, assertionEvidenceFiles) {
		return fmt.Errorf("evidence filename set=%v", names)
	}
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > assertionMaxEvidenceBytes {
			return fmt.Errorf("evidence file %s is absent, redirected, non-regular, empty, or oversized", name)
		}
	}
	manifestBytes, err := readBounded(filepath.Join(root, "manifest.json"), 64<<10)
	if err != nil {
		return err
	}
	var manifest assertionEvidenceManifest
	if err := decodeStrictBytes(manifestBytes, &manifest); err != nil {
		return err
	}
	canonicalManifest, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(manifestBytes, append(canonicalManifest, '\n')) {
		return errors.New("manifest is not canonical JSON")
	}
	if manifest.SchemaVersion != 2 || manifest.CandidateCommit != qualifiedAssertionCommit || manifest.CandidateTree != qualifiedAssertionTree || manifest.MaxFileBytes != assertionMaxEvidenceBytes || len(manifest.Files) != 4 {
		return errors.New("manifest authority is stale or incomplete")
	}
	wantArtifacts := []string{"external-consumption.json", "gates.json", "performance.json", "qualification.json"}
	for index, reference := range manifest.Files {
		if reference.Name != wantArtifacts[index] || reference.Bytes <= 0 || reference.Bytes > manifest.MaxFileBytes || len(reference.SHA256) != 64 {
			return errors.New("manifest artifact set is not exact, ordered, and bounded")
		}
		contents, err := readBounded(filepath.Join(root, reference.Name), manifest.MaxFileBytes)
		if err != nil || int64(len(contents)) != reference.Bytes {
			return fmt.Errorf("artifact %s length: %w", reference.Name, err)
		}
		digest := sha256.Sum256(contents)
		if hex.EncodeToString(digest[:]) != reference.SHA256 {
			return fmt.Errorf("artifact %s digest mismatch", reference.Name)
		}
	}
	publicKey, err := base64.StdEncoding.DecodeString(assertionSignerPublicKeyB64)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("caller-pinned assertion evidence key is invalid")
	}
	signatureText, err := readBounded(filepath.Join(root, "manifest.sig"), 256)
	if err != nil || bytes.Count(signatureText, []byte{'\n'}) > 1 {
		return errors.New("manifest signature encoding is invalid")
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(string(signatureText), "\n"))
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(publicKey), manifestBytes, signature) {
		return errors.New("manifest is not signed by the caller-pinned authority")
	}
	var qualification assertionQualificationEvidence
	var performance assertionPerformanceEvidence
	var external assertionExternalEvidence
	var gates assertionGatesEvidence
	if err := decodeStrictBytes(mustReadPath(filepath.Join(root, "qualification.json")), &qualification); err != nil {
		return err
	}
	if err := decodeStrictBytes(mustReadPath(filepath.Join(root, "performance.json")), &performance); err != nil {
		return err
	}
	if err := decodeStrictBytes(mustReadPath(filepath.Join(root, "external-consumption.json")), &external); err != nil {
		return err
	}
	if err := decodeStrictBytes(mustReadPath(filepath.Join(root, "gates.json")), &gates); err != nil {
		return err
	}
	if err := validateAssertionQualification(qualification, performance); err != nil {
		return err
	}
	if err := validateAssertionExternal(external); err != nil {
		return err
	}
	return validateAssertionGates(gates)
}

func validateAssertionQualification(value assertionQualificationEvidence, performance assertionPerformanceEvidence) error {
	if value.SchemaVersion != 2 || value.CandidateCommit != qualifiedAssertionCommit || value.CandidateTree != qualifiedAssertionTree {
		return errors.New("qualification candidate is stale")
	}
	if value.FrozenAdoption.ContractVersion != "gapdb-phase1/v1" || value.FrozenAdoption.ScenarioCount != 14 || value.FrozenAdoption.ScenariosPassed != 14 || value.FrozenAdoption.SQLiteDecision != "production_pending_external_comparison_and_signed_human_approval" {
		return errors.New("frozen adoption boundary changed")
	}
	wantMutants := []string{"M-ASSERTION-OUTSIDE-WRITER", "M-MUTATE-THEN-ERROR", "M-SUCCESS-WITHOUT-PUBLICATION"}
	if value.Contention.Trials != 1000 || value.Contention.RevisionTrials != 500 || value.Contention.AbsenceTrials != 500 || value.Contention.GuardedFirst <= 0 || value.Contention.AuthorityFirst <= 0 || value.Contention.GuardedFirst+value.Contention.AuthorityFirst != value.Contention.Trials || value.Contention.StaleEffects != 0 || !slices.Equal(value.Contention.MutantsKilled, wantMutants) || !value.Contention.RaceEnabled || value.Contention.RetryCount != 0 {
		return errors.New("contention qualification is incomplete")
	}
	if value.Crash.PredicateSIGKILLSchedules != 4 || value.Crash.DurableSIGKILLSchedules != 10 || !value.Crash.NormalRestartRecovery || !value.Crash.RawResponseLossRecovered || !value.Crash.FailedPredicateZeroEffect || !value.Crash.WatchLiveMutationOnly || !value.Crash.WatchReplayMutationOnly || !value.Crash.TrailingAssertionEventMutantKilled {
		return errors.New("crash/watch qualification is incomplete")
	}
	if !value.Bounds.PublicOperations || !value.Bounds.PublicBytes || !value.Bounds.ProtocolJSON || !value.Bounds.ZeroEffectBeforeRestart || !value.Bounds.ZeroEffectAfterRestart || !value.Bounds.WatchHistoryZeroEffect || !value.Bounds.ExpiryMetadataZeroEffect || !value.Bounds.AfterEffectMutantKilled || !value.Bounds.OffByOneMutantKilled {
		return errors.New("boundary qualification is incomplete")
	}
	if value.Performance.RawArtifact != "performance.json" || value.Performance.StabilityRuns != 11 || value.Performance.StabilityPassed != 11 || value.Performance.RetryCount != 0 {
		return errors.New("performance stability qualification is incomplete")
	}
	return validateAssertionPerformance(performance, value.Performance.Windows)
}

func validateAssertionPerformance(raw assertionPerformanceEvidence, summaries []struct {
	Window               int   `json:"window"`
	MemoryControlP95NS   int64 `json:"memory_control_p95_ns"`
	MemoryAssertedP95NS  int64 `json:"memory_asserted_p95_ns"`
	MemoryAllowanceNS    int64 `json:"memory_allowance_ns"`
	DurableControlP95NS  int64 `json:"durable_control_p95_ns"`
	DurableAssertedP95NS int64 `json:"durable_asserted_p95_ns"`
}) error {
	config := raw.Configuration
	if raw.SchemaVersion != 1 || config.Windows != 3 || config.WarmupPairs != 256 || config.MemorySamples != 2048 || config.DurableSamples != 256 || config.ValueBytes != 5 || config.ControlTokenBytes != 8 || config.AssertedTokenBytes != 8 || config.MemoryRelativeLimit != 0.15 || config.MemoryMinimumNanos != 100000 || config.DurableMaximumNanos != 4000000 || config.Quantile != "nearest_rank_p95" || config.PairOrder != "alternating_control_first_asserted_first" || config.Transport != "public_unix_socket" || config.AcknowledgementModes != "memory_and_durable" || len(raw.Windows) != 3 || len(summaries) != 3 {
		return errors.New("performance configuration is incomplete")
	}
	for index, window := range raw.Windows {
		if window.Window != index || summaries[index].Window != index || len(window.MemoryControlNS) != config.MemorySamples || len(window.MemoryAssertedNS) != config.MemorySamples || len(window.DurableControlNS) != config.DurableSamples || len(window.DurableAssertedNS) != config.DurableSamples {
			return fmt.Errorf("performance window %d sample census mismatch", index)
		}
		for _, samples := range [][]int64{window.MemoryControlNS, window.MemoryAssertedNS, window.DurableControlNS, window.DurableAssertedNS} {
			for _, sample := range samples {
				if sample <= 0 || sample > int64(60e9) {
					return fmt.Errorf("performance window %d contains invalid sample", index)
				}
			}
		}
		memoryControl := nearestRankP95Evidence(window.MemoryControlNS)
		memoryAsserted := nearestRankP95Evidence(window.MemoryAssertedNS)
		durableControl := nearestRankP95Evidence(window.DurableControlNS)
		durableAsserted := nearestRankP95Evidence(window.DurableAssertedNS)
		allowance := max(int64(math.Ceil(float64(memoryControl)*config.MemoryRelativeLimit)), config.MemoryMinimumNanos)
		summary := summaries[index]
		if summary.MemoryControlP95NS != memoryControl || summary.MemoryAssertedP95NS != memoryAsserted || summary.MemoryAllowanceNS != allowance || summary.DurableControlP95NS != durableControl || summary.DurableAssertedP95NS != durableAsserted {
			return fmt.Errorf("performance window %d summary is not recomputable", index)
		}
		if memoryAsserted > memoryControl+allowance || durableAsserted >= config.DurableMaximumNanos {
			return fmt.Errorf("performance window %d exceeded threshold", index)
		}
	}
	return nil
}

func validateAssertionExternal(value assertionExternalEvidence) error {
	if value.SchemaVersion != 1 || value.CandidateCommit != qualifiedAssertionCommit || value.CandidateTree != qualifiedAssertionTree || value.RemoteRef != "refs/heads/qualification/atomic-batch-assertions-01M1P4VH" || value.RemoteSHA != qualifiedAssertionCommit || value.Module != "github.com/spec-kitty/gapdb" || value.ResolvedVersion != "v0.0.0-20260904141708-8de0f7f54979" || value.ModuleSum != "h1:QVa3SoUB1Wwu45ctY8AEuF8nup8VJ7sZTCNc675N2AQ=" || value.GoModSum != "h1:8xNZ3k+SQIqcxLkS8bjbooOAv9xzZcbg5arjKaV6fcQ=" || value.GoWork != "off" || value.GoProxy != "direct" || !value.FreshModuleCache || !value.FreshBuildCache || !value.FreshBinaryDirectory || value.Replace || !value.InstalledDaemon || !value.PassingBatch || !value.StaleBatchRefused || value.StaleErrorCode != "CONDITION_FAILED" || !value.StaleTargetAbsent || value.ProgramOutput != "external assertion qualification passed revision=2 stale_code=CONDITION_FAILED" || value.RetryCount != 0 {
		return errors.New("external consumption evidence is incomplete")
	}
	return nil
}

func validateAssertionGates(value assertionGatesEvidence) error {
	want := []string{"go test ./...", "go test -race ./...", "go vet ./...", "staticcheck ./...", "govulncheck ./...", "go mod verify", "gofmt -l .", "git diff --check", "go build ./cmd/gapdbd", "go build ./cmd/gapctl", "GAPDB_ASSERTION_ACCEPTANCE=1 go test ./tests/performance -run TestAssertionPerformancePairedSameHostWindows -count=11"}
	if value.SchemaVersion != 1 || value.CandidateCommit != qualifiedAssertionCommit || value.CandidateTree != qualifiedAssertionTree || value.GoVersion != "go1.26.7" || value.OS != "linux" || value.Arch != "amd64" || value.Kernel == "" || len(value.Gates) != len(want) {
		return errors.New("gate identity is incomplete")
	}
	for index, gate := range value.Gates {
		if gate.Command != want[index] || gate.Status != "pass" || (gate.Command == "gofmt -l ." && gate.Output != "") {
			return fmt.Errorf("gate %d is absent, reordered, failed, or has formatting output", index)
		}
	}
	return nil
}

func nearestRankP95Evidence(samples []int64) int64 {
	ordered := append([]int64(nil), samples...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	return ordered[(95*len(ordered)-1)/100]
}

func assertionEvidenceDirectory(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "docs", "evidence", "atomic-batch-assertions")
}

func readBounded(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("evidence file is absent, redirected, non-regular, empty, or oversized")
	}
	return os.ReadFile(path)
}

func decodeStrictBytes(contents []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("evidence contains trailing JSON")
	}
	return nil
}

func mustReadPath(path string) []byte { contents, _ := os.ReadFile(path); return contents }
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
func mustWrite(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
func mustWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, append(encoded, '\n'))
}
func mustWriteCanonicalJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, append(encoded, '\n'))
}
func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
func mustRename(t *testing.T, oldPath, newPath string) {
	t.Helper()
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
}

func copyAssertionEvidenceDirectory(t *testing.T, source string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range assertionEvidenceFiles {
		mustWrite(t, filepath.Join(root, name), mustRead(t, filepath.Join(source, name)))
	}
	return root
}

func rehashManifestFile(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, "manifest.json")
	var manifest assertionEvidenceManifest
	if err := decodeStrictBytes(mustRead(t, path), &manifest); err != nil {
		t.Fatal(err)
	}
	contents := mustRead(t, filepath.Join(root, name))
	digest := sha256.Sum256(contents)
	found := false
	for index := range manifest.Files {
		if manifest.Files[index].Name == name {
			manifest.Files[index].Bytes = int64(len(contents))
			manifest.Files[index].SHA256 = hex.EncodeToString(digest[:])
			found = true
		}
	}
	if !found {
		t.Fatalf("manifest entry %s absent", name)
	}
	mustWriteCanonicalJSON(t, path, manifest)
}

func resignManifestWithReplacementKey(t *testing.T, root string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, mustRead(t, filepath.Join(root, "manifest.json")))
	mustWrite(t, filepath.Join(root, "manifest.sig"), append([]byte(base64.StdEncoding.EncodeToString(signature)), '\n'))
}

func cloneJSON[T any](t *testing.T, source T) T {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := decodeStrictBytes(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
