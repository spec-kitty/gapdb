package adoption_test

import (
	"os"
	"path/filepath"
	"testing"

	"gapdb/tests/adoption"
)

func TestAutomatedAdoptionNeverApprovesProductionSwitch(t *testing.T) {
	passed := passingContract("gapdb")
	pending := adoption.EvaluateAdoption(passed, nil, adoption.SafetyEvidence{Crash: true, Race: true, Expiry: true, Watch: true, Authority: true})
	if pending.ProductionBackend != "sqlite" || pending.AdoptionStatus != "not_approved" || pending.HumanApproval.Approved || pending.SQLite.Status != "pending" {
		t.Fatalf("missing SQLite evidence was not explicit and safe: %+v", pending)
	}
	sqlite := passingContract("sqlite")
	complete := adoption.EvaluateAdoption(passed, &sqlite, adoption.SafetyEvidence{Crash: true, Race: true, Expiry: true, Watch: true, Authority: true})
	if complete.TechnicalGates != "complete" || complete.ProductionBackend != "sqlite" || complete.AdoptionStatus != "not_approved" || complete.HumanApproval.Approved {
		t.Fatalf("automation approved production adoption: %+v", complete)
	}
}

func TestReleaseManifestFailsClosedOnEvidenceAuthority(t *testing.T) {
	sourceRoot := repositoryRoot(t)
	root := t.TempDir()
	manifest := readCommittedManifest(t, sourceRoot)
	copyEvidenceTree(t, sourceRoot, root, manifest)
	authority := recordedReleaseAuthority()
	if err := adoption.ValidateReleaseManifest(root, manifest, authority); err != nil {
		t.Fatalf("valid release manifest: %v", err)
	}

	for name, mutate := range map[string]func(*adoption.ReleaseManifest){
		"stale-commit": func(value *adoption.ReleaseManifest) {
			value.CodeUnderTestCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		"mismatched-config": func(value *adoption.ReleaseManifest) { value.ConfigSHA256 = string(make([]byte, 64)) },
		"out-of-bounds": func(value *adoption.ReleaseManifest) {
			value.Evidence[0].ObservedCount = value.Evidence[0].MaximumCount + 1
		},
		"missing-criterion":        func(value *adoption.ReleaseManifest) { delete(value.Criteria, "SC-009") },
		"automated-human-approval": func(value *adoption.ReleaseManifest) { value.Adoption.HumanApproval.Approved = true },
		"automated-backend-switch": func(value *adoption.ReleaseManifest) { value.Adoption.ProductionBackend = "gapdb" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := manifest.Clone()
			mutate(&copy)
			if err := adoption.ValidateReleaseManifest(root, copy, authority); err == nil {
				t.Fatal("modified authority was accepted")
			}
		})
	}

	for _, evidence := range manifest.Evidence {
		t.Run("delete-"+evidence.ID, func(t *testing.T) {
			path := filepath.Join(root, evidence.Path)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := adoption.ValidateReleaseManifest(root, manifest, authority); err == nil {
				t.Fatal("deleted evidence was accepted")
			}
			writeEvidence(t, root, evidence.Path, original)
		})
		t.Run("tamper-"+evidence.ID, func(t *testing.T) {
			path := filepath.Join(root, evidence.Path)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeEvidence(t, root, evidence.Path, append(original, 'x'))
			if err := adoption.ValidateReleaseManifest(root, manifest, authority); err == nil {
				t.Fatal("tampered evidence was accepted")
			}
			writeEvidence(t, root, evidence.Path, original)
		})
	}
}

func passingContract(backend string) adoption.ContractResult {
	result := adoption.ContractResult{SchemaVersion: 1, ContractVersion: adoption.ContractVersion, Backend: backend, Applicable: adoption.ScenarioCount, Passed: true}
	for _, id := range adoption.ScenarioIDs() {
		result.Scenarios = append(result.Scenarios, adoption.ScenarioResult{ID: id, Status: "pass"})
	}
	return result
}

func writeEvidence(t *testing.T, root, relative string, value []byte) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyEvidenceTree(t *testing.T, sourceRoot, destinationRoot string, manifest adoption.ReleaseManifest) {
	t.Helper()
	for _, evidence := range manifest.Evidence {
		value, err := os.ReadFile(filepath.Join(sourceRoot, evidence.Path))
		if err != nil {
			t.Fatal(err)
		}
		writeEvidence(t, destinationRoot, evidence.Path, value)
	}
}
