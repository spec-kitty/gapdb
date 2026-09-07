package server

import "testing"

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
