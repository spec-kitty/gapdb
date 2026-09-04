package gapdb_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/protocol"
)

func TestReviewerClientRejectsNonCanonicalNestedResults(t *testing.T) {
	tests := []string{
		`{"record":{"key":"key","value_base64":"dg==","revision":1,"unknown":true}}`,
		`{"record":{"key":"key","key":"other","value_base64":"dg==","revision":1}}`,
		`{"record":{"key":"key","value_base64":"Zh==","revision":1}}`,
		`{"record":{"key":"key","value_base64":"dg==","revision":1,"expires_at":"2026-08-23T15:00:00-05:00"}}`,
	}
	for _, result := range tests {
		t.Run(result, func(t *testing.T) {
			canonical := []byte(`{"schema_version":1,"ok":true,"request_id":"request","database_id":"00112233445566778899aabbccddeeff","operation":"get","result":` + result + `}`)
			if decoded, err := protocol.DecodeResponse(canonical, gapdb.DefaultMaxFrameBytes); err == nil {
				t.Fatalf("canonical decoder accepted noncanonical result: %+v", decoded)
			}
			path := filepath.Join(t.TempDir(), "strict.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				probe, _ := listener.Accept()
				if probe != nil {
					_ = probe.Close()
				}
				conn, _ := listener.Accept()
				if conn == nil {
					return
				}
				defer conn.Close()
				payload, _ := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
				request, _ := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
				envelope := `{"schema_version":1,"ok":true,"request_id":` + quote(request.RequestID) + `,"database_id":"00112233445566778899aabbccddeeff","operation":"get","result":` + result + `}`
				_ = protocol.WriteFrame(conn, []byte(envelope), gapdb.DefaultMaxFrameBytes)
			}()
			client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if record, err := client.Get(t.Context(), "key"); err == nil {
				t.Fatalf("accepted noncanonical record: %+v", record)
			}
		})
	}
}

func TestReviewerClientAcceptsEveryCanonicalErrorSchema(t *testing.T) {
	file, err := os.Open("../tests/compatibility/protocol/testdata/errors.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var original struct {
			Operation string      `json:"operation"`
			Error     gapdb.Error `json:"error"`
		}
		if err := json.Unmarshal(line, &original); err != nil {
			t.Fatal(err)
		}
		t.Run(string(original.Error.Code), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "error.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				probe, _ := listener.Accept()
				if probe != nil {
					_ = probe.Close()
				}
				conn, _ := listener.Accept()
				if conn == nil {
					return
				}
				defer conn.Close()
				payload, _ := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
				request, _ := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
				var envelope map[string]any
				_ = json.Unmarshal(line, &envelope)
				envelope["request_id"] = request.RequestID
				encoded, _ := json.Marshal(envelope)
				_ = protocol.WriteFrame(conn, encoded, gapdb.DefaultMaxFrameBytes)
			}()
			client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if original.Operation == "watch" {
				_, err = client.Watch(t.Context(), "", 1)
			} else {
				_, err = client.Get(t.Context(), "key")
			}
			if !errors.Is(err, &gapdb.Error{Code: original.Error.Code}) {
				t.Fatalf("Get error = %#v, want %s", err, original.Error.Code)
			}
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestDialFailureIsTransportError(t *testing.T) {
	_, err := gapdb.Dial(filepath.Join(t.TempDir(), "missing.sock"), gapdb.ClientOptions{})
	var transport *gapdb.TransportError
	if !errors.As(err, &transport) || errors.Is(err, &gapdb.Error{Code: gapdb.CodeServerUnavailable}) {
		t.Fatalf("Dial error = %#v", err)
	}
}

func TestReviewerCloseReservesPendingWatchBeforeHandshake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestRead := make(chan protocol.Request, 1)
	release := make(chan struct{})
	go func() {
		probe, _ := listener.Accept()
		if probe != nil {
			_ = probe.Close()
		}
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		payload, readErr := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
		if readErr != nil {
			return
		}
		request, decodeErr := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
		if decodeErr != nil {
			return
		}
		requestRead <- request
		<-release
		encoded, _ := protocol.EncodeResponse(protocol.Response{SchemaVersion: 1, OK: true, RequestID: request.RequestID, DatabaseID: "00112233445566778899aabbccddeeff", Operation: protocol.OperationWatch, Stream: protocol.StreamStarted, RegistrationRevision: 0})
		_ = protocol.WriteFrame(conn, encoded, gapdb.DefaultMaxFrameBytes)
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	type watchResult struct {
		watch *gapdb.Watch
		err   error
	}
	result := make(chan watchResult, 1)
	go func() {
		watch, err := client.Watch(context.Background(), "", 0)
		result <- watchResult{watch, err}
	}()
	<-requestRead
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case outcome := <-result:
		if outcome.watch != nil || outcome.err == nil {
			if outcome.watch != nil {
				outcome.watch.Close()
			}
			t.Fatalf("Watch after concurrent Close = %v, %v", outcome.watch, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending Watch was not canceled")
	}
	if watch, err := client.Watch(t.Context(), "", 0); err == nil || watch != nil {
		t.Fatalf("Watch after Close = %v, %v", watch, err)
	}
}

func TestClientDeadlineAndLostResponseNeverRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var requests atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Dial performs one availability probe.
		probe, _ := listener.Accept()
		if probe != nil {
			_ = probe.Close()
		}
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		var prefix [4]byte
		if _, err := conn.Read(prefix[:]); err == nil {
			requests.Add(1)
		}
		<-time.After(100 * time.Millisecond)
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = client.Put(ctx, "key", []byte("value"), nil, gapdb.AckMemory)
	var transport *gapdb.TransportError
	if !errors.As(err, &transport) || !transport.Ambiguous {
		t.Fatalf("Put error = %#v", err)
	}
	<-done
	if requests.Load() != 1 {
		t.Fatalf("request attempts = %d, want one", requests.Load())
	}
}

func TestClientAtomicBatchSendsAssertionsAndRequiresExactCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assertions.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestSeen := make(chan protocol.Request, 1)
	go func() {
		probe, _ := listener.Accept()
		if probe != nil {
			_ = probe.Close()
		}
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		payload, _ := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
		request, _ := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
		requestSeen <- request
		encoded, _ := protocol.EncodeResponse(protocol.Response{
			SchemaVersion: 1, OK: true, RequestID: request.RequestID,
			DatabaseID: "00112233445566778899aabbccddeeff", Operation: protocol.OperationAtomicBatch,
			Result: gapdb.MutationResult{Revision: 7, Ack: gapdb.AckDurable, DurableThroughRevision: 7, MutationCount: 1, AssertionCount: 2},
		})
		_ = protocol.WriteFrame(conn, encoded, gapdb.DefaultMaxFrameBytes)
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	batch := gapdb.Batch{Ack: gapdb.AckDurable,
		Assertions: []gapdb.Assertion{{Key: "authority", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 6}}, {Key: "vacant", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("created", []byte("secret"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}
	result, err := client.AtomicBatch(t.Context(), batch)
	if err != nil || result.MutationCount != 1 || result.AssertionCount != 2 {
		t.Fatalf("AtomicBatch() = %+v, %v", result, err)
	}
	request := <-requestSeen
	arguments, ok := request.Arguments.(protocol.BatchArguments)
	if !ok || len(arguments.Assertions) != 2 || len(arguments.Mutations) != 1 || arguments.Assertions[0].Key != "authority" {
		t.Fatalf("wire request = %#v", request.Arguments)
	}
}

func TestClientAtomicBatchRejectsAssertionOrMutationCountMismatch(t *testing.T) {
	batch := gapdb.Batch{
		Ack:        gapdb.AckDurable,
		Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("created", []byte{1}, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}
	for name, result := range map[string]string{
		"assertion count removed":   `{"revision":7,"ack":"durable","durable_through_revision":7,"mutation_count":1}`,
		"assertion count increased": `{"revision":7,"ack":"durable","durable_through_revision":7,"mutation_count":1,"assertion_count":2}`,
		"mutation count removed":    `{"revision":7,"ack":"durable","durable_through_revision":7,"assertion_count":1}`,
		"mutation count increased":  `{"revision":7,"ack":"durable","durable_through_revision":7,"mutation_count":2,"assertion_count":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := dialUnaryFixture(t, protocol.OperationAtomicBatch, result)
			if _, err := client.AtomicBatch(t.Context(), batch); err == nil {
				t.Fatalf("AtomicBatch accepted %s", result)
			}
		})
	}
}

func TestClientRejectsAssertionCountOutsideAtomicBatch(t *testing.T) {
	client := dialUnaryFixture(t, protocol.OperationPut, `{"revision":7,"ack":"durable","durable_through_revision":7,"assertion_count":1}`)
	if _, err := client.Put(t.Context(), "created", []byte{1}, nil, gapdb.AckDurable); err == nil {
		t.Fatal("Put accepted assertion_count")
	}
}

func TestClientDecodesAssertionFailureEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assertion-error.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		probe, _ := listener.Accept()
		if probe != nil {
			_ = probe.Close()
		}
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		payload, _ := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
		request, _ := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
		envelope := `{"schema_version":1,"ok":false,"request_id":` + quote(request.RequestID) + `,"database_id":"00112233445566778899aabbccddeeff","operation":"atomic_batch","error":{"code":"CONDITION_FAILED","message":"Atomic batch assertion failed.","retry":"after_reconcile","key":"guard","assertion_index":0,"condition":"revision","expected_revision":6,"actual_revision":7,"safe_actions":["get","rebuild_batch","abort"]}}`
		_ = protocol.WriteFrame(conn, []byte(envelope), gapdb.DefaultMaxFrameBytes)
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	batch := gapdb.Batch{
		Ack:        gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 6}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("created", []byte{1}, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}
	_, err = client.AtomicBatch(t.Context(), batch)
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.Code != gapdb.CodeConditionFailed || failure.AssertionIndex == nil || *failure.AssertionIndex != 0 || failure.ExpectedRevision == nil || *failure.ExpectedRevision != 6 {
		t.Fatalf("AtomicBatch error = %#v (cause %v)", err, errors.Unwrap(err))
	}
}

func dialUnaryFixture(t *testing.T, operation protocol.Operation, result string) *gapdb.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unary.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		probe, _ := listener.Accept()
		if probe != nil {
			_ = probe.Close()
		}
		conn, _ := listener.Accept()
		if conn == nil {
			return
		}
		defer conn.Close()
		payload, _ := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
		request, _ := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
		envelope := `{"schema_version":1,"ok":true,"request_id":` + quote(request.RequestID) + `,"database_id":"00112233445566778899aabbccddeeff","operation":` + quote(string(operation)) + `,"result":` + result + `}`
		_ = protocol.WriteFrame(conn, []byte(envelope), gapdb.DefaultMaxFrameBytes)
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
