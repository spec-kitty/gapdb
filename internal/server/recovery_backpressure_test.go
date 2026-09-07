package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
)

func TestRecoverySnapshotHasDedicatedFailClosedBackpressure(t *testing.T) {
	server := &Server{recovery: make(chan struct{}, 1)}
	if !server.acquireRecoverySnapshot() {
		t.Fatal("first bounded recovery request was not admitted")
	}
	if server.acquireRecoverySnapshot() {
		t.Fatal("concurrent recovery request escaped the dedicated slot")
	}
	server.releaseRecoverySnapshot()
	if !server.acquireRecoverySnapshot() {
		t.Fatal("released recovery slot did not admit the next request")
	}
	server.releaseRecoverySnapshot()
}

func TestRecoverySnapshotSaturatedSlotReturnsCorrelatedFailureDespiteTinySuccessBudget(t *testing.T) {
	srv, err := Open(Config{Directory: t.TempDir(), Options: gapdb.DefaultOptions(), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	client, err := gapdb.Dial(srv.SocketPath(), gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if !srv.acquireRecoverySnapshot() {
		t.Fatal("could not reserve recovery slot")
	}
	t.Cleanup(srv.releaseRecoverySnapshot)
	request := gapdb.RecoverySnapshotRequest{Prefix: "spk/", MaxRecords: 1, MaxBytes: 1}
	if _, err := client.ReadRecoverySnapshot(t.Context(), request); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeServerBusy}) {
		t.Fatalf("saturated tiny snapshot = %v, want SERVER_BUSY", err)
	}
}
