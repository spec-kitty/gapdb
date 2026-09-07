package protocol_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	wire "github.com/spec-kitty/gapdb/internal/protocol"
)

func TestRequestGoldenFixtures(t *testing.T) {
	forEachFixtureLine(t, "requests.golden.jsonl", func(t *testing.T, fixture []byte) {
		request, err := wire.DecodeRequest(fixture, gapdb.DefaultOptions().Limits)
		if err != nil {
			t.Fatalf("DecodeRequest() = %v\nfixture: %s", err, fixture)
		}
		canonical, err := wire.EncodeRequest(request, gapdb.DefaultOptions().Limits)
		if err != nil {
			t.Fatalf("EncodeRequest() = %v", err)
		}
		if !bytes.Equal(canonical, fixture) {
			t.Fatalf("canonical request mismatch:\n got: %s\nwant: %s", canonical, fixture)
		}
	})
}

func TestSuccessAndWatchGoldenFixtures(t *testing.T) {
	forEachFixtureLine(t, "success.golden.jsonl", func(t *testing.T, fixture []byte) {
		response, err := wire.DecodeResponse(fixture, gapdb.DefaultMaxFrameBytes)
		if err != nil {
			t.Fatalf("DecodeResponse() = %v\nfixture: %s", err, fixture)
		}
		canonical, err := wire.EncodeResponse(response)
		if err != nil {
			t.Fatalf("EncodeResponse() = %v", err)
		}
		if !bytes.Equal(canonical, fixture) {
			t.Fatalf("canonical response mismatch:\n got: %s\nwant: %s", canonical, fixture)
		}
	})
}

func TestEveryOnlineOperationHasFrozenRequestAndSuccessAuthority(t *testing.T) {
	operations := wire.Operations()
	requests := make(map[wire.Operation]bool, len(operations))
	forEachFixtureLine(t, "requests.golden.jsonl", func(t *testing.T, fixture []byte) {
		request, err := wire.DecodeRequest(fixture, gapdb.DefaultOptions().Limits)
		if err != nil {
			t.Fatal(err)
		}
		requests[request.Operation] = true
	})
	successes := make(map[wire.Operation]bool, len(operations))
	forEachFixtureLine(t, "success.golden.jsonl", func(t *testing.T, fixture []byte) {
		response, err := wire.DecodeResponse(fixture, gapdb.DefaultMaxFrameBytes)
		if err != nil {
			t.Fatal(err)
		}
		successes[response.Operation] = true
	})
	for _, operation := range operations {
		if !operation.Valid() {
			t.Fatalf("frozen operation %q is not valid", operation)
		}
		if operation == wire.OperationReadRecoverySnapshot {
			if requests[operation] || successes[operation] {
				t.Fatal("candidate recovery extension was folded into adopted protocol-v1 goldens")
			}
			continue
		}
		if !requests[operation] {
			t.Errorf("operation %q has no canonical request fixture", operation)
		}
		if !successes[operation] {
			t.Errorf("operation %q has no canonical success fixture", operation)
		}
	}
}

func TestRecoverySnapshotCandidateExtensionHasIndependentRequestAndSuccessGolden(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("testdata", "recovery-snapshot-extension-v1.request.json"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := wire.DecodeRequest(encoded, gapdb.DefaultOptions().Limits)
	if err != nil || request.Operation != wire.OperationReadRecoverySnapshot {
		t.Fatalf("candidate extension request=%+v err=%v", request, err)
	}
	if !recoverySnapshotGolden(t) {
		t.Fatal("candidate extension success golden is absent")
	}
}

func recoverySnapshotGolden(t *testing.T) bool {
	t.Helper()
	request := gapdb.RecoverySnapshotRequest{Prefix: "workflows/", ExpectedRevision: 102, MaxRecords: 1000, MaxBytes: gapdb.HardMaxRecoveryBytes}
	expires := time.Date(2026, 8, 23, 18, 30, 0, 0, time.UTC)
	payload, err := gapdb.EncodeRecoverySnapshot(gapdb.RecoverySnapshotResult{
		DatabaseID:       "0198f4d4f26a7b1ca3df00c30ca93e73",
		RequestID:        "req-recovery",
		ObservedRevision: 102,
		AsOf:             time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC),
		Records:          []gapdb.Record{{Key: "workflows/123", Value: []byte{1, 2, 3}, Revision: 45, ExpiresAt: &expires}},
	}, request)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(payload))
	const want = "3970dd41fcb1a3ade7147d8ef6fcbcca1279a9e5ff42c6ebf30fc019874438b4"
	if got != want {
		t.Fatalf("recovery binary golden digest = %s, want %s", got, want)
	}
	decoded, err := gapdb.DecodeRecoverySnapshot(payload, "req-recovery", request)
	if err != nil || len(decoded.Records) != 1 || decoded.Records[0].Key != "workflows/123" {
		t.Fatalf("recovery binary golden decode = %+v, %v", decoded, err)
	}
	return true
}

func TestRecoverySnapshotCandidateExtensionIsSeparateFromFrozenProtocolV1(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := os.ReadFile(filepath.Join(root, "docs/formats/protocol-v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(frozen)); got != "68c9b7f52714620d649c6e47b7c56bda10318b697ccbb5bfb88d58f01a9fd9a2" {
		t.Fatalf("adopted protocol-v1 authority changed: %s", got)
	}
	extension, err := os.ReadFile(filepath.Join(root, "docs/formats/recovery-snapshot-extension-v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"technical candidate extension", "does not amend", "read_recovery_snapshot", "GDBREC1", "one recovery snapshot construction at a time", "cannot reslice or append into an adjacent record"} {
		if !bytes.Contains(extension, []byte(token)) {
			t.Fatalf("candidate extension authority token %q is absent", token)
		}
	}
}

func TestEveryStableErrorHasGoldenFixture(t *testing.T) {
	want := make(map[gapdb.ErrorCode]bool)
	for _, definition := range gapdb.ErrorDefinitions() {
		want[definition.Code] = false
	}
	forEachFixtureLine(t, "errors.golden.jsonl", func(t *testing.T, fixture []byte) {
		response, err := wire.DecodeResponse(fixture, gapdb.DefaultMaxFrameBytes)
		if err != nil {
			t.Fatalf("DecodeResponse() = %v\nfixture: %s", err, fixture)
		}
		if _, ok := want[response.Error.Code]; !ok {
			t.Fatalf("fixture has undocumented code %q", response.Error.Code)
		}
		if want[response.Error.Code] {
			t.Fatalf("duplicate fixture for %q", response.Error.Code)
		}
		want[response.Error.Code] = true
		canonical, err := wire.EncodeResponse(response)
		if err != nil {
			t.Fatalf("EncodeResponse() = %v", err)
		}
		if !bytes.Equal(canonical, fixture) {
			t.Fatalf("canonical error mismatch:\n got: %s\nwant: %s", canonical, fixture)
		}
	})
	for code, covered := range want {
		if !covered {
			t.Errorf("missing golden fixture for %s", code)
		}
	}
}

func TestEveryStableErrorCanTerminateWatchRegistration(t *testing.T) {
	forEachFixtureLine(t, "errors.golden.jsonl", func(t *testing.T, fixture []byte) {
		response, err := wire.DecodeResponse(fixture, gapdb.DefaultMaxFrameBytes)
		if err != nil {
			t.Fatal(err)
		}
		response.Operation = wire.OperationWatch
		response.Stream = wire.StreamEnded
		response.EndReason = gapdb.WatchEndedByError
		encoded, err := wire.EncodeResponse(response)
		if err != nil {
			t.Fatalf("EncodeResponse(%s watch failure) = %v", response.Error.Code, err)
		}
		decoded, err := wire.DecodeResponse(encoded, gapdb.DefaultMaxFrameBytes)
		if err != nil || decoded.Stream != wire.StreamEnded || decoded.EndReason != gapdb.WatchEndedByError || decoded.Error == nil || decoded.Error.Code != response.Error.Code {
			t.Fatalf("watch failure round trip = %+v, %v", decoded, err)
		}
	})
}

func TestNegativeGoldenFixtures(t *testing.T) {
	forEachFixtureLine(t, "negative.golden.jsonl", func(t *testing.T, fixture []byte) {
		var entry struct {
			Name           string          `json:"name"`
			PayloadBase64  string          `json:"payload_base64"`
			Generator      string          `json:"generator"`
			Code           gapdb.ErrorCode `json:"code"`
			Field          string          `json:"field"`
			ReasonContains string          `json:"reason_contains"`
			Evidence       string          `json:"evidence"`
		}
		if err := json.Unmarshal(fixture, &entry); err != nil {
			t.Fatalf("invalid fixture wrapper: %v", err)
		}
		t.Run(entry.Name, func(t *testing.T) {
			payload, limits := negativePayload(t, entry.Generator, entry.PayloadBase64)
			var err error
			if entry.Generator == "frame-too-large" {
				_, err = wire.ReadFrame(bytes.NewReader(payload), limits.MaxFrameBytes)
			} else {
				_, err = wire.DecodeRequest(payload, limits)
			}
			if !errors.Is(err, &gapdb.Error{Code: entry.Code}) {
				t.Fatalf("decode = %v, want %s", err, entry.Code)
			}
			var structured *gapdb.Error
			if !errors.As(err, &structured) {
				t.Fatalf("decode = %T, want *gapdb.Error", err)
			}
			if entry.Field != "" && structured.Field != entry.Field {
				t.Errorf("error field = %q, want %q", structured.Field, entry.Field)
			}
			if entry.ReasonContains != "" && !strings.Contains(structured.Reason, entry.ReasonContains) {
				t.Errorf("error reason = %q, want substring %q", structured.Reason, entry.ReasonContains)
			}
			switch entry.Evidence {
			case "received_bytes":
				if structured.ReceivedBytes <= structured.MaximumBytes {
					t.Errorf("byte evidence = %d/%d, want received > maximum", structured.ReceivedBytes, structured.MaximumBytes)
				}
			case "received_operations":
				if structured.ReceivedOperations <= structured.MaximumOperations {
					t.Errorf("operation evidence = %d/%d, want received > maximum", structured.ReceivedOperations, structured.MaximumOperations)
				}
			}
		})
	})
}

func negativePayload(t *testing.T, generator, encoded string) ([]byte, gapdb.Limits) {
	t.Helper()
	limits := gapdb.DefaultOptions().Limits
	marshal := func(value any) []byte {
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	request := func(operation wire.Operation, requestID string, arguments any) []byte {
		return marshal(struct {
			SchemaVersion uint64         `json:"schema_version"`
			RequestID     string         `json:"request_id,omitempty"`
			Operation     wire.Operation `json:"operation"`
			Arguments     any            `json:"arguments"`
		}{wire.SchemaVersion, requestID, operation, arguments})
	}

	switch generator {
	case "":
		payload, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("invalid fixture payload encoding: %v", err)
		}
		return payload, limits
	case "key-too-large":
		return request(wire.OperationGet, "", map[string]any{"key": strings.Repeat("k", limits.MaxKeyBytes+1)}), limits
	case "value-too-large":
		return request(wire.OperationPut, "", map[string]any{"key": "k", "value_base64": make([]byte, limits.MaxValueBytes+1), "ack": gapdb.AckMemory}), limits
	case "frame-too-large":
		prefix := make([]byte, 4)
		binary.BigEndian.PutUint32(prefix, uint32(limits.MaxFrameBytes+1))
		return prefix, limits
	case "batch-operations-too-large":
		mutations := make([]map[string]any, limits.MaxBatchOperations+1)
		for i := range mutations {
			mutations[i] = map[string]any{
				"kind":         gapdb.MutationPut,
				"key":          "k" + itoa(i),
				"condition":    gapdb.Condition{Kind: gapdb.ConditionAny},
				"value_base64": "",
			}
		}
		return request(wire.OperationAtomicBatch, "", map[string]any{"ack": gapdb.AckMemory, "mutations": mutations}), limits
	case "batch-bytes-too-large":
		limits.MaxFrameBytes = gapdb.HardMaxFrameBytes
		mutations := []gapdb.Mutation{
			gapdb.NewPutMutation("a", make([]byte, gapdb.DefaultMaxValueBytes), gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
			gapdb.NewPutMutation("b", make([]byte, gapdb.DefaultMaxValueBytes), gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
		}
		return request(wire.OperationAtomicBatch, "", wire.BatchArguments{Ack: gapdb.AckMemory, Mutations: mutations}), limits
	case "read-many-operations-too-large":
		keys := make([]string, limits.MaxBatchOperations+1)
		for index := range keys {
			keys[index] = "key-" + itoa(index)
		}
		return request(wire.OperationGetMany, "", wire.GetManyArguments{Keys: keys}), limits
	case "read-many-bytes-too-large":
		limits.MaxBatchBytes = 32
		return request(wire.OperationGetMany, "", wire.GetManyArguments{Keys: []string{strings.Repeat("\\\"", 20)}}), limits
	case "request-id-too-large":
		return request(wire.OperationGet, strings.Repeat("r", 257), wire.GetArguments{Key: "k"}), limits
	case "scan-limit-too-large":
		return request(wire.OperationScanPrefix, "", wire.ScanArguments{Prefix: "k", Limit: limits.MaxScanRecords + 1}), limits
	default:
		t.Fatalf("unknown negative fixture generator %q", generator)
		return nil, limits
	}
}

func TestNegativeGoldenCorpusCoverage(t *testing.T) {
	required := map[string]bool{
		"duplicate-field":                false,
		"key-too-large":                  false,
		"value-too-large":                false,
		"frame-too-large":                false,
		"batch-operations-too-large":     false,
		"batch-bytes-too-large":          false,
		"read-many-operations-too-large": false,
		"read-many-bytes-too-large":      false,
		"request-id-too-large":           false,
		"scan-limit-too-large":           false,
	}
	forEachFixtureLine(t, "negative.golden.jsonl", func(t *testing.T, fixture []byte) {
		var entry struct {
			Name           string `json:"name"`
			ReasonContains string `json:"reason_contains"`
		}
		if err := json.Unmarshal(fixture, &entry); err != nil {
			t.Fatal(err)
		}
		if _, ok := required[entry.Name]; ok {
			required[entry.Name] = true
		}
		if entry.Name == "duplicate-field" && entry.ReasonContains == "" {
			t.Error("duplicate-field fixture must assert the duplicate detector's reason")
		}
	})
	for name, covered := range required {
		if !covered {
			t.Errorf("missing negative boundary fixture %q", name)
		}
	}
}

func forEachFixtureLine(t *testing.T, name string, check func(*testing.T, []byte)) {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), gapdb.DefaultMaxFrameBytes)
	line := 0
	for scanner.Scan() {
		line++
		fixture := bytes.Clone(scanner.Bytes())
		if len(bytes.TrimSpace(fixture)) == 0 {
			continue
		}
		t.Run(name+"/line-"+itoa(line), func(t *testing.T) { check(t, fixture) })
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}
