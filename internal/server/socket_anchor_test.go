package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDescriptorAnchoredBindFailsClosedWhenCapabilityUnavailable(t *testing.T) {
	directory := t.TempDir()
	anchor, err := openDirectoryAnchor(directory)
	if err != nil {
		t.Fatal(err)
	}
	bindPath, err := anchor.bindPath("unavailable.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: bindPath, Net: "unix"})
	if listener != nil {
		_ = listener.Close()
		t.Fatal("descriptor path unexpectedly fell back after capability closed")
	}
	if err == nil {
		t.Fatal("unavailable descriptor path unexpectedly accepted")
	}
	if _, statErr := os.Lstat(filepath.Join(directory, "unavailable.sock")); !os.IsNotExist(statErr) {
		t.Fatalf("configured path mutated after descriptor capability closed: %v", statErr)
	}
}
