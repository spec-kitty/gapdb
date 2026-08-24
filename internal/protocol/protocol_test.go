package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
)

func TestDocumentedDefaults(t *testing.T) {
	t.Parallel()

	got := gapdb.DefaultOptions().Limits
	want := gapdb.Limits{
		MaxKeyBytes:          4 << 10,
		MaxValueBytes:        8 << 20,
		MaxFrameBytes:        16 << 20,
		MaxBatchBytes:        16 << 20,
		MaxBatchOperations:   1024,
		MaxScanRecords:       1000,
		MaxScanBytes:         16 << 20,
		WatchBufferEvents:    256,
		MaxWatchClients:      256,
		MaxConcurrentClients: 256,
		MaxHistoryEvents:     100_000,
		MaxHistoryBytes:      64 << 20,
	}
	if got != want {
		t.Fatalf("default limits mismatch:\n got: %#v\nwant: %#v", got, want)
	}
	if err := (gapdb.Options{Limits: got}).Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestLimitsFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(*gapdb.Limits)
	}{
		{"zero key", func(v *gapdb.Limits) { v.MaxKeyBytes = 0 }},
		{"negative value", func(v *gapdb.Limits) { v.MaxValueBytes = -1 }},
		{"frame above ceiling", func(v *gapdb.Limits) { v.MaxFrameBytes = gapdb.HardMaxFrameBytes + 1 }},
		{"batch above frame", func(v *gapdb.Limits) { v.MaxBatchBytes = v.MaxFrameBytes + 1 }},
		{"scan above frame", func(v *gapdb.Limits) { v.MaxScanBytes = v.MaxFrameBytes + 1 }},
		{"history count too small", func(v *gapdb.Limits) { v.MaxHistoryEvents = v.WatchBufferEvents - 1 }},
		{"watchers above clients", func(v *gapdb.Limits) { v.MaxWatchClients = v.MaxConcurrentClients + 1 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := gapdb.DefaultOptions().Limits
			tt.edit(&limits)
			var structured *gapdb.Error
			err := (gapdb.Options{Limits: limits}).Validate()
			if !errors.As(err, &structured) || structured.Code != gapdb.CodeInvalidRequest {
				t.Fatalf("Validate() error = %#v, want INVALID_REQUEST", err)
			}
		})
	}
}

func TestStableErrorCodesAndWrapping(t *testing.T) {
	t.Parallel()

	seen := make(map[gapdb.ErrorCode]bool)
	for _, definition := range gapdb.ErrorDefinitions() {
		if definition.Code == "" || definition.Retry == "" || len(definition.SafeActions) == 0 {
			t.Fatalf("incomplete error definition: %#v", definition)
		}
		if seen[definition.Code] {
			t.Fatalf("duplicate error code %q", definition.Code)
		}
		seen[definition.Code] = true
	}

	cause := io.ErrUnexpectedEOF
	err := &gapdb.Error{
		Code:        gapdb.CodeCorruptWAL,
		Message:     "WAL frame ended early.",
		Retry:       gapdb.RetryAfterOperator,
		SafeActions: []gapdb.SafeAction{gapdb.ActionInspectOffline, gapdb.ActionAbort},
		Cause:       cause,
	}
	wrapped := errors.Join(errors.New("startup failed"), err)
	var got *gapdb.Error
	if !errors.As(wrapped, &got) || got.Code != gapdb.CodeCorruptWAL {
		t.Fatalf("errors.As lost structured evidence: %#v", got)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("errors.Is lost wrapped cause")
	}
	if !errors.Is(wrapped, &gapdb.Error{Code: gapdb.CodeCorruptWAL}) {
		t.Fatal("errors.Is must match stable error code")
	}
}

func TestPublicValueCopies(t *testing.T) {
	t.Parallel()

	source := []byte{1, 2, 3}
	record := gapdb.NewRecord("workflows/123", source, 45, nil)
	source[0] = 9
	if record.Value[0] != 1 {
		t.Fatal("NewRecord retained caller-owned bytes")
	}

	clone := record.Clone()
	clone.Value[1] = 8
	if record.Value[1] != 2 {
		t.Fatal("Record.Clone aliases source bytes")
	}

	mutation := gapdb.NewPutMutation("workflows/123", source, gapdb.Condition{Kind: gapdb.ConditionAny}, nil)
	source[1] = 7
	if mutation.Value[1] != 2 {
		t.Fatal("NewPutMutation retained caller-owned bytes")
	}

	event := gapdb.ChangeEvent{Record: &record}
	eventClone := event.Clone()
	eventClone.Record.Value[2] = 6
	if event.Record.Value[2] != 3 {
		t.Fatal("ChangeEvent.Clone aliases record bytes")
	}
}

func TestBatchValidation(t *testing.T) {
	t.Parallel()

	valid := gapdb.Batch{
		Ack: gapdb.AckDurable,
		Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("new", []byte{}, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			gapdb.NewDeleteMutation("old", 91),
		},
	}
	if err := valid.Validate(gapdb.DefaultOptions().Limits); err != nil {
		t.Fatalf("valid batch rejected: %v", err)
	}

	tests := []struct {
		name  string
		batch gapdb.Batch
		code  gapdb.ErrorCode
	}{
		{"empty", gapdb.Batch{Ack: gapdb.AckMemory}, gapdb.CodeInvalidRequest},
		{"duplicate", gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("same", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil),
			gapdb.NewPutMutation("same", nil, gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		}}, gapdb.CodeDuplicateKey},
		{"delete any", gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{{Kind: gapdb.MutationDelete, Key: "key", Condition: gapdb.Condition{Kind: gapdb.ConditionAny}}}}, gapdb.CodeInvalidRequest},
		{"zero expected revision", gapdb.Batch{Ack: gapdb.AckMemory, Mutations: []gapdb.Mutation{{Kind: gapdb.MutationPut, Key: "key", Condition: gapdb.Condition{Kind: gapdb.ConditionRevision}}}}, gapdb.CodeInvalidRequest},
		{"invalid ack", gapdb.Batch{Ack: "fast", Mutations: []gapdb.Mutation{gapdb.NewPutMutation("key", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil)}}, gapdb.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.batch.Validate(gapdb.DefaultOptions().Limits)
			if !errors.Is(err, &gapdb.Error{Code: tt.code}) {
				t.Fatalf("Validate() = %v, want code %s", err, tt.code)
			}
		})
	}
}

func TestFrameReadWriteBoundaries(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"schema_version":1}`)
	var framed bytes.Buffer
	writer := &shortWriter{w: &framed, max: 3}
	if err := WriteFrame(writer, payload, 1024); err != nil {
		t.Fatalf("WriteFrame() = %v", err)
	}
	got, err := ReadFrame(&shortReader{r: bytes.NewReader(framed.Bytes()), max: 2}, 1024)
	if err != nil {
		t.Fatalf("ReadFrame() = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}

	tests := []struct {
		name string
		wire []byte
		code gapdb.ErrorCode
	}{
		{"short prefix", []byte{0, 0}, gapdb.CodeInvalidRequest},
		{"zero", []byte{0, 0, 0, 0}, gapdb.CodeInvalidRequest},
		{"oversize", []byte{0, 0, 4, 1}, gapdb.CodeFrameTooLarge},
		{"short payload", []byte{0, 0, 0, 2, 1}, gapdb.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tt.wire), 1024)
			if !errors.Is(err, &gapdb.Error{Code: tt.code}) {
				t.Fatalf("ReadFrame() = %v, want %s", err, tt.code)
			}
		})
	}

	if err := WriteFrame(io.Discard, nil, 1024); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("zero WriteFrame = %v", err)
	}
	if err := WriteFrame(io.Discard, make([]byte, 1025), 1024); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeFrameTooLarge}) {
		t.Fatalf("oversize WriteFrame = %v", err)
	}
}

func TestStrictRequestDecode(t *testing.T) {
	t.Parallel()

	valid := `{"schema_version":1,"request_id":"r-1","operation":"put","arguments":{"key":"k","value_base64":"AQID","ack":"durable"}}`
	request, err := DecodeRequest([]byte(valid), gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatalf("DecodeRequest(valid) = %v", err)
	}
	args, ok := request.Arguments.(PutArguments)
	if !ok || !bytes.Equal(args.Value, []byte{1, 2, 3}) {
		t.Fatalf("arguments = %#v", request.Arguments)
	}
	encoded, err := EncodeRequest(request, gapdb.DefaultOptions().Limits)
	if err != nil {
		t.Fatalf("EncodeRequest() = %v", err)
	}
	if string(encoded) != valid {
		t.Fatalf("canonical request:\n got %s\nwant %s", encoded, valid)
	}
	casNullExpiry := []byte(`{"schema_version":1,"operation":"compare_and_swap","arguments":{"key":"k","expected_revision":1,"value_base64":"","expires_at":null,"ack":"memory"}}`)
	if _, err := DecodeRequest(casNullExpiry, gapdb.DefaultOptions().Limits); err != nil {
		t.Fatalf("DecodeRequest(CAS null expiry) = %v", err)
	}

	tests := []struct {
		name string
		json []byte
		code gapdb.ErrorCode
	}{
		{"unsupported version", []byte(`{"schema_version":2,"operation":"get","arguments":{"key":"k"}}`), gapdb.CodeUnsupportedVersion},
		{"unknown field", []byte(`{"schema_version":1,"operation":"get","arguments":{"key":"k","extra":true}}`), gapdb.CodeInvalidRequest},
		{"duplicate envelope field", []byte(`{"schema_version":1,"operation":"get","operation":"put","arguments":{"key":"k"}}`), gapdb.CodeInvalidRequest},
		{"duplicate nested field", []byte(`{"schema_version":1,"operation":"get","arguments":{"key":"k","key":"j"}}`), gapdb.CodeInvalidRequest},
		{"trailing value", []byte(`{"schema_version":1,"operation":"get","arguments":{"key":"k"}} {}`), gapdb.CodeInvalidRequest},
		{"invalid utf8", append([]byte(`{"schema_version":1,"operation":"get","arguments":{"key":"`), 0xff, '"', '}', '}'), gapdb.CodeInvalidRequest},
		{"invalid base64", []byte(`{"schema_version":1,"operation":"put","arguments":{"key":"k","value_base64":"***","ack":"memory"}}`), gapdb.CodeInvalidRequest},
		{"unpadded base64", []byte(`{"schema_version":1,"operation":"put","arguments":{"key":"k","value_base64":"AQI","ack":"memory"}}`), gapdb.CodeInvalidRequest},
		{"null base64", []byte(`{"schema_version":1,"operation":"put","arguments":{"key":"k","value_base64":null,"ack":"memory"}}`), gapdb.CodeInvalidRequest},
		{"missing value", []byte(`{"schema_version":1,"operation":"put","arguments":{"key":"k","ack":"memory"}}`), gapdb.CodeInvalidRequest},
		{"integer overflow", []byte(`{"schema_version":1,"operation":"delete_if_revision","arguments":{"key":"k","expected_revision":18446744073709551616,"ack":"memory"}}`), gapdb.CodeInvalidRequest},
		{"bad enum", []byte(`{"schema_version":1,"operation":"put","arguments":{"key":"k","value_base64":"","ack":"MEMORY"}}`), gapdb.CodeInvalidRequest},
		{"non-UTC expiry", []byte(`{"schema_version":1,"operation":"put","arguments":{"key":"k","value_base64":"","expires_at":"2026-08-23T13:30:00-05:00","ack":"memory"}}`), gapdb.CodeInvalidRequest},
		{"noncanonical expiry", []byte(`{"schema_version":1,"operation":"put","arguments":{"key":"k","value_base64":"","expires_at":"2026-08-23T18:30:00.000Z","ack":"memory"}}`), gapdb.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRequest(tt.json, gapdb.DefaultOptions().Limits)
			if !errors.Is(err, &gapdb.Error{Code: tt.code}) {
				t.Fatalf("DecodeRequest() = %v, want %s", err, tt.code)
			}
		})
	}
}

func TestMutationAcknowledgementPresence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation Operation
		arguments string
		request   Request
	}{
		{"put", OperationPut, `"key":"k","value_base64":""`, Request{SchemaVersion: 1, Operation: OperationPut, Arguments: PutArguments{Key: "k", Value: Base64Bytes{}}}},
		{"put-if-absent", OperationPutIfAbsent, `"key":"k","value_base64":""`, Request{SchemaVersion: 1, Operation: OperationPutIfAbsent, Arguments: PutArguments{Key: "k", Value: Base64Bytes{}}}},
		{"compare-and-swap", OperationCompareAndSwap, `"key":"k","expected_revision":1,"value_base64":""`, Request{SchemaVersion: 1, Operation: OperationCompareAndSwap, Arguments: CompareAndSwapArguments{Key: "k", ExpectedRevision: 1, Value: Base64Bytes{}}}},
		{"delete-if-revision", OperationDeleteIfRevision, `"key":"k","expected_revision":1`, Request{SchemaVersion: 1, Operation: OperationDeleteIfRevision, Arguments: DeleteIfRevisionArguments{Key: "k", ExpectedRevision: 1}}},
		{"atomic-batch", OperationAtomicBatch, `"mutations":[{"kind":"put","key":"k","condition":{"kind":"any"},"value_base64":"AQ=="}]`, Request{SchemaVersion: 1, Operation: OperationAtomicBatch, Arguments: BatchArguments{Mutations: []gapdb.Mutation{gapdb.NewPutMutation("k", []byte{1}, gapdb.Condition{Kind: gapdb.ConditionAny}, nil)}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, variant := range []struct {
				name string
				ack  string
				want gapdb.AckMode
				ok   bool
			}{
				{"omitted", "", gapdb.AckMemory, true},
				{"explicit-empty", `,"ack":""`, "", false},
				{"memory", `,"ack":"memory"`, gapdb.AckMemory, true},
				{"durable", `,"ack":"durable"`, gapdb.AckDurable, true},
			} {
				t.Run(variant.name, func(t *testing.T) {
					payload := []byte(`{"schema_version":1,"operation":"` + string(tt.operation) + `","arguments":{` + tt.arguments + variant.ack + `}}`)
					request, err := DecodeRequest(payload, gapdb.DefaultOptions().Limits)
					if !variant.ok {
						var structured *gapdb.Error
						if !errors.As(err, &structured) || structured.Code != gapdb.CodeInvalidRequest || structured.Field != "arguments.ack" {
							t.Fatalf("DecodeRequest() = %#v, want INVALID_REQUEST at arguments.ack", err)
						}
						return
					}
					if err != nil {
						t.Fatalf("DecodeRequest() = %v", err)
					}
					if got := requestAck(request); got != variant.want {
						t.Fatalf("ack = %q, want %q", got, variant.want)
					}
				})
			}

			encoded, err := EncodeRequest(tt.request, gapdb.DefaultOptions().Limits)
			if err != nil {
				t.Fatalf("EncodeRequest(zero ack) = %v", err)
			}
			if !bytes.Contains(encoded, []byte(`"ack":"memory"`)) || bytes.Contains(encoded, []byte(`"ack":""`)) {
				t.Fatalf("EncodeRequest(zero ack) = %s, want canonical memory ack", encoded)
			}
		})
	}
}

func requestAck(request Request) gapdb.AckMode {
	switch arguments := request.Arguments.(type) {
	case PutArguments:
		return arguments.Ack
	case CompareAndSwapArguments:
		return arguments.Ack
	case DeleteIfRevisionArguments:
		return arguments.Ack
	case BatchArguments:
		return arguments.Ack
	default:
		return ""
	}
}

func TestWatchStartRevisionZero(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"schema_version":1,"ok":true,"database_id":"db","operation":"watch","stream":"started","registration_revision":0}`)
	response, err := DecodeResponse(payload, gapdb.DefaultMaxFrameBytes)
	if err != nil {
		t.Fatalf("DecodeResponse() = %v", err)
	}
	if response.RegistrationRevision != 0 {
		t.Fatalf("registration revision = %d, want 0", response.RegistrationRevision)
	}
	encoded, err := EncodeResponse(response)
	if err != nil {
		t.Fatalf("EncodeResponse() = %v", err)
	}
	if !bytes.Equal(encoded, payload) {
		t.Fatalf("canonical watch start:\n got %s\nwant %s", encoded, payload)
	}
}

func TestStructuredErrorCodeSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"wrong-code-action", `"key":"k","current_revision":0,"safe_actions":["check_storage"]`},
		{"missing-evidence", `"safe_actions":["get","put_if_absent","abort"]`},
		{"irrelevant-evidence", `"path":"/tmp/gapdb","key":"k","current_revision":0,"safe_actions":["get","put_if_absent","abort"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"schema_version":1,"ok":false,"database_id":"db","operation":"get","error":{"code":"NOT_FOUND","message":"Missing.","retry":"after_reconcile",` + tt.body + `}}`)
			_, err := DecodeResponse(payload, gapdb.DefaultMaxFrameBytes)
			var structured *gapdb.Error
			if !errors.As(err, &structured) || structured.Code != gapdb.CodeInvalidRequest {
				t.Fatalf("DecodeResponse() = %#v, want INVALID_REQUEST", err)
			}
		})
	}
	for _, definition := range gapdb.ErrorDefinitions() {
		if _, ok := errorSchema(definition.Code); !ok {
			t.Errorf("errorSchema(%s) is missing", definition.Code)
		}
	}
}

func TestOperationAppliedIsGlobalTrueOnlyEvidence(t *testing.T) {
	t.Parallel()

	current := gapdb.Revision(7)
	response := Response{
		SchemaVersion: SchemaVersion,
		OK:            false,
		DatabaseID:    "db",
		Operation:     OperationGet,
		Error: &gapdb.Error{
			Code:             gapdb.CodeNotFound,
			Message:          "Missing after an earlier operation applied.",
			Retry:            gapdb.RetryAfterReconcile,
			Key:              "k",
			CurrentRevision:  &current,
			OperationApplied: true,
			SafeActions:      []gapdb.SafeAction{gapdb.ActionGet, gapdb.ActionPutIfAbsent, gapdb.ActionAbort},
		},
	}
	encoded, err := EncodeResponse(response)
	if err != nil {
		t.Fatalf("EncodeResponse(global operation_applied) = %v", err)
	}
	decoded, err := DecodeResponse(encoded, gapdb.DefaultMaxFrameBytes)
	if err != nil || decoded.Error == nil || !decoded.Error.OperationApplied {
		t.Fatalf("DecodeResponse(global operation_applied) = %#v, %v", decoded.Error, err)
	}
	for _, invalid := range [][]byte{
		bytes.Replace(encoded, []byte(`"operation_applied":true`), []byte(`"operation_applied":false`), 1),
		bytes.Replace(encoded, []byte(`"operation_applied":true`), []byte(`"operation_applied":null`), 1),
		bytes.Replace(encoded, []byte(`"operation_applied":true`), []byte(`"path":"irrelevant","operation_applied":true`), 1),
	} {
		if _, err := DecodeResponse(invalid, gapdb.DefaultMaxFrameBytes); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
			t.Fatalf("DecodeResponse(%s) = %v, want INVALID_REQUEST", invalid, err)
		}
	}
}

func TestGuardedOfflineOperationArguments(t *testing.T) {
	t.Parallel()

	valid := []string{
		`{"schema_version":1,"operation":"offline_inspect","arguments":{"database_path":"/var/lib/gapdb"}}`,
		`{"schema_version":1,"operation":"offline_verify","arguments":{"database_path":"/var/lib/gapdb","mode":"full"}}`,
		`{"schema_version":1,"operation":"offline_recover_propose","arguments":{"database_path":"/var/lib/gapdb"}}`,
		`{"schema_version":1,"operation":"offline_recover_apply","arguments":{"database_path":"/var/lib/gapdb","action_id":"proposal-1","expected_database_id":"0198f4d4f26a7b1ca3df00c30ca93e73","expected_manifest_generation":0,"destination":"/var/lib/gapdb-quarantine"}}`,
	}
	for _, payload := range valid {
		if _, err := DecodeRequest([]byte(payload), gapdb.DefaultOptions().Limits); err != nil {
			t.Errorf("DecodeRequest(%s) = %v", payload, err)
		}
	}

	emptyApply := []byte(`{"schema_version":1,"operation":"offline_recover_apply","arguments":{}}`)
	if _, err := DecodeRequest(emptyApply, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("DecodeRequest(empty apply) = %v, want INVALID_REQUEST", err)
	}
	nullGeneration := []byte(`{"schema_version":1,"operation":"offline_recover_apply","arguments":{"database_path":"/var/lib/gapdb","action_id":"proposal-1","expected_database_id":"db","expected_manifest_generation":null,"destination":"/var/lib/gapdb-quarantine"}}`)
	if _, err := DecodeRequest(nullGeneration, gapdb.DefaultOptions().Limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("DecodeRequest(null manifest generation) = %v, want INVALID_REQUEST", err)
	}
}

func TestStrictResponseDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
	}{
		{"unknown record field", `{"schema_version":1,"ok":true,"database_id":"db","operation":"get","result":{"record":{"key":"k","value_base64":"","revision":1,"extra":true}}}`},
		{"duplicate result field", `{"schema_version":1,"ok":true,"database_id":"db","operation":"put","result":{"revision":1,"revision":2,"ack":"memory","durable_through_revision":0}}`},
		{"null record bytes", `{"schema_version":1,"ok":true,"database_id":"db","operation":"get","result":{"record":{"key":"k","value_base64":null,"revision":1}}}`},
		{"non-UTC record expiry", `{"schema_version":1,"ok":true,"database_id":"db","operation":"get","result":{"record":{"key":"k","value_base64":"","revision":1,"expires_at":"2026-08-23T13:30:00-05:00"}}}`},
		{"invalid event enum", `{"schema_version":1,"ok":true,"database_id":"db","operation":"watch","stream":"event","event":{"revision":1,"order":0,"kind":"PUT","key":"k"}}`},
		{"unknown error code", `{"schema_version":1,"ok":false,"database_id":"db","operation":"get","error":{"code":"SURPRISE","message":"Fixture.","retry":"never","safe_actions":["abort"]}}`},
		{"wrong retry class", `{"schema_version":1,"ok":false,"database_id":"db","operation":"get","error":{"code":"NOT_FOUND","message":"Fixture.","retry":"immediate","safe_actions":["abort"]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeResponse([]byte(tt.json), gapdb.DefaultMaxFrameBytes)
			if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
				t.Fatalf("DecodeResponse() = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

func TestDeterministicResponseEncoding(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, 8, 23, 18, 30, 0, 0, time.UTC)
	record := gapdb.NewRecord("workflows/123", []byte{1, 2, 3}, 45, &expires)
	response := Response{
		SchemaVersion: SchemaVersion,
		OK:            true,
		RequestID:     "caller-optional-id",
		DatabaseID:    "0198f4d4f26a7b1ca3df00c30ca93e73",
		Operation:     OperationGet,
		Result:        GetResult{Record: record},
	}
	want := `{"schema_version":1,"ok":true,"request_id":"caller-optional-id","database_id":"0198f4d4f26a7b1ca3df00c30ca93e73","operation":"get","result":{"record":{"key":"workflows/123","value_base64":"AQID","revision":45,"expires_at":"2026-08-23T18:30:00Z"}}}`
	for range 2 {
		got, err := EncodeResponse(response)
		if err != nil {
			t.Fatalf("EncodeResponse() = %v", err)
		}
		if string(got) != want {
			t.Fatalf("response:\n got %s\nwant %s", got, want)
		}
	}
}

func TestFrameLengthRejectsUint32Overflow(t *testing.T) {
	t.Parallel()
	if uint64(math.MaxUint32) >= uint64(gapdb.HardMaxFrameBytes) {
		return
	}
	t.Fatal("hard frame ceiling must remain below uint32 framing capacity")
}

func TestDocumentedSizeBoundaries(t *testing.T) {
	t.Parallel()

	limits := gapdb.DefaultOptions().Limits
	if err := gapdb.NewPutMutation(strings.Repeat("k", limits.MaxKeyBytes), make([]byte, limits.MaxValueBytes), gapdb.Condition{Kind: gapdb.ConditionAny}, nil).Validate(limits); err != nil {
		t.Fatalf("exact key/value maxima rejected: %v", err)
	}
	if err := gapdb.NewPutMutation(strings.Repeat("k", limits.MaxKeyBytes+1), nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil).Validate(limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeKeyTooLarge}) {
		t.Fatalf("key above maximum = %v", err)
	}
	if err := gapdb.NewPutMutation("k", make([]byte, limits.MaxValueBytes+1), gapdb.Condition{Kind: gapdb.ConditionAny}, nil).Validate(limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeValueTooLarge}) {
		t.Fatalf("value above maximum = %v", err)
	}

	batch := gapdb.Batch{Ack: gapdb.AckMemory, Mutations: make([]gapdb.Mutation, limits.MaxBatchOperations)}
	for i := range batch.Mutations {
		batch.Mutations[i] = gapdb.NewPutMutation("k/"+strconv.Itoa(i), nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil)
	}
	if err := batch.Validate(limits); err != nil {
		t.Fatalf("exact batch operation maximum rejected: %v", err)
	}
	batch.Mutations = append(batch.Mutations, gapdb.NewPutMutation("over", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil))
	if err := batch.Validate(limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBatchTooLarge}) {
		t.Fatalf("batch above operation maximum = %v", err)
	}

	requestID := strings.Repeat("r", 256)
	payload := []byte(`{"schema_version":1,"request_id":"` + requestID + `","operation":"scan_prefix","arguments":{"prefix":"","limit":1000}}`)
	if _, err := DecodeRequest(payload, limits); err != nil {
		t.Fatalf("exact request ID and scan limit rejected: %v", err)
	}
	payload = []byte(`{"schema_version":1,"request_id":"` + requestID + `x","operation":"scan_prefix","arguments":{"prefix":"","limit":1000}}`)
	if _, err := DecodeRequest(payload, limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("request ID above maximum = %v", err)
	}
	payload = []byte(`{"schema_version":1,"operation":"scan_prefix","arguments":{"prefix":"","limit":1001}}`)
	if _, err := DecodeRequest(payload, limits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeInvalidRequest}) {
		t.Fatalf("scan limit above maximum = %v", err)
	}

	batchPayload := []byte(`{"schema_version":1,"operation":"atomic_batch","arguments":{"ack":"memory","mutations":[{"kind":"put","key":"k","condition":{"kind":"any"},"value_base64":""}]}}`)
	var envelope struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(batchPayload, &envelope); err != nil {
		t.Fatal(err)
	}
	exactBatchLimits := limits
	exactBatchLimits.MaxBatchBytes = len(envelope.Arguments)
	if _, err := DecodeRequest(batchPayload, exactBatchLimits); err != nil {
		t.Fatalf("exact encoded batch byte maximum rejected: %v", err)
	}
	exactBatchLimits.MaxBatchBytes--
	if _, err := DecodeRequest(batchPayload, exactBatchLimits); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeBatchTooLarge}) {
		t.Fatalf("encoded batch above maximum = %v", err)
	}

	frame := make([]byte, limits.MaxFrameBytes)
	if err := WriteFrame(io.Discard, frame, limits.MaxFrameBytes); err != nil {
		t.Fatalf("exact frame maximum rejected: %v", err)
	}
}

func TestExpiryMustBeStrictlyFuture(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	limits := gapdb.DefaultOptions().Limits
	for _, expiry := range []time.Time{now.Add(-time.Nanosecond), now} {
		mutation := gapdb.NewPutMutation("lease", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, &expiry)
		err := mutation.ValidateAt(limits, now)
		if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeExpiryNotFuture}) {
			t.Fatalf("expiry %v = %v", expiry, err)
		}
	}
	future := now.Add(time.Nanosecond)
	if err := gapdb.NewPutMutation("lease", nil, gapdb.Condition{Kind: gapdb.ConditionAny}, &future).ValidateAt(limits, now); err != nil {
		t.Fatalf("future expiry rejected: %v", err)
	}
}

type shortWriter struct {
	w   io.Writer
	max int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.w.Write(p)
}

type shortReader struct {
	r   io.Reader
	max int
}

func (r *shortReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.r.Read(p)
}
