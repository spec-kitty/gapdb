package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/protocol"
)

func TestEncodeResponseForFrameHandlesHardCeilingBeforePublication(t *testing.T) {
	limits := gapdb.DefaultOptions().Limits
	limits.MaxFrameBytes = gapdb.HardMaxFrameBytes
	server := &Server{limits: limits}
	response := protocol.Response{
		SchemaVersion: protocol.SchemaVersion,
		OK:            true,
		RequestID:     strings.Repeat("\x00", 256),
		DatabaseID:    "0198f4d4f26a7b1ca3df00c30ca93e73",
		Operation:     protocol.OperationGetMany,
		Result:        strings.Repeat("x", gapdb.HardMaxFrameBytes),
	}
	payload, err := server.encodeResponseForFrame(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > gapdb.HardMaxFrameBytes {
		t.Fatalf("fallback frame bytes = %d", len(payload))
	}
	decoded, err := protocol.DecodeResponse(payload, gapdb.HardMaxFrameBytes)
	var oversized *gapdb.Error
	if err != nil || decoded.OK || !errors.As(decoded.Error, &oversized) || oversized.Code != gapdb.CodeFrameTooLarge || oversized.ReceivedBytes <= gapdb.HardMaxFrameBytes || oversized.MaximumBytes != gapdb.HardMaxFrameBytes || decoded.RequestID != response.RequestID {
		t.Fatalf("hard-ceiling fallback = %+v, %v", decoded, err)
	}
}
