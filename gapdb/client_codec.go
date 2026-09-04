package gapdb

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const maxAssertionDiagnosticBytes = 4 << 10

// validateClientResponse is the deletion-sensitive protocol-v1 validation
// boundary. Public result decoding is deliberately performed only after this
// function has accepted every nested field and spelling.
func validateClientResponse(payload []byte) error {
	var envelope struct {
		SchemaVersion        *uint64         `json:"schema_version"`
		OK                   *bool           `json:"ok"`
		RequestID            string          `json:"request_id,omitempty"`
		DatabaseID           string          `json:"database_id"`
		Operation            string          `json:"operation"`
		Result               json.RawMessage `json:"result,omitempty"`
		Stream               string          `json:"stream,omitempty"`
		RegistrationRevision *Revision       `json:"registration_revision,omitempty"`
		Event                json.RawMessage `json:"event,omitempty"`
		Reason               WatchEndReason  `json:"reason,omitempty"`
		Error                json.RawMessage `json:"error,omitempty"`
	}
	if err := clientDecodeStrict(payload, &envelope); err != nil {
		return err
	}
	if envelope.SchemaVersion == nil || *envelope.SchemaVersion != clientSchemaVersion || envelope.OK == nil || envelope.DatabaseID == "" || !clientOperation(envelope.Operation) || len(envelope.RequestID) > 256 {
		return errors.New("response envelope is incomplete")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return err
	}
	has := func(name string) bool {
		raw, ok := fields[name]
		return ok && !bytes.Equal(raw, []byte("null"))
	}
	hasError := has("error")
	if *envelope.OK == hasError {
		return errors.New("response ok/error presence is inconsistent")
	}
	if hasError {
		var remote Error
		if err := clientDecodeStrict(envelope.Error, &remote); err != nil {
			return fmt.Errorf("error: %w", err)
		}
		if err := validateRemoteErrorRaw(&remote, envelope.Error); err != nil {
			return err
		}
		if remote.AssertionIndex != nil && envelope.Operation != "atomic_batch" {
			return errors.New("assertion_index is invalid outside atomic_batch")
		}
		if remote.AssertionIndex != nil && len(payload) >= maxAssertionDiagnosticBytes {
			return errors.New("assertion diagnostic exceeds its safe bound")
		}
	}
	if envelope.Operation != "watch" {
		if has("stream") || has("registration_revision") || has("event") || has("reason") {
			return errors.New("watch fields are invalid for unary response")
		}
		if *envelope.OK {
			if !has("result") {
				return errors.New("successful unary result is required")
			}
			return validateClientResult(envelope.Operation, envelope.Result)
		}
		if has("result") {
			return errors.New("failed unary result must be omitted")
		}
		return nil
	}
	if has("result") {
		return errors.New("watch result must be omitted")
	}
	switch envelope.Stream {
	case "started":
		if !*envelope.OK || envelope.RegistrationRevision == nil || has("event") || has("reason") || hasError {
			return errors.New("invalid watch started frame")
		}
	case "event":
		if !*envelope.OK || envelope.RegistrationRevision != nil || !has("event") || has("reason") || hasError {
			return errors.New("invalid watch event frame")
		}
		if err := validateClientEvent(envelope.Event); err != nil {
			return err
		}
	case "ended":
		if envelope.RegistrationRevision != nil || has("event") {
			return errors.New("invalid watch ended frame")
		}
		if *envelope.OK {
			if envelope.Reason != WatchEndedByClient && envelope.Reason != WatchEndedByShutdown {
				return errors.New("normal watch end reason is invalid")
			}
		} else if envelope.Reason != WatchEndedByError {
			return errors.New("error watch end reason is invalid")
		}
	default:
		return errors.New("watch stream kind is invalid")
	}
	return nil
}

func validateClientResult(operation string, raw []byte) error {
	required := map[string][]string{
		"get": {"record"},
		"put": {"revision", "ack", "durable_through_revision"}, "put_if_absent": {"revision", "ack", "durable_through_revision"}, "compare_and_swap": {"revision", "ack", "durable_through_revision"}, "delete_if_revision": {"revision", "ack", "durable_through_revision"}, "atomic_batch": {"revision", "ack", "durable_through_revision", "mutation_count"},
		"scan_prefix":     {"observed_revision", "as_of", "records", "truncated"},
		"status":          {"lifecycle", "database_id", "current_revision", "durable_through_revision", "reserved_revision_end", "snapshot_revision", "active_wal_start", "earliest_watch_revision", "record_count", "watch_count", "queue_depth", "active_clients", "limits", "owner_pid", "owner_started_at", "snapshot_in_progress", "backup_in_progress"},
		"health":          {"lifecycle", "healthy", "failing_subsystems", "safe_actions"},
		"stats":           {"schema_version", "live_records", "expired_records", "approximate_value_bytes", "commits", "condition_failures", "wal_bytes", "sync_count", "sync_errors", "watchers", "lagged_watches", "clients", "queue_depth", "snapshot_duration_nanos", "recovery_duration_nanos"},
		"describe_config": {"effective", "source", "safe_ceilings", "restart_required"},
		"verify":          {"mode", "verified", "database_id", "manifest_generation", "current_revision", "snapshot_revision", "active_wal_start", "checked_files"},
		"create_snapshot": {"revision", "filename", "checksum", "new_wal_start", "duration"},
		"compact":         {"before_revision", "after_revision", "removed", "removed_count", "skipped_count", "paths_truncated", "directory_synced"},
		"backup":          {"database_id", "revision", "manifest_checksum", "files", "byte_count", "verified"},
	}[operation]
	if len(required) != 0 {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		if operation != "atomic_batch" {
			if _, present := object["assertion_count"]; present {
				return errors.New("assertion_count is invalid outside atomic_batch")
			}
		}
		for _, name := range required {
			if _, ok := object[name]; !ok {
				return fmt.Errorf("result field %q is required", name)
			}
		}
	}
	var destination any
	switch operation {
	case "get":
		destination = &struct {
			Record clientWireRecord `json:"record"`
		}{}
	case "put", "put_if_absent", "compare_and_swap", "delete_if_revision", "atomic_batch":
		destination = &MutationResult{}
	case "scan_prefix":
		var value struct {
			ObservedRevision Revision           `json:"observed_revision"`
			AsOf             clientUTCInstant   `json:"as_of"`
			Records          []clientWireRecord `json:"records"`
			Cursor           string             `json:"cursor,omitempty"`
			Truncated        bool               `json:"truncated"`
		}
		if err := clientDecodeStrict(raw, &value); err != nil {
			return err
		}
		for index := range value.Records {
			if err := value.Records[index].validate(); err != nil {
				return err
			}
			if index > 0 && value.Records[index-1].Key >= value.Records[index].Key {
				return errors.New("scan records are not strictly sorted")
			}
		}
		if value.Truncated != (value.Cursor != "") {
			return errors.New("scan cursor presence is inconsistent")
		}
		return nil
	case "status":
		destination = &StatusResult{}
	case "health":
		destination = &HealthResult{}
	case "stats":
		destination = &StatsResult{}
	case "describe_config":
		destination = &ConfigResult{}
	case "verify":
		destination = &VerifyResult{}
	case "create_snapshot":
		destination = &SnapshotResult{}
	case "compact":
		destination = &CompactionResult{}
	case "backup":
		destination = &BackupResult{}
	default:
		return errors.New("operation has no client result schema")
	}
	if err := clientDecodeStrict(raw, destination); err != nil {
		return err
	}
	switch value := destination.(type) {
	case *struct {
		Record clientWireRecord `json:"record"`
	}:
		return value.Record.validate()
	case *MutationResult:
		if value.Revision == 0 || !value.Ack.Valid() || value.DurableThroughRevision > value.Revision || value.Ack == AckDurable && value.DurableThroughRevision < value.Revision || value.MutationCount < 0 || value.AssertionCount < 0 {
			return errors.New("mutation acknowledgement is invalid")
		}
	case *StatusResult:
		if !clientLifecycle(value.Lifecycle) || value.DatabaseID == "" || value.OwnerPID <= 0 || value.OwnerStartedAt == "" || (Options{Limits: value.Limits}).Validate() != nil {
			return errors.New("status result is invalid")
		}
		var instant clientUTCInstant
		if encoded, err := json.Marshal(value.OwnerStartedAt); err != nil || instant.UnmarshalJSON(encoded) != nil {
			return errors.New("owner_started_at is invalid")
		}
	case *HealthResult:
		if !clientLifecycle(value.Lifecycle) || value.FailingSubsystems == nil || value.SafeActions == nil {
			return errors.New("health result is invalid")
		}
	case *StatsResult:
		if value.SchemaVersion != 1 || value.LiveRecords < 0 || value.ExpiredRecords < 0 || value.Watchers < 0 || value.Clients < 0 || value.QueueDepth < 0 {
			return errors.New("stats result is invalid")
		}
	case *ConfigResult:
		if value.Source != "default" && value.Source != "file" && value.Source != "flag" || (Options{Limits: value.Effective.Limits}).Validate() != nil || value.SafeCeilings.MaxFrameBytes != HardMaxFrameBytes {
			return errors.New("config result is invalid")
		}
	case *VerifyResult:
		if value.Mode != "sampled" && value.Mode != "full" || !value.Verified || value.DatabaseID == "" || value.ManifestGeneration == 0 || value.CheckedFiles == nil {
			return errors.New("verify result is invalid")
		}
	case *SnapshotResult:
		if value.Filename == "" || value.Checksum == "" || value.Revision == ^Revision(0) || value.NewWALStart != value.Revision+1 || value.Duration < 0 {
			return errors.New("snapshot result is invalid")
		}
	case *CompactionResult:
		if value.Removed == nil || value.RemovedCount < len(value.Removed) || value.SkippedCount < 0 || !value.DirectorySynced {
			return errors.New("compaction result is invalid")
		}
	case *BackupResult:
		if value.DatabaseID == "" || value.ManifestChecksum == "" || value.Files == nil || value.ByteCount < 0 || !value.Verified {
			return errors.New("backup result is invalid")
		}
		var total int64
		for index, file := range value.Files {
			if file.Name == "" || file.Size < 0 || file.SHA256 == "" || index > 0 && value.Files[index-1].Name >= file.Name {
				return errors.New("backup file list is invalid")
			}
			total += file.Size
		}
		if total != value.ByteCount {
			return errors.New("backup byte count is invalid")
		}
	}
	return nil
}

func clientLifecycle(value LifecycleState) bool {
	switch value {
	case LifecycleReady, LifecycleDegradedReadOnly, LifecycleDraining, LifecycleInspectionOnly:
		return true
	default:
		return false
	}
}

type clientBase64 []byte

func (value *clientBase64) UnmarshalJSON(encoded []byte) error {
	if bytes.Equal(encoded, []byte("null")) {
		return errors.New("base64 value must be a string")
	}
	var text string
	if err := json.Unmarshal(encoded, &text); err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != text {
		return errors.New("base64 value is not canonical padded standard base64")
	}
	*value = decoded
	return nil
}

type clientUTCInstant time.Time

func (value *clientUTCInstant) UnmarshalJSON(encoded []byte) error {
	var text string
	if err := json.Unmarshal(encoded, &text); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || !strings.HasSuffix(text, "Z") || parsed.UTC().Format(time.RFC3339Nano) != text {
		return errors.New("timestamp is not canonical RFC3339Nano UTC")
	}
	*value = clientUTCInstant(parsed.UTC())
	return nil
}

type clientWireRecord struct {
	Key       string            `json:"key"`
	Value     clientBase64      `json:"value_base64"`
	Revision  Revision          `json:"revision"`
	ExpiresAt *clientUTCInstant `json:"expires_at,omitempty"`
}

func (record *clientWireRecord) UnmarshalJSON(encoded []byte) error {
	type plain clientWireRecord
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return err
	}
	for _, name := range []string{"key", "value_base64", "revision"} {
		if _, ok := object[name]; !ok {
			return fmt.Errorf("record field %q is required", name)
		}
	}
	var decoded plain
	if err := clientDecodeStrict(encoded, &decoded); err != nil {
		return err
	}
	*record = clientWireRecord(decoded)
	return nil
}

func (record clientWireRecord) validate() error {
	limits := DefaultOptions().Limits
	limits.MaxKeyBytes, limits.MaxValueBytes = HardMaxKeyBytes, HardMaxValueBytes
	if err := NewPutMutation(record.Key, record.Value, Condition{Kind: ConditionAny}, nil).Validate(limits); err != nil {
		return err
	}
	if record.Revision == 0 {
		return errors.New("record revision must be nonzero")
	}
	return nil
}

func validateClientEvent(raw []byte) error {
	var value struct {
		Revision Revision          `json:"revision"`
		Order    uint32            `json:"order"`
		Kind     ChangeKind        `json:"kind"`
		Key      string            `json:"key"`
		Record   *clientWireRecord `json:"record,omitempty"`
	}
	if err := clientDecodeStrict(raw, &value); err != nil {
		return err
	}
	if value.Revision == 0 {
		return errors.New("event revision must be nonzero")
	}
	switch value.Kind {
	case ChangePut:
		if value.Record == nil || value.Record.Key != value.Key || value.Record.Revision != value.Revision {
			return errors.New("put event record must match event")
		}
		return value.Record.validate()
	case ChangeDelete, ChangeExpire:
		if value.Record != nil {
			return errors.New("delete/expire event cannot contain record")
		}
	default:
		return errors.New("event kind is invalid")
	}
	return nil
}

func clientOperation(operation string) bool {
	switch operation {
	case "get", "put", "put_if_absent", "compare_and_swap", "delete_if_revision", "atomic_batch", "scan_prefix", "watch", "status", "health", "stats", "describe_config", "verify", "create_snapshot", "compact", "backup", "offline_inspect", "offline_verify", "offline_recover_propose", "offline_recover_apply":
		return true
	default:
		return false
	}
}

func clientDecodeStrict(payload []byte, destination any) error {
	if len(payload) == 0 || !utf8.Valid(payload) {
		return errors.New("empty or invalid UTF-8 JSON")
	}
	if err := clientRejectDuplicateFields(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func clientRejectDuplicateFields(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := clientInspectJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func clientInspectJSONValue(decoder *json.Decoder, token json.Token) error {
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
				return errors.New("object name is not a string")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate field %q", name)
			}
			seen[name] = struct{}{}
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := clientInspectJSONValue(decoder, value); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := clientInspectJSONValue(decoder, value); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON closing delimiter")
	}
	_, err := decoder.Token()
	return err
}

type clientErrorSchema struct {
	required    []string
	requiredAny [][]string
	allowed     map[string]struct{}
}

func validateRemoteErrorRaw(remote *Error, raw []byte) error {
	if err := validateRemoteError(remote); err != nil {
		return err
	}
	if remote.AssertionIndex != nil && *remote.AssertionIndex < 0 {
		return errors.New("remote assertion index is invalid")
	}
	if remote.AssertionIndex != nil && (len(raw) >= maxAssertionDiagnosticBytes || remote.Key != diagnosticKey(remote.Key)) {
		return errors.New("remote assertion diagnostic exceeds its safe bound")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	schema, ok := clientErrorSchemaFor(remote.Code)
	if !ok {
		return errors.New("remote error has no evidence schema")
	}
	base := map[string]struct{}{"code": {}, "message": {}, "retry": {}, "safe_actions": {}}
	for name, encoded := range object {
		if _, ok := base[name]; ok {
			continue
		}
		if _, ok := schema.allowed[name]; !ok || !clientMeaningfulEvidence(encoded) {
			return fmt.Errorf("remote error evidence %q is invalid", name)
		}
	}
	for _, name := range schema.required {
		if !clientMeaningfulEvidence(object[name]) {
			return fmt.Errorf("remote error evidence %q is required", name)
		}
	}
	for _, group := range schema.requiredAny {
		found := false
		for _, name := range group {
			found = found || clientMeaningfulEvidence(object[name])
		}
		if !found {
			return errors.New("remote error is missing required alternative evidence")
		}
	}
	if encoded, present := object["operation_applied"]; present {
		var applied bool
		if json.Unmarshal(encoded, &applied) != nil || !applied {
			return errors.New("operation_applied must be true when present")
		}
	}
	pair := func(left, right string) bool {
		return clientMeaningfulEvidence(object[left]) && clientMeaningfulEvidence(object[right])
	}
	if remote.Code == CodeBatchTooLarge && !pair("received_operations", "maximum_operations") && !pair("received_bytes", "maximum_bytes") {
		return errors.New("batch evidence requires a complete pair")
	}
	if remote.Code == CodeAdminPreconditionFailed && !pair("expected_database_id", "actual_database_id") && !pair("expected_revision", "actual_revision") && !pair("expected_manifest_generation", "actual_manifest_generation") {
		return errors.New("admin evidence requires a complete pair")
	}
	if remote.Code == CodeConditionFailed {
		_, hasMutation := object["mutation_index"]
		_, hasAssertion := object["assertion_index"]
		if hasMutation == hasAssertion {
			return errors.New("condition evidence must identify exactly one mutation or assertion")
		}
		if hasAssertion {
			switch ConditionKind(remote.Condition) {
			case ConditionAbsent:
				if _, present := object["expected_revision"]; present {
					return errors.New("expected_revision is invalid for an absent assertion")
				}
			case ConditionRevision:
				if remote.ExpectedRevision == nil || *remote.ExpectedRevision == 0 {
					return errors.New("expected_revision must be positive for a revision assertion")
				}
			default:
				return errors.New("assertion condition must be absent or revision")
			}
		}
	}
	return nil
}

func clientEvidence(required, optional []string, groups ...[]string) clientErrorSchema {
	allowed := map[string]struct{}{"operation_applied": {}}
	for _, name := range append(append([]string(nil), required...), optional...) {
		allowed[name] = struct{}{}
	}
	for _, group := range groups {
		for _, name := range group {
			allowed[name] = struct{}{}
		}
	}
	return clientErrorSchema{required: required, requiredAny: groups, allowed: allowed}
}

func clientErrorSchemaFor(code ErrorCode) (clientErrorSchema, bool) {
	var schema clientErrorSchema
	switch code {
	case CodeInvalidRequest:
		schema = clientEvidence([]string{"reason"}, nil, []string{"field", "path"})
	case CodeUnsupportedVersion:
		schema = clientEvidence([]string{"received_version", "supported_versions"}, nil)
	case CodeFrameTooLarge:
		schema = clientEvidence([]string{"received_bytes", "maximum_bytes"}, nil)
	case CodeKeyTooLarge, CodeValueTooLarge:
		schema = clientEvidence([]string{"received_bytes", "maximum_bytes"}, []string{"field"})
	case CodeBatchTooLarge:
		schema = clientEvidence(nil, []string{"received_operations", "maximum_operations", "received_bytes", "maximum_bytes"})
	case CodeDuplicateKey:
		schema = clientEvidence([]string{"key"}, []string{"mutation_index"}, []string{"mutation_indexes", "assertion_index"})
	case CodeExpiryNotFuture:
		schema = clientEvidence([]string{"supplied_expiry", "effective_time"}, nil)
	case CodeInvalidCursor:
		schema = clientEvidence([]string{"reason"}, nil)
	case CodeNotFound:
		schema = clientEvidence([]string{"key", "current_revision"}, nil)
	case CodeAlreadyExists:
		schema = clientEvidence([]string{"key", "actual_revision"}, nil)
	case CodeRevisionMismatch:
		schema = clientEvidence([]string{"key", "expected_revision", "actual_revision"}, nil)
	case CodeConditionFailed:
		schema = clientEvidence([]string{"key", "condition"}, []string{"expected_revision"}, []string{"mutation_index", "assertion_index"}, []string{"actual_state", "actual_revision"})
	case CodeScanStale:
		schema = clientEvidence([]string{"cursor_revision", "current_revision"}, nil)
	case CodeRevisionAhead:
		schema = clientEvidence([]string{"requested_revision", "current_revision"}, nil)
	case CodeRevisionCompacted:
		schema = clientEvidence([]string{"requested_revision", "earliest_available_revision"}, nil)
	case CodeWatchLagged:
		schema = clientEvidence([]string{"last_delivered_revision", "current_revision"}, nil)
	case CodeOwnerExists:
		schema = clientEvidence([]string{"path"}, nil)
	case CodeServerBusy:
		schema = clientEvidence([]string{"active_clients", "maximum_clients", "queue_depth"}, nil)
	case CodeServerShuttingDown:
		schema = clientEvidence([]string{"lifecycle"}, nil)
	case CodeDeadlineExceeded:
		schema = clientEvidence([]string{"operation", "deadline"}, nil)
	case CodeServerUnavailable:
		schema = clientEvidence([]string{"socket_path", "connection_reason"}, nil)
	case CodePermissionDenied:
		schema = clientEvidence([]string{"path", "operation"}, nil)
	case CodeStorageDegraded:
		schema = clientEvidence([]string{"failed_stage", "current_revision", "durable_through_revision"}, nil)
	case CodeCorruptIdentity:
		schema = clientEvidence([]string{"file"}, []string{"expected_database_id"}, []string{"offset", "reason"})
	case CodeCorruptManifest:
		schema = clientEvidence([]string{"file", "reason"}, nil)
	case CodeCorruptSnapshot:
		schema = clientEvidence([]string{"file", "offset", "reason"}, nil)
	case CodeCorruptWAL:
		schema = clientEvidence([]string{"file", "offset", "reason"}, []string{"actual_revision"})
	case CodeUnknownFormat:
		schema = clientEvidence([]string{"file", "received_version", "supported_versions"}, nil)
	case CodeDatabaseIDMismatch:
		schema = clientEvidence([]string{"expected_database_id", "actual_database_id"}, nil, []string{"file", "operation"})
	case CodeRevisionRangeExhausted:
		schema = clientEvidence([]string{"reserved_revision_end", "reason"}, nil)
	case CodeIOError:
		schema = clientEvidence([]string{"operation", "path", "os_error_category"}, nil)
	case CodeAdminPreconditionFailed:
		schema = clientEvidence(nil, []string{"expected_database_id", "actual_database_id", "expected_revision", "actual_revision", "expected_manifest_generation", "actual_manifest_generation"})
	case CodeSnapshotInProgress:
		schema = clientEvidence([]string{"operation_id", "started_at"}, nil)
	case CodeCompactionNotSafe:
		schema = clientEvidence([]string{"through_revision", "active_snapshot_revision"}, nil)
	case CodeBackupDestinationExists:
		schema = clientEvidence([]string{"destination"}, nil)
	case CodeBackupInvalid:
		schema = clientEvidence([]string{"backup_path"}, nil, []string{"file", "reason"})
	case CodeRecoveryRequired:
		schema = clientEvidence([]string{"detected_corruption", "proposal_available"}, nil)
	case CodeRecoveryActionMismatch:
		schema = clientEvidence([]string{"expected_database_id", "actual_database_id", "expected_manifest_generation", "actual_manifest_generation", "proposal_id"}, nil)
	case CodeAuditFailedAfterApply:
		schema = clientEvidence([]string{"operation", "reason", "current_revision", "operation_applied"}, nil)
	case CodeInternal:
		schema = clientEvidence([]string{"correlation_id"}, []string{"lifecycle", "reason"})
	default:
		return clientErrorSchema{}, false
	}
	return schema, true
}

func clientMeaningfulEvidence(encoded json.RawMessage) bool {
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
