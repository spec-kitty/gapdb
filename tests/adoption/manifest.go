package adoption

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type SafetyEvidence struct {
	Crash     bool `json:"crash"`
	Race      bool `json:"race"`
	Expiry    bool `json:"expiry"`
	Watch     bool `json:"watch"`
	Authority bool `json:"authority"`
}

type BackendGate struct {
	Backend string `json:"backend"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

type HumanApproval struct {
	Approved bool   `json:"approved"`
	Source   string `json:"source"`
}

type AdoptionRecommendation struct {
	SchemaVersion     uint16         `json:"schema_version"`
	ContractVersion   string         `json:"contract_version"`
	ProductionBackend string         `json:"production_backend"`
	TechnicalGates    string         `json:"technical_gates"`
	AdoptionStatus    string         `json:"adoption_status"`
	Gapdb             BackendGate    `json:"gapdb"`
	SQLite            BackendGate    `json:"sqlite"`
	Safety            SafetyEvidence `json:"safety"`
	HumanApproval     HumanApproval  `json:"human_approval"`
}

// EvaluateAdoption has no approval input by design. An external signed human
// record is consumed by the originating application, never by this test run.
func EvaluateAdoption(gapdbResult ContractResult, sqliteResult *ContractResult, safety SafetyEvidence) AdoptionRecommendation {
	recommendation := AdoptionRecommendation{
		SchemaVersion:     1,
		ContractVersion:   ContractVersion,
		ProductionBackend: "sqlite",
		TechnicalGates:    "incomplete",
		AdoptionStatus:    "not_approved",
		Gapdb:             backendGate("gapdb", gapdbResult),
		SQLite:            BackendGate{Backend: "sqlite", Status: "pending", Reason: "external SQLite adapter result is absent"},
		Safety:            safety,
		HumanApproval:     HumanApproval{Approved: false, Source: "external_signed_record_required"},
	}
	if sqliteResult != nil {
		recommendation.SQLite = backendGate("sqlite", *sqliteResult)
	}
	if recommendation.Gapdb.Status == "pass" && recommendation.SQLite.Status == "pass" && safety.complete() {
		recommendation.TechnicalGates = "complete"
	}
	return recommendation
}

func backendGate(expected string, result ContractResult) BackendGate {
	gate := BackendGate{Backend: expected, Status: "fail"}
	if err := validateContractResult(expected, result); err != nil {
		gate.Reason = boundedError(err)
		return gate
	}
	gate.Status = "pass"
	return gate
}

func validateContractResult(expected string, result ContractResult) error {
	if result.SchemaVersion != 1 || result.ContractVersion != ContractVersion || result.Backend != expected || !result.Passed || result.Applicable != ScenarioCount || len(result.Scenarios) != ScenarioCount {
		return errors.New("contract result identity or summary is incomplete")
	}
	expectedIDs := ScenarioIDs()
	actualIDs := make([]string, 0, len(result.Scenarios))
	seen := make(map[string]bool, len(result.Scenarios))
	for _, scenario := range result.Scenarios {
		if scenario.ID == "" || seen[scenario.ID] || scenario.Status != "pass" || scenario.Error != "" {
			return fmt.Errorf("scenario result %q is not one unique pass", scenario.ID)
		}
		seen[scenario.ID] = true
		actualIDs = append(actualIDs, scenario.ID)
	}
	sort.Strings(actualIDs)
	if strings.Join(actualIDs, "\n") != strings.Join(expectedIDs, "\n") {
		return errors.New("contract result scenario set differs from the pinned version")
	}
	return nil
}

func (evidence SafetyEvidence) complete() bool {
	return evidence.Crash && evidence.Race && evidence.Expiry && evidence.Watch && evidence.Authority
}

type EvidenceReference struct {
	ID            string       `json:"id"`
	Kind          EvidenceKind `json:"kind"`
	Path          string       `json:"path"`
	SHA256        string       `json:"sha256"`
	Bytes         int64        `json:"bytes"`
	Status        string       `json:"status"`
	SourceCommit  string       `json:"source_commit"`
	ConfigSHA256  string       `json:"config_sha256"`
	Command       string       `json:"command"`
	ObservedCount int64        `json:"observed_count"`
	MinimumCount  int64        `json:"minimum_count"`
	MaximumCount  int64        `json:"maximum_count"`
}

type CriterionEvidence struct {
	Status      string   `json:"status"`
	Command     string   `json:"command"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type ReleaseCoverage struct {
	RacePassed           bool   `json:"race_passed"`
	CrashSchedules       int    `json:"crash_schedules"`
	FormatFixturesPassed bool   `json:"format_fixtures_passed"`
	ModelOpsPassed       bool   `json:"modelops_passed"`
	PermissionsPassed    bool   `json:"permissions_passed"`
	PerformancePassed    bool   `json:"performance_passed"`
	DualBackendStatus    string `json:"dual_backend_status"`
}

type ReleaseManifest struct {
	SchemaVersion       uint16                       `json:"schema_version"`
	CodeUnderTestCommit string                       `json:"code_under_test_commit"`
	ConfigSHA256        string                       `json:"config_sha256"`
	MaxEvidenceBytes    int64                        `json:"max_evidence_bytes"`
	MVPStatus           string                       `json:"mvp_status"`
	Adoption            AdoptionRecommendation       `json:"adoption"`
	Coverage            ReleaseCoverage              `json:"coverage"`
	Evidence            []EvidenceReference          `json:"evidence"`
	Criteria            map[string]CriterionEvidence `json:"criteria"`
}

func (manifest ReleaseManifest) Clone() ReleaseManifest {
	clone := manifest
	clone.Evidence = append([]EvidenceReference(nil), manifest.Evidence...)
	clone.Criteria = make(map[string]CriterionEvidence, len(manifest.Criteria))
	for id, criterion := range manifest.Criteria {
		criterion.EvidenceIDs = append([]string(nil), criterion.EvidenceIDs...)
		clone.Criteria[id] = criterion
	}
	return clone
}

func ValidateReleaseManifest(root string, manifest ReleaseManifest, authority ReleaseAuthority) error {
	if manifest.SchemaVersion != 2 || manifest.MVPStatus != "complete" || manifest.CodeUnderTestCommit != authority.CodeUnderTestCommit || manifest.ConfigSHA256 != authority.ConfigSHA256 || !canonicalHex(manifest.CodeUnderTestCommit, 40) || !canonicalHex(manifest.ConfigSHA256, 64) {
		return errors.New("release identity is stale, malformed, or mismatched")
	}
	if manifest.MaxEvidenceBytes <= 0 || manifest.MaxEvidenceBytes > 8<<20 || len(manifest.Evidence) == 0 || len(manifest.Evidence) > 64 {
		return errors.New("release evidence bounds are invalid")
	}
	if manifest.Adoption.ProductionBackend != "sqlite" || manifest.Adoption.AdoptionStatus != "not_approved" || manifest.Adoption.HumanApproval.Approved || manifest.Adoption.HumanApproval.Source != "external_signed_record_required" {
		return errors.New("automated adoption authority is unsafe")
	}
	if !manifest.Coverage.RacePassed || manifest.Coverage.CrashSchedules < 1_000 || manifest.Coverage.CrashSchedules > 100_000 || !manifest.Coverage.FormatFixturesPassed || !manifest.Coverage.ModelOpsPassed || !manifest.Coverage.PermissionsPassed || !manifest.Coverage.PerformancePassed {
		return errors.New("release coverage is incomplete or outside bounds")
	}
	if manifest.Coverage.DualBackendStatus != "sqlite_pending" && manifest.Coverage.DualBackendStatus != "complete" {
		return errors.New("dual-backend status is invalid")
	}
	if manifest.Adoption.SchemaVersion != 1 || manifest.Adoption.ContractVersion != ContractVersion || manifest.Adoption.TechnicalGates != "incomplete" || manifest.Adoption.Gapdb.Status != "pass" || manifest.Adoption.SQLite.Status != "pending" || !manifest.Adoption.Safety.complete() || manifest.Coverage.DualBackendStatus != "sqlite_pending" {
		return errors.New("release adoption state is not the pinned external-pending state")
	}

	if len(authority.Evidence) != len(manifest.Evidence) {
		return errors.New("release evidence authority set is incomplete")
	}
	evidenceByID := make(map[string]bool, len(manifest.Evidence))
	for _, evidence := range manifest.Evidence {
		expected, expectedOK := authority.Evidence[evidence.ID]
		if evidence.ID == "" || evidenceByID[evidence.ID] || !expectedOK || evidence.Status != "pass" || evidence.Kind != expected.Kind || evidence.SourceCommit != expected.SourceCommit || evidence.ConfigSHA256 != expected.ConfigSHA256 || evidence.Command != expected.Command || evidence.ObservedCount != expected.ObservedCount || !canonicalHex(evidence.SourceCommit, 40) || !canonicalHex(evidence.SHA256, 64) {
			return fmt.Errorf("evidence %q metadata is incomplete or mismatched", evidence.ID)
		}
		if evidence.MinimumCount < 0 || evidence.MaximumCount <= 0 || evidence.MaximumCount > 1_000_000 || evidence.ObservedCount < evidence.MinimumCount || evidence.ObservedCount > evidence.MaximumCount {
			return fmt.Errorf("evidence %q count is outside authority bounds", evidence.ID)
		}
		path, err := safeEvidencePath(root, evidence.Path)
		if err != nil {
			return fmt.Errorf("evidence %q path: %w", evidence.ID, err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != evidence.Bytes || info.Size() > manifest.MaxEvidenceBytes {
			return fmt.Errorf("evidence %q file is absent, redirected, mismatched, or oversized", evidence.ID)
		}
		digest, err := boundedFileSHA256(path, manifest.MaxEvidenceBytes)
		if err != nil || digest != evidence.SHA256 {
			return fmt.Errorf("evidence %q digest mismatch: %w", evidence.ID, err)
		}
		semantic, err := validateSemanticEvidence(path, evidence)
		if err != nil {
			return fmt.Errorf("evidence %q semantic authority: %w", evidence.ID, err)
		}
		if semantic.SourceCommit != expected.SourceCommit || semantic.ConfigSHA256 != expected.ConfigSHA256 || semantic.Command != expected.Command || semantic.ObservedCount != expected.ObservedCount {
			return fmt.Errorf("evidence %q internal authority mismatches caller pins", evidence.ID)
		}
		evidenceByID[evidence.ID] = true
	}
	crashAuthority, hasCrash := authority.Evidence["crash-race"]
	if !hasCrash || manifest.Coverage.CrashSchedules != int(crashAuthority.ObservedCount) {
		return errors.New("release coverage does not match semantic crash evidence")
	}

	required := requiredCriteria()
	if len(manifest.Criteria) != len(required) || len(authority.Criteria) != len(required) {
		return errors.New("release criteria set is incomplete")
	}
	for _, id := range required {
		criterion, exists := manifest.Criteria[id]
		expected, expectedOK := authority.Criteria[id]
		if !exists || !expectedOK || criterion.Status != "pass" || criterion.Command != expected.Command || strings.Join(criterion.EvidenceIDs, "\n") != strings.Join(expected.EvidenceIDs, "\n") || criterion.Command == "" || len(criterion.Command) > 512 || len(criterion.EvidenceIDs) == 0 || len(criterion.EvidenceIDs) > 8 {
			return fmt.Errorf("criterion %s is absent, unbounded, or not passed", id)
		}
		seen := make(map[string]bool, len(criterion.EvidenceIDs))
		for _, evidenceID := range criterion.EvidenceIDs {
			if !evidenceByID[evidenceID] || seen[evidenceID] {
				return fmt.Errorf("criterion %s references absent or duplicate evidence %q", id, evidenceID)
			}
			seen[evidenceID] = true
		}
	}
	return nil
}

func requiredCriteria() []string {
	result := make([]string, 0, 21)
	for index := 1; index <= 9; index++ {
		result = append(result, fmt.Sprintf("SC-%03d", index))
	}
	for index := 1; index <= 12; index++ {
		result = append(result, fmt.Sprintf("NFR-%03d", index))
	}
	return result
}

func safeEvidencePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path must be a clean relative file")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, relative)
	if path == root || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", errors.New("path escapes evidence root")
	}
	return path, nil
}

func boundedFileSHA256(path string, maximum int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", err
	}
	if written > maximum {
		return "", errors.New("file exceeds evidence bound")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func canonicalHex(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
