package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spec-kitty/gapdb/gapdb"
)

const SchemaVersion uint64 = 1

type Operation string

const (
	OperationGet                   Operation = "get"
	OperationPut                   Operation = "put"
	OperationPutIfAbsent           Operation = "put_if_absent"
	OperationCompareAndSwap        Operation = "compare_and_swap"
	OperationDeleteIfRevision      Operation = "delete_if_revision"
	OperationAtomicBatch           Operation = "atomic_batch"
	OperationScanPrefix            Operation = "scan_prefix"
	OperationWatch                 Operation = "watch"
	OperationStatus                Operation = "status"
	OperationHealth                Operation = "health"
	OperationStats                 Operation = "stats"
	OperationDescribeConfig        Operation = "describe_config"
	OperationVerify                Operation = "verify"
	OperationCreateSnapshot        Operation = "create_snapshot"
	OperationCompact               Operation = "compact"
	OperationBackup                Operation = "backup"
	OperationOfflineInspect        Operation = "offline_inspect"
	OperationOfflineVerify         Operation = "offline_verify"
	OperationOfflineRecoverPropose Operation = "offline_recover_propose"
	OperationOfflineRecoverApply   Operation = "offline_recover_apply"
)

func (o Operation) Valid() bool {
	switch o {
	case OperationGet, OperationPut, OperationPutIfAbsent, OperationCompareAndSwap,
		OperationDeleteIfRevision, OperationAtomicBatch, OperationScanPrefix,
		OperationWatch, OperationStatus, OperationHealth, OperationStats,
		OperationDescribeConfig, OperationVerify, OperationCreateSnapshot,
		OperationCompact, OperationBackup, OperationOfflineInspect,
		OperationOfflineVerify, OperationOfflineRecoverPropose,
		OperationOfflineRecoverApply:
		return true
	default:
		return false
	}
}

type Request struct {
	SchemaVersion uint64
	RequestID     string
	Operation     Operation
	Arguments     any
}

type GetArguments struct {
	Key string `json:"key"`
}

type PutArguments struct {
	Key       string        `json:"key"`
	Value     Base64Bytes   `json:"value_base64"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
	Ack       gapdb.AckMode `json:"ack"`
}

type CompareAndSwapArguments struct {
	Key              string         `json:"key"`
	ExpectedRevision gapdb.Revision `json:"expected_revision"`
	Value            Base64Bytes    `json:"value_base64"`
	ExpiresAt        *time.Time     `json:"expires_at,omitempty"`
	Ack              gapdb.AckMode  `json:"ack"`
}

type DeleteIfRevisionArguments struct {
	Key              string         `json:"key"`
	ExpectedRevision gapdb.Revision `json:"expected_revision"`
	Ack              gapdb.AckMode  `json:"ack"`
}

type BatchArguments struct {
	Ack       gapdb.AckMode    `json:"ack"`
	Mutations []gapdb.Mutation `json:"mutations"`
}

type ScanArguments struct {
	Prefix string `json:"prefix"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

type WatchArguments struct {
	Prefix        string         `json:"prefix"`
	AfterRevision gapdb.Revision `json:"after_revision"`
}

type VerifyArguments struct {
	Mode string `json:"mode,omitempty"`
}

type SnapshotArguments struct {
	ExpectedDatabaseID string         `json:"expected_database_id"`
	ExpectedRevision   gapdb.Revision `json:"expected_revision"`
}

type CompactArguments struct {
	ExpectedDatabaseID string         `json:"expected_database_id"`
	ThroughRevision    gapdb.Revision `json:"through_revision"`
}

type BackupArguments struct {
	ExpectedDatabaseID string         `json:"expected_database_id"`
	ExpectedRevision   gapdb.Revision `json:"expected_revision"`
	Destination        string         `json:"destination"`
}

type OfflineInspectArguments struct {
	DatabasePath string `json:"database_path"`
}

type OfflineVerifyArguments struct {
	DatabasePath string `json:"database_path"`
	Mode         string `json:"mode,omitempty"`
}

type OfflineRecoverProposeArguments struct {
	DatabasePath string `json:"database_path"`
}

type OfflineRecoverApplyArguments struct {
	DatabasePath               string `json:"database_path"`
	ActionID                   string `json:"action_id"`
	ExpectedDatabaseID         string `json:"expected_database_id"`
	ExpectedManifestGeneration uint64 `json:"expected_manifest_generation"`
	Destination                string `json:"destination"`
}

type EmptyArguments struct{}

// Base64Bytes enforces the protocol's padded standard-base64 spelling. It is
// intentionally internal; public values remain ordinary opaque byte slices.
type Base64Bytes []byte

func (value Base64Bytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString(value))
}

func (value *Base64Bytes) UnmarshalJSON(encoded []byte) error {
	if bytes.Equal(encoded, []byte("null")) {
		return fmt.Errorf("must be a padded standard-base64 string")
	}
	var text string
	if err := json.Unmarshal(encoded, &text); err != nil {
		return fmt.Errorf("must be a padded standard-base64 string: %w", err)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != text {
		return fmt.Errorf("must be canonical padded standard base64")
	}
	*value = decoded
	return nil
}

type StreamKind string

const (
	StreamStarted StreamKind = "started"
	StreamEvent   StreamKind = "event"
	StreamEnded   StreamKind = "ended"
)

type Response struct {
	SchemaVersion        uint64
	OK                   bool
	RequestID            string
	DatabaseID           string
	Operation            Operation
	Result               any
	Stream               StreamKind
	RegistrationRevision gapdb.Revision
	Event                *gapdb.ChangeEvent
	EndReason            gapdb.WatchEndReason
	Error                *gapdb.Error
}

type GetResult struct {
	Record gapdb.Record `json:"record"`
}

func DecodeRequest(payload []byte, limits gapdb.Limits) (Request, error) {
	if len(payload) > limits.MaxFrameBytes {
		return Request{}, frameTooLarge(len(payload), limits.MaxFrameBytes)
	}
	var envelope struct {
		SchemaVersion *uint64         `json:"schema_version"`
		RequestID     string          `json:"request_id,omitempty"`
		Operation     Operation       `json:"operation"`
		Arguments     json.RawMessage `json:"arguments"`
	}
	if err := decodeStrict(payload, &envelope); err != nil {
		return Request{}, invalidProtocol("envelope", err.Error(), err)
	}
	if envelope.SchemaVersion == nil {
		return Request{}, invalidProtocol("schema_version", "is required", nil)
	}
	if *envelope.SchemaVersion != SchemaVersion {
		return Request{}, unsupportedVersion(*envelope.SchemaVersion)
	}
	if len(envelope.RequestID) > 256 {
		return Request{}, invalidProtocol("request_id", "must not exceed 256 bytes", nil)
	}
	if !envelope.Operation.Valid() {
		return Request{}, invalidProtocol("operation", "is not a protocol v1 operation", nil)
	}
	if len(envelope.Arguments) == 0 || bytes.Equal(envelope.Arguments, []byte("null")) {
		return Request{}, invalidProtocol("arguments", "must be an object", nil)
	}
	arguments, err := decodeArguments(envelope.Operation, envelope.Arguments, limits)
	if err != nil {
		return Request{}, err
	}
	return Request{SchemaVersion: *envelope.SchemaVersion, RequestID: envelope.RequestID, Operation: envelope.Operation, Arguments: arguments}, nil
}

func EncodeRequest(request Request, limits gapdb.Limits) ([]byte, error) {
	if request.SchemaVersion != SchemaVersion {
		return nil, unsupportedVersion(request.SchemaVersion)
	}
	if len(request.RequestID) > 256 {
		return nil, invalidProtocol("request_id", "must not exceed 256 bytes", nil)
	}
	if !request.Operation.Valid() {
		return nil, invalidProtocol("operation", "is not a protocol v1 operation", nil)
	}
	if err := validateArgumentsType(request.Operation, request.Arguments); err != nil {
		return nil, err
	}
	arguments := normalizeRequestArguments(request.Arguments)
	envelope := struct {
		SchemaVersion uint64    `json:"schema_version"`
		RequestID     string    `json:"request_id,omitempty"`
		Operation     Operation `json:"operation"`
		Arguments     any       `json:"arguments"`
	}{request.SchemaVersion, request.RequestID, request.Operation, arguments}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, invalidProtocol("arguments", "could not encode", err)
	}
	canonical, err := DecodeRequest(payload, limits)
	if err != nil {
		return nil, err
	}
	_ = canonical
	return payload, nil
}

func EncodeResponse(response Response) ([]byte, error) {
	if response.SchemaVersion != SchemaVersion {
		return nil, unsupportedVersion(response.SchemaVersion)
	}
	if !response.Operation.Valid() {
		return nil, invalidProtocol("operation", "is not a protocol v1 operation", nil)
	}
	if len(response.RequestID) > 256 {
		return nil, invalidProtocol("request_id", "must not exceed 256 bytes", nil)
	}
	if response.Error != nil && response.OK {
		return nil, invalidProtocol("ok", "cannot be true when error is present", nil)
	}
	if response.Error == nil && !response.OK {
		return nil, invalidProtocol("error", "is required when ok is false", nil)
	}
	result := normalizeResult(response.Result)
	var event *gapdb.ChangeEvent
	if response.Event != nil {
		clone := response.Event.Clone()
		event = &clone
	}
	structuredError := response.Error.Clone()
	var registrationRevision *gapdb.Revision
	if response.Stream == StreamStarted {
		value := response.RegistrationRevision
		registrationRevision = &value
	}
	envelope := struct {
		SchemaVersion        uint64               `json:"schema_version"`
		OK                   bool                 `json:"ok"`
		RequestID            string               `json:"request_id,omitempty"`
		DatabaseID           string               `json:"database_id"`
		Operation            Operation            `json:"operation"`
		Result               any                  `json:"result,omitempty"`
		Stream               StreamKind           `json:"stream,omitempty"`
		RegistrationRevision *gapdb.Revision      `json:"registration_revision,omitempty"`
		Event                *gapdb.ChangeEvent   `json:"event,omitempty"`
		EndReason            gapdb.WatchEndReason `json:"reason,omitempty"`
		Error                *gapdb.Error         `json:"error,omitempty"`
	}{
		response.SchemaVersion, response.OK, response.RequestID, response.DatabaseID,
		response.Operation, result, response.Stream, registrationRevision,
		event, response.EndReason, structuredError,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, invalidProtocol("response", "could not encode", err)
	}
	if len(payload) > gapdb.HardMaxFrameBytes {
		return nil, frameTooLarge(len(payload), gapdb.HardMaxFrameBytes)
	}
	if _, err := DecodeResponse(payload, gapdb.HardMaxFrameBytes); err != nil {
		return nil, err
	}
	return payload, nil
}

func DecodeResponse(payload []byte, maximum int) (Response, error) {
	if maximum <= 0 || maximum > gapdb.HardMaxFrameBytes {
		return Response{}, invalidProtocol("maximum_frame_bytes", "is outside the supported range", nil)
	}
	if len(payload) > maximum {
		return Response{}, frameTooLarge(len(payload), maximum)
	}
	var envelope struct {
		SchemaVersion        *uint64              `json:"schema_version"`
		OK                   *bool                `json:"ok"`
		RequestID            string               `json:"request_id,omitempty"`
		DatabaseID           string               `json:"database_id"`
		Operation            Operation            `json:"operation"`
		Result               json.RawMessage      `json:"result,omitempty"`
		Stream               StreamKind           `json:"stream,omitempty"`
		RegistrationRevision *gapdb.Revision      `json:"registration_revision,omitempty"`
		Event                json.RawMessage      `json:"event,omitempty"`
		EndReason            gapdb.WatchEndReason `json:"reason,omitempty"`
		Error                json.RawMessage      `json:"error,omitempty"`
	}
	if err := decodeStrict(payload, &envelope); err != nil {
		return Response{}, invalidProtocol("envelope", err.Error(), err)
	}
	if envelope.SchemaVersion == nil {
		return Response{}, invalidProtocol("schema_version", "is required", nil)
	}
	if *envelope.SchemaVersion != SchemaVersion {
		return Response{}, unsupportedVersion(*envelope.SchemaVersion)
	}
	if envelope.OK == nil {
		return Response{}, invalidProtocol("ok", "is required", nil)
	}
	if len(envelope.RequestID) > 256 {
		return Response{}, invalidProtocol("request_id", "must not exceed 256 bytes", nil)
	}
	if envelope.DatabaseID == "" {
		return Response{}, invalidProtocol("database_id", "is required", nil)
	}
	if !envelope.Operation.Valid() {
		return Response{}, invalidProtocol("operation", "is not a protocol v1 operation", nil)
	}
	hasError := len(envelope.Error) != 0 && !bytes.Equal(envelope.Error, []byte("null"))
	if *envelope.OK && hasError {
		return Response{}, invalidProtocol("error", "must be omitted when ok is true", nil)
	}
	if !*envelope.OK && !hasError {
		return Response{}, invalidProtocol("error", "is required when ok is false", nil)
	}
	var presence map[string]json.RawMessage
	if err := json.Unmarshal(payload, &presence); err != nil {
		return Response{}, invalidProtocol("envelope", "could not inspect field presence", err)
	}
	present := func(name string) bool {
		value, ok := presence[name]
		return ok && !bytes.Equal(value, []byte("null"))
	}
	if envelope.Operation != OperationWatch {
		if present("stream") || present("registration_revision") || present("event") || present("reason") {
			return Response{}, invalidProtocol("stream", "watch fields are invalid for a unary response", nil)
		}
		if *envelope.OK != present("result") {
			return Response{}, invalidProtocol("result", "must be present exactly for successful unary responses", nil)
		}
	} else if present("result") {
		return Response{}, invalidProtocol("result", "must be omitted for watch", nil)
	}
	var structuredError *gapdb.Error
	if hasError {
		var decoded gapdb.Error
		if err := decodeStrict(envelope.Error, &decoded); err != nil {
			return Response{}, invalidProtocol("error", err.Error(), err)
		}
		if err := validateStructuredError(&decoded, envelope.Error); err != nil {
			return Response{}, err
		}
		structuredError = &decoded
	}
	result, err := decodeResult(envelope.Operation, envelope.Result)
	if err != nil {
		return Response{}, err
	}
	var event *gapdb.ChangeEvent
	if len(envelope.Event) != 0 {
		decoded, err := decodeEvent(envelope.Event)
		if err != nil {
			return Response{}, err
		}
		event = &decoded
	}
	if envelope.Stream != "" {
		if envelope.Operation != OperationWatch {
			return Response{}, invalidProtocol("stream", "is valid only for watch", nil)
		}
		switch envelope.Stream {
		case StreamStarted:
			if !*envelope.OK || envelope.RegistrationRevision == nil || event != nil || envelope.EndReason != "" || hasError {
				return Response{}, invalidProtocol("registration_revision", "is required for a started watch", nil)
			}
		case StreamEvent:
			if !*envelope.OK || envelope.RegistrationRevision != nil || event == nil || envelope.EndReason != "" || hasError {
				return Response{}, invalidProtocol("event", "is required for a watch event", nil)
			}
		case StreamEnded:
			if envelope.RegistrationRevision != nil || event != nil || (*envelope.OK && envelope.EndReason != gapdb.WatchEndedByClient && envelope.EndReason != gapdb.WatchEndedByShutdown) || (!*envelope.OK && envelope.EndReason != gapdb.WatchEndedByError) {
				return Response{}, invalidProtocol("reason", "is required for a normal watch end", nil)
			}
		default:
			return Response{}, invalidProtocol("stream", "must be started, event, or ended", nil)
		}
	} else if envelope.Operation == OperationWatch {
		return Response{}, invalidProtocol("stream", "is required for watch", nil)
	}
	return Response{
		SchemaVersion:        *envelope.SchemaVersion,
		OK:                   *envelope.OK,
		RequestID:            envelope.RequestID,
		DatabaseID:           envelope.DatabaseID,
		Operation:            envelope.Operation,
		Result:               result,
		Stream:               envelope.Stream,
		RegistrationRevision: revisionValue(envelope.RegistrationRevision),
		Event:                event,
		EndReason:            envelope.EndReason,
		Error:                structuredError,
	}, nil
}

func decodeResult(operation Operation, raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if err := requireResultFields(operation, raw); err != nil {
		return nil, err
	}
	var destination any
	switch operation {
	case OperationGet:
		var wire struct {
			Record wireRecord `json:"record"`
		}
		if err := decodeStrict(raw, &wire); err != nil {
			return nil, invalidProtocol("result", err.Error(), err)
		}
		record, err := wire.Record.public()
		if err != nil {
			return nil, err
		}
		return GetResult{Record: record}, nil
	case OperationPut, OperationPutIfAbsent, OperationCompareAndSwap,
		OperationDeleteIfRevision, OperationAtomicBatch:
		destination = &gapdb.MutationResult{}
	case OperationScanPrefix:
		var wire struct {
			ObservedRevision gapdb.Revision `json:"observed_revision"`
			AsOf             UTCInstant     `json:"as_of"`
			Records          []wireRecord   `json:"records"`
			Cursor           string         `json:"cursor,omitempty"`
			Truncated        bool           `json:"truncated"`
		}
		if err := decodeStrict(raw, &wire); err != nil {
			return nil, invalidProtocol("result", err.Error(), err)
		}
		page := gapdb.ScanPage{ObservedRevision: wire.ObservedRevision, AsOf: time.Time(wire.AsOf).UTC(), Cursor: wire.Cursor, Truncated: wire.Truncated, Records: make([]gapdb.Record, len(wire.Records))}
		for i, encoded := range wire.Records {
			record, err := encoded.public()
			if err != nil {
				return nil, err
			}
			if i > 0 && page.Records[i-1].Key >= record.Key {
				return nil, invalidProtocol("result.records", "must be strictly key-sorted", nil)
			}
			page.Records[i] = record
		}
		if page.Truncated != (page.Cursor != "") {
			return nil, invalidProtocol("result.cursor", "must be present exactly when truncated is true", nil)
		}
		return page, nil
	case OperationStatus:
		destination = &gapdb.StatusResult{}
	case OperationHealth:
		destination = &gapdb.HealthResult{}
	case OperationStats:
		destination = &gapdb.StatsResult{}
	case OperationDescribeConfig:
		destination = &gapdb.ConfigResult{}
	case OperationVerify:
		destination = &gapdb.VerifyResult{}
	case OperationCreateSnapshot:
		destination = &gapdb.SnapshotResult{}
	case OperationCompact:
		destination = &gapdb.CompactionResult{}
	case OperationBackup:
		destination = &gapdb.BackupResult{}
	default:
		var generic any
		if err := decodeStrict(raw, &generic); err != nil {
			return nil, invalidProtocol("result", err.Error(), err)
		}
		return json.RawMessage(append([]byte(nil), raw...)), nil
	}
	if err := decodeStrict(raw, destination); err != nil {
		return nil, invalidProtocol("result", err.Error(), err)
	}
	switch value := destination.(type) {
	case *gapdb.MutationResult:
		if value.Revision == 0 || !value.Ack.Valid() || value.DurableThroughRevision > value.Revision {
			return nil, invalidProtocol("result", "contains an invalid mutation acknowledgement", nil)
		}
		if value.Ack == gapdb.AckDurable && value.DurableThroughRevision < value.Revision {
			return nil, invalidProtocol("result.durable_through_revision", "must cover a durable acknowledgement", nil)
		}
		return *value, nil
	case *gapdb.StatusResult:
		if value.DatabaseID == "" || value.OwnerPID <= 0 || value.OwnerStartedAt == "" || (gapdb.Options{Limits: value.Limits}).Validate() != nil {
			return nil, invalidProtocol("result", "contains invalid status evidence", nil)
		}
		return *value, nil
	case *gapdb.HealthResult:
		if value.FailingSubsystems == nil || value.SafeActions == nil {
			return nil, invalidProtocol("result", "contains invalid health evidence", nil)
		}
		return *value, nil
	case *gapdb.StatsResult:
		if value.SchemaVersion != 1 || value.LiveRecords < 0 || value.Watchers < 0 || value.Clients < 0 || value.QueueDepth < 0 {
			return nil, invalidProtocol("result", "contains invalid statistics", nil)
		}
		return *value, nil
	case *gapdb.ConfigResult:
		if value.Source != "default" && value.Source != "file" && value.Source != "flag" || (gapdb.Options{Limits: value.Effective.Limits}).Validate() != nil || value.SafeCeilings.MaxFrameBytes != gapdb.HardMaxFrameBytes {
			return nil, invalidProtocol("result", "contains invalid effective configuration", nil)
		}
		return *value, nil
	case *gapdb.VerifyResult:
		if value.Mode != "sampled" && value.Mode != "full" || !value.Verified || value.DatabaseID == "" || value.ManifestGeneration == 0 || value.CheckedFiles == nil {
			return nil, invalidProtocol("result", "contains invalid verification evidence", nil)
		}
		return *value, nil
	case *gapdb.SnapshotResult:
		if value.Filename == "" || value.Checksum == "" || value.Revision == ^gapdb.Revision(0) || value.NewWALStart != value.Revision+1 || value.Duration < 0 {
			return nil, invalidProtocol("result", "contains invalid snapshot evidence", nil)
		}
		return *value, nil
	case *gapdb.CompactionResult:
		if value.Removed == nil || value.RemovedCount < len(value.Removed) || value.SkippedCount < 0 || !value.DirectorySynced {
			return nil, invalidProtocol("result", "contains invalid compaction evidence", nil)
		}
		return *value, nil
	case *gapdb.BackupResult:
		if value.DatabaseID == "" || value.ManifestChecksum == "" || value.Files == nil || value.ByteCount < 0 || !value.Verified {
			return nil, invalidProtocol("result", "contains invalid backup evidence", nil)
		}
		var total int64
		for index, file := range value.Files {
			if file.Name == "" || file.Size < 0 || file.SHA256 == "" || index > 0 && value.Files[index-1].Name >= file.Name {
				return nil, invalidProtocol("result.files", "must be a sorted complete evidence list", nil)
			}
			total += file.Size
		}
		if total != value.ByteCount {
			return nil, invalidProtocol("result.byte_count", "must equal the file byte count", nil)
		}
		return *value, nil
	default:
		return nil, invalidProtocol("result", "unsupported result type", nil)
	}
}

func requireResultFields(operation Operation, raw []byte) error {
	required := map[Operation][]string{
		OperationGet: {"record"},
		OperationPut: {"revision", "ack", "durable_through_revision"}, OperationPutIfAbsent: {"revision", "ack", "durable_through_revision"}, OperationCompareAndSwap: {"revision", "ack", "durable_through_revision"}, OperationDeleteIfRevision: {"revision", "ack", "durable_through_revision"}, OperationAtomicBatch: {"revision", "ack", "durable_through_revision", "mutation_count"},
		OperationScanPrefix:     {"observed_revision", "as_of", "records", "truncated"},
		OperationStatus:         {"lifecycle", "database_id", "current_revision", "durable_through_revision", "reserved_revision_end", "snapshot_revision", "active_wal_start", "earliest_watch_revision", "record_count", "watch_count", "queue_depth", "active_clients", "limits", "owner_pid", "owner_started_at", "snapshot_in_progress", "backup_in_progress"},
		OperationHealth:         {"lifecycle", "healthy", "failing_subsystems", "safe_actions"},
		OperationStats:          {"schema_version", "live_records", "expired_records", "approximate_value_bytes", "commits", "condition_failures", "wal_bytes", "sync_count", "sync_errors", "watchers", "lagged_watches", "clients", "queue_depth", "snapshot_duration_nanos", "recovery_duration_nanos"},
		OperationDescribeConfig: {"effective", "source", "safe_ceilings", "restart_required"},
		OperationVerify:         {"mode", "verified", "database_id", "manifest_generation", "current_revision", "snapshot_revision", "active_wal_start", "checked_files"},
		OperationCreateSnapshot: {"revision", "filename", "checksum", "new_wal_start", "duration"},
		OperationCompact:        {"before_revision", "after_revision", "removed", "removed_count", "skipped_count", "paths_truncated", "directory_synced"},
		OperationBackup:         {"database_id", "revision", "manifest_checksum", "files", "byte_count", "verified"},
	}[operation]
	if len(required) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return invalidProtocol("result", "must be an object", err)
	}
	for _, name := range required {
		if _, present := object[name]; !present {
			return invalidProtocol("result."+name, "is required", nil)
		}
	}
	return nil
}

type wireRecord struct {
	Key       string         `json:"key"`
	Value     Base64Bytes    `json:"value_base64"`
	Revision  gapdb.Revision `json:"revision"`
	ExpiresAt *UTCInstant    `json:"expires_at,omitempty"`
}

func (record *wireRecord) UnmarshalJSON(encoded []byte) error {
	type plain wireRecord
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return err
	}
	for _, name := range []string{"key", "value_base64", "revision"} {
		if _, present := object[name]; !present {
			return fmt.Errorf("record field %q is required", name)
		}
	}
	var decoded plain
	if err := decodeStrict(encoded, &decoded); err != nil {
		return err
	}
	*record = wireRecord(decoded)
	return nil
}

func (record wireRecord) public() (gapdb.Record, error) {
	limits := gapdb.DefaultOptions().Limits
	limits.MaxKeyBytes = gapdb.HardMaxKeyBytes
	limits.MaxValueBytes = gapdb.HardMaxValueBytes
	expiresAt := record.ExpiresAt.timePointer()
	mutation := gapdb.NewPutMutation(record.Key, record.Value, gapdb.Condition{Kind: gapdb.ConditionAny}, expiresAt)
	if err := mutation.Validate(limits); err != nil {
		return gapdb.Record{}, err
	}
	if record.Revision == 0 {
		return gapdb.Record{}, invalidProtocol("record.revision", "must be greater than zero", nil)
	}
	return gapdb.NewRecord(record.Key, record.Value, record.Revision, expiresAt), nil
}

type UTCInstant time.Time

func (value UTCInstant) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(value).UTC().Format(time.RFC3339Nano))
}

func (value *UTCInstant) UnmarshalJSON(encoded []byte) error {
	var text string
	if err := json.Unmarshal(encoded, &text); err != nil {
		return fmt.Errorf("must be an RFC3339Nano UTC string: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || !strings.HasSuffix(text, "Z") || parsed.UTC().Format(time.RFC3339Nano) != text {
		return fmt.Errorf("must be canonical RFC3339Nano UTC with a Z suffix")
	}
	*value = UTCInstant(parsed.UTC())
	return nil
}

func (value *UTCInstant) timePointer() *time.Time {
	if value == nil {
		return nil
	}
	result := time.Time(*value).UTC()
	return &result
}

func decodeEvent(raw []byte) (gapdb.ChangeEvent, error) {
	var wire struct {
		Revision gapdb.Revision   `json:"revision"`
		Order    uint32           `json:"order"`
		Kind     gapdb.ChangeKind `json:"kind"`
		Key      string           `json:"key"`
		Record   *wireRecord      `json:"record,omitempty"`
	}
	if err := decodeStrict(raw, &wire); err != nil {
		return gapdb.ChangeEvent{}, invalidProtocol("event", err.Error(), err)
	}
	if wire.Revision == 0 {
		return gapdb.ChangeEvent{}, invalidProtocol("event.revision", "must be greater than zero", nil)
	}
	event := gapdb.ChangeEvent{Revision: wire.Revision, Order: wire.Order, Kind: wire.Kind, Key: wire.Key}
	switch wire.Kind {
	case gapdb.ChangePut:
		if wire.Record == nil {
			return gapdb.ChangeEvent{}, invalidProtocol("event.record", "is required for put", nil)
		}
		record, err := wire.Record.public()
		if err != nil {
			return gapdb.ChangeEvent{}, err
		}
		if record.Key != wire.Key || record.Revision != wire.Revision {
			return gapdb.ChangeEvent{}, invalidProtocol("event.record", "must match the event key and revision", nil)
		}
		event.Record = &record
	case gapdb.ChangeDelete, gapdb.ChangeExpire:
		if wire.Record != nil {
			return gapdb.ChangeEvent{}, invalidProtocol("event.record", "must be omitted for delete and expire", nil)
		}
	default:
		return gapdb.ChangeEvent{}, invalidProtocol("event.kind", "must be put, delete, or expire", nil)
	}
	return event, nil
}

type errorEvidenceSchema struct {
	required    []string
	requiredAny [][]string
	allowed     map[string]struct{}
}

func validateStructuredError(value *gapdb.Error, raw []byte) error {
	if value.Message == "" {
		return invalidProtocol("error.message", "must not be empty", nil)
	}
	var definition *gapdb.ErrorDefinition
	for _, candidate := range gapdb.ErrorDefinitions() {
		if candidate.Code == value.Code {
			copy := candidate
			definition = &copy
			break
		}
	}
	if definition == nil {
		return invalidProtocol("error.code", "is not a protocol v1 error code", nil)
	}
	if definition.Retry != value.Retry {
		return invalidProtocol("error.retry", "does not match the stable code", nil)
	}
	if !equalSafeActions(value.SafeActions, definition.SafeActions) {
		return invalidProtocol("error.safe_actions", "must exactly match the stable code-specific actions", nil)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return invalidProtocol("error", "could not inspect evidence", err)
	}
	schema, ok := errorSchema(value.Code)
	if !ok {
		return invalidProtocol("error.code", "has no evidence schema", nil)
	}
	base := map[string]struct{}{"code": {}, "message": {}, "retry": {}, "safe_actions": {}}
	for name, encoded := range object {
		if _, ok := base[name]; ok {
			continue
		}
		if _, ok := schema.allowed[name]; !ok {
			return invalidProtocol("error."+name, "is not relevant to this error code", nil)
		}
		if !meaningfulEvidence(encoded) {
			return invalidProtocol("error."+name, "must be omitted when evidence is absent", nil)
		}
	}
	for _, name := range schema.required {
		if !meaningfulEvidence(object[name]) {
			return invalidProtocol("error."+name, "is required for this error code", nil)
		}
	}
	for _, alternatives := range schema.requiredAny {
		found := false
		for _, name := range alternatives {
			found = found || meaningfulEvidence(object[name])
		}
		if !found {
			return invalidProtocol("error", "is missing required code-specific evidence", nil)
		}
	}
	if encoded, present := object["operation_applied"]; present {
		var applied bool
		if json.Unmarshal(encoded, &applied) != nil || !applied {
			return invalidProtocol("error.operation_applied", "must be true when present", nil)
		}
	}
	if err := validateAlternativeEvidence(value.Code, object); err != nil {
		return err
	}
	return nil
}

func equalSafeActions(got, want []gapdb.SafeAction) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func evidenceSchema(required, optional []string, requiredAny ...[]string) errorEvidenceSchema {
	allowed := make(map[string]struct{}, len(required)+len(optional)+1)
	// The v1 error contract makes prior application evidence global: any
	// code-specific failure may follow an operation that already applied.
	// Validation below still requires the field to be true when present.
	allowed["operation_applied"] = struct{}{}
	for _, name := range append(append([]string(nil), required...), optional...) {
		allowed[name] = struct{}{}
	}
	for _, group := range requiredAny {
		for _, name := range group {
			allowed[name] = struct{}{}
		}
	}
	return errorEvidenceSchema{required: required, requiredAny: requiredAny, allowed: allowed}
}

func errorSchema(code gapdb.ErrorCode) (errorEvidenceSchema, bool) {
	var schema errorEvidenceSchema
	switch code {
	case gapdb.CodeInvalidRequest:
		schema = evidenceSchema([]string{"reason"}, nil, []string{"field", "path"})
	case gapdb.CodeUnsupportedVersion:
		schema = evidenceSchema([]string{"received_version", "supported_versions"}, nil)
	case gapdb.CodeFrameTooLarge:
		schema = evidenceSchema([]string{"received_bytes", "maximum_bytes"}, nil)
	case gapdb.CodeKeyTooLarge, gapdb.CodeValueTooLarge:
		schema = evidenceSchema([]string{"received_bytes", "maximum_bytes"}, []string{"field"})
	case gapdb.CodeBatchTooLarge:
		schema = evidenceSchema(nil, []string{"received_operations", "maximum_operations", "received_bytes", "maximum_bytes"})
	case gapdb.CodeDuplicateKey:
		schema = evidenceSchema([]string{"key", "mutation_indexes"}, []string{"mutation_index"})
	case gapdb.CodeExpiryNotFuture:
		schema = evidenceSchema([]string{"supplied_expiry", "effective_time"}, nil)
	case gapdb.CodeInvalidCursor:
		schema = evidenceSchema([]string{"reason"}, nil)
	case gapdb.CodeNotFound:
		schema = evidenceSchema([]string{"key", "current_revision"}, nil)
	case gapdb.CodeAlreadyExists:
		schema = evidenceSchema([]string{"key", "actual_revision"}, nil)
	case gapdb.CodeRevisionMismatch:
		schema = evidenceSchema([]string{"key", "expected_revision", "actual_revision"}, nil)
	case gapdb.CodeConditionFailed:
		schema = evidenceSchema([]string{"mutation_index", "key", "condition"}, nil, []string{"actual_state", "actual_revision"})
	case gapdb.CodeScanStale:
		schema = evidenceSchema([]string{"cursor_revision", "current_revision"}, nil)
	case gapdb.CodeRevisionAhead:
		schema = evidenceSchema([]string{"requested_revision", "current_revision"}, nil)
	case gapdb.CodeRevisionCompacted:
		schema = evidenceSchema([]string{"requested_revision", "earliest_available_revision"}, nil)
	case gapdb.CodeWatchLagged:
		schema = evidenceSchema([]string{"last_delivered_revision", "current_revision"}, nil)
	case gapdb.CodeOwnerExists:
		schema = evidenceSchema([]string{"path"}, nil)
	case gapdb.CodeServerBusy:
		schema = evidenceSchema([]string{"active_clients", "maximum_clients", "queue_depth"}, nil)
	case gapdb.CodeServerShuttingDown:
		schema = evidenceSchema([]string{"lifecycle"}, nil)
	case gapdb.CodeDeadlineExceeded:
		schema = evidenceSchema([]string{"operation", "deadline"}, []string{"operation_applied"})
	case gapdb.CodeServerUnavailable:
		schema = evidenceSchema([]string{"socket_path", "connection_reason"}, nil)
	case gapdb.CodePermissionDenied:
		schema = evidenceSchema([]string{"path", "operation"}, nil)
	case gapdb.CodeStorageDegraded:
		schema = evidenceSchema([]string{"failed_stage", "current_revision", "durable_through_revision"}, nil)
	case gapdb.CodeCorruptIdentity:
		schema = evidenceSchema([]string{"file"}, []string{"expected_database_id"}, []string{"offset", "reason"})
	case gapdb.CodeCorruptManifest:
		schema = evidenceSchema([]string{"file", "reason"}, nil)
	case gapdb.CodeCorruptSnapshot:
		schema = evidenceSchema([]string{"file", "offset", "reason"}, nil)
	case gapdb.CodeCorruptWAL:
		schema = evidenceSchema([]string{"file", "offset", "reason"}, []string{"actual_revision"})
	case gapdb.CodeUnknownFormat:
		schema = evidenceSchema([]string{"file", "received_version", "supported_versions"}, nil)
	case gapdb.CodeDatabaseIDMismatch:
		schema = evidenceSchema([]string{"expected_database_id", "actual_database_id"}, nil, []string{"file", "operation"})
	case gapdb.CodeRevisionRangeExhausted:
		schema = evidenceSchema([]string{"reserved_revision_end", "reason"}, nil)
	case gapdb.CodeIOError:
		schema = evidenceSchema([]string{"operation", "path", "os_error_category"}, []string{"operation_applied"})
	case gapdb.CodeAdminPreconditionFailed:
		schema = evidenceSchema(nil, []string{"expected_database_id", "actual_database_id", "expected_revision", "actual_revision", "expected_manifest_generation", "actual_manifest_generation"})
	case gapdb.CodeSnapshotInProgress:
		schema = evidenceSchema([]string{"operation_id", "started_at"}, nil)
	case gapdb.CodeCompactionNotSafe:
		schema = evidenceSchema([]string{"through_revision", "active_snapshot_revision"}, nil)
	case gapdb.CodeBackupDestinationExists:
		schema = evidenceSchema([]string{"destination"}, nil)
	case gapdb.CodeBackupInvalid:
		schema = evidenceSchema([]string{"backup_path"}, nil, []string{"file", "reason"})
	case gapdb.CodeRecoveryRequired:
		schema = evidenceSchema([]string{"detected_corruption", "proposal_available"}, nil)
	case gapdb.CodeRecoveryActionMismatch:
		schema = evidenceSchema([]string{"expected_database_id", "actual_database_id", "expected_manifest_generation", "actual_manifest_generation", "proposal_id"}, nil)
	case gapdb.CodeAuditFailedAfterApply:
		schema = evidenceSchema([]string{"operation", "reason", "current_revision", "operation_applied"}, nil)
	case gapdb.CodeInternal:
		schema = evidenceSchema([]string{"correlation_id"}, []string{"lifecycle", "reason", "operation_applied"})
	default:
		return errorEvidenceSchema{}, false
	}
	return schema, true
}

func meaningfulEvidence(encoded json.RawMessage) bool {
	if len(encoded) == 0 || bytes.Equal(encoded, []byte("null")) {
		return false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return typed != ""
	case []any:
		return len(typed) != 0
	default:
		return true
	}
}

func validateAlternativeEvidence(code gapdb.ErrorCode, object map[string]json.RawMessage) error {
	pair := func(left, right string) bool {
		return meaningfulEvidence(object[left]) && meaningfulEvidence(object[right])
	}
	switch code {
	case gapdb.CodeBatchTooLarge:
		if !pair("received_operations", "maximum_operations") && !pair("received_bytes", "maximum_bytes") {
			return invalidProtocol("error", "batch evidence must provide an operations or bytes pair", nil)
		}
	case gapdb.CodeAdminPreconditionFailed:
		if !pair("expected_database_id", "actual_database_id") && !pair("expected_revision", "actual_revision") && !pair("expected_manifest_generation", "actual_manifest_generation") {
			return invalidProtocol("error", "admin evidence must provide one complete expected/actual pair", nil)
		}
	}
	return nil
}

func decodeArguments(operation Operation, raw []byte, limits gapdb.Limits) (any, error) {
	decode := func(destination any) error {
		if err := decodeStrict(raw, destination); err != nil {
			return invalidProtocol("arguments", err.Error(), err)
		}
		return nil
	}
	validateMutation := func(mutation gapdb.Mutation) error {
		if err := mutation.Validate(limits); err != nil {
			return err
		}
		return nil
	}

	switch operation {
	case OperationGet:
		var value GetArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateMutation(gapdb.NewPutMutation(value.Key, nil, gapdb.Condition{Kind: gapdb.ConditionAny}, nil)); err != nil {
			return nil, err
		}
		return value, nil
	case OperationPut, OperationPutIfAbsent:
		var value PutArguments
		if !objectHasField(raw, "value_base64") {
			return nil, invalidProtocol("arguments.value_base64", "is required", nil)
		}
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateOptionalUTCTimeField(raw, "expires_at"); err != nil {
			return nil, err
		}
		ack, err := decodeAcknowledgement(raw)
		if err != nil {
			return nil, err
		}
		value.Ack = ack
		condition := gapdb.Condition{Kind: gapdb.ConditionAny}
		if operation == OperationPutIfAbsent {
			condition.Kind = gapdb.ConditionAbsent
		}
		if !value.Ack.Valid() {
			return nil, invalidProtocol("arguments.ack", "must be memory or durable", nil)
		}
		if err := validateMutation(gapdb.NewPutMutation(value.Key, value.Value, condition, value.ExpiresAt)); err != nil {
			return nil, err
		}
		return value, nil
	case OperationCompareAndSwap:
		var value CompareAndSwapArguments
		if !objectHasField(raw, "value_base64") {
			return nil, invalidProtocol("arguments.value_base64", "is required", nil)
		}
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateOptionalUTCTimeField(raw, "expires_at"); err != nil {
			return nil, err
		}
		ack, err := decodeAcknowledgement(raw)
		if err != nil {
			return nil, err
		}
		value.Ack = ack
		if !value.Ack.Valid() {
			return nil, invalidProtocol("arguments.ack", "must be memory or durable", nil)
		}
		mutation := gapdb.NewPutMutation(value.Key, value.Value, gapdb.Condition{Kind: gapdb.ConditionRevision, ExpectedRevision: value.ExpectedRevision}, value.ExpiresAt)
		if err := validateMutation(mutation); err != nil {
			return nil, err
		}
		return value, nil
	case OperationDeleteIfRevision:
		var value DeleteIfRevisionArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		ack, err := decodeAcknowledgement(raw)
		if err != nil {
			return nil, err
		}
		value.Ack = ack
		if !value.Ack.Valid() {
			return nil, invalidProtocol("arguments.ack", "must be memory or durable", nil)
		}
		if err := validateMutation(gapdb.NewDeleteMutation(value.Key, value.ExpectedRevision)); err != nil {
			return nil, err
		}
		return value, nil
	case OperationAtomicBatch:
		var value BatchArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateBatchWire(raw); err != nil {
			return nil, err
		}
		ack, err := decodeAcknowledgement(raw)
		if err != nil {
			return nil, err
		}
		value.Ack = ack
		batch := gapdb.Batch{Ack: value.Ack, Mutations: value.Mutations}
		if err := batch.Validate(limits); err != nil {
			return nil, err
		}
		if len(raw) > limits.MaxBatchBytes {
			return nil, &gapdb.Error{Code: gapdb.CodeBatchTooLarge, Message: "Batch exceeds the configured byte limit.", Retry: gapdb.RetryNever, ReceivedBytes: len(raw), MaximumBytes: limits.MaxBatchBytes, SafeActions: []gapdb.SafeAction{gapdb.ActionSplitBatch, gapdb.ActionAbort}}
		}
		return value, nil
	case OperationScanPrefix:
		var value ScanArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if !utf8.ValidString(value.Prefix) || len(value.Prefix) > limits.MaxKeyBytes {
			return nil, invalidProtocol("arguments.prefix", "must be valid UTF-8 within the key limit", nil)
		}
		if value.Limit <= 0 || value.Limit > limits.MaxScanRecords {
			return nil, invalidProtocol("arguments.limit", "is outside the configured scan limit", nil)
		}
		return value, nil
	case OperationWatch:
		var value WatchArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if !utf8.ValidString(value.Prefix) || len(value.Prefix) > limits.MaxKeyBytes {
			return nil, invalidProtocol("arguments.prefix", "must be valid UTF-8 within the key limit", nil)
		}
		return value, nil
	case OperationVerify:
		var value VerifyArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if value.Mode != "" && value.Mode != "sampled" && value.Mode != "full" {
			return nil, invalidProtocol("arguments.mode", "must be sampled or full", nil)
		}
		return value, nil
	case OperationOfflineInspect:
		var value OfflineInspectArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateAbsolutePath("arguments.database_path", value.DatabasePath); err != nil {
			return nil, err
		}
		return value, nil
	case OperationOfflineVerify:
		var value OfflineVerifyArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateAbsolutePath("arguments.database_path", value.DatabasePath); err != nil {
			return nil, err
		}
		if value.Mode != "" && value.Mode != "sampled" && value.Mode != "full" {
			return nil, invalidProtocol("arguments.mode", "must be sampled or full", nil)
		}
		return value, nil
	case OperationOfflineRecoverPropose:
		var value OfflineRecoverProposeArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateAbsolutePath("arguments.database_path", value.DatabasePath); err != nil {
			return nil, err
		}
		return value, nil
	case OperationOfflineRecoverApply:
		var value OfflineRecoverApplyArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if err := validateAbsolutePath("arguments.database_path", value.DatabasePath); err != nil {
			return nil, err
		}
		if err := validateAbsolutePath("arguments.destination", value.Destination); err != nil {
			return nil, err
		}
		if filepath.Clean(value.DatabasePath) == filepath.Clean(value.Destination) {
			return nil, invalidProtocol("arguments.destination", "must differ from database_path", nil)
		}
		if value.ActionID == "" || len(value.ActionID) > 256 {
			return nil, invalidProtocol("arguments.action_id", "must be non-empty and at most 256 bytes", nil)
		}
		if value.ExpectedDatabaseID == "" {
			return nil, invalidProtocol("arguments.expected_database_id", "must not be empty", nil)
		}
		if !objectHasNonNullField(raw, "expected_manifest_generation") {
			return nil, invalidProtocol("arguments.expected_manifest_generation", "is required", nil)
		}
		return value, nil
	case OperationCreateSnapshot:
		var value SnapshotArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if value.ExpectedDatabaseID == "" {
			return nil, invalidProtocol("arguments.expected_database_id", "must not be empty", nil)
		}
		return value, nil
	case OperationCompact:
		var value CompactArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if value.ExpectedDatabaseID == "" {
			return nil, invalidProtocol("arguments.expected_database_id", "must not be empty", nil)
		}
		return value, nil
	case OperationBackup:
		var value BackupArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		if value.ExpectedDatabaseID == "" || value.Destination == "" {
			return nil, invalidProtocol("arguments", "database ID and destination are required", nil)
		}
		return value, nil
	case OperationStatus, OperationHealth, OperationStats, OperationDescribeConfig:
		var value EmptyArguments
		if err := decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, invalidProtocol("operation", "is not implemented", nil)
	}
}

func validateArgumentsType(operation Operation, arguments any) error {
	valid := false
	switch operation {
	case OperationGet:
		_, valid = arguments.(GetArguments)
	case OperationPut, OperationPutIfAbsent:
		_, valid = arguments.(PutArguments)
	case OperationCompareAndSwap:
		_, valid = arguments.(CompareAndSwapArguments)
	case OperationDeleteIfRevision:
		_, valid = arguments.(DeleteIfRevisionArguments)
	case OperationAtomicBatch:
		_, valid = arguments.(BatchArguments)
	case OperationScanPrefix:
		_, valid = arguments.(ScanArguments)
	case OperationWatch:
		_, valid = arguments.(WatchArguments)
	case OperationVerify:
		_, valid = arguments.(VerifyArguments)
	case OperationCreateSnapshot:
		_, valid = arguments.(SnapshotArguments)
	case OperationCompact:
		_, valid = arguments.(CompactArguments)
	case OperationBackup:
		_, valid = arguments.(BackupArguments)
	case OperationOfflineInspect:
		_, valid = arguments.(OfflineInspectArguments)
	case OperationOfflineVerify:
		_, valid = arguments.(OfflineVerifyArguments)
	case OperationOfflineRecoverPropose:
		_, valid = arguments.(OfflineRecoverProposeArguments)
	case OperationOfflineRecoverApply:
		_, valid = arguments.(OfflineRecoverApplyArguments)
	default:
		_, valid = arguments.(EmptyArguments)
	}
	if !valid {
		return invalidProtocol("arguments", typeReason(arguments), nil)
	}
	return nil
}

func normalizeRequestArguments(arguments any) any {
	switch value := arguments.(type) {
	case PutArguments:
		if value.Ack == "" {
			value.Ack = gapdb.AckMemory
		}
		return value
	case CompareAndSwapArguments:
		if value.Ack == "" {
			value.Ack = gapdb.AckMemory
		}
		return value
	case DeleteIfRevisionArguments:
		if value.Ack == "" {
			value.Ack = gapdb.AckMemory
		}
		return value
	case BatchArguments:
		if value.Ack == "" {
			value.Ack = gapdb.AckMemory
		}
		return value
	default:
		return arguments
	}
}

func objectHasField(raw []byte, field string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return false
	}
	_, ok := object[field]
	return ok
}

func objectHasNonNullField(raw []byte, field string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return false
	}
	encoded, ok := object[field]
	return ok && !bytes.Equal(encoded, []byte("null"))
}

func decodeAcknowledgement(raw []byte) (gapdb.AckMode, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", invalidProtocol("arguments.ack", "could not inspect acknowledgement", err)
	}
	encoded, present := object["ack"]
	if !present {
		return gapdb.AckMemory, nil
	}
	var acknowledgement gapdb.AckMode
	if err := json.Unmarshal(encoded, &acknowledgement); err != nil || !acknowledgement.Valid() {
		return "", invalidProtocol("arguments.ack", "must be memory or durable when present", err)
	}
	return acknowledgement, nil
}

func validateAbsolutePath(field, value string) error {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return invalidProtocol(field, "must be a clean absolute path", nil)
	}
	return nil
}

func validateBatchWire(raw []byte) error {
	var object struct {
		Mutations []map[string]json.RawMessage `json:"mutations"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return invalidProtocol("arguments.mutations", "could not inspect mutation values", err)
	}
	for index, mutation := range object.Mutations {
		var kind gapdb.MutationKind
		if err := json.Unmarshal(mutation["kind"], &kind); err != nil {
			return invalidProtocol(fmt.Sprintf("arguments.mutations[%d].kind", index), "must be put or delete", err)
		}
		value, hasValue := mutation["value_base64"]
		_, hasExpiry := mutation["expires_at"]
		if hasExpiry {
			encoded, err := json.Marshal(mutation)
			if err != nil {
				return invalidProtocol(fmt.Sprintf("arguments.mutations[%d].expires_at", index), "could not inspect timestamp", err)
			}
			if err := validateOptionalUTCTimeField(encoded, "expires_at"); err != nil {
				return err
			}
		}
		switch kind {
		case gapdb.MutationPut:
			if !hasValue {
				return invalidProtocol(fmt.Sprintf("arguments.mutations[%d].value_base64", index), "is required for put", nil)
			}
			var decoded Base64Bytes
			if err := json.Unmarshal(value, &decoded); err != nil {
				return invalidProtocol(fmt.Sprintf("arguments.mutations[%d].value_base64", index), err.Error(), err)
			}
		case gapdb.MutationDelete:
			if hasValue || hasExpiry {
				return invalidProtocol(fmt.Sprintf("arguments.mutations[%d]", index), "delete cannot carry value_base64 or expires_at", nil)
			}
		}
	}
	return nil
}

func decodeStrict(payload []byte, destination any) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty JSON")
	}
	if !utf8.Valid(payload) {
		return fmt.Errorf("JSON is not valid UTF-8")
	}
	if err := rejectDuplicateFields(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateOptionalUTCTimeField(raw []byte, field string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return invalidProtocol(field, "could not inspect timestamp", err)
	}
	encoded, present := object[field]
	if !present {
		return nil
	}
	if bytes.Equal(encoded, []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(encoded, &text); err != nil {
		return invalidProtocol(field, "must be an RFC3339Nano UTC string", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || !strings.HasSuffix(text, "Z") || parsed.UTC().Format(time.RFC3339Nano) != text {
		return invalidProtocol(field, "must be canonical RFC3339Nano UTC with a Z suffix", err)
	}
	return nil
}

func rejectDuplicateFields(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := inspectJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("object field name is not a string")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate field %q", name)
			}
			seen[name] = struct{}{}
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := inspectJSONValue(decoder, value); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("object did not terminate")
		}
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := inspectJSONValue(decoder, value); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("array did not terminate")
		}
	default:
		return fmt.Errorf("unexpected closing delimiter %q", delimiter)
	}
	return nil
}

func normalizeResult(result any) any {
	switch value := result.(type) {
	case GetResult:
		value.Record = value.Record.Clone()
		return value
	case gapdb.Record:
		return value.Clone()
	case gapdb.ScanPage:
		return value.Clone()
	case gapdb.ChangeEvent:
		return value.Clone()
	default:
		return result
	}
}

func revisionValue(value *gapdb.Revision) gapdb.Revision {
	if value == nil {
		return 0
	}
	return *value
}
