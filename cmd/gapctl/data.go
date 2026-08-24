package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/protocol"
)

func runDataCommand(options globalOptions, command string, arguments []string, stdin io.Reader, stdout io.Writer) int {
	var result any
	var err error
	switch command {
	case "get":
		result, err = runGet(options, arguments)
	case "put", "put-if-absent", "compare-and-swap":
		result, err = runPut(options, command, arguments, stdin)
	case "delete-if-revision":
		result, err = runDelete(options, arguments)
	case "atomic-batch":
		result, err = runBatch(options, arguments, stdin)
	case "scan-prefix":
		result, err = runScan(options, arguments)
	case "watch":
		return runWatch(options, arguments, stdout)
	}
	if err != nil {
		return writeError(stdout, command, options.requestID, err)
	}
	return writeSuccess(stdout, command, options.requestID, result)
}

func runGet(options globalOptions, arguments []string) (any, error) {
	flags := commandFlags("get")
	key := flags.String("key", "", "record key")
	if err := parseCommandFlags(flags, arguments); err != nil {
		return nil, err
	}
	return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
		record, err := client.Get(ctx, *key)
		if err != nil {
			return nil, err
		}
		return struct {
			Record gapdb.Record `json:"record"`
		}{record}, nil
	})
}

func runPut(options globalOptions, command string, arguments []string, stdin io.Reader) (any, error) {
	flags := commandFlags(command)
	key := flags.String("key", "", "record key")
	ackText := flags.String("ack", "", "memory or durable")
	expiryText := flags.String("expires-at", "", "strict UTC RFC3339Nano expiry")
	expectedText := flags.String("expected-revision", "", "required current revision")
	var encoded, file optionalString
	flags.Var(&encoded, "value-base64", "canonical padded base64 value")
	flags.Var(&file, "value-file", "value file")
	fromStdin := flags.Bool("value-stdin", false, "read value bytes from stdin")
	if err := parseCommandFlags(flags, arguments); err != nil {
		return nil, err
	}
	ack, err := parseAck(*ackText)
	if err != nil {
		return nil, err
	}
	value, err := readValue(encoded, file, *fromStdin, stdin, gapdb.DefaultOptions().Limits.MaxValueBytes)
	if err != nil {
		return nil, err
	}
	expiry, err := parseExpiry(*expiryText)
	if err != nil {
		return nil, err
	}
	var expected gapdb.Revision
	if command == "compare-and-swap" {
		expected, err = parseRevision("expected-revision", *expectedText, true)
		if err != nil {
			return nil, err
		}
	} else if *expectedText != "" {
		return nil, invalidRequest("expected-revision", "is valid only for compare-and-swap")
	}
	return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
		switch command {
		case "put":
			return client.Put(ctx, *key, value, expiry, ack)
		case "put-if-absent":
			return client.PutIfAbsent(ctx, *key, value, expiry, ack)
		default:
			return client.CompareAndSwap(ctx, *key, expected, value, expiry, ack)
		}
	})
}

func runDelete(options globalOptions, arguments []string) (any, error) {
	flags := commandFlags("delete-if-revision")
	key := flags.String("key", "", "record key")
	expectedText := flags.String("expected-revision", "", "required current revision")
	ackText := flags.String("ack", "", "memory or durable")
	if err := parseCommandFlags(flags, arguments); err != nil {
		return nil, err
	}
	expected, err := parseRevision("expected-revision", *expectedText, true)
	if err != nil {
		return nil, err
	}
	ack, err := parseAck(*ackText)
	if err != nil {
		return nil, err
	}
	return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
		return client.DeleteIfRevision(ctx, *key, expected, ack)
	})
}

func runBatch(options globalOptions, arguments []string, stdin io.Reader) (any, error) {
	flags := commandFlags("atomic-batch")
	var file optionalString
	flags.Var(&file, "file", "strict batch JSON file")
	fromStdin := flags.Bool("stdin", false, "read strict batch JSON from stdin")
	if err := parseCommandFlags(flags, arguments); err != nil {
		return nil, err
	}
	if file.set == *fromStdin {
		return nil, invalidRequest("batch_source", "select exactly one of --file or --stdin")
	}
	var reader io.Reader = stdin
	if file.set {
		opened, err := os.Open(file.value)
		if err != nil {
			return nil, invalidRequest("file", "cannot open batch file")
		}
		defer opened.Close()
		reader = opened
	}
	maximum := gapdb.DefaultOptions().Limits.MaxFrameBytes
	payload, err := readBounded(reader, maximum, func(received int) error {
		return &gapdb.Error{Code: gapdb.CodeFrameTooLarge, Message: "Batch input exceeds the configured frame limit.", Retry: gapdb.RetryNever, ReceivedBytes: received, MaximumBytes: maximum, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceRequest, gapdb.ActionAbort}}
	})
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, invalidRequest("batch", "batch JSON is empty")
	}
	envelope := append([]byte(`{"schema_version":1,"operation":"atomic_batch","arguments":`), payload...)
	envelope = append(envelope, '}')
	request, err := protocol.DecodeRequest(envelope, gapdb.DefaultOptions().Limits)
	if err != nil {
		return nil, err
	}
	decoded, ok := request.Arguments.(protocol.BatchArguments)
	if !ok {
		return nil, invalidRequest("batch", "batch arguments are invalid")
	}
	batch := gapdb.Batch{Ack: decoded.Ack, Mutations: decoded.Mutations}
	return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
		return client.AtomicBatch(ctx, batch)
	})
}

func runScan(options globalOptions, arguments []string) (any, error) {
	flags := commandFlags("scan-prefix")
	prefix := flags.String("prefix", "", "raw key prefix")
	limit := flags.Int("limit", 0, "page record limit")
	cursor := flags.String("cursor", "", "opaque continuation cursor")
	if err := parseCommandFlags(flags, arguments); err != nil {
		return nil, err
	}
	return callClient(options, func(ctx context.Context, client *gapdb.Client) (any, error) {
		return client.ScanPrefix(ctx, *prefix, *limit, *cursor)
	})
}

func runWatch(options globalOptions, arguments []string, stdout io.Writer) int {
	if options.output != "jsonl" {
		return emitWatchError(stdout, options, "", requestedWatchRevision(arguments), invalidRequest("output", "watch requires --output=jsonl"))
	}
	flags := commandFlags("watch")
	prefix := flags.String("prefix", "", "raw key prefix")
	afterText := flags.String("after-revision", "", "exclusive resume revision")
	if err := parseCommandFlags(flags, arguments); err != nil {
		return emitWatchError(stdout, options, "", requestedWatchRevision(arguments), err)
	}
	afterValue, err := strconv.ParseUint(*afterText, 10, 64)
	if err != nil || *afterText == "" {
		return emitWatchError(stdout, options, "", 0, invalidRequest("after-revision", "must be a base-10 integer including zero"))
	}
	if options.socket == "" || options.database != "" {
		return emitWatchError(stdout, options, "", gapdb.Revision(afterValue), invalidRequest("target", "watch requires --socket and forbids --db"))
	}
	client, err := gapdb.Dial(options.socket, gapdb.ClientOptions{Timeout: options.deadline})
	if err != nil {
		return emitWatchError(stdout, options, "", gapdb.Revision(afterValue), err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), options.deadline)
	defer cancel()
	if options.requestID != "" {
		ctx = gapdb.WithRequestID(ctx, options.requestID)
	}
	status, err := client.Status(ctx)
	if err != nil {
		return emitWatchError(stdout, options, "", gapdb.Revision(afterValue), err)
	}
	buffered := bufio.NewWriter(stdout)
	writeFrame := func(frame watchFrame) bool {
		if err := jsonEncodeLine(buffered, frame); err != nil {
			return false
		}
		return buffered.Flush() == nil
	}
	watch, err := client.Watch(ctx, *prefix, gapdb.Revision(afterValue))
	if err != nil {
		structured := structuredError(err, "watch")
		frame := terminalWatchFrame(options, status.DatabaseID, gapdb.Revision(afterValue), gapdb.WatchEndedByError, structured)
		if !writeFrame(frame) {
			return 4
		}
		return exitCode(structured.Code)
	}
	defer watch.Close()
	registration := watch.RegistrationRevision
	if !writeFrame(watchFrame{SchemaVersion: schemaVersion, OK: true, Operation: "watch", RequestID: options.requestID, DatabaseID: status.DatabaseID, Stream: "started", RegistrationRevision: &registration, Limits: gapdb.DefaultOptions().Limits}) {
		return 4
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	last := gapdb.Revision(afterValue)
	events := watch.Events
	ended := watch.Ended
	for {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			copy := event.Clone()
			if !writeFrame(watchFrame{SchemaVersion: schemaVersion, OK: true, Operation: "watch", RequestID: options.requestID, DatabaseID: status.DatabaseID, Stream: "event", Event: &copy, Limits: gapdb.DefaultOptions().Limits}) {
				return 4
			}
			last = event.Revision
		case termination, ok := <-ended:
			if !ok {
				structured := unavailableWatchTerminal(options.socket, "stream_closed_without_terminal")
				if !writeFrame(terminalWatchFrame(options, status.DatabaseID, last, gapdb.WatchEndedByError, structured)) {
					return 4
				}
				return exitCode(structured.Code)
			}
			return emitWatchTermination(writeFrame, options, status.DatabaseID, termination, last)
		case <-signals:
			select {
			case termination, ok := <-ended:
				if ok && termination.Reason != gapdb.WatchEndedByClient {
					return emitWatchTermination(writeFrame, options, status.DatabaseID, termination, last)
				}
			default:
			}
			watch.Close()
			structured := &gapdb.Error{Code: gapdb.CodeDeadlineExceeded, Message: "The watch was cancelled locally by signal.", Retry: gapdb.RetryAfterReconcile, Operation: "watch", Deadline: time.Now().UTC().Format(time.RFC3339Nano), SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionReconcile, gapdb.ActionAbort}}
			if !writeFrame(terminalWatchFrame(options, status.DatabaseID, last, gapdb.WatchEndedByClient, structured)) {
				return 4
			}
			return exitCode(structured.Code)
		case <-ctx.Done():
			watch.Close()
			structured := &gapdb.Error{Code: gapdb.CodeDeadlineExceeded, Message: "The watch deadline elapsed.", Retry: gapdb.RetryAfterReconcile, Operation: "watch", Deadline: time.Now().UTC().Format(time.RFC3339Nano), SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionReconcile, gapdb.ActionAbort}}
			if !writeFrame(terminalWatchFrame(options, status.DatabaseID, last, gapdb.WatchEndedByClient, structured)) {
				return 4
			}
			return exitCode(structured.Code)
		}
	}
}

func emitWatchError(stdout io.Writer, options globalOptions, databaseID string, last gapdb.Revision, err error) int {
	structured := structuredError(err, "watch")
	frame := terminalWatchFrame(options, databaseID, last, gapdb.WatchEndedByError, structured)
	if encodeErr := jsonEncodeLine(stdout, frame); encodeErr != nil {
		return 6
	}
	return exitCode(structured.Code)
}

type watchFrame struct {
	SchemaVersion        uint16               `json:"schema_version"`
	OK                   bool                 `json:"ok"`
	Operation            string               `json:"operation"`
	RequestID            string               `json:"request_id,omitempty"`
	DatabaseID           string               `json:"database_id"`
	Stream               string               `json:"stream"`
	RegistrationRevision *gapdb.Revision      `json:"registration_revision,omitempty"`
	Event                *gapdb.ChangeEvent   `json:"event,omitempty"`
	Reason               gapdb.WatchEndReason `json:"reason,omitempty"`
	LastDelivered        *gapdb.Revision      `json:"last_delivered_revision,omitempty"`
	Error                *gapdb.Error         `json:"error,omitempty"`
	Limits               gapdb.Limits         `json:"limits"`
}

func jsonEncodeLine(writer io.Writer, value any) error {
	return json.NewEncoder(writer).Encode(value)
}

func terminalWatchFrame(options globalOptions, databaseID string, last gapdb.Revision, reason gapdb.WatchEndReason, err *gapdb.Error) watchFrame {
	return watchFrame{SchemaVersion: schemaVersion, OK: err == nil, Operation: "watch", RequestID: options.requestID, DatabaseID: databaseID, Stream: "ended", Reason: reason, LastDelivered: &last, Error: err, Limits: gapdb.DefaultOptions().Limits}
}

func emitWatchTermination(writeFrame func(watchFrame) bool, options globalOptions, databaseID string, termination gapdb.WatchTermination, fallback gapdb.Revision) int {
	last := termination.LastDeliveredRevision
	if last == 0 && fallback != 0 {
		last = fallback
	}
	structured := termination.Error
	if structured != nil {
		structured = structured.Clone()
	}
	if !writeFrame(terminalWatchFrame(options, databaseID, last, termination.Reason, structured)) {
		return 4
	}
	if structured != nil {
		return exitCode(structured.Code)
	}
	return 0
}

func unavailableWatchTerminal(socket, reason string) *gapdb.Error {
	return &gapdb.Error{Code: gapdb.CodeServerUnavailable, Message: "The watch transport became unavailable.", Retry: gapdb.RetryAfterRestart, SocketPath: socket, ConnectionReason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionStatusOffline, gapdb.ActionStartServer, gapdb.ActionAbort}}
}

func callClient(options globalOptions, call func(context.Context, *gapdb.Client) (any, error)) (any, error) {
	if options.socket == "" {
		return nil, invalidRequest("socket", "is required for online commands")
	}
	if options.database != "" {
		return nil, invalidRequest("target", "--db cannot be combined with an online command")
	}
	client, err := gapdb.Dial(options.socket, gapdb.ClientOptions{Timeout: options.deadline})
	if err != nil {
		return nil, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), options.deadline)
	defer cancel()
	if options.requestID != "" {
		ctx = gapdb.WithRequestID(ctx, options.requestID)
	}
	return call(ctx, client)
}

func commandFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func parseCommandFlags(flags *flag.FlagSet, arguments []string) error {
	if err := flags.Parse(arguments); err != nil {
		return invalidRequest("arguments", err.Error())
	}
	if flags.NArg() != 0 {
		return invalidRequest("arguments", "unexpected positional arguments")
	}
	return nil
}

type optionalString struct {
	value string
	set   bool
}

func (value *optionalString) String() string { return value.value }
func (value *optionalString) Set(text string) error {
	value.value, value.set = text, true
	return nil
}

func readValue(encoded, file optionalString, fromStdin bool, stdin io.Reader, maximum int) ([]byte, error) {
	selected := 0
	if encoded.set {
		selected++
	}
	if file.set {
		selected++
	}
	if fromStdin {
		selected++
	}
	if selected != 1 {
		return nil, invalidRequest("value_source", "select exactly one value source")
	}
	if encoded.set {
		decoded, err := base64.StdEncoding.Strict().DecodeString(encoded.value)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded.value {
			return nil, invalidRequest("value-base64", "must be canonical padded standard base64")
		}
		if len(decoded) > maximum {
			return nil, &gapdb.Error{Code: gapdb.CodeValueTooLarge, Message: "Value exceeds the configured limit.", Retry: gapdb.RetryNever, ReceivedBytes: len(decoded), MaximumBytes: maximum, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceValue, gapdb.ActionAbort}}
		}
		return decoded, nil
	}
	reader := stdin
	if file.set {
		opened, err := os.Open(file.value)
		if err != nil {
			return nil, invalidRequest("value-file", "cannot open value file")
		}
		defer opened.Close()
		reader = opened
	}
	return readBounded(reader, maximum, func(received int) error {
		return &gapdb.Error{Code: gapdb.CodeValueTooLarge, Message: "Selected input exceeds the configured limit.", Retry: gapdb.RetryNever, ReceivedBytes: received, MaximumBytes: maximum, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceValue, gapdb.ActionAbort}}
	})
}

func readBounded(reader io.Reader, maximum int, tooLarge func(int) error) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	if err != nil {
		return nil, invalidRequest("input", "cannot read selected input")
	}
	if len(value) > maximum {
		return nil, tooLarge(len(value))
	}
	return value, nil
}

func parseAck(text string) (gapdb.AckMode, error) {
	ack := gapdb.AckMode(text)
	if !ack.Valid() {
		return "", invalidRequest("ack", "must be memory or durable")
	}
	return ack, nil
}

func parseExpiry(text string) (*time.Time, error) {
	if text == "" {
		return nil, nil
	}
	if !strings.HasSuffix(text, "Z") {
		return nil, invalidRequest("expires-at", "must be UTC RFC3339Nano with a Z suffix")
	}
	value, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || value.UTC().Format(time.RFC3339Nano) != text {
		return nil, invalidRequest("expires-at", "must be canonical UTC RFC3339Nano")
	}
	return &value, nil
}

func parseRevision(field, text string, required bool) (gapdb.Revision, error) {
	if text == "" {
		if required {
			return 0, invalidRequest(field, "is required")
		}
		return 0, nil
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value == 0 {
		return 0, invalidRequest(field, "must be a base-10 integer greater than zero")
	}
	return gapdb.Revision(value), nil
}
