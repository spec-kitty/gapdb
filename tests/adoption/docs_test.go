package adoption_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spec-kitty/gapdb/tests/adoption"
)

const (
	recordedPerformanceCommit = "3a98149c005031a77a85c42c04caae7073b80dad"
	recordedCrashCommit       = "d34eadaea72b5763e0270472635e03423f7bffcc"
	recordedProtocolCommit    = "1527eb112acf14908b46c6d84b701d4c77d2b560"
	recordedConfigSHA256      = "8651e5d4d9d1d3ae8804045cda5d8fda97f3d51523a82a23e4c9dfe923166fc4"
)

func TestCommittedReleaseManifestIsCompleteAndBounded(t *testing.T) {
	root := repositoryRoot(t)
	path := filepath.Join(root, "docs/evidence/performance/release-manifest.json")
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest adoption.ReleaseManifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("release manifest has trailing JSON: %v", err)
	}
	if err := adoption.ValidateReleaseManifest(root, manifest, recordedReleaseAuthority()); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedManifestRejectsCoherentSemanticRewrite(t *testing.T) {
	root := repositoryRoot(t)
	temporary := t.TempDir()
	manifest := readCommittedManifest(t, root)
	for _, evidence := range manifest.Evidence {
		source := filepath.Join(root, evidence.Path)
		destination := filepath.Join(temporary, evidence.Path)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		value, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	crash := filepath.Join(temporary, "docs/evidence/crash/results.json")
	var value map[string]any
	encoded, err := os.ReadFile(crash)
	if err != nil || json.Unmarshal(encoded, &value) != nil {
		t.Fatalf("read crash evidence: %v", err)
	}
	value["code_under_test_commit"] = strings.Repeat("0", 40)
	encoded, err = json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(crash, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	for index := range manifest.Evidence {
		if manifest.Evidence[index].ID == "crash-race" {
			manifest.Evidence[index].SHA256 = hex.EncodeToString(digest[:])
			manifest.Evidence[index].Bytes = int64(len(encoded))
		}
	}
	if err := adoption.ValidateReleaseManifest(temporary, manifest, recordedReleaseAuthority()); err == nil {
		t.Fatal("coherent crash commit rewrite was accepted")
	}
}

func TestSemanticEvidenceAdversarialRewriteMatrix(t *testing.T) {
	sourceRoot := repositoryRoot(t)
	authority := recordedReleaseAuthority()
	zeros40 := strings.Repeat("0", 40)
	zeros64 := strings.Repeat("0", 64)
	type rewrite struct {
		evidenceID string
		apply      func(*testing.T, string, *adoption.ReleaseManifest)
	}
	jsonRewrite := func(id string, path []string, replacement any, coherent func(*adoption.EvidenceReference, *adoption.ReleaseManifest)) rewrite {
		return rewrite{evidenceID: id, apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			mutateJSONPath(t, filepath.Join(root, evidencePath(*manifest, id)), path, replacement)
			if coherent != nil {
				coherent(evidenceReference(*manifest, id), manifest)
			}
		}}
	}
	coherentSource := func(source string) func(*adoption.EvidenceReference, *adoption.ReleaseManifest) {
		return func(reference *adoption.EvidenceReference, _ *adoption.ReleaseManifest) {
			reference.SourceCommit = source
		}
	}
	coherentConfig := func(config string) func(*adoption.EvidenceReference, *adoption.ReleaseManifest) {
		return func(reference *adoption.EvidenceReference, _ *adoption.ReleaseManifest) {
			reference.ConfigSHA256 = config
		}
	}
	coherentCommand := func(command string) func(*adoption.EvidenceReference, *adoption.ReleaseManifest) {
		return func(reference *adoption.EvidenceReference, _ *adoption.ReleaseManifest) { reference.Command = command }
	}
	coherentCount := func(count int64) func(*adoption.EvidenceReference, *adoption.ReleaseManifest) {
		return func(reference *adoption.EvidenceReference, _ *adoption.ReleaseManifest) {
			reference.ObservedCount = count
		}
	}
	rewrites := map[string]rewrite{
		"performance/source-commit":            jsonRewrite("performance-results", []string{"code_commit"}, zeros40, coherentSource(zeros40)),
		"performance/config":                   jsonRewrite("performance-results", []string{"config_sha256"}, zeros64, coherentConfig(zeros64)),
		"performance/command":                  jsonRewrite("performance-results", []string{"command"}, "go test ./fake", coherentCommand("go test ./fake")),
		"performance/live-count":               jsonRewrite("performance-results", []string{"configuration", "live_records"}, float64(99_999), coherentCount(99_999)),
		"performance/readers":                  jsonRewrite("performance-results", []string{"configuration", "readers"}, float64(7), nil),
		"performance/threshold":                jsonRewrite("performance-results", []string{"metrics", "get", "target_p95_ms"}, float64(200), nil),
		"performance/outcome":                  jsonRewrite("performance-results", []string{"metrics", "get", "status"}, "fail", nil),
		"performance/allocations-over-ceiling": jsonRewrite("performance-results", []string{"metrics", "get", "allocs_per_op"}, float64(1_000_001), nil),
		"performance/window":                   jsonRewrite("performance-results", []string{"windows", "durable_put", "writer_active"}, false, nil),
		"performance/sync-count-lower":         jsonRewrite("performance-results", []string{"observed_wal_syncs"}, float64(155), nil),
		"performance/sync-count-higher":        jsonRewrite("performance-results", []string{"observed_wal_syncs"}, float64(157), nil),
		"performance/revision":                 jsonRewrite("performance-results", []string{"recovery", "snapshot_revision"}, float64(1330), nil),
		"crash/source-commit":                  jsonRewrite("crash-race", []string{"code_under_test_commit"}, zeros40, coherentSource(zeros40)),
		"crash/command":                        jsonRewrite("crash-race", []string{"fault_matrix", "command"}, "go test ./fake", coherentCommand("go test ./fake")),
		"crash/schedule-count": jsonRewrite("crash-race", []string{"fault_matrix", "schedule_count"}, float64(1025), func(reference *adoption.EvidenceReference, manifest *adoption.ReleaseManifest) {
			reference.ObservedCount = 1025
			manifest.Coverage.CrashSchedules = 1025
		}),
		"crash/status":         jsonRewrite("crash-race", []string{"fault_matrix", "status"}, "fail", nil),
		"crash/class-count":    jsonRewrite("crash-race", []string{"fault_matrix", "observed_class_counts", "memory"}, float64(0), nil),
		"crash/hook-coverage":  jsonRewrite("crash-race", []string{"fault_matrix", "named_points"}, float64(38), nil),
		"adoption/source":      jsonRewrite("adoption-contract", []string{"code_commit"}, zeros40, coherentSource(zeros40)),
		"adoption/config":      jsonRewrite("adoption-contract", []string{"config_sha256"}, zeros64, coherentConfig(zeros64)),
		"adoption/command":     jsonRewrite("adoption-contract", []string{"command"}, "go test ./fake", coherentCommand("go test ./fake")),
		"adoption/count":       jsonRewrite("adoption-contract", []string{"gapdb", "applicable"}, float64(13), coherentCount(13)),
		"adoption/sqlite-pass": jsonRewrite("adoption-contract", []string{"sqlite", "status"}, "pass", nil),
		"adoption/human-pass":  jsonRewrite("adoption-contract", []string{"human_approval", "approved"}, true, nil),
		"manifest/observed-count": {evidenceID: "performance-results", apply: func(_ *testing.T, _ string, manifest *adoption.ReleaseManifest) {
			evidenceReference(*manifest, "performance-results").ObservedCount++
		}},
		"manifest/coverage": {evidenceID: "crash-race", apply: func(_ *testing.T, _ string, manifest *adoption.ReleaseManifest) {
			manifest.Coverage.CrashSchedules++
		}},
		"manifest/status": {evidenceID: "performance-results", apply: func(_ *testing.T, _ string, manifest *adoption.ReleaseManifest) {
			evidenceReference(*manifest, "performance-results").Status = "pending"
		}},
		"criterion/unrelated-evidence": {apply: func(_ *testing.T, _ string, manifest *adoption.ReleaseManifest) {
			criterion := manifest.Criteria["SC-001"]
			criterion.EvidenceIDs = []string{"crash-race"}
			manifest.Criteria["SC-001"] = criterion
		}},
		"criterion/fake-command": {apply: func(_ *testing.T, _ string, manifest *adoption.ReleaseManifest) {
			criterion := manifest.Criteria["NFR-004"]
			criterion.Command = "true"
			manifest.Criteria["NFR-004"] = criterion
		}},
		"raw/source": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", recordedPerformanceCommit, zeros40)
			evidenceReference(*manifest, "performance-raw").SourceCommit = zeros40
		}},
		"raw/reference-result": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"snapshot_revision":1329`, `"snapshot_revision":1330`)
		}},
		"raw/sync-lower": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"observed_wal_syncs":156`, `"observed_wal_syncs":155`)
		}},
		"raw/sync-higher": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"observed_wal_syncs":156`, `"observed_wal_syncs":157`)
		}},
		"raw/trailing-json": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"reference_accepted":true}`, `"reference_accepted":true} {}`)
		}},
		"raw/schema": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"schema_version":1`, `"schema_version":2`)
		}},
		"raw/seed": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"seed":5134473304279631186`, `"seed":5134473304279631187`)
		}},
		"raw/live-records": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"live_records":100000`, `"live_records":99999`)
		}},
		"raw/value-bytes": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"value_bytes":1024`, `"value_bytes":1023`)
		}},
		"raw/readers": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"readers":8`, `"readers":7`)
		}},
		"raw/writers": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"writers":1`, `"writers":2`)
		}},
		"raw/transport": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"transport":"unix_socket"`, `"transport":"in_process"`)
		}},
		"raw/filesystem": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"filesystem":"os_fsync_enabled"`, `"filesystem":"no_sync"`)
		}},
		"raw/socket-mode": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"socket_mode":"0600"`, `"socket_mode":"0666"`)
		}},
		"raw/durable-successes": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"successful_durable_barriers":151`, `"successful_durable_barriers":152`)
		}},
		"raw/metric-identity": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"durable_put":{`, `"durable_write":{`)
		}},
		"raw/metric-order": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"p50_ms":0.487707`, `"p50_ms":1.5`)
		}},
		"raw/sample-budget": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"samples":2048`, `"samples":2047`)
		}},
		"raw/metric-target": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"target_p95_ms":50`, `"target_p95_ms":51`)
		}},
		"raw/metric-outcome": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"target_p95_ms":50,"target_passed":true`, `"target_p95_ms":50,"target_passed":false`)
		}},
		"raw/metric-allocations": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"allocs_per_op":882`, `"allocs_per_op":0`)
		}},
		"raw/metric-allocations-over-ceiling": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"allocs_per_op":882`, `"allocs_per_op":1000001`)
		}},
		"raw/window-identity": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"windows":{"durable_put":{`, `"windows":{"durable_write":{`)
		}},
		"raw/window-budget": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"read_operations":2048`, `"read_operations":2047`)
		}},
		"raw/window-write-budget": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"write_operations":128`, `"write_operations":127`)
		}},
		"raw/window-reader-readiness": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"readers_ready":8`, `"readers_ready":7`)
		}},
		"raw/window-reader-activity": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"readers_active":8`, `"readers_active":7`)
		}},
		"raw/window-writer-readiness": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"writer_ready":true`, `"writer_ready":false`)
		}},
		"raw/window-writer-activity": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"writer_active":true`, `"writer_active":false`)
		}},
		"raw/recovery-snapshot": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"snapshot_revision":1329`, `"snapshot_revision":1330`)
		}},
		"raw/recovery-commits": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"later_wal_commits":10000`, `"later_wal_commits":9999`)
		}},
		"raw/recovery-target": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"target_ms":5000`, `"target_ms":6000`)
		}},
		"raw/recovery-order": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"ready_ms":522.302385`, `"ready_ms":5001`)
		}},
		"raw/recovery-outcome": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"target_ms":5000,"target_passed":true`, `"target_ms":5000,"target_passed":false`)
		}},
		"raw/reference-outcome": {evidenceID: "performance-raw", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "performance-raw", `"reference_accepted":true`, `"reference_accepted":false`)
		}},
		"protocol/token": {evidenceID: "protocol-format", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "protocol-format", "Memory acknowledgement", "Memory response")
		}},
		"storage/token": {evidenceID: "storage-format", apply: func(t *testing.T, root string, manifest *adoption.ReleaseManifest) {
			replaceEvidenceText(t, root, *manifest, "storage-format", "GAPWAL01", "BADWAL01")
		}},
	}
	for name, rewrite := range rewrites {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			manifest := readCommittedManifest(t, sourceRoot)
			copyEvidenceTree(t, sourceRoot, root, manifest)
			rewrite.apply(t, root, &manifest)
			if rewrite.evidenceID != "" {
				resignEvidence(t, root, &manifest, rewrite.evidenceID)
			}
			if err := adoption.ValidateReleaseManifest(root, manifest, authority); err == nil {
				t.Fatal("coherent semantic/outer rewrite was accepted")
			}
		})
	}
}

func mutateJSONPath(t *testing.T, path string, keys []string, replacement any) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	cursor := root
	for _, key := range keys[:len(keys)-1] {
		next, ok := cursor[key].(map[string]any)
		if !ok {
			t.Fatalf("semantic path %v is not an object", keys)
		}
		cursor = next
	}
	cursor[keys[len(keys)-1]] = replacement
	encoded, err = json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func replaceEvidenceText(t *testing.T, root string, manifest adoption.ReleaseManifest, id, old, replacement string) {
	t.Helper()
	path := filepath.Join(root, evidencePath(manifest, id))
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(encoded, []byte(old), []byte(replacement), 1)
	if bytes.Equal(updated, encoded) {
		t.Fatalf("evidence %s did not contain mutation target %q", id, old)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

func evidencePath(manifest adoption.ReleaseManifest, id string) string {
	for _, evidence := range manifest.Evidence {
		if evidence.ID == id {
			return evidence.Path
		}
	}
	return ""
}

func evidenceReference(manifest adoption.ReleaseManifest, id string) *adoption.EvidenceReference {
	for index := range manifest.Evidence {
		if manifest.Evidence[index].ID == id {
			return &manifest.Evidence[index]
		}
	}
	return nil
}

func resignEvidence(t *testing.T, root string, manifest *adoption.ReleaseManifest, id string) {
	t.Helper()
	reference := evidenceReference(*manifest, id)
	if reference == nil {
		t.Fatalf("evidence reference %q absent", id)
	}
	encoded, err := os.ReadFile(filepath.Join(root, reference.Path))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	reference.SHA256 = hex.EncodeToString(digest[:])
	reference.Bytes = int64(len(encoded))
}

func recordedReleaseAuthority() adoption.ReleaseAuthority {
	const (
		performance = "GAPDB_REFERENCE_ACCEPTANCE=1 go test ./tests/performance -run TestReferencePerformanceProfile -count=1 -v"
		adoptionRun = "go test ./tests/adoption -run TestGapdbAdapterPassesAllApplicableScenarios -count=1"
		crash       = "go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1 -v"
		doc         = "go test ./tests/adoption -run TestLivingDocumentationLinksAndLockedSurface -count=1"
	)
	authority := adoption.ReleaseAuthority{
		CodeUnderTestCommit: recordedPerformanceCommit,
		ConfigSHA256:        recordedConfigSHA256,
		Evidence: map[string]adoption.EvidenceAuthority{
			"performance-results": {Kind: adoption.EvidencePerformance, SourceCommit: recordedPerformanceCommit, ConfigSHA256: recordedConfigSHA256, Command: performance, ObservedCount: 100_000},
			"performance-raw":     {Kind: adoption.EvidenceRaw, SourceCommit: recordedPerformanceCommit, ConfigSHA256: recordedConfigSHA256, Command: performance, ObservedCount: 3},
			"adoption-contract":   {Kind: adoption.EvidenceAdoption, SourceCommit: recordedPerformanceCommit, ConfigSHA256: recordedConfigSHA256, Command: adoptionRun, ObservedCount: adoption.ScenarioCount},
			"crash-race":          {Kind: adoption.EvidenceCrash, SourceCommit: recordedCrashCommit, Command: crash, ObservedCount: 1_024},
			"protocol-format":     {Kind: adoption.EvidenceProtocolDoc, SourceCommit: recordedProtocolCommit, ConfigSHA256: recordedConfigSHA256, Command: doc, ObservedCount: 30},
			"storage-format":      {Kind: adoption.EvidenceStorageDoc, SourceCommit: recordedPerformanceCommit, ConfigSHA256: recordedConfigSHA256, Command: doc, ObservedCount: 11},
		},
		Criteria: map[string]adoption.CriterionAuthority{},
	}
	criteria := map[string]adoption.CriterionAuthority{
		"SC-001":  {Command: "go test ./tests/contract/... ./tests/adoption -count=1", EvidenceIDs: []string{"protocol-format", "adoption-contract"}},
		"SC-002":  {Command: "go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1", EvidenceIDs: []string{"crash-race"}},
		"SC-003":  {Command: "go test -race ./... -count=1", EvidenceIDs: []string{"crash-race"}},
		"SC-004":  {Command: "go test ./tests/contract/modelops ./tests/contract/cli -count=1", EvidenceIDs: []string{"crash-race"}},
		"SC-005":  {Command: "go test ./internal/engine ./internal/server ./tests/adoption -count=1", EvidenceIDs: []string{"crash-race", "adoption-contract"}},
		"SC-006":  {Command: "go test ./internal/engine ./tests/adoption -count=1", EvidenceIDs: []string{"crash-race", "adoption-contract"}},
		"SC-007":  {Command: performance, EvidenceIDs: []string{"performance-results", "performance-raw"}},
		"SC-008":  {Command: "go test ./tests/adoption -count=1", EvidenceIDs: []string{"adoption-contract"}},
		"SC-009":  {Command: "go test ./tests/compatibility/... ./tests/adoption -count=1", EvidenceIDs: []string{"protocol-format", "storage-format"}},
		"NFR-001": {Command: performance, EvidenceIDs: []string{"performance-results"}},
		"NFR-002": {Command: performance, EvidenceIDs: []string{"performance-results"}},
		"NFR-003": {Command: performance, EvidenceIDs: []string{"performance-results"}},
		"NFR-004": {Command: "go test ./tests/crash -run TestAcceptanceDeterministicFaultSchedules -count=1", EvidenceIDs: []string{"crash-race"}},
		"NFR-005": {Command: "go test -race ./... -count=1", EvidenceIDs: []string{"crash-race"}},
		"NFR-006": {Command: "go test ./tests/contract/modelops -count=1", EvidenceIDs: []string{"crash-race"}},
		"NFR-007": {Command: "go test ./tests/compatibility/protocol -count=1", EvidenceIDs: []string{"protocol-format"}},
		"NFR-008": {Command: "go test ./tests/contract/api ./tests/contract/cli -count=1", EvidenceIDs: []string{"protocol-format"}},
		"NFR-009": {Command: "go test ./internal/server ./tests/crash -count=1", EvidenceIDs: []string{"storage-format", "crash-race"}},
		"NFR-010": {Command: performance, EvidenceIDs: []string{"performance-results"}},
		"NFR-011": {Command: "go test ./tests/compatibility/... -count=1", EvidenceIDs: []string{"protocol-format", "storage-format"}},
		"NFR-012": {Command: "go test ./tests/contract/modelops ./tests/contract/cli -count=1", EvidenceIDs: []string{"adoption-contract", "crash-race"}},
	}
	for id, criterion := range criteria {
		authority.Criteria[id] = criterion
	}
	return authority
}

func readCommittedManifest(t *testing.T, root string) adoption.ReleaseManifest {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(root, "docs/evidence/performance/release-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest adoption.ReleaseManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestLivingDocumentationLinksAndLockedSurface(t *testing.T) {
	root := repositoryRoot(t)
	documents := []string{
		"README.md",
		"docs/formats/protocol-v1.md",
		"docs/formats/storage-v1.md",
		"docs/evidence/performance/README.md",
	}
	link := regexp.MustCompile(`\[[^]]+\]\(([^)]+)\)`)
	for _, relative := range documents {
		path := filepath.Join(root, relative)
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range link.FindAllSubmatch(encoded, -1) {
			target := string(match[1])
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "#") {
				continue
			}
			if index := strings.IndexByte(target, '#'); index >= 0 {
				target = target[:index]
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), filepath.FromSlash(target))); err != nil {
				t.Errorf("%s has broken link %q: %v", relative, target, err)
			}
		}
	}

	protocol, err := os.ReadFile(filepath.Join(root, "docs/formats/protocol-v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(protocol)
	for _, token := range []string{
		"get", "put", "put_if_absent", "compare_and_swap", "delete_if_revision", "atomic_batch", "scan_prefix", "watch",
		"status", "health", "stats", "describe_config", "verify", "create_snapshot", "compact", "backup",
		"offline_inspect", "offline_verify", "offline_recover_propose", "offline_recover_apply",
		"max_key_bytes", "max_value_bytes", "max_frame_bytes", "max_batch_bytes", "max_batch_operations", "max_scan_records", "max_scan_bytes", "watch_buffer_events", "max_watch_clients", "max_concurrent_clients", "max_history_events", "max_history_bytes",
		"Memory acknowledgement", "not durable", "0600",
	} {
		if !strings.Contains(text, token) {
			t.Errorf("protocol documentation omits locked token %q", token)
		}
	}
	storage, err := os.ReadFile(filepath.Join(root, "docs/formats/storage-v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"GAPID001", "GAPCUR01", "GAPWAL01", "GAPSNAP1", "CRC32C", "SHA-256", "0700", "0600", "incomplete final frame"} {
		if !strings.Contains(string(storage), token) {
			t.Errorf("storage documentation omits locked token %q", token)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
