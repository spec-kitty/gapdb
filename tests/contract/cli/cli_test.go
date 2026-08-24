package cli_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"gapdb/gapdb"
)

type commandResult struct {
	stdout []byte
	stderr []byte
	exit   int
}

func buildGapctl(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "gapctl")
	command := exec.Command("go", "build", "-o", binary, "./cmd/gapctl")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build gapctl: %v\n%s", err, output)
	}
	return binary
}

func buildBinary(t *testing.T, name, target string) string {
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
	var stderr bytes.Buffer
	command.Stderr = &stderr
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
		t.Fatalf("daemon readiness = %+v, %v, stderr %q", ready, err, stderr.String())
	}
	instance.socket = ready.Socket
	return instance
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
		t.Error("daemon shutdown timed out")
	}
}

func runGapctl(t *testing.T, binary string, stdin []byte, arguments ...string) commandResult {
	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exit := 0
	if err != nil {
		var status *exec.ExitError
		if !errors.As(err, &status) {
			t.Fatalf("run gapctl: %v", err)
		}
		exit = status.ExitCode()
	}
	return commandResult{stdout.Bytes(), stderr.Bytes(), exit}
}

func decodeObject(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("decode output %q: %v", payload, err)
	}
	return value
}

func TestDeterministicCommandFramework(t *testing.T) {
	binary := buildGapctl(t)
	help := runGapctl(t, binary, nil, "help")
	if help.exit != 0 || len(help.stderr) != 0 || !bytes.HasSuffix(help.stdout, []byte("}\n")) {
		t.Fatalf("help = exit %d stdout %q stderr %q", help.exit, help.stdout, help.stderr)
	}
	object := decodeObject(t, help.stdout)
	if object["schema_version"] != float64(1) || object["ok"] != true || object["operation"] != "help" || object["limits"] == nil {
		t.Fatalf("help object = %#v", object)
	}
	repeat := runGapctl(t, binary, []byte("ignored stdin"), "help")
	if !bytes.Equal(help.stdout, repeat.stdout) {
		t.Fatalf("help is not byte deterministic:\n%s\n%s", help.stdout, repeat.stdout)
	}

	for _, arguments := range [][]string{{"unknown"}, {"--unknown", "value", "help"}, {"--output", "yaml", "help"}} {
		failure := runGapctl(t, binary, []byte("must not be read"), arguments...)
		if failure.exit != 2 || len(failure.stderr) != 0 || bytes.Count(failure.stdout, []byte("\n")) != 1 {
			t.Fatalf("gapctl %v = exit %d stdout %q stderr %q", arguments, failure.exit, failure.stdout, failure.stderr)
		}
		object := decodeObject(t, failure.stdout)
		errorObject, _ := object["error"].(map[string]any)
		if object["schema_version"] != float64(1) || object["ok"] != false || errorObject["code"] != "INVALID_REQUEST" || errorObject["safe_actions"] == nil || object["limits"] == nil {
			t.Fatalf("failure object = %#v", object)
		}
	}
}

func TestDataCommandsAgainstRealDaemon(t *testing.T) {
	gapctl := buildGapctl(t)
	gapdbd := buildBinary(t, "gapdbd", "./cmd/gapdbd")
	instance := startDaemon(t, gapdbd, filepath.Join(t.TempDir(), "database"))
	global := []string{"--socket", instance.socket, "--request-id", "cli-data"}
	run := func(arguments ...string) commandResult {
		return runGapctl(t, gapctl, nil, append(global, arguments...)...)
	}

	put := run("put", "--key", "items/a", "--value-base64", "AAEC", "--ack", "durable")
	putObject := decodeObject(t, put.stdout)
	putResult, _ := putObject["result"].(map[string]any)
	if put.exit != 0 || putResult["ack"] != "durable" || putResult["revision"] == nil || putResult["durable_through_revision"] == nil {
		t.Fatalf("put = exit %d %#v", put.exit, putObject)
	}
	revision := int(putResult["revision"].(float64))

	get := run("get", "--key", "items/a")
	getResult := decodeObject(t, get.stdout)["result"].(map[string]any)
	record := getResult["record"].(map[string]any)
	if get.exit != 0 || record["value_base64"] != "AAEC" || int(record["revision"].(float64)) != revision {
		t.Fatalf("get = exit %d %#v", get.exit, getResult)
	}

	loser := run("put-if-absent", "--key", "items/a", "--value-base64", "bG9zZXI=", "--ack", "durable")
	loserObject := decodeObject(t, loser.stdout)
	if loser.exit != 3 || loserObject["error"].(map[string]any)["code"] != "ALREADY_EXISTS" {
		t.Fatalf("put-if-absent = exit %d %#v", loser.exit, loserObject)
	}

	cas := run("compare-and-swap", "--key", "items/a", "--expected-revision", fmt.Sprint(revision), "--value-base64", "bmV4dA==", "--ack", "durable")
	casResult := decodeObject(t, cas.stdout)["result"].(map[string]any)
	if cas.exit != 0 || casResult["ack"] != "durable" {
		t.Fatalf("CAS = exit %d %#v", cas.exit, casResult)
	}

	emptyFile := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(emptyFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	emptyPut := run("put", "--key", "items/empty", "--value-file", emptyFile, "--ack", "memory")
	if emptyPut.exit != 0 {
		t.Fatalf("empty put = exit %d %s", emptyPut.exit, emptyPut.stdout)
	}
	emptyRevision := int(decodeObject(t, emptyPut.stdout)["result"].(map[string]any)["revision"].(float64))
	deleted := run("delete-if-revision", "--key", "items/empty", "--expected-revision", fmt.Sprint(emptyRevision), "--ack", "durable")
	if deleted.exit != 0 || decodeObject(t, deleted.stdout)["result"].(map[string]any)["ack"] != "durable" {
		t.Fatalf("delete = exit %d %s", deleted.exit, deleted.stdout)
	}

	batchFile := filepath.Join(t.TempDir(), "batch.json")
	batch := `{"ack":"durable","mutations":[{"kind":"put","key":"items/b","condition":{"kind":"absent"},"value_base64":"Yg=="},{"kind":"delete","key":"items/a","condition":{"kind":"revision","expected_revision":` + fmt.Sprint(int(casResult["revision"].(float64))) + `}}]}`
	if err := os.WriteFile(batchFile, []byte(batch), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := run("atomic-batch", "--file", batchFile); result.exit != 0 {
		t.Fatalf("batch = exit %d %s", result.exit, result.stdout)
	}
	page := run("scan-prefix", "--prefix", "items/", "--limit", "10")
	pageResult := decodeObject(t, page.stdout)["result"].(map[string]any)
	if page.exit != 0 || pageResult["observed_revision"] == nil || pageResult["as_of"] == nil || pageResult["truncated"] != false || len(pageResult["records"].([]any)) != 1 {
		t.Fatalf("scan = exit %d %#v", page.exit, pageResult)
	}

	for _, arguments := range [][]string{
		{"put", "--key", "bad", "--value-base64", "***", "--ack", "durable"},
		{"put", "--key", "bad", "--value-base64", "", "--ack", "sometimes"},
		{"put", "--key", "bad", "--value-base64", "", "--expires-at", "2026-08-23T12:00:00-05:00", "--ack", "durable"},
	} {
		result := runGapctl(t, gapctl, []byte("unselected stdin"), arguments...)
		if result.exit != 2 || decodeObject(t, result.stdout)["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
			t.Fatalf("invalid %v = exit %d %s", arguments, result.exit, result.stdout)
		}
	}
	for name, payload := range map[string]string{
		"duplicate": `{"ack":"memory","ack":"durable","mutations":[]}`,
		"unknown":   `{"ack":"memory","mutations":[],"surprise":true}`,
	} {
		path := filepath.Join(t.TempDir(), name+".json")
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		result := runGapctl(t, gapctl, nil, "atomic-batch", "--file", path)
		if result.exit != 2 || decodeObject(t, result.stdout)["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
			t.Fatalf("%s batch = exit %d %s", name, result.exit, result.stdout)
		}
	}
}

func TestWatchJSONLFlushesFramesAndTerminatesOnSignal(t *testing.T) {
	gapctl := buildGapctl(t)
	gapdbd := buildBinary(t, "gapdbd", "./cmd/gapdbd")
	instance := startDaemon(t, gapdbd, filepath.Join(t.TempDir(), "database"))
	command := exec.Command(gapctl, "--socket", instance.socket, "--request-id", "watch-cli", "--deadline", "30s", "--output", "jsonl", "watch", "--prefix", "watch/", "--after-revision", "0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	readFrame := func() map[string]any {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read watch frame: %v, line %q, stderr %q", err, line, stderr.String())
		}
		return decodeObject(t, line)
	}
	started := readFrame()
	if started["stream"] != "started" || started["registration_revision"] == nil || started["limits"] == nil {
		t.Fatalf("started = %#v", started)
	}
	put := runGapctl(t, gapctl, nil, "--socket", instance.socket, "put", "--key", "watch/key", "--value-base64", "dmFsdWU=", "--ack", "durable")
	if put.exit != 0 {
		t.Fatalf("put = exit %d %s", put.exit, put.stdout)
	}
	event := readFrame()
	if event["stream"] != "event" || event["event"].(map[string]any)["key"] != "watch/key" {
		t.Fatalf("event = %#v", event)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	ended := readFrame()
	if ended["stream"] != "ended" || ended["reason"] != "client_closed" || ended["ok"] != false {
		t.Fatalf("ended = %#v", ended)
	}
	if err := command.Wait(); err == nil || command.ProcessState.ExitCode() != 4 {
		t.Fatalf("watch exit = %v (%d), stderr %q", err, command.ProcessState.ExitCode(), stderr.String())
	}
	if trailing, err := reader.ReadBytes('\n'); len(trailing) != 0 || err == nil {
		t.Fatalf("unexpected trailing watch output %q, %v", trailing, err)
	}
}

func TestWatchRegistrationAndDisconnectHaveOneTerminalFrame(t *testing.T) {
	gapctl := buildGapctl(t)
	gapdbd := buildBinary(t, "gapdbd", "./cmd/gapdbd")
	instance := startDaemon(t, gapdbd, filepath.Join(t.TempDir(), "database"))
	ahead := runGapctl(t, gapctl, nil, "--socket", instance.socket, "--output", "jsonl", "watch", "--prefix", "", "--after-revision", "9")
	if ahead.exit != 3 || bytes.Count(ahead.stdout, []byte("\n")) != 1 {
		t.Fatalf("ahead watch = exit %d %q", ahead.exit, ahead.stdout)
	}
	aheadFrame := decodeObject(t, ahead.stdout)
	if aheadFrame["stream"] != "ended" || aheadFrame["error"].(map[string]any)["code"] != "REVISION_AHEAD" || aheadFrame["limits"] == nil {
		t.Fatalf("ahead frame = %#v", aheadFrame)
	}

	command := exec.Command(gapctl, "--socket", instance.socket, "--deadline", "30s", "--output", "jsonl", "watch", "--prefix", "", "--after-revision", "0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	started, err := reader.ReadBytes('\n')
	if err != nil || decodeObject(t, started)["stream"] != "started" {
		t.Fatalf("started = %q, %v", started, err)
	}
	if err := instance.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = instance.command.Wait()
	ended, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("ended = %q, %v", ended, err)
	}
	endedFrame := decodeObject(t, ended)
	if endedFrame["stream"] != "ended" || endedFrame["error"].(map[string]any)["code"] != "SERVER_UNAVAILABLE" || endedFrame["limits"] == nil {
		t.Fatalf("ended frame = %#v", endedFrame)
	}
	if err := command.Wait(); err == nil || command.ProcessState.ExitCode() != 4 {
		t.Fatalf("watch exit = %v (%d)", err, command.ProcessState.ExitCode())
	}
	if trailing, _ := reader.ReadBytes('\n'); len(trailing) != 0 {
		t.Fatalf("trailing watch output = %q", trailing)
	}
}

func TestWatchSlowPipeEndsWithCompleteLagFrame(t *testing.T) {
	gapctl := buildGapctl(t)
	gapdbd := buildBinary(t, "gapdbd", "./cmd/gapdbd")
	instance := startDaemon(t, gapdbd, filepath.Join(t.TempDir(), "database"))
	command := exec.Command(gapctl, "--socket", instance.socket, "--deadline", "15s", "--output", "jsonl", "watch", "--prefix", "slow/", "--after-revision", "0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	started, err := reader.ReadBytes('\n')
	if err != nil || decodeObject(t, started)["stream"] != "started" {
		t.Fatalf("started = %q, %v", started, err)
	}
	client, err := gapdb.Dial(instance.socket, gapdb.ClientOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for batchIndex := range 100 {
		mutations := make([]gapdb.Mutation, 100)
		for index := range mutations {
			key := fmt.Sprintf("slow/%04d/%03d", batchIndex, index)
			mutations[index] = gapdb.NewPutMutation(key, []byte("value"), gapdb.Condition{Kind: gapdb.ConditionAbsent}, nil)
		}
		if _, err := client.AtomicBatch(t.Context(), gapdb.Batch{Ack: gapdb.AckMemory, Mutations: mutations}); err != nil {
			t.Fatalf("batch %d: %v", batchIndex, err)
		}
	}
	type streamResult struct {
		frames []map[string]any
		err    error
	}
	done := make(chan streamResult, 1)
	go func() {
		var frames []map[string]any
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) != 0 {
				var frame map[string]any
				if decodeErr := json.Unmarshal(line, &frame); decodeErr != nil {
					done <- streamResult{frames, decodeErr}
					return
				}
				frames = append(frames, frame)
				if frame["stream"] == "ended" {
					done <- streamResult{frames, nil}
					return
				}
			}
			if readErr != nil {
				done <- streamResult{frames, readErr}
				return
			}
		}
	}()
	select {
	case stream := <-done:
		if stream.err != nil || len(stream.frames) == 0 {
			t.Fatalf("slow stream = %d frames, %v", len(stream.frames), stream.err)
		}
		terminal := stream.frames[len(stream.frames)-1]
		errorObject, _ := terminal["error"].(map[string]any)
		if terminal["stream"] != "ended" || errorObject["code"] != "WATCH_LAGGED" || terminal["limits"] == nil {
			t.Fatalf("terminal = %#v", terminal)
		}
	case <-time.After(20 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("slow watch did not terminate")
	}
	if err := command.Wait(); err == nil || command.ProcessState.ExitCode() != 3 {
		t.Fatalf("slow watch exit = %v (%d)", err, command.ProcessState.ExitCode())
	}
}

func TestOnlineAdministrationCommandsAgainstRealDaemon(t *testing.T) {
	gapctl := buildGapctl(t)
	gapdbd := buildBinary(t, "gapdbd", "./cmd/gapdbd")
	instance := startDaemon(t, gapdbd, filepath.Join(t.TempDir(), "database"))
	run := func(arguments ...string) commandResult {
		return runGapctl(t, gapctl, nil, append([]string{"--socket", instance.socket, "--request-id", "admin-cli"}, arguments...)...)
	}
	if seed := run("put", "--key", "admin/key", "--value-base64", "dmFsdWU=", "--ack", "durable"); seed.exit != 0 {
		t.Fatalf("seed = exit %d %s", seed.exit, seed.stdout)
	}
	status := run("status")
	statusResult := decodeObject(t, status.stdout)["result"].(map[string]any)
	if status.exit != 0 || statusResult["database_id"] == "" || statusResult["current_revision"] == nil || statusResult["limits"] == nil {
		t.Fatalf("status = exit %d %#v", status.exit, statusResult)
	}
	databaseID := statusResult["database_id"].(string)
	revision := int(statusResult["current_revision"].(float64))
	for _, arguments := range [][]string{{"health"}, {"stats"}, {"describe-config"}, {"verify", "--mode", "sampled"}} {
		result := run(arguments...)
		if result.exit != 0 || decodeObject(t, result.stdout)["result"] == nil {
			t.Fatalf("%v = exit %d %s", arguments, result.exit, result.stdout)
		}
	}
	snapshot := run("create-snapshot", "--expected-database-id", databaseID, "--expected-revision", fmt.Sprint(revision))
	snapshotObject := decodeObject(t, snapshot.stdout)
	snapshotResult, ok := snapshotObject["result"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot = exit %d %#v", snapshot.exit, snapshotObject)
	}
	if snapshot.exit != 0 || snapshotResult["filename"] == "" || snapshotResult["checksum"] == "" || snapshotResult["new_wal_start"] == nil {
		t.Fatalf("snapshot = exit %d %#v", snapshot.exit, snapshotResult)
	}
	compact := run("compact", "--expected-database-id", databaseID, "--through-revision", fmt.Sprint(revision))
	compactResult := decodeObject(t, compact.stdout)["result"].(map[string]any)
	if compact.exit != 0 || compactResult["directory_synced"] != true || compactResult["removed"] == nil {
		t.Fatalf("compact = exit %d %#v", compact.exit, compactResult)
	}
	backupDestination := filepath.Join(t.TempDir(), "backup")
	backup := run("backup", "--expected-database-id", databaseID, "--expected-revision", fmt.Sprint(revision), "--destination", backupDestination)
	backupResult := decodeObject(t, backup.stdout)["result"].(map[string]any)
	if backup.exit != 0 || backupResult["verified"] != true || backupResult["manifest_checksum"] == "" || backupResult["files"] == nil {
		t.Fatalf("backup = exit %d %#v", backup.exit, backupResult)
	}
	stale := run("create-snapshot", "--expected-database-id", "00000000000000000000000000000000", "--expected-revision", fmt.Sprint(revision))
	staleObject := decodeObject(t, stale.stdout)
	if stale.exit != 5 || staleObject["error"].(map[string]any)["code"] != "ADMIN_PRECONDITION_FAILED" {
		t.Fatalf("stale snapshot = exit %d %#v", stale.exit, staleObject)
	}
	unsafe := run("backup", "--expected-database-id", databaseID, "--expected-revision", fmt.Sprint(revision), "--destination", "relative")
	if unsafe.exit != 2 || decodeObject(t, unsafe.stdout)["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
		t.Fatalf("relative backup = exit %d %s", unsafe.exit, unsafe.stdout)
	}
}

func TestOfflineInspectionProposalAndGuardedApply(t *testing.T) {
	gapctl := buildGapctl(t)
	gapdbd := buildBinary(t, "gapdbd", "./cmd/gapdbd")
	database := filepath.Join(t.TempDir(), "database")
	instance := startDaemon(t, gapdbd, database)
	if seed := runGapctl(t, gapctl, nil, "--socket", instance.socket, "put", "--key", "offline/key", "--value-base64", "dmFsdWU=", "--ack", "durable"); seed.exit != 0 {
		t.Fatalf("seed = exit %d %s", seed.exit, seed.stdout)
	}
	beforeLive := regularTreeDigest(t, database)
	live := runGapctl(t, gapctl, nil, "--db", database, "inspect")
	if live.exit != 4 || decodeObject(t, live.stdout)["error"].(map[string]any)["code"] != "OWNER_EXISTS" || beforeLive != regularTreeDigest(t, database) {
		t.Fatalf("live inspect = exit %d %s", live.exit, live.stdout)
	}
	instance.stop(t)
	healthy := runGapctl(t, gapctl, nil, "--db", database, "inspect")
	if healthy.exit != 0 || decodeObject(t, healthy.stdout)["result"].(map[string]any)["verified"] != true {
		t.Fatalf("healthy inspect = exit %d %s", healthy.exit, healthy.stdout)
	}

	wals, err := filepath.Glob(filepath.Join(database, "wal-*.gdb"))
	if err != nil || len(wals) != 1 {
		t.Fatalf("WAL files = %v, %v", wals, err)
	}
	validWAL, err := os.ReadFile(wals[0])
	if err != nil || len(validWAL) < 2 {
		t.Fatalf("read WAL: %v", err)
	}
	corruptWAL := append([]byte(nil), validWAL...)
	corruptWAL[len(corruptWAL)-1] ^= 0xff
	if err := os.WriteFile(wals[0], corruptWAL, 0o600); err != nil {
		t.Fatal(err)
	}
	proposalOutput := runGapctl(t, gapctl, nil, "--db", database, "recover-propose")
	proposalEnvelope := decodeObject(t, proposalOutput.stdout)
	proposalResult, ok := proposalEnvelope["result"].(map[string]any)
	if proposalOutput.exit != 0 || !ok {
		t.Fatalf("proposal = exit %d %#v", proposalOutput.exit, proposalEnvelope)
	}
	proposal := proposalResult["proposal"].(map[string]any)
	if proposal["id"] == "" || proposal["database_id"] == "" || proposal["manifest_generation"] == nil || proposal["evidence_sha256"] == "" {
		t.Fatalf("proposal = %#v", proposal)
	}
	proposalJSON, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	proposalFile := filepath.Join(t.TempDir(), "proposal.json")
	if err := os.WriteFile(proposalFile, proposalJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	actionID := proposal["id"].(string)
	databaseID := proposal["database_id"].(string)
	generation := fmt.Sprint(int(proposal["manifest_generation"].(float64)))
	destination := t.TempDir()
	applyArguments := []string{"--db", database, "recover-apply", "--proposal-file", proposalFile, "--action-id", actionID, "--expected-database-id", databaseID, "--expected-manifest-generation", generation, "--destination", destination}

	beforeTamper := regularTreeDigest(t, database)
	duplicateProposal := bytes.Replace(proposalJSON, []byte(`"id":`), []byte(`"id":"duplicate","id":`), 1)
	duplicateFile := filepath.Join(t.TempDir(), "duplicate-proposal.json")
	if err := os.WriteFile(duplicateFile, duplicateProposal, 0o600); err != nil {
		t.Fatal(err)
	}
	duplicateArguments := append([]string(nil), applyArguments...)
	for index := range duplicateArguments {
		if duplicateArguments[index] == proposalFile {
			duplicateArguments[index] = duplicateFile
		}
	}
	duplicate := runGapctl(t, gapctl, nil, duplicateArguments...)
	if duplicate.exit != 2 || decodeObject(t, duplicate.stdout)["error"].(map[string]any)["code"] != "INVALID_REQUEST" || beforeTamper != regularTreeDigest(t, database) {
		t.Fatalf("duplicate proposal = exit %d %s", duplicate.exit, duplicate.stdout)
	}
	tampered := append([]string(nil), applyArguments...)
	for index := range tampered {
		if tampered[index] == actionID {
			tampered[index] += "00"
		}
	}
	rejected := runGapctl(t, gapctl, nil, tampered...)
	if rejected.exit != 5 || decodeObject(t, rejected.stdout)["error"].(map[string]any)["code"] != "RECOVERY_ACTION_MISMATCH" || beforeTamper != regularTreeDigest(t, database) {
		t.Fatalf("tampered apply = exit %d %s", rejected.exit, rejected.stdout)
	}
	for name, mutate := range map[string]func(map[string]any){
		"finding": func(value map[string]any) { value["finding_code"] = "CORRUPT_SNAPSHOT" },
		"actions": func(value map[string]any) {
			value["allowed_actions"] = []any{"upgrade_gapdb", "use_compatible_binary", "abort"}
		},
		"unknown_finding": func(value map[string]any) {
			value["finding_code"] = "UNKNOWN_FORMAT"
			value["allowed_actions"] = []any{"upgrade_gapdb", "use_compatible_binary", "abort"}
		},
	} {
		var changed map[string]any
		if err := json.Unmarshal(proposalJSON, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(changed)
		reidentifyRecoveryProposal(t, changed)
		changedJSON, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		changedFile := filepath.Join(t.TempDir(), name+"-proposal.json")
		if err := os.WriteFile(changedFile, changedJSON, 0o600); err != nil {
			t.Fatal(err)
		}
		changedArguments := append([]string(nil), applyArguments...)
		for index := range changedArguments {
			switch changedArguments[index] {
			case proposalFile:
				changedArguments[index] = changedFile
			case actionID:
				changedArguments[index] = changed["id"].(string)
			}
		}
		blocked := runGapctl(t, gapctl, nil, changedArguments...)
		if blocked.exit != 5 || decodeObject(t, blocked.stdout)["error"].(map[string]any)["code"] != "RECOVERY_ACTION_MISMATCH" || beforeTamper != regularTreeDigest(t, database) {
			t.Fatalf("%s-authority apply = exit %d %s", name, blocked.exit, blocked.stdout)
		}
	}

	if err := os.WriteFile(wals[0], validWAL, 0o600); err != nil {
		t.Fatal(err)
	}
	liveOwner := startDaemon(t, gapdbd, database)
	beforeApplyLive := regularTreeDigest(t, database)
	liveApply := runGapctl(t, gapctl, nil, applyArguments...)
	if liveApply.exit != 4 || decodeObject(t, liveApply.stdout)["error"].(map[string]any)["code"] != "OWNER_EXISTS" || beforeApplyLive != regularTreeDigest(t, database) {
		t.Fatalf("live apply = exit %d %s", liveApply.exit, liveApply.stdout)
	}
	liveOwner.stop(t)
	if err := os.WriteFile(wals[0], corruptWAL, 0o600); err != nil {
		t.Fatal(err)
	}
	applied := runGapctl(t, gapctl, nil, applyArguments...)
	appliedResult := decodeObject(t, applied.stdout)["result"].(map[string]any)
	if applied.exit != 0 || appliedResult["proposal_id"] != actionID || appliedResult["operation_applied"] != true || appliedResult["quarantined_path"] == "" {
		t.Fatalf("apply = exit %d %#v", applied.exit, appliedResult)
	}
}

func TestReviewerUnknownFormatCannotGrantOrApplyQuarantine(t *testing.T) {
	gapctl := buildGapctl(t)
	gapdbd := buildBinary(t, "gapdbd", "./cmd/gapdbd")
	database := filepath.Join(t.TempDir(), "database")
	instance := startDaemon(t, gapdbd, database)
	instance.stop(t)
	snapshots, err := filepath.Glob(filepath.Join(database, "snapshot-*.gdb"))
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots = %v, %v", snapshots, err)
	}
	value, err := os.ReadFile(snapshots[0])
	if err != nil || len(value) < 10 {
		t.Fatalf("snapshot = %d bytes, %v", len(value), err)
	}
	value[8], value[9] = 0xff, 0xff
	if err := os.WriteFile(snapshots[0], value, 0o600); err != nil {
		t.Fatal(err)
	}
	before := regularTreeDigest(t, database)
	proposal := runGapctl(t, gapctl, nil, "--db", database, "recover", "propose")
	object := decodeObject(t, proposal.stdout)
	if proposal.exit != 5 || object["operation"] != "offline_recover_propose" || object["error"].(map[string]any)["code"] != "UNKNOWN_FORMAT" || object["result"] != nil {
		t.Fatalf("unknown proposal = exit %d %#v", proposal.exit, object)
	}
	if before != regularTreeDigest(t, database) {
		t.Fatal("unknown-format proposal mutated database")
	}
}

func TestOfflineMachineOperationNamesAndStrictLongFlags(t *testing.T) {
	gapctl := buildGapctl(t)
	for _, test := range []struct {
		args      []string
		operation string
	}{
		{[]string{"--db", "/definitely/not/a/gapdb", "inspect"}, "offline_inspect"},
		{[]string{"--db", "/definitely/not/a/gapdb", "verify"}, "offline_verify"},
		{[]string{"--db", "/definitely/not/a/gapdb", "recover", "propose"}, "offline_recover_propose"},
		{[]string{"--db", "/definitely/not/a/gapdb", "recover", "apply"}, "offline_recover_apply"},
	} {
		result := runGapctl(t, gapctl, nil, test.args...)
		if got := decodeObject(t, result.stdout)["operation"]; got != test.operation {
			t.Fatalf("%v operation = %v, want %s", test.args, got, test.operation)
		}
	}
	for _, args := range [][]string{
		{"-output=json", "schema"},
		{"--output=json", "--output", "json", "schema"},
		{"--socket", "/missing", "get", "-key", "a"},
		{"--socket", "/missing", "get", "--key", "a", "--key=a"},
	} {
		result := runGapctl(t, gapctl, nil, args...)
		if result.exit != 2 || decodeObject(t, result.stdout)["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
			t.Fatalf("strict flags %v = exit %d %s", args, result.exit, result.stdout)
		}
	}
}

func TestParseErrorsAttributeOnlyTheRequestedCommand(t *testing.T) {
	gapctl := buildGapctl(t)
	for _, test := range []struct {
		arguments []string
		operation string
		watch     bool
	}{
		{[]string{"--request-id", "inspect", "--request-id=again", "schema"}, "schema", false},
		{[]string{"--socket=verify", "--socket", "again", "status"}, "status", false},
		{[]string{"--db", "recover-apply", "--db=again", "inspect"}, "offline_inspect", false},
		{[]string{"--output", "watch", "--output=jsonl", "recover", "propose"}, "offline_recover_propose", false},
		{[]string{"--deadline", "verify", "--deadline=1s", "watch", "--after-revision", "0"}, "watch", true},
		{[]string{"--request-id", "inspect", "--unknown-global", "limits"}, "limits", false},
		{[]string{"--request-id", "inspect", "--request-id=again", "--", "recover", "apply"}, "offline_recover_apply", false},
	} {
		result := runGapctl(t, gapctl, nil, test.arguments...)
		if result.exit != 2 || bytes.Count(result.stdout, []byte{'\n'}) != 1 {
			t.Fatalf("%q = exit %d %q", test.arguments, result.exit, result.stdout)
		}
		envelope := decodeObject(t, result.stdout)
		if envelope["schema_version"] != float64(1) || envelope["limits"] == nil || envelope["operation"] != test.operation {
			t.Fatalf("%q = %#v, want operation %q", test.arguments, envelope, test.operation)
		}
		if test.watch {
			if envelope["stream"] != "ended" || envelope["reason"] != "error" {
				t.Fatalf("watch parse error = %#v", envelope)
			}
		} else if envelope["stream"] != nil {
			t.Fatalf("unary parse error has stream: %#v", envelope)
		}
	}
	noDialSocket := filepath.Join(t.TempDir(), "must-not-dial.sock")
	listener, err := net.Listen("unix", noDialSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	unixListener := listener.(*net.UnixListener)
	if err := unixListener.SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	result := runGapctl(t, gapctl, nil, "--socket", noDialSocket, "--request-id", "inspect", "--request-id=again", "status")
	if result.exit != 2 || decodeObject(t, result.stdout)["operation"] != "status" {
		t.Fatalf("no-dial parse error = exit %d %s", result.exit, result.stdout)
	}
	connection, err := unixListener.AcceptUnix()
	if err == nil {
		_ = connection.Close()
		t.Fatal("global parse error dialed the configured socket")
	}
	if netError, ok := err.(net.Error); !ok || !netError.Timeout() {
		t.Fatalf("no-dial probe = %v", err)
	}
}

func TestMalformedSplitGlobalsCannotImpersonateCommand(t *testing.T) {
	gapctl := buildGapctl(t)
	for _, test := range []struct {
		arguments []string
		operation string
		watch     bool
	}{
		{[]string{"-request-id", "inspect", "schema"}, "schema", false},
		{[]string{"---request-id", "inspect", "schema"}, "schema", false},
		{[]string{"----socket", "verify", "status"}, "status", false},
		{[]string{"--------db", "recover-apply", "verify"}, "verify", false},
		{[]string{"-deadline", "watch", "recover", "propose"}, "offline_recover_propose", false},
		{[]string{"---output", "watch", "schema"}, "schema", false},
		{[]string{"-output=watch", "schema"}, "schema", false},
		{[]string{"---output=watch", "watch", "--after-revision", "0"}, "watch", true},
	} {
		result := runGapctl(t, gapctl, nil, test.arguments...)
		if result.exit != 2 || bytes.Count(result.stdout, []byte{'\n'}) != 1 {
			t.Fatalf("%q = exit %d %q", test.arguments, result.exit, result.stdout)
		}
		envelope := decodeObject(t, result.stdout)
		if envelope["operation"] != test.operation || envelope["schema_version"] != float64(1) || envelope["limits"] == nil {
			t.Fatalf("%q = %#v, want operation %q", test.arguments, envelope, test.operation)
		}
		if test.watch {
			if envelope["stream"] != "ended" || envelope["reason"] != "error" {
				t.Fatalf("watch malformed flag = %#v", envelope)
			}
		} else if envelope["stream"] != nil {
			t.Fatalf("unary malformed flag has stream: %#v", envelope)
		}
	}
	noDialSocket := filepath.Join(t.TempDir(), "malformed-must-not-dial.sock")
	listener, err := net.Listen("unix", noDialSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	unixListener := listener.(*net.UnixListener)
	if err := unixListener.SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	result := runGapctl(t, gapctl, nil, "-socket", noDialSocket, "status")
	if result.exit != 2 || decodeObject(t, result.stdout)["operation"] != "status" {
		t.Fatalf("malformed no-dial parse error = exit %d %s", result.exit, result.stdout)
	}
	connection, err := unixListener.AcceptUnix()
	if err == nil {
		_ = connection.Close()
		t.Fatal("malformed global parse error dialed the configured socket")
	}
	if netError, ok := err.(net.Error); !ok || !netError.Timeout() {
		t.Fatalf("malformed no-dial probe = %v", err)
	}
}

func TestEarlyRequestIDRealBinaryParityAndBounds(t *testing.T) {
	gapctl := buildGapctl(t)
	maximum := strings.Repeat("m", 256)
	tooLong := strings.Repeat("m", 257)
	controlBoundary := "\n\t\x7f\u0085" + strings.Repeat("m", 251)
	for _, test := range []struct {
		arguments []string
		want      string
		watch     bool
	}{
		{[]string{"--request-id", "correlation-17", "--unknown", "schema"}, "correlation-17", false},
		{[]string{"--request-id=correlation-17", "--unknown", "watch"}, "correlation-17", true},
		{[]string{"--request-id=", "--unknown", "schema"}, "", false},
		{[]string{"--request-id=x", "--unknown", "watch"}, "x", true},
		{[]string{"--request-id", maximum, "--unknown", "schema"}, maximum, false},
		{[]string{"--request-id", tooLong, "--unknown", "watch"}, "", true},
		{[]string{"--request-id", string([]byte{0xff}), "--unknown", "schema"}, "", false},
		{[]string{"--request-id=first", "--request-id", "second", "schema"}, "", false},
		{[]string{"-request-id", "bad", "watch"}, "", true},
		{[]string{"--request-id", "line\nbreak", "--unknown", "schema"}, "line\nbreak", false},
		{[]string{"--request-id", "tab\tvalue", "--unknown", "watch"}, "tab\tvalue", true},
		{[]string{"--request-id", "bad\x7fvalue", "--unknown", "schema"}, "bad\x7fvalue", false},
		{[]string{"--request-id", "bad\u0085value", "--unknown", "watch"}, "bad\u0085value", true},
		{[]string{"--request-id", "mix\n\t\x7f\u0085value", "--unknown", "schema"}, "mix\n\t\x7f\u0085value", false},
		{[]string{"--request-id", controlBoundary, "--unknown", "watch"}, controlBoundary, true},
		{[]string{"--request-id"}, "", false},
	} {
		result := runGapctl(t, gapctl, nil, test.arguments...)
		if result.exit != 2 || bytes.Count(result.stdout, []byte{'\n'}) != 1 {
			t.Fatalf("%q = exit %d %q", test.arguments, result.exit, result.stdout)
		}
		envelope := decodeObject(t, result.stdout)
		if got, _ := envelope["request_id"].(string); got != test.want {
			t.Fatalf("%q request_id = %q, want %q", test.arguments, got, test.want)
		}
		if envelope["schema_version"] != float64(1) || envelope["limits"] == nil || test.watch != (envelope["stream"] == "ended") {
			t.Fatalf("%q = %#v", test.arguments, envelope)
		}
	}
	noDialSocket := filepath.Join(t.TempDir(), "request-id-must-not-dial.sock")
	listener, err := net.Listen("unix", noDialSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	unixListener := listener.(*net.UnixListener)
	if err := unixListener.SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	result := runGapctl(t, gapctl, nil, "--socket", noDialSocket, "--request-id=correlation-17", "--unknown", "status")
	envelope := decodeObject(t, result.stdout)
	if result.exit != 2 || envelope["operation"] != "status" || envelope["request_id"] != "correlation-17" {
		t.Fatalf("request-id no-dial parse error = exit %d %s", result.exit, result.stdout)
	}
	connection, err := unixListener.AcceptUnix()
	if err == nil {
		_ = connection.Close()
		t.Fatal("early request-ID parse error dialed the configured socket")
	}
	if netError, ok := err.(net.Error); !ok || !netError.Timeout() {
		t.Fatalf("request-ID no-dial probe = %v", err)
	}
}

func TestWatchPreRegistrationFailuresAreSingleTerminalFrames(t *testing.T) {
	gapctl := buildGapctl(t)
	statusSocket := filepath.Join(t.TempDir(), "status-fails.sock")
	listener, err := net.Listen("unix", statusSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
		close(accepted)
	}()
	for _, args := range [][]string{
		{"--output=jsonl", "watch", "--after-revision", "not-a-revision"},
		{"--output=jsonl", "watch", "--after-revision", "4"},
		{"--output=jsonl", "--socket", filepath.Join(t.TempDir(), "missing.sock"), "watch", "--after-revision", "4"},
		{"--output=jsonl", "--deadline", "1s", "--socket", statusSocket, "watch", "--after-revision", "4"},
		{"--output=jsonl", "--socket", "/missing", "watch", "--after-revision", "1", "--after-revision", "2"},
	} {
		result := runGapctl(t, gapctl, nil, args...)
		lines := bytes.Split(bytes.TrimSpace(result.stdout), []byte{'\n'})
		if result.exit == 0 || len(lines) != 1 {
			t.Fatalf("watch failure %v = exit %d lines %d %q", args, result.exit, len(lines), result.stdout)
		}
		frame := decodeObject(t, lines[0])
		if frame["stream"] != "ended" || frame["reason"] != "error" || frame["error"] == nil || frame["limits"] == nil || frame["last_delivered_revision"] == nil {
			t.Fatalf("watch failure %v = %#v", args, frame)
		}
	}
	<-accepted
}

func regularTreeDigest(t *testing.T, directory string) string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		relative, _ := filepath.Rel(directory, path)
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hash.Write([]byte(relative))
		hash.Write([]byte{0})
		hash.Write(value)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func reidentifyRecoveryProposal(t *testing.T, proposal map[string]any) {
	t.Helper()
	actions := make([]string, 0)
	for _, action := range proposal["allowed_actions"].([]any) {
		actions = append(actions, action.(string))
	}
	evidence := struct {
		DatabaseID         string   `json:"database_id"`
		ManifestGeneration uint64   `json:"manifest_generation"`
		Action             string   `json:"action"`
		FindingCode        string   `json:"finding_code"`
		AllowedActions     []string `json:"allowed_actions"`
		File               string   `json:"file"`
		EvidenceSHA256     string   `json:"evidence_sha256"`
		EvidenceSize       int64    `json:"evidence_size"`
		EvidenceDevice     uint64   `json:"evidence_device"`
		EvidenceInode      uint64   `json:"evidence_inode"`
	}{
		proposal["database_id"].(string), uint64(proposal["manifest_generation"].(float64)), proposal["action"].(string), proposal["finding_code"].(string), actions, proposal["file"].(string), proposal["evidence_sha256"].(string), int64(proposal["evidence_size"].(float64)), uint64(proposal["evidence_device"].(float64)), uint64(proposal["evidence_inode"].(float64)),
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	proposal["id"] = fmt.Sprintf("%x", digest[:])
}
