package faultfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOSDelegatesAndEmitsOrderedBoundaries(t *testing.T) {
	dir := t.TempDir()
	recorder := &Recorder{}
	fsys := NewOS(recorder)
	temp := filepath.Join(dir, "value.tmp")
	final := filepath.Join(dir, "value")

	file, err := fsys.OpenFile(PointIdentityTempCreate, temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Write(PointIdentityTempWrite, file, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Truncate(PointWALTailTruncate, file, 2); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Sync(PointIdentityTempSync, file); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Rename(PointIdentityRename, temp, final); err != nil {
		t.Fatal(err)
	}
	if err := fsys.SyncDir(PointIdentityDirectorySync, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(PointStat, final); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDir(PointReadDir, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("ab")) {
		t.Fatalf("file = %q, want ab", got)
	}
	if err := fsys.Remove(PointCompactionRemove, final); err != nil {
		t.Fatal(err)
	}

	want := []Event{
		{PointIdentityTempCreate, Before}, {PointIdentityTempCreate, After},
		{PointIdentityTempWrite, Before}, {PointIdentityTempWrite, After},
		{PointWALTailTruncate, Before}, {PointWALTailTruncate, After},
		{PointIdentityTempSync, Before}, {PointIdentityTempSync, After},
		{PointIdentityRename, Before}, {PointIdentityRename, After},
		{PointIdentityDirectorySync, Before}, {PointIdentityDirectorySync, After},
		{PointStat, Before}, {PointStat, After},
		{PointReadDir, Before}, {PointReadDir, After},
		{PointCompactionRemove, Before}, {PointCompactionRemove, After},
	}
	if !reflect.DeepEqual(recorder.Events(), want) {
		t.Fatalf("events:\n got %#v\nwant %#v", recorder.Events(), want)
	}
}

func TestInjectorTargetsExactOccurrenceAndSeed(t *testing.T) {
	injected := errors.New("injected")
	injector := NewInjector(23, Rule{Point: PointWALFileSync, Phase: Before, Occurrence: 2, Seed: 23, Err: injected})
	fsys := NewOS(injector)
	file, err := fsys.OpenFile(PointWALHeaderCreate, filepath.Join(t.TempDir(), "wal"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := fsys.Sync(PointWALFileSync, file); err != nil {
		t.Fatalf("first sync = %v", err)
	}
	if err := fsys.Sync(PointWALFileSync, file); !errors.Is(err, injected) {
		t.Fatalf("second sync = %v, want injected", err)
	}
	if err := fsys.Sync(PointWALFileSync, file); err != nil {
		t.Fatalf("third sync = %v", err)
	}

	stopper := NewInjector(9, Rule{Point: PointManifestRename, Phase: After, Occurrence: 1, Seed: 9, Stop: true})
	stopFS := NewOS(stopper)
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	newPath := filepath.Join(dir, "new")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stopFS.Rename(PointManifestRename, old, newPath); !errors.Is(err, ErrProcessStop) {
		t.Fatalf("rename = %v, want process stop", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("after-hook must run after rename: %v", err)
	}
}
