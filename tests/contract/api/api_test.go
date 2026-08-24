package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"gapdb/gapdb"
)

type daemon struct {
	command *exec.Cmd
	socket  string
}

func startDaemon(t *testing.T, binary, directory string) *daemon {
	t.Helper()
	command := exec.Command(binary, "--db", directory, "--json")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	instance := &daemon{command: command}
	t.Cleanup(func() { instance.stop(t) })
	var ready struct {
		Ready  bool   `json:"ready"`
		Socket string `json:"socket_path"`
	}
	if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&ready); err != nil || !ready.Ready || ready.Socket == "" {
		_ = command.Process.Kill()
		t.Fatalf("readiness = %+v, %v", ready, err)
	}
	instance.socket = ready.Socket
	return instance
}

func buildDaemon(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "gapdbd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/gapdbd")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gapdbd: %v\n%s", err, output)
	}
	return binary
}

func (daemon *daemon) stop(t *testing.T) {
	t.Helper()
	if daemon == nil || daemon.command == nil || daemon.command.ProcessState != nil {
		return
	}
	if err := daemon.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("signal daemon: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = daemon.command.Process.Kill()
		<-done
		t.Errorf("daemon shutdown timed out")
	}
}

func TestDaemonPublicClientContractAndRestart(t *testing.T) {
	binary := buildDaemon(t)
	directory := filepath.Join(t.TempDir(), "database")
	daemon := startDaemon(t, binary, directory)
	client, err := gapdb.Dial(daemon.socket, gapdb.ClientOptions{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if watch, err := client.Watch(t.Context(), "", 1); watch != nil || !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionAhead}) {
		if watch != nil {
			watch.Close()
		}
		t.Fatalf("ahead watch registration = %v, %#v", watch, err)
	}

	first, err := client.Put(gapdb.WithRequestID(t.Context(), "put-1"), "items/a", []byte{0, 1, 2}, nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutIfAbsent(t.Context(), "items/a", []byte("loser"), nil, gapdb.AckDurable); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAlreadyExists}) {
		t.Fatalf("PutIfAbsent error = %#v", err)
	}
	second, err := client.CompareAndSwap(t.Context(), "items/a", first.Revision, []byte("next"), nil, gapdb.AckDurable)
	if err != nil || second.DurableThroughRevision < second.Revision {
		t.Fatalf("CAS = %+v, %v", second, err)
	}
	batch, err := client.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("items/b", []byte("b"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		gapdb.NewPutMutation("items/c", []byte("c"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
	}})
	if err != nil || batch.MutationCount != 2 {
		t.Fatalf("batch = %+v, %v", batch, err)
	}
	page, err := client.ScanPrefix(t.Context(), "items/", 2, "")
	if err != nil || len(page.Records) != 2 || !page.Truncated || page.Cursor == "" {
		t.Fatalf("scan = %+v, %v", page, err)
	}
	page2, err := client.ScanPrefix(t.Context(), "items/", 2, page.Cursor)
	if err != nil || len(page2.Records) != 1 {
		t.Fatalf("scan continuation = %+v, %v", page2, err)
	}

	watchCtx, cancel := context.WithCancel(t.Context())
	watch, err := client.Watch(watchCtx, "watch/", 0)
	if err != nil {
		t.Fatal(err)
	}
	put, err := client.Put(t.Context(), "watch/a", []byte("v"), nil, gapdb.AckDurable)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-watch.Events:
		if event.Revision != put.Revision {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch timeout")
	}
	cancel()
	<-watch.Ended

	status, err := client.Status(t.Context())
	if err != nil || status.DatabaseID == "" {
		t.Fatalf("status=%+v %v", status, err)
	}
	if _, err := client.Health(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stats(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DescribeConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Verify(t.Context(), "sampled"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSnapshot(t.Context(), status.DatabaseID, status.CurrentRevision); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := client.Compact(t.Context(), status.DatabaseID, status.CurrentRevision); err != nil {
		t.Fatalf("compact: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if result, err := client.Backup(t.Context(), status.DatabaseID, status.CurrentRevision, backup); err != nil || result.Revision != status.CurrentRevision {
		t.Fatalf("backup=%+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(backup, "backup.json")); err != nil {
		t.Fatalf("backup not published: %v", err)
	}
	expires := time.Now().Add(50 * time.Millisecond).UTC()
	if _, err := client.Put(t.Context(), "expires", []byte("v"), &expires, gapdb.AckDurable); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := client.Get(t.Context(), "expires"); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
		t.Fatalf("expired get=%v", err)
	}
	if _, err := client.DeleteIfRevision(t.Context(), "items/a", first.Revision, gapdb.AckDurable); !errors.Is(err, &gapdb.Error{Code: gapdb.CodeRevisionMismatch}) {
		t.Fatalf("delete mismatch=%#v", err)
	}

	// Exactly one concurrent conditional winner.
	var winners int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, _ := gapdb.Dial(daemon.socket, gapdb.ClientOptions{Timeout: 3 * time.Second})
			if other == nil {
				return
			}
			defer other.Close()
			if _, err := other.PutIfAbsent(context.Background(), "race/key", []byte("v"), nil, gapdb.AckDurable); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			} else if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeAlreadyExists}) {
				t.Errorf("race error=%v", err)
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}

	daemon.stop(t)
	daemon = startDaemon(t, binary, directory)
	record, err := client.Get(t.Context(), "items/a")
	if err != nil || string(record.Value) != "next" {
		t.Fatalf("restarted record=%+v %v", record, err)
	}
}

func TestDaemonRefusesSecondOwnerAndCorruptStartupWithoutSocket(t *testing.T) {
	binary := buildDaemon(t)
	directory := filepath.Join(t.TempDir(), "database")
	first := startDaemon(t, binary, directory)
	second := exec.Command(binary, "--db", directory, "--json")
	output, err := second.Output()
	if err == nil {
		t.Fatal("second owner unexpectedly succeeded")
	}
	var failure struct {
		Error *gapdb.Error `json:"error"`
	}
	if decodeErr := json.Unmarshal(output, &failure); decodeErr != nil || failure.Error == nil || failure.Error.Code != gapdb.CodeOwnerExists {
		t.Fatalf("second owner output=%s decode=%v", output, decodeErr)
	}
	client, err := gapdb.Dial(first.socket, gapdb.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("first owner socket was disturbed: %v", err)
	}
	_ = client.Close()

	corrupt := filepath.Join(t.TempDir(), "corrupt")
	if err := os.Mkdir(corrupt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "IDENTITY"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--db", corrupt, "--json")
	output, err = command.Output()
	if err == nil {
		t.Fatal("corrupt startup unexpectedly succeeded")
	}
	failure.Error = nil
	if decodeErr := json.Unmarshal(output, &failure); decodeErr != nil || failure.Error == nil || failure.Error.Code != gapdb.CodeCorruptIdentity {
		t.Fatalf("corrupt startup output=%s decode=%v", output, decodeErr)
	}
	if _, statErr := os.Lstat(filepath.Join(corrupt, "gapdb.sock")); !os.IsNotExist(statErr) {
		t.Fatalf("corrupt startup created socket: %v", statErr)
	}
}
