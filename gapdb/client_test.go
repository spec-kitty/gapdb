package gapdb_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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

func TestClientCanReuseOneUnaryConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reuse.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var accepted atomic.Int64
	served := make(chan error, 1)
	go func() {
		probe, acceptErr := listener.Accept()
		if acceptErr != nil {
			served <- acceptErr
			return
		}
		accepted.Add(1)
		_ = probe.Close()

		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			served <- acceptErr
			return
		}
		accepted.Add(1)
		defer conn.Close()
		for revision := gapdb.Revision(1); revision <= 2; revision++ {
			payload, readErr := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
			if readErr != nil {
				served <- readErr
				return
			}
			request, decodeErr := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
			if decodeErr != nil {
				served <- decodeErr
				return
			}
			response := protocol.Response{
				SchemaVersion: protocol.SchemaVersion,
				OK:            true,
				RequestID:     request.RequestID,
				DatabaseID:    "00112233445566778899aabbccddeeff",
				Operation:     request.Operation,
				Result: gapdb.MutationResult{
					Revision:               revision,
					Ack:                    gapdb.AckMemory,
					DurableThroughRevision: revision - 1,
				},
			}
			encoded, encodeErr := protocol.EncodeResponse(response)
			if encodeErr != nil {
				served <- encodeErr
				return
			}
			if writeErr := protocol.WriteFrame(conn, encoded, gapdb.DefaultMaxFrameBytes); writeErr != nil {
				served <- writeErr
				return
			}
		}
		served <- nil
	}()

	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second, ReuseUnaryConnection: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for index := 0; index < 2; index++ {
		if _, err := client.Put(t.Context(), "key", []byte("value"), nil, gapdb.AckMemory); err != nil {
			t.Fatalf("Put %d: %v", index, err)
		}
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if got := accepted.Load(); got != 2 {
		t.Fatalf("accepted connections = %d, want probe plus one reusable unary connection", got)
	}
}

func TestReusableUnaryCloseInterruptsLostMutationResponseWithoutReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lost-response.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan struct{})
	var requests atomic.Int64
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
		if _, readErr := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes); readErr == nil {
			requests.Add(1)
			close(received)
		}
		var one [1]byte
		_, _ = conn.Read(one[:])
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{ReuseUnaryConnection: true})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, callErr := client.Put(context.Background(), "authority", []byte("accepted"), nil, gapdb.AckDurable)
		result <- callErr
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("server did not receive mutation")
	}
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind an unresponsive unary call")
	}
	select {
	case err := <-result:
		var transport *gapdb.TransportError
		if !errors.As(err, &transport) || !transport.Ambiguous {
			t.Fatalf("mutation error = %#v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mutation remained blocked after Close")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("received mutation requests = %d, want exactly one", got)
	}
}

func TestReusableUnaryCanceledWaiterDoesNotTransmitOrPoisonConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queued-cancel.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	firstReceived := make(chan protocol.Request, 1)
	release := make(chan struct{})
	var requests atomic.Int64
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
		requests.Add(1)
		firstReceived <- request
		<-release
		record := gapdb.NewRecord("first", []byte("value"), 1, nil)
		response, _ := protocol.EncodeResponse(protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: true, RequestID: request.RequestID, DatabaseID: "db", Operation: request.Operation, Result: protocol.GetResult{Record: record}})
		_ = protocol.WriteFrame(conn, response, gapdb.DefaultMaxFrameBytes)
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second, ReuseUnaryConnection: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	firstResult := make(chan error, 1)
	go func() {
		_, callErr := client.Get(context.Background(), "first")
		firstResult <- callErr
	}()
	select {
	case <-firstReceived:
	case <-time.After(time.Second):
		t.Fatal("first request not received")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Get(canceled, "second"); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued canceled Get = %v", err)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Get was poisoned by canceled waiter: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("transmitted requests = %d, want one", got)
	}
}

func TestReusableUnaryReplacesPoisonedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replace.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	var accepted atomic.Int64
	go func() {
		for connection := 0; connection < 3; connection++ {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				served <- acceptErr
				return
			}
			accepted.Add(1)
			if connection == 0 {
				_ = conn.Close()
				continue
			}
			payload, readErr := protocol.ReadFrame(conn, gapdb.DefaultMaxFrameBytes)
			if readErr != nil {
				_ = conn.Close()
				served <- readErr
				return
			}
			if connection == 1 {
				_ = conn.Close()
				continue
			}
			request, decodeErr := protocol.DecodeRequest(payload, gapdb.DefaultOptions().Limits)
			if decodeErr != nil {
				_ = conn.Close()
				served <- decodeErr
				return
			}
			record := gapdb.NewRecord("key", []byte("value"), 1, nil)
			response, encodeErr := protocol.EncodeResponse(protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: true, RequestID: request.RequestID, DatabaseID: "db", Operation: request.Operation, Result: protocol.GetResult{Record: record}})
			if encodeErr == nil {
				encodeErr = protocol.WriteFrame(conn, response, gapdb.DefaultMaxFrameBytes)
			}
			_ = conn.Close()
			served <- encodeErr
			return
		}
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second, ReuseUnaryConnection: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Get(t.Context(), "key"); err == nil {
		t.Fatal("lost response was accepted")
	}
	record, err := client.Get(t.Context(), "key")
	if err != nil || string(record.Value) != "value" {
		t.Fatalf("replacement Get = %+v, %v", record, err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if got := accepted.Load(); got != 3 {
		t.Fatalf("accepted connections = %d, want probe, poisoned, replacement", got)
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
	client := dialUnaryErrorFixture(t, `{"code":"CONDITION_FAILED","message":"Atomic batch assertion failed.","retry":"after_reconcile","key":"guard","assertion_index":0,"condition":"revision","expected_revision":6,"actual_revision":7,"safe_actions":["get","rebuild_batch","abort"]}`)
	batch := gapdb.Batch{
		Ack:        gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: 6}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("created", []byte{1}, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}
	_, err := client.AtomicBatch(t.Context(), batch)
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.Code != gapdb.CodeConditionFailed || failure.AssertionIndex == nil || *failure.AssertionIndex != 0 || failure.ExpectedRevision == nil || *failure.ExpectedRevision != 6 {
		t.Fatalf("AtomicBatch error = %#v (cause %v)", err, errors.Unwrap(err))
	}
}

func TestClientRejectsMalformedAssertionConditionEvidence(t *testing.T) {
	batch := gapdb.Batch{
		Ack:        gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: "guard", Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("created", []byte{1}, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}
	for name, evidence := range map[string]string{
		"any":                       `"condition":"any","actual_state":"present"`,
		"unknown":                   `"condition":"unchanged","actual_state":"present"`,
		"revision without expected": `"condition":"revision","actual_revision":2`,
		"revision with zero":        `"condition":"revision","expected_revision":0,"actual_revision":2`,
		"absent with expected":      `"condition":"absent","expected_revision":1,"actual_state":"present"`,
	} {
		t.Run(name, func(t *testing.T) {
			remote := `{"code":"CONDITION_FAILED","message":"failed","retry":"after_reconcile","key":"guard","assertion_index":0,` + evidence + `,"safe_actions":["get","rebuild_batch","abort"]}`
			client := dialUnaryErrorFixture(t, remote)
			_, err := client.AtomicBatch(t.Context(), batch)
			if err == nil || errors.Is(err, &gapdb.Error{Code: gapdb.CodeConditionFailed}) {
				t.Fatalf("client admitted malformed assertion evidence: %#v", err)
			}
		})
	}

	oversized := `{"code":"CONDITION_FAILED","message":` + quote(strings.Repeat("secret-message/", 300)) + `,"retry":"after_reconcile","key":"guard","assertion_index":0,"condition":"absent","actual_state":"present","safe_actions":["get","rebuild_batch","abort"]}`
	client := dialUnaryErrorFixture(t, oversized)
	_, err := client.AtomicBatch(t.Context(), batch)
	if err == nil || errors.Is(err, &gapdb.Error{Code: gapdb.CodeConditionFailed}) {
		t.Fatalf("client admitted oversized assertion diagnostic: %#v", err)
	}
}

func TestClientRejectsAssertionFailureEvidenceOutsideAtomicBatch(t *testing.T) {
	type probe struct {
		operation protocol.Operation
		invoke    func(*gapdb.Client) error
	}
	probes := []probe{
		{protocol.OperationPut, func(client *gapdb.Client) error {
			_, err := client.Put(t.Context(), "record", []byte{1}, nil, gapdb.AckMemory)
			return err
		}},
		{protocol.OperationCompareAndSwap, func(client *gapdb.Client) error {
			_, err := client.CompareAndSwap(t.Context(), "record", 1, []byte{1}, nil, gapdb.AckMemory)
			return err
		}},
		{protocol.OperationDeleteIfRevision, func(client *gapdb.Client) error {
			_, err := client.DeleteIfRevision(t.Context(), "record", 1, gapdb.AckMemory)
			return err
		}},
	}
	assertionError := `{"code":"CONDITION_FAILED","message":"failed","retry":"after_reconcile","key":"guard","assertion_index":0,"condition":"absent","actual_state":"present","safe_actions":["get","rebuild_batch","abort"]}`
	for _, tt := range probes {
		t.Run(string(tt.operation), func(t *testing.T) {
			client := dialUnaryErrorFixtureForOperation(t, tt.operation, assertionError)
			err := tt.invoke(client)
			if err == nil || errors.Is(err, &gapdb.Error{Code: gapdb.CodeConditionFailed}) {
				t.Fatalf("client admitted %s assertion evidence: %#v", tt.operation, err)
			}
		})
	}

	mutationError := `{"code":"CONDITION_FAILED","message":"failed","retry":"after_reconcile","key":"record","mutation_index":0,"condition":"revision","actual_state":"missing","safe_actions":["get","rebuild_batch","abort"]}`
	client := dialUnaryErrorFixtureForOperation(t, protocol.OperationPut, mutationError)
	_, err := client.Put(t.Context(), "record", []byte{1}, nil, gapdb.AckMemory)
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.MutationIndex == nil || failure.AssertionIndex != nil {
		t.Fatalf("ordinary mutation condition evidence = %#v, %v", failure, err)
	}
}

func TestClientAcceptsStrictlyBoundedMaximumKeyAssertionFailure(t *testing.T) {
	key := strings.Repeat("k", gapdb.DefaultOptions().Limits.MaxKeyBytes)
	index := 0
	response, err := protocol.EncodeResponse(protocol.Response{
		SchemaVersion: 1, DatabaseID: "00112233445566778899aabbccddeeff", Operation: protocol.OperationAtomicBatch,
		Error: &gapdb.Error{Code: gapdb.CodeConditionFailed, Message: "Atomic batch assertion failed.", Retry: gapdb.RetryAfterReconcile,
			Key: key, AssertionIndex: &index, Condition: string(gapdb.ConditionAbsent), ActualState: "present",
			SafeActions: []gapdb.SafeAction{gapdb.ActionGet, gapdb.ActionRebuildBatch, gapdb.ActionAbort}},
	})
	if err != nil || len(response) >= 4096 {
		t.Fatalf("EncodeResponse() length=%d, err=%v", len(response), err)
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		t.Fatal(err)
	}
	client := dialUnaryErrorFixture(t, string(envelope.Error))
	batch := gapdb.Batch{
		Ack:        gapdb.AckMemory,
		Assertions: []gapdb.Assertion{{Key: key, Condition: gapdb.Condition{Kind: gapdb.ConditionAbsent}}},
		Mutations:  []gapdb.Mutation{gapdb.NewPutMutation("created", []byte{1}, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)},
	}
	_, err = client.AtomicBatch(t.Context(), batch)
	var failure *gapdb.Error
	if !errors.As(err, &failure) || failure.AssertionIndex == nil || failure.Key == "" || len(failure.Key) >= len(key) {
		t.Fatalf("client assertion diagnostic = %#v, %v", failure, err)
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

func dialUnaryErrorFixture(t *testing.T, remoteError string) *gapdb.Client {
	t.Helper()
	return dialUnaryErrorFixtureForOperation(t, protocol.OperationAtomicBatch, remoteError)
}

func dialUnaryErrorFixtureForOperation(t *testing.T, operation protocol.Operation, remoteError string) *gapdb.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unary-error.sock")
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
		envelope := `{"schema_version":1,"ok":false,"request_id":` + quote(request.RequestID) + `,"database_id":"00112233445566778899aabbccddeeff","operation":` + quote(string(operation)) + `,"error":` + remoteError + `}`
		_ = protocol.WriteFrame(conn, []byte(envelope), gapdb.DefaultMaxFrameBytes)
	}()
	client, err := gapdb.Dial(path, gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
