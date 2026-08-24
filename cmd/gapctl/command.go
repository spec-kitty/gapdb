package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spec-kitty/gapdb/gapdb"
)

type globalOptions struct {
	socket    string
	database  string
	deadline  time.Duration
	requestID string
	output    string
}

func execute(arguments []string, stdin io.Reader, stdout io.Writer) int {
	options, command, commandArguments, err := parseGlobal(arguments)
	if err != nil {
		early := lexEarlyCommand(arguments)
		if early.watch {
			return emitWatchError(stdout, early.options, "", requestedWatchRevision(arguments), invalidRequest("arguments", err.Error()))
		}
		return writeError(stdout, early.operation, early.options.requestID, invalidRequest("arguments", err.Error()))
	}
	if command == "watch" {
		return runCommand(options, command, commandArguments, stdin, stdout)
	}
	envelopeCommand := command
	if command == "recover" && len(commandArguments) != 0 && (commandArguments[0] == "propose" || commandArguments[0] == "apply") {
		envelopeCommand = "recover-" + commandArguments[0]
	}
	if options.output != "json" && options.output != "jsonl" {
		return writeError(stdout, envelopeOperation(options, envelopeCommand), options.requestID, invalidRequest("output", "must be json or jsonl"))
	}
	if options.output == "jsonl" && command != "watch" {
		return writeError(stdout, envelopeOperation(options, envelopeCommand), options.requestID, invalidRequest("output", "jsonl is valid only for watch"))
	}
	return runCommand(options, command, commandArguments, stdin, stdout)
}

func envelopeOperation(options globalOptions, command string) string {
	if options.database != "" && (command == "inspect" || command == "verify" || command == "recover-propose" || command == "recover-apply") {
		return offlineOperation(command)
	}
	return command
}

func parseGlobal(arguments []string) (globalOptions, string, []string, error) {
	var options globalOptions
	flags := flag.NewFlagSet("gapctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.socket, "socket", "", "Unix socket path")
	flags.StringVar(&options.database, "db", "", "database directory")
	flags.DurationVar(&options.deadline, "deadline", 30*time.Second, "operation deadline")
	flags.StringVar(&options.requestID, "request-id", "", "caller correlation identifier")
	flags.StringVar(&options.output, "output", "json", "json or jsonl")
	if err := rejectDuplicateFlags(arguments); err != nil {
		return options, "command", nil, err
	}
	if err := flags.Parse(arguments); err != nil {
		return options, "command", nil, err
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return options, "command", nil, fmt.Errorf("a command is required")
	}
	if options.deadline <= 0 {
		return options, remaining[0], nil, fmt.Errorf("deadline must be greater than zero")
	}
	if len(options.requestID) > 256 {
		return options, remaining[0], nil, fmt.Errorf("request ID exceeds 256 bytes")
	}
	return options, remaining[0], remaining[1:], nil
}

func runCommand(options globalOptions, command string, arguments []string, stdin io.Reader, stdout io.Writer) int {
	switch command {
	case "help":
		flags := commandFlags("help")
		text := flags.Bool("text", false, "emit bounded deterministic text help")
		if err := parseCommandFlags(flags, arguments); err != nil {
			return writeError(stdout, command, options.requestID, err)
		}
		if *text {
			return writeTextHelp(stdout)
		}
		return writeSuccess(stdout, command, options.requestID, helpResult())
	case "schema":
		if len(arguments) != 0 {
			return writeError(stdout, command, options.requestID, invalidRequest("arguments", "schema takes no arguments"))
		}
		return writeSuccess(stdout, command, options.requestID, schemaResult())
	case "limits":
		if len(arguments) != 0 {
			return writeError(stdout, command, options.requestID, invalidRequest("arguments", "limits takes no arguments"))
		}
		return writeSuccess(stdout, command, options.requestID, gapdb.DefaultOptions().Limits)
	case "get", "put", "put-if-absent", "compare-and-swap", "delete-if-revision", "atomic-batch", "scan-prefix", "watch":
		return runDataCommand(options, command, arguments, stdin, stdout)
	case "status", "health", "stats", "describe-config", "verify", "create-snapshot", "compact", "backup", "inspect", "recover-propose", "recover-apply":
		return runAdminCommand(options, command, arguments, stdin, stdout)
	case "recover":
		if len(arguments) == 0 || arguments[0] != "propose" && arguments[0] != "apply" {
			return writeError(stdout, "recover", options.requestID, invalidRequest("recover_action", "must be propose or apply"))
		}
		return runAdminCommand(options, "recover-"+arguments[0], arguments[1:], stdin, stdout)
	default:
		return writeError(stdout, command, options.requestID, invalidRequest("command", "unknown command"))
	}
}

func writeTextHelp(writer io.Writer) int {
	limits := gapdb.DefaultOptions().Limits
	document := helpResult()
	if _, err := fmt.Fprintf(writer, "gapctl schema_version=1\ncommands: %s\nglobal_options: %s\nlimits: max_frame_bytes=%d max_value_bytes=%d max_batch_operations=%d max_scan_records=%d watch_buffer_events=%d\n", strings.Join(document.Commands, " "), strings.Join(document.GlobalOptions, " "), limits.MaxFrameBytes, limits.MaxValueBytes, limits.MaxBatchOperations, limits.MaxScanRecords, limits.WatchBufferEvents); err != nil {
		return 6
	}
	return 0
}

func rejectDuplicateFlags(arguments []string) error {
	seen := make(map[string]struct{})
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") {
			return fmt.Errorf("single-dash options are not supported: %s", argument)
		}
		if strings.HasPrefix(argument, "---") {
			return fmt.Errorf("option must use exactly two leading dashes: %s", argument)
		}
		if len(argument) < 3 || argument[:2] != "--" {
			continue
		}
		name := argument[2:]
		for index, value := range name {
			if value == '=' {
				name = name[:index]
				break
			}
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("flag --%s occurs more than once", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

type earlyCommand struct {
	operation           string
	watch               bool
	options             globalOptions
	requestIDCandidate  string
	requestIDExactCount int
	requestIDAmbiguous  bool
}

const maxAttributionFlagDashes = 8

// lexEarlyCommand recovers only the command position needed to attribute a
// global parse error. It mirrors the global flag grammar without validating
// values: known split flags consume exactly one following token, known equals
// flags consume none, and unknown flags never claim a following token. For
// rejected spellings, one through maxAttributionFlagDashes leading dashes are
// recognized only to skip a known flag's value; they never establish target
// authority or change recovered options.
func lexEarlyCommand(arguments []string) earlyCommand {
	result := earlyCommand{operation: "command", options: globalOptions{deadline: 30 * time.Second, output: "json"}}
	databaseTarget := false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			if index+1 < len(arguments) {
				return finalizeEarlyCommand(classifyEarlyCommand(result, arguments[index+1:], databaseTarget))
			}
			return finalizeEarlyCommand(result)
		}
		if strings.HasPrefix(argument, "-") {
			name, value, hasValue, exact, known := attributionGlobalFlag(argument)
			if !known {
				continue
			}
			if name == "--request-id" {
				if exact {
					result.requestIDExactCount++
				} else {
					result.requestIDAmbiguous = true
				}
			}
			if !hasValue {
				if index+1 >= len(arguments) {
					if name == "--request-id" {
						result.requestIDAmbiguous = true
					}
					return finalizeEarlyCommand(result)
				}
				index++
				value = arguments[index]
			}
			if exact {
				switch name {
				case "--socket":
					result.options.socket = value
				case "--db":
					result.options.database = value
					databaseTarget = value != ""
				case "--request-id":
					result.requestIDCandidate = value
				case "--output":
					result.options.output = value
				case "--deadline":
					if parsed, err := time.ParseDuration(value); err == nil {
						result.options.deadline = parsed
					}
				}
			}
			continue
		}
		return finalizeEarlyCommand(classifyEarlyCommand(result, arguments[index:], databaseTarget))
	}
	return finalizeEarlyCommand(result)
}

func finalizeEarlyCommand(result earlyCommand) earlyCommand {
	result.options.requestID = ""
	if result.requestIDExactCount == 1 && !result.requestIDAmbiguous && echoableRequestID(result.requestIDCandidate) {
		result.options.requestID = result.requestIDCandidate
	}
	return result
}

func echoableRequestID(value string) bool {
	return len(value) <= 256 && utf8.ValidString(value)
}

func attributionGlobalFlag(argument string) (name, value string, hasValue, exact, known bool) {
	dashes := 0
	for dashes < len(argument) && argument[dashes] == '-' {
		dashes++
	}
	if dashes == 0 || dashes > maxAttributionFlagDashes || dashes == len(argument) {
		return "", "", false, false, false
	}
	body, value, hasValue := strings.Cut(argument[dashes:], "=")
	name = "--" + body
	return name, value, hasValue, dashes == 2, knownGlobalFlag(name)
}

func knownGlobalFlag(name string) bool {
	switch name {
	case "--socket", "--db", "--deadline", "--request-id", "--output":
		return true
	default:
		return false
	}
}

func classifyEarlyCommand(result earlyCommand, command []string, databaseTarget bool) earlyCommand {
	if len(command) == 0 {
		return result
	}
	name := command[0]
	if name == "recover" && len(command) > 1 {
		switch command[1] {
		case "propose":
			name = "recover-propose"
		case "apply":
			name = "recover-apply"
		}
	}
	if name == "inspect" || name == "recover-propose" || name == "recover-apply" || name == "verify" && databaseTarget {
		result.operation = offlineOperation(name)
	} else {
		result.operation = name
	}
	result.watch = name == "watch"
	return result
}

func requestedWatchRevision(arguments []string) gapdb.Revision {
	for index, argument := range arguments {
		value := ""
		if argument == "--after-revision" && index+1 < len(arguments) {
			value = arguments[index+1]
		} else if strings.HasPrefix(argument, "--after-revision=") {
			value = strings.TrimPrefix(argument, "--after-revision=")
		}
		if parsed, err := strconv.ParseUint(value, 10, 64); err == nil && value != "" {
			return gapdb.Revision(parsed)
		}
	}
	return 0
}

type helpDocument struct {
	Commands      []string `json:"commands"`
	GlobalOptions []string `json:"global_options"`
	ValueSources  []string `json:"value_sources"`
}

func helpResult() helpDocument {
	return helpDocument{
		Commands:      []string{"help", "schema", "limits", "get", "put", "put-if-absent", "compare-and-swap", "delete-if-revision", "atomic-batch", "scan-prefix", "watch", "status", "health", "stats", "describe-config", "verify", "create-snapshot", "compact", "backup", "inspect", "recover propose", "recover apply"},
		GlobalOptions: []string{"--socket", "--db", "--deadline", "--request-id", "--output=json|jsonl"},
		ValueSources:  []string{"--value-base64", "--value-file", "--value-stdin"},
	}
}

type schemaDocument struct {
	Unary       string         `json:"unary"`
	Watch       []string       `json:"watch"`
	ExitClasses map[string]int `json:"exit_classes"`
}

func schemaResult() schemaDocument {
	return schemaDocument{
		Unary: "one JSON object followed by newline",
		Watch: []string{"started", "event", "ended"},
		ExitClasses: map[string]int{
			"success": 0, "request": 2, "state": 3, "availability": 4, "storage": 5, "internal": 6,
		},
	}
}
