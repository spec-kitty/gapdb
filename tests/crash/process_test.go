package crash_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
)

type crashDaemon struct {
	command *exec.Cmd
	socket  string
	stderr  *bytes.Buffer
	wait    sync.Once
	waitErr error
}

func buildGapdbd(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "gapdbd")
	command := exec.Command("go", "build", "-trimpath", "-tags", "gapdb_crash_evidence", "-o", binary, "./cmd/gapdbd")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build gapdbd: %v\n%s", err, output)
	}
	return binary
}

func startCrashDaemon(t *testing.T, binary, directory string) *crashDaemon {
	return startCrashDaemonWithEnvironment(t, binary, directory, nil)
}

func startCrashDaemonWithEnvironment(t *testing.T, binary, directory string, environment map[string]string) *crashDaemon {
	t.Helper()
	command := exec.Command(binary, "--db", directory, "--json", "--read-timeout", "2s", "--write-timeout", "2s", "--idle-timeout", "3s")
	command.Env = os.Environ()
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	instance := &crashDaemon{command: command, stderr: stderr}
	var ready struct {
		SchemaVersion uint16 `json:"schema_version"`
		Ready         bool   `json:"ready"`
		SocketPath    string `json:"socket_path"`
	}
	result := make(chan error, 1)
	go func() { result <- json.NewDecoder(bufio.NewReader(stdout)).Decode(&ready) }()
	select {
	case err := <-result:
		if err != nil || ready.SchemaVersion != 1 || !ready.Ready || ready.SocketPath == "" {
			_ = command.Process.Kill()
			_ = instance.waitForExit()
			t.Fatalf("readiness=%+v err=%v stderr=%s", ready, err, stderr)
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		_ = instance.waitForExit()
		t.Fatalf("daemon readiness timeout: %s", stderr)
	}
	instance.socket = ready.SocketPath
	return instance
}

func TestGapdbdHookedCrashMilestones(t *testing.T) {
	binary := buildGapdbd(t)
	mutationPoints := []faultfs.Point{faultfs.PointWALFrameWrite, faultfs.PointWALBufferFlush, faultfs.PointWALFileSync, faultfs.PointMapApply, faultfs.PointResponsePublish}
	for _, point := range mutationPoints {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(string(point)+"/"+string(phase), func(t *testing.T) {
				root := t.TempDir()
				directory := filepath.Join(root, "database")
				initial := startCrashDaemon(t, binary, directory)
				initialClient := dialCrashClient(t, initial.socket)
				baseline, err := initialClient.Put(t.Context(), "baseline", []byte("durable"), nil, gapdb.AckDurable)
				if err != nil {
					t.Fatal(err)
				}
				initialStatus, err := initialClient.Status(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_ = initialClient.Close()
				initial.graceful(t)

				journal, err := openLedger(filepath.Join(root, "hook-ledger.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				defer journal.close()
				effects := []oracleEffect{{"target/a", "value"}, {"target/b", "value"}}
				if initialStatus.ReservedRevisionEnd == ^gapdb.Revision(0) {
					t.Fatal("cannot predict the next reserved revision at maximum")
				}
				targetRevision := initialStatus.ReservedRevisionEnd + 1
				if err := journal.append(processLedgerEntry{SchemaVersion: 1, OperationID: "target", Milestone: "before_" + string(point) + "_" + string(phase), Class: unacknowledged, Revision: targetRevision, Effects: effects}); err != nil {
					t.Fatal(err)
				}
				instance := startCrashDaemonWithEnvironment(t, binary, directory, map[string]string{
					"GAPDB_CRASH_POINT": string(point), "GAPDB_CRASH_PHASE": string(phase), "GAPDB_CRASH_OCCURRENCE": "1",
				})
				client := dialCrashClient(t, instance.socket)
				mutation, mutationErr := client.AtomicBatch(context.Background(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
					gapdb.NewPutMutation("target/a", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
					gapdb.NewPutMutation("target/b", []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
				}})
				_ = client.Close()
				instance.waitForCrash(t)
				class := unacknowledged
				if mutationErr == nil {
					class = acknowledgedDurable
					if err := journal.append(processLedgerEntry{SchemaVersion: 1, OperationID: "target", Milestone: "response_observed", Class: class, Revision: mutation.Revision, Effects: effects}); err != nil {
						t.Fatal(err)
					}
				}

				restarted := startCrashDaemon(t, binary, directory)
				reconciler := dialCrashClient(t, restarted.socket)
				status, err := reconciler.Status(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				observation := recoveryObservation{Revision: status.CurrentRevision, Records: make(map[string]string), RecordRevisions: make(map[string]gapdb.Revision)}
				for _, key := range []string{"baseline", "target/a", "target/b"} {
					record, getErr := reconciler.Get(t.Context(), key)
					if getErr == nil {
						observation.Records[key] = string(record.Value)
						observation.RecordRevisions[key] = record.Revision
					} else if !errors.Is(getErr, &gapdb.Error{Code: gapdb.CodeNotFound}) {
						t.Fatal(getErr)
					}
				}
				commits := []oracleCommit{
					{Schedule: "process/baseline", Revision: baseline.Revision, Class: acknowledgedDurable, Effects: []oracleEffect{{"baseline", "durable"}}},
				}
				journalCommits, err := readProcessLedger(filepath.Join(root, "hook-ledger.jsonl"), processLedgerAuthority{
					Terminal: journal.terminal(),
					RequestedAcknowledgements: map[string]acknowledgementClass{
						"target": acknowledgedDurable,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				commits = append(commits, journalCommits...)
				if err := reconcile(commits, observation); err != nil {
					t.Fatal(err)
				}
				_ = reconciler.Close()
				restarted.graceful(t)
			})
		}
	}

	for _, point := range []faultfs.Point{faultfs.PointSnapshotRename, faultfs.PointManifestRename} {
		for _, phase := range []faultfs.Phase{faultfs.Before, faultfs.After} {
			t.Run(string(point)+"/"+string(phase), func(t *testing.T) {
				directory := filepath.Join(t.TempDir(), "database")
				initial := startCrashDaemon(t, binary, directory)
				client := dialCrashClient(t, initial.socket)
				baseline, err := client.Put(t.Context(), "snapshot/baseline", []byte("durable"), nil, gapdb.AckDurable)
				if err != nil {
					t.Fatal(err)
				}
				_ = client.Close()
				initial.graceful(t)
				instance := startCrashDaemonWithEnvironment(t, binary, directory, map[string]string{
					"GAPDB_CRASH_POINT": string(point), "GAPDB_CRASH_PHASE": string(phase), "GAPDB_CRASH_OCCURRENCE": "1",
				})
				client = dialCrashClient(t, instance.socket)
				status, err := client.Status(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_, _ = client.CreateSnapshot(context.Background(), status.DatabaseID, status.CurrentRevision)
				_ = client.Close()
				instance.waitForCrash(t)
				restarted := startCrashDaemon(t, binary, directory)
				reconciler := dialCrashClient(t, restarted.socket)
				record, err := reconciler.Get(t.Context(), "snapshot/baseline")
				if err != nil || string(record.Value) != "durable" || record.Revision != baseline.Revision {
					t.Fatalf("snapshot crash recovery=%+v %v", record, err)
				}
				if verified, err := reconciler.Verify(t.Context(), "full"); err != nil || !verified.Verified {
					t.Fatalf("snapshot crash verify=%+v %v", verified, err)
				}
				_ = reconciler.Close()
				restarted.graceful(t)
			})
		}
	}
}

func (daemon *crashDaemon) waitForExit() error {
	daemon.wait.Do(func() { daemon.waitErr = daemon.command.Wait() })
	return daemon.waitErr
}

func (daemon *crashDaemon) waitForCrash(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- daemon.waitForExit() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("evidence-tagged daemon exited cleanly at a crash milestone")
		}
		status, ok := daemon.command.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("evidence-tagged daemon termination=%v, want SIGKILL; err=%v stderr=%s", daemon.command.ProcessState, err, daemon.stderr)
		}
	case <-time.After(5 * time.Second):
		daemon.kill(t)
		t.Fatal("real gapdbd did not crash at configured milestone")
	}
}

func (daemon *crashDaemon) kill(t *testing.T) {
	t.Helper()
	if err := daemon.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("SIGKILL: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.waitForExit() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("killed daemon was not reaped")
	}
}

func (daemon *crashDaemon) graceful(t *testing.T) {
	t.Helper()
	if err := daemon.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.waitForExit() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful exit: %v stderr=%s", err, daemon.stderr)
		}
	case <-time.After(10 * time.Second):
		_ = daemon.command.Process.Kill()
		t.Fatal("graceful drain timed out")
	}
}

type processLedgerEntry struct {
	SchemaVersion uint16               `json:"schema_version"`
	OperationID   string               `json:"operation_id"`
	Milestone     string               `json:"milestone"`
	Class         acknowledgementClass `json:"acknowledgement_class"`
	Revision      gapdb.Revision       `json:"revision"`
	Effects       []oracleEffect       `json:"effects"`
}

type processLedgerAuthority struct {
	Terminal                  ledgerTerminal
	RequestedAcknowledgements map[string]acknowledgementClass
}

func readProcessLedger(path string, authority processLedgerAuthority) ([]oracleCommit, error) {
	if authority.Terminal.SchemaVersion != 1 || authority.Terminal.EntryCount == 0 || len(authority.Terminal.SHA256) != 64 || len(authority.RequestedAcknowledgements) == 0 {
		return nil, errors.New("independently retained ledger authority is absent or invalid")
	}
	if decoded, err := hex.DecodeString(authority.Terminal.SHA256); err != nil || len(decoded) != 32 {
		return nil, errors.New("independently retained ledger authority has a noncanonical digest")
	}
	for operationID, class := range authority.RequestedAcknowledgements {
		if operationID == "" || (class != acknowledgedDurable && class != acknowledgedMemory) {
			return nil, errors.New("independently retained requested acknowledgement authority is invalid")
		}
	}
	terminal, err := readLedgerTerminal(path + ".digest")
	if err != nil {
		return nil, fmt.Errorf("ledger terminal digest: %w", err)
	}
	if terminal != authority.Terminal {
		return nil, errors.New("ledger terminal digest does not match independently retained authority")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 64<<10)
	order := make([]string, 0)
	byOperation := make(map[string]oracleCommit)
	var chain [32]byte
	line := 0
	for scanner.Scan() {
		line++
		var frame ledgerFrame
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&frame); err != nil {
			return nil, fmt.Errorf("ledger frame %d is not strict JSON: %w", line, err)
		}
		if trailingErr := decoder.Decode(&struct{}{}); !errors.Is(trailingErr, io.EOF) {
			return nil, fmt.Errorf("ledger frame %d has trailing content: %w", line, trailingErr)
		}
		if frame.SchemaVersion != 1 || frame.Sequence != uint64(line) || frame.PreviousSHA256 != hex.EncodeToString(chain[:]) || len(frame.Entry) == 0 {
			return nil, fmt.Errorf("ledger frame %d has invalid sequence or predecessor integrity evidence", line)
		}
		computed := processLedgerHash(chain, frame.Sequence, frame.Entry)
		if frame.SHA256 != hex.EncodeToString(computed[:]) {
			return nil, fmt.Errorf("ledger frame %d integrity hash does not match", line)
		}
		chain = computed
		var entry processLedgerEntry
		decoder = json.NewDecoder(bytes.NewReader(frame.Entry))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("ledger line %d is not strict JSON: %w", line, err)
		}
		if trailingErr := decoder.Decode(&struct{}{}); !errors.Is(trailingErr, io.EOF) {
			return nil, fmt.Errorf("ledger line %d has trailing content: %w", line, trailingErr)
		}
		if entry.SchemaVersion != 1 || entry.OperationID == "" || entry.Milestone == "" || entry.Revision == 0 || len(entry.Effects) == 0 {
			return nil, fmt.Errorf("ledger line %d omits required authority evidence", line)
		}
		if entry.Class != acknowledgedDurable && entry.Class != acknowledgedMemory && entry.Class != unacknowledged {
			return nil, fmt.Errorf("ledger line %d has unknown acknowledgement class", line)
		}
		seenKeys := make(map[string]bool)
		for _, effect := range entry.Effects {
			if effect.Key == "" || seenKeys[effect.Key] {
				return nil, fmt.Errorf("ledger line %d has an empty or duplicate effect", line)
			}
			seenKeys[effect.Key] = true
		}
		commit := oracleCommit{Schedule: "ledger/" + entry.OperationID, Revision: entry.Revision, Class: entry.Class, Effects: append([]oracleEffect(nil), entry.Effects...)}
		requestedClass, requested := authority.RequestedAcknowledgements[entry.OperationID]
		if !requested {
			return nil, fmt.Errorf("ledger line %d has no independently retained requested acknowledgement", line)
		}
		if commit.Class != unacknowledged && commit.Class != requestedClass {
			return nil, fmt.Errorf("ledger line %d acknowledgement %q contradicts independently retained request %q", line, commit.Class, requestedClass)
		}
		previous, exists := byOperation[entry.OperationID]
		if exists {
			if previous.Revision != commit.Revision || !equalEffects(previous.Effects, commit.Effects) || previous.Class != unacknowledged || commit.Class == unacknowledged {
				return nil, fmt.Errorf("ledger line %d contradicts operation %q", line, entry.OperationID)
			}
		} else {
			order = append(order, entry.OperationID)
		}
		byOperation[entry.OperationID] = commit
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if line == 0 {
		return nil, errors.New("ledger is empty")
	}
	if terminal.SchemaVersion != 1 || terminal.EntryCount != uint64(line) || terminal.SHA256 != hex.EncodeToString(chain[:]) {
		return nil, fmt.Errorf("ledger terminal digest does not match %d verified frames", line)
	}
	commits := make([]oracleCommit, 0, len(order))
	for _, operationID := range order {
		commits = append(commits, byOperation[operationID])
	}
	if len(order) != len(authority.RequestedAcknowledgements) {
		return nil, errors.New("ledger operations do not match independently retained requests")
	}
	return commits, nil
}

func readLedgerTerminal(path string) (ledgerTerminal, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return ledgerTerminal{}, err
	}
	var terminal ledgerTerminal
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&terminal); err != nil {
		return ledgerTerminal{}, err
	}
	if trailingErr := decoder.Decode(&struct{}{}); !errors.Is(trailingErr, io.EOF) {
		return ledgerTerminal{}, fmt.Errorf("trailing content: %w", trailingErr)
	}
	if terminal.SchemaVersion != 1 || terminal.EntryCount == 0 || len(terminal.SHA256) != 64 {
		return ledgerTerminal{}, errors.New("terminal digest omits required integrity evidence")
	}
	if decoded, err := hex.DecodeString(terminal.SHA256); err != nil || len(decoded) != 32 {
		return ledgerTerminal{}, errors.New("terminal digest hash is not canonical hexadecimal")
	}
	return terminal, nil
}

func equalEffects(left, right []oracleEffect) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func observeFromLedger(t *testing.T, client *gapdb.Client, commits []oracleCommit) recoveryObservation {
	t.Helper()
	status, err := client.Status(t.Context())
	if err != nil {
		t.Fatalf("observe ledger status: %v", err)
	}
	observation := recoveryObservation{Revision: status.CurrentRevision, Records: make(map[string]string), RecordRevisions: make(map[string]gapdb.Revision)}
	seen := make(map[string]bool)
	for _, commit := range commits {
		for _, effect := range commit.Effects {
			if seen[effect.Key] {
				continue
			}
			seen[effect.Key] = true
			record, getErr := client.Get(t.Context(), effect.Key)
			switch {
			case getErr == nil:
				observation.Records[effect.Key] = string(record.Value)
				observation.RecordRevisions[effect.Key] = record.Revision
			case errors.Is(getErr, &gapdb.Error{Code: gapdb.CodeNotFound}):
			default:
				t.Fatalf("observe ledger key %q: %v", effect.Key, getErr)
			}
		}
	}
	return observation
}

func dialCrashClient(t *testing.T, socket string) *gapdb.Client {
	t.Helper()
	client, err := gapdb.Dial(socket, gapdb.ClientOptions{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGapdbdCrashLedgerAndRestart(t *testing.T) {
	binary := buildGapdbd(t)
	root := t.TempDir()
	directory := filepath.Join(root, "database")
	journalPath := filepath.Join(root, "external-ledger.jsonl")
	journal, err := openLedger(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.close()

	instance := startCrashDaemon(t, binary, directory)
	client := dialCrashClient(t, instance.socket)
	batch, err := client.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
		gapdb.NewPutMutation("durable/a", []byte("one"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		gapdb.NewPutMutation("durable/b", []byte("two"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
	}})
	if err != nil || batch.DurableThroughRevision < batch.Revision {
		t.Fatalf("durable batch=%+v %v", batch, err)
	}
	if err := journal.append(processLedgerEntry{SchemaVersion: 1, OperationID: "durable-batch", Milestone: "after_append_flush_sync_apply_publish_response", Class: acknowledgedDurable, Revision: batch.Revision, Effects: []oracleEffect{{"durable/a", "one"}, {"durable/b", "two"}}}); err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.CreateSnapshot(t.Context(), status.DatabaseID, status.CurrentRevision)
	if err != nil || snapshot.Revision != status.CurrentRevision {
		t.Fatalf("snapshot=%+v %v", snapshot, err)
	}

	second := exec.Command(binary, "--db", directory, "--json")
	if output, err := second.Output(); err == nil {
		t.Fatalf("second owner started: %s", output)
	}
	_ = client.Close()
	instance.kill(t)
	if info, err := os.Lstat(instance.socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("SIGKILL did not leave the expected stale socket: %v %v", info, err)
	}

	instance = startCrashDaemon(t, binary, directory)
	client = dialCrashClient(t, instance.socket)
	durableLedger, err := readProcessLedger(journalPath, processLedgerAuthority{
		Terminal: journal.terminal(),
		RequestedAcknowledgements: map[string]acknowledgementClass{
			"durable-batch": acknowledgedDurable,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile(durableLedger, observeFromLedger(t, client, durableLedger)); err != nil {
		t.Fatal(err)
	}
	if verified, err := client.Verify(t.Context(), "full"); err != nil || !verified.Verified || verified.CurrentRevision < batch.Revision {
		t.Fatalf("post-crash verify=%+v %v", verified, err)
	}

	memory, err := client.Put(t.Context(), "memory/graceful", []byte("drained"), nil, gapdb.AckMemory)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.append(processLedgerEntry{SchemaVersion: 1, OperationID: "memory-drain", Milestone: "graceful_shutdown_drain", Class: acknowledgedMemory, Revision: memory.Revision, Effects: []oracleEffect{{"memory/graceful", "drained"}}}); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	instance.graceful(t)

	instance = startCrashDaemon(t, binary, directory)
	client = dialCrashClient(t, instance.socket)
	allLedger, err := readProcessLedger(journalPath, processLedgerAuthority{
		Terminal: journal.terminal(),
		RequestedAcknowledgements: map[string]acknowledgementClass{
			"durable-batch": acknowledgedDurable,
			"memory-drain":  acknowledgedMemory,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile(allLedger, observeFromLedger(t, client, allLedger)); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	instance.graceful(t)

	if len(allLedger) != 2 {
		t.Fatalf("verified ledger entries=%d, want 2", len(allLedger))
	}
	t.Logf("process_evidence ledger=%s entries=%d durable_revision=%d snapshot=%s", filepath.Base(journalPath), len(allLedger), batch.Revision, snapshot.Filename)
}

func TestGapdbdKilledDuringUnacknowledgedBatchRecoversNoneOrWhole(t *testing.T) {
	binary := buildGapdbd(t)
	directory := filepath.Join(t.TempDir(), "database")
	instance := startCrashDaemon(t, binary, directory)
	client := dialCrashClient(t, instance.socket)
	result := make(chan error, 1)
	go func() {
		_, err := client.AtomicBatch(context.Background(), gapdb.Batch{Ack: gapdb.AckDurable, Mutations: []gapdb.Mutation{
			gapdb.NewPutMutation("ambiguous/a", bytes.Repeat([]byte("a"), 1<<20), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
			gapdb.NewPutMutation("ambiguous/b", bytes.Repeat([]byte("b"), 1<<20), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil),
		}})
		result <- err
	}()
	time.Sleep(time.Millisecond)
	instance.kill(t)
	_ = client.Close()
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight client did not observe process death")
	}

	instance = startCrashDaemon(t, binary, directory)
	defer instance.graceful(t)
	client = dialCrashClient(t, instance.socket)
	defer client.Close()
	present := 0
	var revision gapdb.Revision
	for _, key := range []string{"ambiguous/a", "ambiguous/b"} {
		record, err := client.Get(t.Context(), key)
		if err == nil {
			present++
			if revision != 0 && revision != record.Revision {
				t.Fatalf("partial batch revisions: %d and %d", revision, record.Revision)
			}
			revision = record.Revision
		} else if !errors.Is(err, &gapdb.Error{Code: gapdb.CodeNotFound}) {
			t.Fatalf("reconcile %s: %v", key, err)
		}
	}
	if present != 0 && present != 2 {
		t.Fatalf("unacknowledged batch partially recovered: %d/2", present)
	}
	classification := "absent"
	if present == 2 {
		classification = "complete"
	}
	t.Logf("unacknowledged_recovery classification=%s revision=%d", classification, revision)
}

func TestProcessLedgerIsStrictAndAuthoritative(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ledger.jsonl")
	journal, err := openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	intent := processLedgerEntry{SchemaVersion: 1, OperationID: "batch", Milestone: "intent", Class: unacknowledged, Revision: 7, Effects: []oracleEffect{{"a", "one"}, {"b", "two"}}}
	acknowledgement := intent
	acknowledgement.Milestone = "response"
	acknowledgement.Class = acknowledgedDurable
	if err := journal.append(intent); err != nil {
		t.Fatal(err)
	}
	if err := journal.append(acknowledgement); err != nil {
		t.Fatal(err)
	}
	authority := processLedgerAuthority{
		Terminal: journal.terminal(),
		RequestedAcknowledgements: map[string]acknowledgementClass{
			"batch": acknowledgedDurable,
		},
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	commits, err := readProcessLedger(path, authority)
	if err != nil || len(commits) != 1 || commits[0].Class != acknowledgedDurable {
		t.Fatalf("valid ledger=%+v err=%v", commits, err)
	}
	observed := recoveryObservation{Revision: 7, Records: map[string]string{"a": "one", "b": "two"}, RecordRevisions: map[string]gapdb.Revision{"a": 7, "b": 7}}
	if err := reconcile(commits, observed); err != nil {
		t.Fatalf("valid ledger did not drive oracle: %v", err)
	}

	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rechainedRevision, rechainedRevisionTerminal := rechainProcessLedger(t, valid, func(entry *processLedgerEntry) { entry.Revision = 6 })
	for name, test := range map[string]struct {
		corrupt []byte
		want    string
	}{
		"revision":              {bytes.ReplaceAll(valid, []byte("\"revision\":7"), []byte("\"revision\":6")), "integrity hash"},
		"revision-rechained":    {rechainedRevision, "terminal digest"},
		"key":                   {bytes.ReplaceAll(valid, []byte("\"key\":\"a\""), []byte("\"key\":\"z\"")), "integrity hash"},
		"value":                 {bytes.ReplaceAll(valid, []byte("\"value\":\"one\""), []byte("\"value\":\"won\"")), "integrity hash"},
		"acknowledgement-class": {bytes.Replace(valid, []byte("\"acknowledgement_class\":\"durable\""), []byte("\"acknowledgement_class\":\"memory\""), 1), "integrity hash"},
		"trailing-json":         {append(bytes.TrimSuffix(append([]byte(nil), valid...), []byte("\n")), []byte(" {}\n")...), "trailing content"},
	} {
		t.Run(name, func(t *testing.T) {
			corruptPath := copyLedgerFixture(t, path, name)
			if err := os.WriteFile(corruptPath, test.corrupt, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readProcessLedger(corruptPath, authority); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tamper error=%v, want %q", err, test.want)
			}
		})
	}

	t.Run("coherent-revision-rewrite", func(t *testing.T) {
		corruptPath := copyLedgerFixture(t, path, "coherent-revision")
		writeCoherentLedgerRewrite(t, corruptPath, rechainedRevision, rechainedRevisionTerminal)
		if _, err := readProcessLedger(corruptPath, authority); err == nil || !strings.Contains(err.Error(), "retained authority") {
			t.Fatalf("original retained anchor accepted coherent rewrite: %v", err)
		}
		parsed, err := readProcessLedger(corruptPath, processLedgerAuthority{
			Terminal: rechainedRevisionTerminal,
			RequestedAcknowledgements: map[string]acknowledgementClass{
				"batch": acknowledgedDurable,
			},
		})
		if err != nil {
			t.Fatalf("coherently rewritten fixture did not reach public reconciliation: %v", err)
		}
		if err := reconcile(parsed, observed); err == nil || !strings.Contains(err.Error(), "at revision 7 instead of 6") {
			t.Fatalf("coherent revision rewrite was not rejected by public record revision: %v", err)
		}
	})

	t.Run("coherent-acknowledgement-downgrade", func(t *testing.T) {
		rewritten, terminal := rechainProcessLedger(t, valid, func(entry *processLedgerEntry) {
			if entry.Class == acknowledgedDurable {
				entry.Class = acknowledgedMemory
			}
		})
		corruptPath := copyLedgerFixture(t, path, "coherent-acknowledgement")
		writeCoherentLedgerRewrite(t, corruptPath, rewritten, terminal)
		if _, err := readProcessLedger(corruptPath, authority); err == nil || !strings.Contains(err.Error(), "retained authority") {
			t.Fatalf("original retained anchor accepted coherent rewrite: %v", err)
		}
		if _, err := readProcessLedger(corruptPath, processLedgerAuthority{
			Terminal: terminal,
			RequestedAcknowledgements: map[string]acknowledgementClass{
				"batch": acknowledgedDurable,
			},
		}); err == nil || !strings.Contains(err.Error(), "contradicts independently retained request") {
			t.Fatalf("coherent acknowledgement downgrade error=%v", err)
		}
	})

	t.Run("semantic-revision-mismatch", func(t *testing.T) {
		mismatchPath := filepath.Join(root, "semantic-revision.jsonl")
		mismatch, err := openLedger(mismatchPath)
		if err != nil {
			t.Fatal(err)
		}
		entry := acknowledgement
		entry.Revision = 6
		if err := mismatch.append(entry); err != nil {
			t.Fatal(err)
		}
		mismatchAuthority := processLedgerAuthority{
			Terminal: mismatch.terminal(),
			RequestedAcknowledgements: map[string]acknowledgementClass{
				"batch": acknowledgedDurable,
			},
		}
		if err := mismatch.close(); err != nil {
			t.Fatal(err)
		}
		parsed, err := readProcessLedger(mismatchPath, mismatchAuthority)
		if err != nil {
			t.Fatal(err)
		}
		if err := reconcile(parsed, observed); err == nil || !strings.Contains(err.Error(), "at revision 7 instead of 6") {
			t.Fatalf("semantic revision mismatch error=%v", err)
		}
	})

	for _, test := range []struct {
		name    string
		entries []processLedgerEntry
	}{
		{"invalid-transition", []processLedgerEntry{acknowledgement, intent}},
		{"duplicate-acknowledgement", []processLedgerEntry{intent, acknowledgement, acknowledgement}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalidPath := filepath.Join(root, test.name+".jsonl")
			invalid, err := openLedger(invalidPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range test.entries {
				if err := invalid.append(entry); err != nil {
					t.Fatal(err)
				}
			}
			invalidAuthority := processLedgerAuthority{
				Terminal: invalid.terminal(),
				RequestedAcknowledgements: map[string]acknowledgementClass{
					"batch": acknowledgedDurable,
				},
			}
			if err := invalid.close(); err != nil {
				t.Fatal(err)
			}
			if _, err := readProcessLedger(invalidPath, invalidAuthority); err == nil || !strings.Contains(err.Error(), "contradicts operation") {
				t.Fatalf("transition error=%v", err)
			}
		})
	}
}

func TestProcessLedgerRequiresIndependentRetainedAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	journal, err := openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	intent := processLedgerEntry{SchemaVersion: 1, OperationID: "batch", Milestone: "intent", Class: unacknowledged, Revision: 7, Effects: []oracleEffect{{"a", "one"}}}
	if err := journal.append(intent); err != nil {
		t.Fatal(err)
	}
	staleTerminal := journal.terminal()
	response := intent
	response.Milestone = "response"
	response.Class = acknowledgedDurable
	if err := journal.append(response); err != nil {
		t.Fatal(err)
	}
	authority := processLedgerAuthority{
		Terminal: journal.terminal(),
		RequestedAcknowledgements: map[string]acknowledgementClass{
			"batch": acknowledgedDurable,
		},
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readProcessLedger(path, authority); err != nil {
		t.Fatalf("retained authority rejected: %v", err)
	}

	for name, altered := range map[string]processLedgerAuthority{
		"absent": {},
		"stale": {
			Terminal:                  staleTerminal,
			RequestedAcknowledgements: authority.RequestedAcknowledgements,
		},
		"wrong": {
			Terminal:                  ledgerTerminal{SchemaVersion: 1, EntryCount: authority.Terminal.EntryCount, SHA256: strings.Repeat("f", 64)},
			RequestedAcknowledgements: authority.RequestedAcknowledgements,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readProcessLedger(path, altered); err == nil || !strings.Contains(err.Error(), "retained") {
				t.Fatalf("authority error=%v, want retained authority failure", err)
			}
		})
	}
}

func copyLedgerFixture(t *testing.T, source, name string) string {
	t.Helper()
	destination := filepath.Join(filepath.Dir(source), name+".jsonl")
	for _, suffix := range []string{"", ".digest"} {
		encoded, err := os.ReadFile(source + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination+suffix, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return destination
}

func rechainProcessLedger(t *testing.T, encoded []byte, mutate func(*processLedgerEntry)) ([]byte, ledgerTerminal) {
	t.Helper()
	var chain [32]byte
	var result []byte
	scanner := bufio.NewScanner(bytes.NewReader(encoded))
	for scanner.Scan() {
		var frame ledgerFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		var entry processLedgerEntry
		if err := json.Unmarshal(frame.Entry, &entry); err != nil {
			t.Fatal(err)
		}
		mutate(&entry)
		entryBytes, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		hash := processLedgerHash(chain, frame.Sequence, entryBytes)
		frame.PreviousSHA256 = hex.EncodeToString(chain[:])
		frame.Entry = entryBytes
		frame.SHA256 = hex.EncodeToString(hash[:])
		frameBytes, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, frameBytes...)
		result = append(result, '\n')
		chain = hash
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return result, ledgerTerminal{SchemaVersion: 1, EntryCount: uint64(bytes.Count(result, []byte{'\n'})), SHA256: hex.EncodeToString(chain[:])}
}

func writeCoherentLedgerRewrite(t *testing.T, path string, encoded []byte, terminal ledgerTerminal) {
	t.Helper()
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".digest", digest, 0o600); err != nil {
		t.Fatal(err)
	}
}
