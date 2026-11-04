package protocol_test

import (
	"errors"
	"testing"

	"github.com/spec-kitty/gapdb/gapdb"
	wire "github.com/spec-kitty/gapdb/internal/protocol"
)

// Argument-decode errors must themselves survive the unchanged response codec.
func TestReadManyDuplicateDecodeErrorHasEncodableWireResponse(t *testing.T) {
	for _, keys := range []string{`["same","same"]`, `["same","other","same"]`} {
		t.Run(keys, func(t *testing.T) {
			raw := []byte(`{"schema_version":1,"request_id":"invalid-1","operation":"get_many","arguments":{"keys":` + keys + `}}`)
			_, err := wire.DecodeRequest(raw, gapdb.DefaultOptions().Limits)
			var failure *gapdb.Error
			if !errors.As(err, &failure) {
				t.Fatalf("decode error = %v", err)
			}
			encoded, encodeErr := wire.EncodeResponse(wire.Response{SchemaVersion: wire.SchemaVersion, DatabaseID: "test-db", Operation: wire.OperationStatus, Error: failure})
			if encodeErr != nil {
				t.Fatalf("argument error cannot be encoded as server refusal: %v (decode error: %v)", encodeErr, err)
			}
			decoded, decodeErr := wire.DecodeResponse(encoded, gapdb.DefaultMaxFrameBytes)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if decoded.OK || decoded.RequestID != "" || decoded.Operation != wire.OperationStatus || decoded.Error == nil || decoded.Error.Code != gapdb.CodeInvalidRequest || decoded.Error.Field != "arguments.keys" || decoded.Error.Retry != gapdb.RetryNever || len(decoded.Error.SafeActions) == 0 {
				t.Fatalf("malformed-request refusal = %+v", decoded)
			}
		})
	}
}
