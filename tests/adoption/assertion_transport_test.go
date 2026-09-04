package adoption_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/protocol"
	"github.com/spec-kitty/gapdb/internal/server"
	"github.com/spec-kitty/gapdb/tests/adoption"
)

func TestAssertionQualificationIsAdditiveToFrozenAdoptionContract(t *testing.T) {
	if adoption.ContractVersion != "gapdb-phase1/v1" || adoption.ScenarioCount != 14 || len(adoption.ScenarioIDs()) != 14 {
		t.Fatalf("portable adoption contract drifted: version=%q count=%d ids=%d", adoption.ContractVersion, adoption.ScenarioCount, len(adoption.ScenarioIDs()))
	}
}

func TestCombinedOperationAndPublicByteBoundariesLMinusOneLAndLPlusOne(t *testing.T) {
	batch := gapdb.Batch{
		Ack: gapdb.AckMemory,
		Assertions: []gapdb.Assertion{
			{Key: "guard/a", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}},
			{Key: "guard/b", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}},
		},
		Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("target/a", []byte("a"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			gapdb.NewPutMutation("target/b", []byte("b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		},
	}
	for _, maximum := range []int{3, 4, 5} {
		limits := gapdb.DefaultOptions().Limits
		limits.MaxBatchOperations = maximum
		err := batch.Validate(limits)
		if maximum == 3 {
			if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBatchTooLarge}) {
				t.Fatalf("operation L-1 = %v", err)
			}
		} else if err != nil {
			t.Fatalf("operation limit %d = %v", maximum, err)
		}
	}

	exactBytes := 32
	for _, assertion := range batch.Assertions {
		exactBytes += 20 + len(assertion.Key)
	}
	for _, mutation := range batch.Mutations {
		exactBytes += 20 + len(mutation.Key) + len(mutation.Value)
	}
	for _, maximum := range []int{exactBytes - 1, exactBytes, exactBytes + 1} {
		limits := gapdb.DefaultOptions().Limits
		limits.MaxBatchBytes = maximum
		err := batch.Validate(limits)
		if maximum == exactBytes-1 {
			if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBatchTooLarge}) {
				t.Fatalf("public byte L-1 = %v", err)
			}
		} else if err != nil {
			t.Fatalf("public byte limit %d = %v", maximum, err)
		}
	}
}

func TestCanonicalAssertionJSONByteBoundaryThroughRealUnixServer(t *testing.T) {
	request := protocol.Request{
		SchemaVersion: protocol.SchemaVersion,
		Operation:     protocol.OperationAtomicBatch,
		Arguments: protocol.BatchArguments{
			Ack:        gapdb.AckMemory,
			Assertions: []gapdb.Assertion{{Key: "wire/guard", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}},
			Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("wire/target", []byte("opaque"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
		},
	}
	payload, err := protocol.EncodeRequest(request, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil || !bytes.Equal(compact.Bytes(), payload) {
		t.Fatalf("request is not canonical compact JSON: %v", err)
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
			instance, err := server.Open(server.Config{Directory: filepath.Join(t.TempDir(), "database"), Options: options, ToolVersion: "assertion-wire-boundary"})
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close(context.Background())
			connection, err := net.Dial("unix", instance.SocketPath())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := protocol.WriteFrame(connection, payload, options.Limits.MaxFrameBytes); err != nil {
				t.Fatal(err)
			}
			encoded, err := protocol.ReadFrame(connection, options.Limits.MaxFrameBytes)
			if err != nil {
				t.Fatal(err)
			}
			response, err := protocol.DecodeResponse(encoded, options.Limits.MaxFrameBytes)
			if err != nil {
				t.Fatal(err)
			}
			if maximum == exact-1 {
				if response.Error == nil || response.Error.Code != gapdb.CodeBatchTooLarge {
					t.Fatalf("wire L-1 response = %+v", response)
				}
				return
			}
			result, ok := response.Result.(gapdb.MutationResult)
			if response.Error != nil || !ok || result.AssertionCount != 1 || result.MutationCount != 1 {
				t.Fatalf("wire limit %d response = %+v", maximum, response)
			}
		})
	}
}

func TestAssertionWatchesContainOnlyLiveAndRecoveredMutationEvents(t *testing.T) {
	backend := openGapdbBackend(t)
	defer backend.Close()
	guard, err := backend.client.Put(t.Context(), "watch/guard", []byte("authority"), nil, gapdb.AckDurable)
	if err != nil {
		t.Fatal(err)
	}
	live, err := backend.client.Watch(t.Context(), "watch/", guard.Revision)
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.client.AtomicBatch(t.Context(), gapdb.Batch{
		Ack:        gapdb.AckDurable,
		Assertions: []gapdb.Assertion{{Key: "watch/guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: guard.Revision}}},
		Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("watch/a", []byte("a"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			gapdb.NewPutMutation("watch/b", []byte("b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requireWatchMutationPair(t, live, result.Revision)
	live.Close()
	if err := backend.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	replayed, err := backend.client.Watch(t.Context(), "watch/", guard.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer replayed.Close()
	requireWatchMutationPair(t, replayed, result.Revision)
}

func requireWatchMutationPair(t *testing.T, watch *gapdb.Watch, revision gapdb.Revision) {
	t.Helper()
	for order, key := range []string{"watch/a", "watch/b"} {
		select {
		case event := <-watch.Events:
			if event.Key != key || event.Revision != revision || event.Order != uint32(order) {
				t.Fatalf("watch event %d = %+v", order, event)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("watch event %d timed out", order)
		}
	}
}

func TestRawAssertionRequestResponseLossRecoversCommittedMutation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "database")
	seed, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "raw-response-loss"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := gapdb.Dial(seed.SocketPath(), gapdb.ClientOptions{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := client.Put(t.Context(), "raw/guard", []byte("authority"), nil, gapdb.AckDurable)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := seed.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	injector := faultfs.NewInjector(97, faultfs.Rule{Point: faultfs.PointResponsePublish, Phase: faultfs.Before, Occurrence: 1, Seed: 97, Err: errors.New("drop raw response")})
	instance, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "raw-response-loss", FS: faultfs.NewOS(injector)})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("unix", instance.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.EncodeRequest(protocol.Request{SchemaVersion: 1, Operation: protocol.OperationAtomicBatch, Arguments: protocol.BatchArguments{
		Ack:        gapdb.AckDurable,
		Assertions: []gapdb.Assertion{{Key: "raw/guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: guard.Revision}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("raw/target", []byte("committed"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}}, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(connection, payload, gapdb.DefaultOptions().Limits.MaxFrameBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.ReadFrame(connection, gapdb.DefaultOptions().Limits.MaxFrameBytes); err == nil {
		t.Fatal("raw response-loss request unexpectedly returned a result")
	}
	_ = connection.Close()
	if err := instance.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err := server.Open(server.Config{Directory: directory, Options: gapdb.DefaultOptions(), ToolVersion: "raw-response-loss"})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(context.Background())
	reconciler, err := gapdb.Dial(restarted.SocketPath(), gapdb.ClientOptions{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	record, err := reconciler.Get(t.Context(), "raw/target")
	if err != nil || string(record.Value) != "committed" || record.Revision <= guard.Revision {
		t.Fatalf("raw response-loss recovery = %+v, %v", record, err)
	}
}
