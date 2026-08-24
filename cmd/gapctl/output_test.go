package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"gapdb/gapdb"
)

type failRead struct{}

func (failRead) Read([]byte) (int, error) { panic("stdin read during global parse error") }

func TestEarlyParseOperationAttributionSkipsGlobalOptionValues(t *testing.T) {
	commands := []struct {
		arguments []string
		operation string
		watch     bool
	}{
		{[]string{"help"}, "help", false},
		{[]string{"schema"}, "schema", false},
		{[]string{"limits"}, "limits", false},
		{[]string{"get"}, "get", false},
		{[]string{"put"}, "put", false},
		{[]string{"put-if-absent"}, "put-if-absent", false},
		{[]string{"compare-and-swap"}, "compare-and-swap", false},
		{[]string{"delete-if-revision"}, "delete-if-revision", false},
		{[]string{"atomic-batch"}, "atomic-batch", false},
		{[]string{"scan-prefix"}, "scan-prefix", false},
		{[]string{"watch"}, "watch", true},
		{[]string{"status"}, "status", false},
		{[]string{"health"}, "health", false},
		{[]string{"stats"}, "stats", false},
		{[]string{"describe-config"}, "describe-config", false},
		{[]string{"verify"}, "verify", false},
		{[]string{"create-snapshot"}, "create-snapshot", false},
		{[]string{"compact"}, "compact", false},
		{[]string{"backup"}, "backup", false},
		{[]string{"inspect"}, "offline_inspect", false},
		{[]string{"recover-propose"}, "offline_recover_propose", false},
		{[]string{"recover-apply"}, "offline_recover_apply", false},
		{[]string{"recover", "propose"}, "offline_recover_propose", false},
		{[]string{"recover", "apply"}, "offline_recover_apply", false},
	}
	valueNames := make([]string, 0, len(commands))
	for _, command := range commands {
		valueNames = append(valueNames, command.arguments[0])
	}
	globalOptions := []string{"request-id", "socket", "db", "deadline", "output"}
	for _, option := range globalOptions {
		for _, value := range valueNames {
			t.Run(option+"/"+value, func(t *testing.T) {
				assertEarlyParseEnvelope(t, append([]string{"--" + option, value, "--" + option + "=again"}, "schema"), "schema", false)
			})
		}
	}
	for _, command := range commands {
		t.Run("command/"+command.operation, func(t *testing.T) {
			arguments := append([]string{"--request-id", "inspect", "--request-id=again"}, command.arguments...)
			assertEarlyParseEnvelope(t, arguments, command.operation, command.watch)
		})
	}
	assertEarlyParseEnvelope(t, []string{"--request-id", "inspect", "--request-id=again", "--", "schema"}, "schema", false)
	assertEarlyParseEnvelope(t, []string{"--request-id", "inspect", "--unknown", "schema"}, "schema", false)
	assertEarlyParseEnvelope(t, []string{"-request-id=inspect", "schema"}, "schema", false)
	assertEarlyParseEnvelope(t, []string{"---request-id=inspect", "schema"}, "schema", false)
	assertEarlyParseEnvelope(t, []string{"--db", "inspect", "--request-id", "status", "--request-id=again", "verify"}, "offline_verify", false)
	assertEarlyParseEnvelope(t, []string{"--request-id", "inspect", "--request-id=again"}, "command", false)
	assertEarlyParseEnvelope(t, []string{"--request-id"}, "command", false)
}

func TestMalformedKnownGlobalsConsumeSplitValuesForAttributionOnly(t *testing.T) {
	commands := []string{"help", "schema", "limits", "get", "put", "put-if-absent", "compare-and-swap", "delete-if-revision", "atomic-batch", "scan-prefix", "watch", "status", "health", "stats", "describe-config", "verify", "create-snapshot", "compact", "backup", "inspect", "recover-propose", "recover-apply", "recover"}
	for _, dashes := range []string{"-", "---", "----", "--------"} {
		for _, option := range []string{"request-id", "socket", "db", "deadline", "output"} {
			for _, value := range commands {
				t.Run(dashes+option+"/split/"+value, func(t *testing.T) {
					assertEarlyParseEnvelope(t, []string{dashes + option, value, "schema"}, "schema", false)
				})
				t.Run(dashes+option+"/equals/"+value, func(t *testing.T) {
					assertEarlyParseEnvelope(t, []string{dashes + option + "=" + value, "schema"}, "schema", false)
				})
			}
			assertEarlyParseEnvelope(t, []string{dashes + option}, "command", false)
			assertEarlyParseEnvelope(t, []string{dashes + option, "schema"}, "command", false)
		}
	}
	assertEarlyParseEnvelope(t, []string{"-request-id", "--", "schema"}, "schema", false)
	assertEarlyParseEnvelope(t, []string{"schema", "-output", "watch"}, "schema", false)
	assertEarlyParseEnvelope(t, []string{"-unknown", "inspect", "schema"}, "offline_inspect", false)
	assertEarlyParseEnvelope(t, []string{"---unknown", "watch", "schema"}, "watch", true)
	assertEarlyParseEnvelope(t, []string{"-request-id", "inspect", "recover", "propose"}, "offline_recover_propose", false)
	assertEarlyParseEnvelope(t, []string{"---request-id", "inspect", "recover", "apply"}, "offline_recover_apply", false)
}

func TestEarlyRequestIDIsEchoedOnlyWhenCanonicalUnambiguousAndValid(t *testing.T) {
	maximum := strings.Repeat("r", 256)
	tooLong := strings.Repeat("r", 257)
	controlBoundary := "\n\t\x7f\u0085" + strings.Repeat("b", 251)
	for _, test := range []struct {
		name      string
		arguments []string
		want      string
		watch     bool
	}{
		{"split unary", []string{"--request-id", "correlation-17", "--unknown", "schema"}, "correlation-17", false},
		{"equals unary", []string{"--request-id=correlation-17", "--unknown", "schema"}, "correlation-17", false},
		{"command value unary", []string{"--request-id", "inspect", "--unknown", "schema"}, "inspect", false},
		{"split watch", []string{"--request-id", "correlation-17", "--unknown", "watch"}, "correlation-17", true},
		{"equals watch", []string{"--request-id=correlation-17", "--unknown", "watch"}, "correlation-17", true},
		{"empty", []string{"--request-id=", "--unknown", "schema"}, "", false},
		{"one byte", []string{"--request-id=x", "--unknown", "schema"}, "x", false},
		{"maximum", []string{"--request-id", maximum, "--unknown", "schema"}, maximum, false},
		{"too long unary", []string{"--request-id", tooLong, "--unknown", "schema"}, "", false},
		{"too long watch", []string{"--request-id", tooLong, "--unknown", "watch"}, "", true},
		{"invalid utf8", []string{"--request-id", string([]byte{0xff}), "--unknown", "schema"}, "", false},
		{"newline unary", []string{"--request-id", "line\nbreak", "--unknown", "schema"}, "line\nbreak", false},
		{"tab watch", []string{"--request-id", "tab\tvalue", "--unknown", "watch"}, "tab\tvalue", true},
		{"delete unary", []string{"--request-id", "bad\x7fvalue", "--unknown", "schema"}, "bad\x7fvalue", false},
		{"next line watch", []string{"--request-id", "bad\u0085value", "--unknown", "watch"}, "bad\u0085value", true},
		{"mixed controls unary", []string{"--request-id", "mix\n\t\x7f\u0085value", "--unknown", "schema"}, "mix\n\t\x7f\u0085value", false},
		{"control byte boundary watch", []string{"--request-id", controlBoundary, "--unknown", "watch"}, controlBoundary, true},
		{"duplicate split", []string{"--request-id", "first", "--request-id", "second", "schema"}, "", false},
		{"duplicate mixed", []string{"--request-id=first", "--request-id", "second", "watch"}, "", true},
		{"malformed split", []string{"-request-id", "bad", "schema"}, "", false},
		{"malformed equals", []string{"---request-id=bad", "schema"}, "", false},
		{"exact plus malformed", []string{"--request-id=valid", "-request-id", "bad", "schema"}, "", false},
		{"missing", []string{"--request-id"}, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if exit := execute(test.arguments, failRead{}, &stdout); exit != 2 {
				t.Fatalf("execute(%q) exit = %d, output %s", test.arguments, exit, stdout.String())
			}
			var envelope map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("decode %q: %v", stdout.Bytes(), err)
			}
			if bytes.Count(stdout.Bytes(), []byte{'\n'}) != 1 {
				t.Fatalf("execute(%q) emitted multiple physical lines: %q", test.arguments, stdout.Bytes())
			}
			if got, _ := envelope["request_id"].(string); got != test.want {
				t.Fatalf("execute(%q) request_id = %q, want %q", test.arguments, got, test.want)
			}
			if envelope["schema_version"] != float64(1) || envelope["limits"] == nil {
				t.Fatalf("execute(%q) = %#v", test.arguments, envelope)
			}
			if test.watch != (envelope["stream"] == "ended") {
				t.Fatalf("execute(%q) shape = %#v", test.arguments, envelope)
			}
		})
	}
}

func assertEarlyParseEnvelope(t *testing.T, arguments []string, operation string, watch bool) {
	t.Helper()
	var stdout bytes.Buffer
	if exit := execute(arguments, failRead{}, &stdout); exit != 2 {
		t.Fatalf("execute(%q) exit = %d, output %s", arguments, exit, stdout.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout.Bytes(), err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("trailing output %q: %v", stdout.Bytes(), err)
	}
	if envelope["schema_version"] != float64(1) || envelope["limits"] == nil || envelope["operation"] != operation {
		t.Fatalf("execute(%q) = %#v, want operation %q", arguments, envelope, operation)
	}
	errorObject, _ := envelope["error"].(map[string]any)
	if errorObject["code"] != string(gapdb.CodeInvalidRequest) {
		t.Fatalf("execute(%q) error = %#v", arguments, errorObject)
	}
	if watch {
		if envelope["stream"] != "ended" || envelope["reason"] != "error" {
			t.Fatalf("watch parse error = %#v", envelope)
		}
	} else if envelope["stream"] != nil {
		t.Fatalf("unary parse error has stream: %#v", envelope)
	}
}

func TestEveryStableErrorCodeHasExactExitClass(t *testing.T) {
	want := map[gapdb.ErrorCode]int{
		gapdb.CodeInvalidRequest: 2, gapdb.CodeUnsupportedVersion: 2, gapdb.CodeFrameTooLarge: 2, gapdb.CodeKeyTooLarge: 2,
		gapdb.CodeValueTooLarge: 2, gapdb.CodeBatchTooLarge: 2, gapdb.CodeDuplicateKey: 2, gapdb.CodeExpiryNotFuture: 2, gapdb.CodeInvalidCursor: 2,
		gapdb.CodeNotFound: 3, gapdb.CodeAlreadyExists: 3, gapdb.CodeRevisionMismatch: 3, gapdb.CodeConditionFailed: 3,
		gapdb.CodeScanStale: 3, gapdb.CodeRevisionAhead: 3, gapdb.CodeRevisionCompacted: 3, gapdb.CodeWatchLagged: 3,
		gapdb.CodeOwnerExists: 4, gapdb.CodeServerBusy: 4, gapdb.CodeServerShuttingDown: 4, gapdb.CodeDeadlineExceeded: 4, gapdb.CodeServerUnavailable: 4, gapdb.CodePermissionDenied: 4,
		gapdb.CodeStorageDegraded: 5, gapdb.CodeCorruptIdentity: 5, gapdb.CodeCorruptManifest: 5, gapdb.CodeCorruptSnapshot: 5, gapdb.CodeCorruptWAL: 5,
		gapdb.CodeUnknownFormat: 5, gapdb.CodeDatabaseIDMismatch: 5, gapdb.CodeRevisionRangeExhausted: 5, gapdb.CodeIOError: 5,
		gapdb.CodeAdminPreconditionFailed: 5, gapdb.CodeSnapshotInProgress: 5, gapdb.CodeCompactionNotSafe: 5, gapdb.CodeBackupDestinationExists: 5,
		gapdb.CodeBackupInvalid: 5, gapdb.CodeRecoveryRequired: 5, gapdb.CodeRecoveryActionMismatch: 5,
		gapdb.CodeAuditFailedAfterApply: 6, gapdb.CodeInternal: 6,
	}
	definitions := gapdb.ErrorDefinitions()
	if len(want) != len(definitions) {
		t.Fatalf("mapped %d codes, contract defines %d", len(want), len(definitions))
	}
	for _, definition := range definitions {
		if got := exitCode(definition.Code); got != want[definition.Code] {
			t.Errorf("exitCode(%s) = %d, want %d", definition.Code, got, want[definition.Code])
		}
	}
}
