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

	"gapdb/gapdb"
	"gapdb/internal/protocol"
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
