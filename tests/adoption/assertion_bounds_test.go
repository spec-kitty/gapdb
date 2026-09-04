package adoption_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/protocol"
	"github.com/spec-kitty/gapdb/internal/server"
)

type rejectionFingerprint struct {
	currentRevision        gapdb.Revision
	durableThroughRevision gapdb.Revision
	reservedRevisionEnd    gapdb.Revision
	recordCount            int
	liveRecords            int
	expiredRecords         int
	commits                uint64
	conditionFailures      uint64
	walBytes               int64
	syncCount              uint64
}

func TestPublicAtomicBatchOperationAndByteBoundsAreZeroEffectAtLMinusOne(t *testing.T) {
	base := gapdb.Batch{
		Ack: gapdb.AckMemory,
		Assertions: []gapdb.Assertion{
			{Key: "public-bound/guard-a", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}},
			{Key: "public-bound/guard-b", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}},
		},
		Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("public-bound/target-a", []byte("a"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, timePointer(time.Now().Add(time.Hour))),
			gapdb.NewPutMutation("public-bound/target-b", []byte("b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		},
	}
	exactBytes := 32
	for _, assertion := range base.Assertions {
		exactBytes += 20 + len(assertion.Key)
	}
	for _, mutation := range base.Mutations {
		exactBytes += 20 + len(mutation.Key) + len(mutation.Value)
	}
	cases := []struct {
		name       string
		operations int
		bytes      int
		accept     bool
	}{
		{"operations_l_minus_1", 3, gapdb.DefaultOptions().Limits.MaxBatchBytes, false},
		{"operations_l", 4, gapdb.DefaultOptions().Limits.MaxBatchBytes, true},
		{"operations_l_plus_1", 5, gapdb.DefaultOptions().Limits.MaxBatchBytes, true},
		{"bytes_l_minus_1", gapdb.DefaultOptions().Limits.MaxBatchOperations, exactBytes - 1, false},
		{"bytes_l", gapdb.DefaultOptions().Limits.MaxBatchOperations, exactBytes, true},
		{"bytes_l_plus_1", gapdb.DefaultOptions().Limits.MaxBatchOperations, exactBytes + 1, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			received := 4
			limit := test.operations
			if test.bytes != gapdb.DefaultOptions().Limits.MaxBatchBytes {
				received, limit = exactBytes, test.bytes
			}
			if !boundaryDecisionValid(received, limit, test.accept) {
				t.Fatalf("boundary case does not encode the exact inclusive limit: received=%d limit=%d accept=%v", received, limit, test.accept)
			}
			options := gapdb.DefaultOptions()
			options.Limits.MaxBatchOperations = test.operations
			options.Limits.MaxBatchBytes = test.bytes
			instance, client, directory := openBoundaryServer(t, gapdb.DefaultOptions(), options.Limits)
			watch, err := client.Watch(t.Context(), "public-bound/", 0)
			if err != nil {
				t.Fatal(err)
			}
			before := captureRejectionFingerprint(t, client)
			result, err := client.AtomicBatch(t.Context(), base)
			if test.accept {
				if err != nil || result.AssertionCount != 2 || result.MutationCount != 2 {
					t.Fatalf("accepted boundary result=%+v err=%v", result, err)
				}
				watch.Close()
				closeBoundaryServer(t, instance, client)
				return
			}
			requireBatchTooLargeDiagnostic(t, err, test.operations, test.bytes)
			assertRejectedBatchZeroEffect(t, client, watch, before, "public-bound/", []string{"public-bound/target-a", "public-bound/target-b"})
			closeBoundaryServer(t, instance, client)
			assertBoundaryRestartContainsOnlyDelimiter(t, directory, gapdb.DefaultOptions(), "public-bound/", []string{"public-bound/target-a", "public-bound/target-b"})
		})
	}
}

func TestCanonicalUnixAssertionJSONBoundaryIsZeroEffectAtLMinusOne(t *testing.T) {
	request := protocol.Request{
		SchemaVersion: protocol.SchemaVersion,
		Operation:     protocol.OperationAtomicBatch,
		Arguments: protocol.BatchArguments{
			Ack:        gapdb.AckMemory,
			Assertions: []gapdb.Assertion{{Key: "protocol-bound/guard", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}},
			Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("protocol-bound/target", []byte("opaque"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, timePointer(time.Now().Add(time.Hour)))},
		},
	}
	payload, err := protocol.EncodeRequest(request, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	exact := len(envelope.Arguments)
	for _, maximum := range []int{exact - 1, exact, exact + 1} {
		t.Run(fmt.Sprintf("limit-%d", maximum), func(t *testing.T) {
			options := gapdb.DefaultOptions()
			options.Limits.MaxBatchBytes = maximum
			instance, client, directory := openBoundaryServer(t, options, gapdb.DefaultOptions().Limits)
			watch, err := client.Watch(t.Context(), "protocol-bound/", 0)
			if err != nil {
				t.Fatal(err)
			}
			before := captureRejectionFingerprint(t, client)
			connection, err := net.Dial("unix", instance.SocketPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := protocol.WriteFrame(connection, payload, options.Limits.MaxFrameBytes); err != nil {
				t.Fatal(err)
			}
			encoded, err := protocol.ReadFrame(connection, options.Limits.MaxFrameBytes)
			_ = connection.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) >= 4096 {
				t.Fatalf("boundary diagnostic response=%d bytes", len(encoded))
			}
			response, err := protocol.DecodeResponse(encoded, options.Limits.MaxFrameBytes)
			if err != nil {
				t.Fatal(err)
			}
			if maximum >= exact {
				result, ok := response.Result.(gapdb.MutationResult)
				if response.Error != nil || !ok || result.AssertionCount != 1 || result.MutationCount != 1 {
					t.Fatalf("accepted wire boundary response=%+v", response)
				}
				watch.Close()
				closeBoundaryServer(t, instance, client)
				return
			}
			if response.Error == nil || response.Error.Code != gapdb.CodeBatchTooLarge || response.Error.ReceivedBytes != exact || response.Error.MaximumBytes != maximum || response.Error.OperationApplied {
				t.Fatalf("wire L-1 diagnostic=%+v", response.Error)
			}
			assertRejectedBatchZeroEffect(t, client, watch, before, "protocol-bound/", []string{"protocol-bound/target"})
			closeBoundaryServer(t, instance, client)
			assertBoundaryRestartContainsOnlyDelimiter(t, directory, options, "protocol-bound/", []string{"protocol-bound/target"})
		})
	}
}

func requireBatchTooLargeDiagnostic(t *testing.T, err error, operationLimit, byteLimit int) {
	t.Helper()
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.Code != gapdb.CodeBatchTooLarge || failure.OperationApplied || len(failure.Error()) >= 4096 {
		t.Fatalf("batch boundary diagnostic=%+v", err)
	}
	if operationLimit == 3 {
		if failure.ReceivedOperations != 4 || failure.MaximumOperations != 3 || failure.ReceivedBytes != 0 {
			t.Fatalf("operation diagnostic=%+v", failure)
		}
	} else if failure.ReceivedBytes != byteLimit+1 || failure.MaximumBytes != byteLimit || failure.ReceivedOperations != 0 {
		t.Fatalf("byte diagnostic=%+v", failure)
	}
}

func assertRejectedBatchZeroEffect(t *testing.T, client *gapdb.Client, watch *gapdb.Watch, before rejectionFingerprint, prefix string, targets []string) {
	t.Helper()
	for _, target := range targets {
		if record, err := client.Get(t.Context(), target); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
			t.Fatalf("rejected boundary materialized %s=%+v err=%v", target, record, err)
		}
	}
	after := captureRejectionFingerprint(t, client)
	if err := validateRejectedBoundaryFingerprint(before, after); err != nil {
		t.Fatal(err)
	}
	barrier, err := client.Put(t.Context(), prefix+"delimiter", []byte("barrier"), nil, gapdb.AckDurable)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-watch.Events:
		if event.Key != prefix+"delimiter" || event.Revision != barrier.Revision || event.Order != 0 {
			t.Fatalf("rejected boundary emitted non-delimiter event: %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delimiter watch event timed out")
	}
	watch.Close()
}

func validateRejectedBoundaryFingerprint(before, after rejectionFingerprint) error {
	if before != after {
		return fmt.Errorf("rejected boundary changed status/stats: before=%+v after=%+v", before, after)
	}
	return nil
}

func boundaryDecisionValid(received, maximum int, accepted bool) bool {
	return accepted == (received <= maximum)
}

func TestBoundaryQualificationKillsAfterEffectAndOffByOneMutants(t *testing.T) {
	before := rejectionFingerprint{currentRevision: 7, durableThroughRevision: 7, reservedRevisionEnd: 64, commits: 7, walBytes: 1024}
	afterEffect := before
	afterEffect.currentRevision++
	afterEffect.recordCount++
	afterEffect.commits++
	if err := validateRejectedBoundaryFingerprint(before, afterEffect); err == nil {
		t.Fatal("after-effect rejection mutant survived")
	}
	if boundaryDecisionValid(4, 4, false) {
		t.Fatal("exclusive/off-by-one limit mutant survived")
	}
	if !boundaryDecisionValid(3, 4, true) || !boundaryDecisionValid(4, 4, true) || !boundaryDecisionValid(5, 4, false) {
		t.Fatal("inclusive L-1/L/L+1 boundary control failed")
	}
}

func assertBoundaryRestartContainsOnlyDelimiter(t *testing.T, directory string, options gapdb.Options, prefix string, targets []string) {
	t.Helper()
	restarted, err := server.Open(server.Config{Directory: directory, Options: options, ToolVersion: "assertion-boundary-restart"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := gapdb.Dial(restarted.SocketPath(), gapdb.ClientOptions{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBoundaryServer(t, restarted, client)
	for _, target := range targets {
		if record, err := client.Get(t.Context(), target); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
			t.Fatalf("rejected target survived restart %s=%+v err=%v", target, record, err)
		}
	}
	watch, err := client.Watch(t.Context(), prefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	select {
	case event := <-watch.Events:
		if event.Key != prefix+"delimiter" || event.Order != 0 {
			t.Fatalf("restart history included rejected effect: %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart delimiter replay timed out")
	}
}

func captureRejectionFingerprint(t *testing.T, client *gapdb.Client) rejectionFingerprint {
	t.Helper()
	status, err := client.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stats, err := client.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rejectionFingerprint{
		status.CurrentRevision, status.DurableThroughRevision, status.ReservedRevisionEnd, status.RecordCount,
		stats.LiveRecords, stats.ExpiredRecords, stats.Commits, stats.ConditionFailures, stats.WALBytes, stats.SyncCount,
	}
}

func openBoundaryServer(t *testing.T, options gapdb.Options, clientLimits gapdb.Limits) (*server.Server, *gapdb.Client, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "database")
	instance, err := server.Open(server.Config{Directory: directory, Options: options, ToolVersion: "assertion-boundary"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := gapdb.Dial(instance.SocketPath(), gapdb.ClientOptions{Timeout: 3 * time.Second, Limits: clientLimits})
	if err != nil {
		_ = instance.Close(context.Background())
		t.Fatal(err)
	}
	return instance, client, directory
}

func closeBoundaryServer(t *testing.T, instance *server.Server, client *gapdb.Client) {
	t.Helper()
	_ = client.Close()
	if err := instance.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
