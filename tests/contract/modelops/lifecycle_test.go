package modelops_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type result struct {
	stdout []byte
	exit   int
}

type ownerProcess struct {
	command *exec.Cmd
	socket  string
}

func TestModelOnlyLifecycleUsesStableMachineEvidence(t *testing.T) {
	gapctl := build(t, "gapctl", "./cmd/gapctl")
	gapdbd := build(t, "gapdbd", "./cmd/gapdbd")
	database := filepath.Join(t.TempDir(), "database")
	owner := startOwner(t, gapdbd, database)
	run := func(id string, arguments ...string) result {
		global := []string{"--socket", owner.socket, "--request-id", id}
		return runCommand(t, gapctl, append(global, arguments...)...)
	}

	status := requireSuccess(t, run("model-status", "status"), "status")
	statusResult := status["result"].(map[string]any)
	databaseID := statusResult["database_id"].(string)
	watchCommand := exec.Command(gapctl, "--socket", owner.socket, "--request-id", "model-watch", "--deadline", "30s", "--output", "jsonl", "watch", "--prefix", "model/", "--after-revision", "0")
	watchOutput, err := watchCommand.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := watchCommand.Start(); err != nil {
		t.Fatal(err)
	}
	watchReader := bufio.NewReader(watchOutput)
	started := readJSONL(t, watchReader)
	if started["stream"] != "started" || started["schema_version"] != float64(1) || started["limits"] == nil {
		t.Fatalf("watch started = %#v", started)
	}
	put := requireSuccess(t, run("model-put", "put", "--key", "model/lease", "--value-base64", "b3duZXItMQ==", "--ack", "durable"), "put")
	revision := int(put["result"].(map[string]any)["revision"].(float64))
	event := readJSONL(t, watchReader)
	if event["stream"] != "event" || event["event"].(map[string]any)["key"] != "model/lease" || event["limits"] == nil {
		t.Fatalf("watch event = %#v", event)
	}
	if err := watchCommand.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	ended := readJSONL(t, watchReader)
	if ended["stream"] != "ended" || ended["reason"] != "client_closed" || ended["error"].(map[string]any)["code"] != "DEADLINE_EXCEEDED" || ended["limits"] == nil {
		t.Fatalf("watch ended = %#v", ended)
	}
	if err := watchCommand.Wait(); err == nil || watchCommand.ProcessState.ExitCode() != 4 {
		t.Fatalf("watch exit = %v (%d)", err, watchCommand.ProcessState.ExitCode())
	}
	status = requireSuccess(t, run("model-status-2", "status"), "status")
	if int(status["result"].(map[string]any)["durable_through_revision"].(float64)) < revision {
		t.Fatal("durable evidence did not cover the lease mutation")
	}
	requireSuccess(t, run("model-verify", "verify", "--mode", "full"), "verify")
	requireSuccess(t, run("model-snapshot", "create-snapshot", "--expected-database-id", databaseID, "--expected-revision", fmt.Sprint(revision)), "create-snapshot")
	requireSuccess(t, run("model-compact", "compact", "--expected-database-id", databaseID, "--through-revision", fmt.Sprint(revision)), "compact")
	backup := filepath.Join(t.TempDir(), "backup")
	requireSuccess(t, run("model-backup", "backup", "--expected-database-id", databaseID, "--expected-revision", fmt.Sprint(revision), "--destination", backup), "backup")

	owner.stop(t)
	owner = startOwner(t, gapdbd, database)
	restarted := requireSuccess(t, run("model-restart-status", "status"), "status")
	if restarted["result"].(map[string]any)["database_id"] != databaseID {
		t.Fatal("restart changed database authority")
	}
	owner.stop(t)
	injectWALCorruption(t, database)
	offline := func(arguments ...string) result {
		return runCommand(t, gapctl, append([]string{"--db", database}, arguments...)...)
	}
	first := offline("inspect")
	second := offline("inspect")
	if first.exit != 5 || !bytes.Equal(first.stdout, second.stdout) {
		t.Fatalf("offline inspection is not stable: exit %d\n%s\n%s", first.exit, first.stdout, second.stdout)
	}
	failure := requireEnvelope(t, first, "offline_inspect")
	errorObject := failure["error"].(map[string]any)
	if errorObject["code"] != "CORRUPT_WAL" || !hasAction(errorObject, "recover_propose") || failure["evidence"] == nil {
		t.Fatalf("inspection evidence = %#v", failure)
	}
	proposal := requireSuccess(t, offline("recover-propose"), "offline_recover_propose")
	proposalObject := proposal["result"].(map[string]any)["proposal"].(map[string]any)
	if proposalObject["id"] == "" || proposalObject["database_id"] != databaseID || proposalObject["manifest_generation"] == nil {
		t.Fatalf("proposal = %#v", proposalObject)
	}
}

func readJSONL(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read JSONL: %v, %q", err, line)
	}
	var frame map[string]any
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatalf("decode JSONL: %v, %q", err, line)
	}
	return frame
}

func requireSuccess(t *testing.T, value result, operation string) map[string]any {
	t.Helper()
	envelope := requireEnvelope(t, value, operation)
	if value.exit != 0 || envelope["ok"] != true || envelope["result"] == nil {
		t.Fatalf("%s = exit %d %#v", operation, value.exit, envelope)
	}
	return envelope
}

func requireEnvelope(t *testing.T, value result, operation string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(value.stdout, &envelope); err != nil {
		t.Fatalf("decode %s output %q: %v", operation, value.stdout, err)
	}
	if envelope["schema_version"] != float64(1) || envelope["operation"] != operation || envelope["limits"] == nil {
		t.Fatalf("%s lacks stable schema/limits: %#v", operation, envelope)
	}
	return envelope
}

func hasAction(errorObject map[string]any, wanted string) bool {
	actions, _ := errorObject["safe_actions"].([]any)
	for _, action := range actions {
		if action == wanted {
			return true
		}
	}
	return false
}

func build(t *testing.T, name, target string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-o", binary, target)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, output)
	}
	return binary
}

func runCommand(t *testing.T, binary string, arguments ...string) result {
	t.Helper()
	command := exec.Command(binary, arguments...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	exit := 0
	if err != nil {
		var status *exec.ExitError
		if !errors.As(err, &status) {
			t.Fatalf("run gapctl: %v", err)
		}
		exit = status.ExitCode()
	}
	if stderr.Len() != 0 {
		t.Fatalf("gapctl emitted stderr: %q", stderr.String())
	}
	return result{stdout.Bytes(), exit}
}

func startOwner(t *testing.T, binary, database string) *ownerProcess {
	t.Helper()
	command := exec.Command(binary, "--db", database, "--json")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	owner := &ownerProcess{command: command}
	t.Cleanup(func() { owner.stop(t) })
	var ready struct {
		SchemaVersion uint16 `json:"schema_version"`
		Ready         bool   `json:"ready"`
		Socket        string `json:"socket_path"`
	}
	if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&ready); err != nil || ready.SchemaVersion != 1 || !ready.Ready || ready.Socket == "" {
		_ = command.Process.Kill()
		t.Fatalf("read owner readiness: %+v, %v", ready, err)
	}
	owner.socket = ready.Socket
	return owner
}

func (owner *ownerProcess) stop(t *testing.T) {
	t.Helper()
	if owner == nil || owner.command == nil || owner.command.ProcessState != nil {
		return
	}
	if err := owner.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("stop owner: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- owner.command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("owner exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = owner.command.Process.Kill()
		<-done
		t.Error("owner shutdown timed out")
	}
}

func injectWALCorruption(t *testing.T, database string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(database, "wal-*.gdb"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("WAL selection = %v, %v", paths, err)
	}
	value, err := os.ReadFile(paths[0])
	if err != nil || len(value) < 2 {
		t.Fatalf("read WAL: %v", err)
	}
	value[len(value)-1] ^= 0xff
	if err := os.WriteFile(paths[0], value, 0o600); err != nil {
		t.Fatal(err)
	}
}
