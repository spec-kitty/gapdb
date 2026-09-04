package adoption_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
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
	"testing"
)

const (
	qualifiedAssertionCommit = "a5f79adb7cd49c4328d132179b8828a5806e68fa"
	qualifiedAssertionTree   = "e0f674eb2c947129bf141221059b432553494947"
)

type assertionEvidenceManifest struct {
	SchemaVersion       uint16            `json:"schema_version"`
	Algorithm           string            `json:"algorithm"`
	SignedFile          string            `json:"signed_file"`
	PublicKeyPKIXBase64 string            `json:"public_key_pkix_base64"`
	SignatureBase64     string            `json:"signature_base64"`
	Files               map[string]string `json:"files"`
}

type assertionQualificationEvidence struct {
	SchemaVersion uint16 `json:"schema_version"`
	Candidate     struct {
		Commit         string `json:"commit"`
		Tree           string `json:"tree"`
		RemoteRef      string `json:"remote_ref"`
		RemoteVerified bool   `json:"remote_verified"`
	} `json:"candidate"`
	FrozenAdoption struct {
		ContractVersion string `json:"contract_version"`
		ScenarioCount   int    `json:"scenario_count"`
		ScenariosPassed int    `json:"scenarios_passed"`
		SQLiteDecision  string `json:"sqlite_decision"`
	} `json:"frozen_adoption"`
	Contention struct {
		Trials         int  `json:"trials"`
		RevisionTrials int  `json:"revision_trials"`
		AbsenceTrials  int  `json:"absence_trials"`
		StaleSuccesses int  `json:"stale_successes"`
		MutantKilled   bool `json:"mutant_killed"`
		RaceEnabled    bool `json:"race_enabled"`
		RetryCount     int  `json:"retry_count"`
	} `json:"contention"`
	Crash struct {
		PredicateSIGKILLSchedules int  `json:"predicate_sigkill_schedules"`
		DurableSIGKILLSchedules   int  `json:"durable_sigkill_schedules"`
		NormalRestartRecovery     bool `json:"normal_restart_recovery"`
		RawResponseLossRecovered  bool `json:"raw_response_loss_recovered"`
		FailedPredicateZeroEffect bool `json:"failed_predicate_zero_effect"`
		WatchLiveMutationOnly     bool `json:"watch_live_mutation_only"`
		WatchReplayMutationOnly   bool `json:"watch_replay_mutation_only"`
	} `json:"crash"`
	Bounds struct {
		CombinedOperations  bool `json:"combined_operations_l_minus_1_l_l_plus_1"`
		CombinedPublicBytes bool `json:"combined_public_bytes_l_minus_1_l_l_plus_1"`
		CanonicalJSONBytes  bool `json:"canonical_json_bytes_l_minus_1_l_l_plus_1"`
	} `json:"bounds"`
	Performance struct {
		Calculation         string  `json:"calculation"`
		MemoryAllowanceRule string  `json:"memory_allowance_rule"`
		DurableLimitUS      float64 `json:"durable_limit_us"`
		Windows             []struct {
			MemoryControlUS   float64 `json:"memory_control_us"`
			MemoryAssertedUS  float64 `json:"memory_asserted_us"`
			MemoryOverheadUS  float64 `json:"memory_overhead_us"`
			MemoryAllowanceUS float64 `json:"memory_allowance_us"`
			DurableControlUS  float64 `json:"durable_control_us"`
			DurableAssertedUS float64 `json:"durable_asserted_us"`
		} `json:"windows"`
	} `json:"performance"`
	ExternalConsumption struct {
		Module               string `json:"module"`
		RequestedCommit      string `json:"requested_commit"`
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
		RetryCount           int    `json:"retry_count"`
	} `json:"external_consumption"`
	Environment struct {
		GoVersion  string `json:"go_version"`
		OS         string `json:"os"`
		Arch       string `json:"arch"`
		Kernel     string `json:"kernel"`
		Transport  string `json:"transport"`
		Filesystem string `json:"filesystem"`
	} `json:"environment"`
	Gates []string `json:"gates"`
}

func TestAssertionQualificationEvidenceIsSignedHashedAndSemanticallySealed(t *testing.T) {
	directory := filepath.Join("..", "..", "docs", "evidence", "atomic-batch-assertions")
	manifestBytes := mustReadEvidenceFile(t, filepath.Join(directory, "manifest.json"))
	var manifest assertionEvidenceManifest
	decodeStrictEvidence(t, manifestBytes, &manifest)
	if manifest.SchemaVersion != 1 || manifest.Algorithm != "ed25519" || manifest.SignedFile != "qualification.json" || len(manifest.Files) != 4 {
		t.Fatalf("invalid assertion evidence manifest: %+v", manifest)
	}
	for name, expected := range manifest.Files {
		contents := mustReadEvidenceFile(t, filepath.Join(directory, name))
		digest := sha256.Sum256(contents)
		if hex.EncodeToString(digest[:]) != expected {
			t.Fatalf("evidence hash mismatch for %s", name)
		}
	}
	qualificationBytes := mustReadEvidenceFile(t, filepath.Join(directory, manifest.SignedFile))
	publicDER, err := base64.StdEncoding.DecodeString(manifest.PublicKeyPKIXBase64)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKIXPublicKey(publicDER)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("manifest key type = %T", parsed)
	}
	signature, err := base64.StdEncoding.DecodeString(manifest.SignatureBase64)
	if err != nil || !ed25519.Verify(publicKey, qualificationBytes, signature) {
		t.Fatalf("assertion evidence signature invalid: %v", err)
	}
	var evidence assertionQualificationEvidence
	decodeStrictEvidence(t, qualificationBytes, &evidence)
	if err := validateAssertionQualificationEvidence(evidence); err != nil {
		t.Fatal(err)
	}
}

func TestAssertionQualificationSemanticMutantsAreRejected(t *testing.T) {
	contents := mustReadEvidenceFile(t, filepath.Join("..", "..", "docs", "evidence", "atomic-batch-assertions", "qualification.json"))
	var valid assertionQualificationEvidence
	decodeStrictEvidence(t, contents, &valid)
	mutants := []struct {
		name   string
		mutate func(*assertionQualificationEvidence)
	}{
		{"trials_999", func(value *assertionQualificationEvidence) { value.Contention.Trials = 999 }},
		{"one_stale_success", func(value *assertionQualificationEvidence) { value.Contention.StaleSuccesses = 1 }},
		{"mutant_not_killed", func(value *assertionQualificationEvidence) { value.Contention.MutantKilled = false }},
		{"bad_p95_math", func(value *assertionQualificationEvidence) { value.Performance.Windows[0].MemoryOverheadUS++ }},
		{"bad_p95_threshold", func(value *assertionQualificationEvidence) { value.Performance.Windows[0].MemoryAssertedUS += 200 }},
		{"stale_commit", func(value *assertionQualificationEvidence) {
			value.Candidate.Commit = "0000000000000000000000000000000000000000"
		}},
		{"stale_tree", func(value *assertionQualificationEvidence) {
			value.Candidate.Tree = "0000000000000000000000000000000000000000"
		}},
		{"missing_gate", func(value *assertionQualificationEvidence) { value.Gates = value.Gates[:len(value.Gates)-1] }},
		{"contention_retry", func(value *assertionQualificationEvidence) { value.Contention.RetryCount = 1 }},
		{"network_retry", func(value *assertionQualificationEvidence) { value.ExternalConsumption.RetryCount = 1 }},
		{"local_replace", func(value *assertionQualificationEvidence) { value.ExternalConsumption.Replace = true }},
	}
	for _, mutant := range mutants {
		t.Run(mutant.name, func(t *testing.T) {
			candidate := cloneAssertionEvidence(t, valid)
			mutant.mutate(&candidate)
			if err := validateAssertionQualificationEvidence(candidate); err == nil {
				t.Fatal("semantic evidence mutant survived")
			}
		})
	}
}

func validateAssertionQualificationEvidence(value assertionQualificationEvidence) error {
	if value.SchemaVersion != 1 || value.Candidate.Commit != qualifiedAssertionCommit || value.Candidate.Tree != qualifiedAssertionTree || !value.Candidate.RemoteVerified || value.Candidate.RemoteRef != "refs/heads/qualification/atomic-batch-assertions-01M1P4VH" {
		return errors.New("candidate identity is not the qualified remote commit and tree")
	}
	if value.FrozenAdoption.ContractVersion != "gapdb-phase1/v1" || value.FrozenAdoption.ScenarioCount != 14 || value.FrozenAdoption.ScenariosPassed != 14 || value.FrozenAdoption.SQLiteDecision != "production_pending_external_comparison_and_signed_human_approval" {
		return errors.New("frozen adoption evidence changed")
	}
	if value.Contention.Trials != 1000 || value.Contention.RevisionTrials != 500 || value.Contention.AbsenceTrials != 500 || value.Contention.RevisionTrials+value.Contention.AbsenceTrials != value.Contention.Trials || value.Contention.StaleSuccesses != 0 || !value.Contention.MutantKilled || !value.Contention.RaceEnabled || value.Contention.RetryCount != 0 {
		return errors.New("contention qualification is incomplete")
	}
	if value.Crash.PredicateSIGKILLSchedules != 4 || value.Crash.DurableSIGKILLSchedules != 10 || !value.Crash.NormalRestartRecovery || !value.Crash.RawResponseLossRecovered || !value.Crash.FailedPredicateZeroEffect || !value.Crash.WatchLiveMutationOnly || !value.Crash.WatchReplayMutationOnly {
		return errors.New("crash/recovery qualification is incomplete")
	}
	if !value.Bounds.CombinedOperations || !value.Bounds.CombinedPublicBytes || !value.Bounds.CanonicalJSONBytes {
		return errors.New("boundary qualification is incomplete")
	}
	if value.Performance.Calculation != "nearest_rank_p95_per_window" || value.Performance.MemoryAllowanceRule != "max(control_p95*0.15,100us)" || value.Performance.DurableLimitUS != 4000 || len(value.Performance.Windows) != 3 {
		return errors.New("performance methodology changed")
	}
	for index, window := range value.Performance.Windows {
		allowance := math.Max(window.MemoryControlUS*0.15, 100)
		if math.Abs(window.MemoryAllowanceUS-allowance) > 0.001 || math.Abs(window.MemoryOverheadUS-(window.MemoryAssertedUS-window.MemoryControlUS)) > 0.001 {
			return fmt.Errorf("performance window %d calculation mismatch", index)
		}
		if window.MemoryAssertedUS > window.MemoryControlUS+window.MemoryAllowanceUS || window.DurableAssertedUS >= value.Performance.DurableLimitUS || window.MemoryControlUS <= 0 || window.DurableControlUS <= 0 {
			return fmt.Errorf("performance window %d exceeded its bound", index)
		}
	}
	external := value.ExternalConsumption
	if external.Module != "github.com/spec-kitty/gapdb" || external.RequestedCommit != qualifiedAssertionCommit || external.ResolvedVersion != "v0.0.0-20260904135054-a5f79adb7cd4" || external.ModuleSum != "h1:xRnnGpjrSq+nf562y1j7WN/i5YqZXoh8s+LoYyhpp/o=" || external.GoModSum == "" || external.GoWork != "off" || external.GoProxy != "direct" || !external.FreshModuleCache || !external.FreshBuildCache || !external.FreshBinaryDirectory || external.Replace || !external.InstalledDaemon || !external.PassingBatch || !external.StaleBatchRefused || external.RetryCount != 0 {
		return errors.New("external consumption proof is incomplete")
	}
	requiredGates := []string{"go_test_all", "go_test_race_all", "go_vet_all", "staticcheck_all", "govulncheck_all", "go_mod_verify", "gofmt_clean", "git_diff_check", "build_gapdbd", "build_gapctl", "performance_acceptance"}
	if len(value.Gates) != len(requiredGates) {
		return errors.New("gate count mismatch")
	}
	for _, gate := range requiredGates {
		if !slices.Contains(value.Gates, gate) {
			return fmt.Errorf("missing gate %s", gate)
		}
	}
	return nil
}

func mustReadEvidenceFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func decodeStrictEvidence(t *testing.T, contents []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("evidence contains trailing JSON: %v", err)
	}
}

func cloneAssertionEvidence(t *testing.T, source assertionQualificationEvidence) assertionQualificationEvidence {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone assertionQualificationEvidence
	decodeStrictEvidence(t, encoded, &clone)
	return clone
}
