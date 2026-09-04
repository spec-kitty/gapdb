package gapdb

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

import "time"

type Revision uint64

type ErrorCode string

const (
	CodeInvalidRequest          ErrorCode = "INVALID_REQUEST"
	CodeUnsupportedVersion      ErrorCode = "UNSUPPORTED_VERSION"
	CodeFrameTooLarge           ErrorCode = "FRAME_TOO_LARGE"
	CodeKeyTooLarge             ErrorCode = "KEY_TOO_LARGE"
	CodeValueTooLarge           ErrorCode = "VALUE_TOO_LARGE"
	CodeBatchTooLarge           ErrorCode = "BATCH_TOO_LARGE"
	CodeDuplicateKey            ErrorCode = "DUPLICATE_KEY"
	CodeExpiryNotFuture         ErrorCode = "EXPIRY_NOT_FUTURE"
	CodeInvalidCursor           ErrorCode = "INVALID_CURSOR"
	CodeNotFound                ErrorCode = "NOT_FOUND"
	CodeAlreadyExists           ErrorCode = "ALREADY_EXISTS"
	CodeRevisionMismatch        ErrorCode = "REVISION_MISMATCH"
	CodeConditionFailed         ErrorCode = "CONDITION_FAILED"
	CodeScanStale               ErrorCode = "SCAN_STALE"
	CodeRevisionAhead           ErrorCode = "REVISION_AHEAD"
	CodeRevisionCompacted       ErrorCode = "REVISION_COMPACTED"
	CodeWatchLagged             ErrorCode = "WATCH_LAGGED"
	CodeOwnerExists             ErrorCode = "OWNER_EXISTS"
	CodeServerBusy              ErrorCode = "SERVER_BUSY"
	CodeServerShuttingDown      ErrorCode = "SERVER_SHUTTING_DOWN"
	CodeDeadlineExceeded        ErrorCode = "DEADLINE_EXCEEDED"
	CodeServerUnavailable       ErrorCode = "SERVER_UNAVAILABLE"
	CodePermissionDenied        ErrorCode = "PERMISSION_DENIED"
	CodeStorageDegraded         ErrorCode = "STORAGE_DEGRADED"
	CodeCorruptIdentity         ErrorCode = "CORRUPT_IDENTITY"
	CodeCorruptManifest         ErrorCode = "CORRUPT_MANIFEST"
	CodeCorruptSnapshot         ErrorCode = "CORRUPT_SNAPSHOT"
	CodeCorruptWAL              ErrorCode = "CORRUPT_WAL"
	CodeUnknownFormat           ErrorCode = "UNKNOWN_FORMAT"
	CodeDatabaseIDMismatch      ErrorCode = "DATABASE_ID_MISMATCH"
	CodeRevisionRangeExhausted  ErrorCode = "REVISION_RANGE_EXHAUSTED"
	CodeIOError                 ErrorCode = "IO_ERROR"
	CodeAdminPreconditionFailed ErrorCode = "ADMIN_PRECONDITION_FAILED"
	CodeSnapshotInProgress      ErrorCode = "SNAPSHOT_IN_PROGRESS"
	CodeCompactionNotSafe       ErrorCode = "COMPACTION_NOT_SAFE"
	CodeBackupDestinationExists ErrorCode = "BACKUP_DESTINATION_EXISTS"
	CodeBackupInvalid           ErrorCode = "BACKUP_INVALID"
	CodeRecoveryRequired        ErrorCode = "RECOVERY_REQUIRED"
	CodeRecoveryActionMismatch  ErrorCode = "RECOVERY_ACTION_MISMATCH"
	CodeAuditFailedAfterApply   ErrorCode = "AUDIT_FAILED_AFTER_APPLY"
	CodeInternal                ErrorCode = "INTERNAL"
)

type RetryClass string

const (
	RetryNever          RetryClass = "never"
	RetryImmediate      RetryClass = "immediate"
	RetryAfterReconcile RetryClass = "after_reconcile"
	RetryAfterRescan    RetryClass = "after_rescan"
	RetryAfterRestart   RetryClass = "after_restart"
	RetryAfterOperator  RetryClass = "after_operator"
)

type SafeAction string

const (
	ActionAbort                 SafeAction = "abort"
	ActionFixRequest            SafeAction = "fix_request"
	ActionUseSupportedVersion   SafeAction = "use_supported_version"
	ActionUpgradeClient         SafeAction = "upgrade_client"
	ActionReduceRequest         SafeAction = "reduce_request"
	ActionReduceKey             SafeAction = "reduce_key"
	ActionReduceValue           SafeAction = "reduce_value"
	ActionSplitBatch            SafeAction = "split_batch"
	ActionDeduplicateBatch      SafeAction = "deduplicate_batch"
	ActionChooseFutureExpiry    SafeAction = "choose_future_expiry"
	ActionRestartScan           SafeAction = "restart_scan"
	ActionGet                   SafeAction = "get"
	ActionPutIfAbsent           SafeAction = "put_if_absent"
	ActionCompareAndSwap        SafeAction = "compare_and_swap"
	ActionRetryWithNewCondition SafeAction = "retry_with_new_condition"
	ActionRebuildBatch          SafeAction = "rebuild_batch"
	ActionScanPrefix            SafeAction = "scan_prefix"
	ActionRestartWatch          SafeAction = "restart_watch"
	ActionStatus                SafeAction = "status"
	ActionWait                  SafeAction = "wait"
	ActionRetryWithBackoff      SafeAction = "retry_with_backoff"
	ActionWaitForRestart        SafeAction = "wait_for_restart"
	ActionReconcile             SafeAction = "reconcile"
	ActionStatusOffline         SafeAction = "status_offline"
	ActionStartServer           SafeAction = "start_server"
	ActionFixPermissions        SafeAction = "fix_permissions"
	ActionVerify                SafeAction = "verify"
	ActionRestartAfterRecovery  SafeAction = "restart_after_recovery"
	ActionInspectOffline        SafeAction = "inspect_offline"
	ActionRestoreBackup         SafeAction = "restore_backup"
	ActionRecoverPropose        SafeAction = "recover_propose"
	ActionUpgradeGapdb          SafeAction = "upgrade_gapdb"
	ActionUseCompatibleBinary   SafeAction = "use_compatible_binary"
	ActionSelectCorrectDatabase SafeAction = "select_correct_database"
	ActionVerifyStorage         SafeAction = "verify_storage"
	ActionCheckStorage          SafeAction = "check_storage"
	ActionRebuildRequest        SafeAction = "rebuild_request"
	ActionCreateSnapshot        SafeAction = "create_snapshot"
	ActionChooseNewDestination  SafeAction = "choose_new_destination"
	ActionVerifyBackup          SafeAction = "verify_backup"
	ActionChooseOtherBackup     SafeAction = "choose_other_backup"
	ActionRepairAuditStorage    SafeAction = "repair_audit_storage"
)

// Error is the stable machine-readable failure surface. Optional evidence uses
// pointers where zero is meaningful, so absent evidence is omitted rather than
// fabricated. Cause is retained for errors.Is/errors.As and never serialized.
type Error struct {
	Code                       ErrorCode      `json:"code"`
	Message                    string         `json:"message"`
	Retry                      RetryClass     `json:"retry"`
	Field                      string         `json:"field,omitempty"`
	Path                       string         `json:"path,omitempty"`
	Reason                     string         `json:"reason,omitempty"`
	Key                        string         `json:"key,omitempty"`
	Operation                  string         `json:"operation,omitempty"`
	Deadline                   string         `json:"deadline,omitempty"`
	OSErrorCategory            string         `json:"os_error_category,omitempty"`
	ExpectedRevision           *Revision      `json:"expected_revision,omitempty"`
	ActualRevision             *Revision      `json:"actual_revision,omitempty"`
	CurrentRevision            *Revision      `json:"current_revision,omitempty"`
	RequestedRevision          *Revision      `json:"requested_revision,omitempty"`
	CursorRevision             *Revision      `json:"cursor_revision,omitempty"`
	EarliestRevision           *Revision      `json:"earliest_available_revision,omitempty"`
	LastDeliveredRevision      *Revision      `json:"last_delivered_revision,omitempty"`
	DurableThroughRevision     *Revision      `json:"durable_through_revision,omitempty"`
	ReservedRevisionEnd        *Revision      `json:"reserved_revision_end,omitempty"`
	ThroughRevision            *Revision      `json:"through_revision,omitempty"`
	ActiveSnapshotRevision     *Revision      `json:"active_snapshot_revision,omitempty"`
	MutationIndex              *int           `json:"mutation_index,omitempty"`
	MutationIndexes            []int          `json:"mutation_indexes,omitempty"`
	AssertionIndex             *int           `json:"assertion_index,omitempty"`
	ReceivedVersion            *uint64        `json:"received_version,omitempty"`
	SupportedVersions          []uint64       `json:"supported_versions,omitempty"`
	ExpectedManifestGeneration *uint64        `json:"expected_manifest_generation,omitempty"`
	ActualManifestGeneration   *uint64        `json:"actual_manifest_generation,omitempty"`
	Offset                     *int64         `json:"offset,omitempty"`
	ActiveClients              *int           `json:"active_clients,omitempty"`
	MaximumClients             *int           `json:"maximum_clients,omitempty"`
	QueueDepth                 *int           `json:"queue_depth,omitempty"`
	ReceivedBytes              int            `json:"received_bytes,omitempty"`
	MaximumBytes               int            `json:"maximum_bytes,omitempty"`
	ReceivedOperations         int            `json:"received_operations,omitempty"`
	MaximumOperations          int            `json:"maximum_operations,omitempty"`
	ExpectedDatabaseID         string         `json:"expected_database_id,omitempty"`
	ActualDatabaseID           string         `json:"actual_database_id,omitempty"`
	Condition                  string         `json:"condition,omitempty"`
	ActualState                string         `json:"actual_state,omitempty"`
	Lifecycle                  LifecycleState `json:"lifecycle,omitempty"`
	FailedStage                string         `json:"failed_stage,omitempty"`
	File                       string         `json:"file,omitempty"`
	SocketPath                 string         `json:"socket_path,omitempty"`
	ConnectionReason           string         `json:"connection_reason,omitempty"`
	OperationID                string         `json:"operation_id,omitempty"`
	StartedAt                  string         `json:"started_at,omitempty"`
	Destination                string         `json:"destination,omitempty"`
	BackupPath                 string         `json:"backup_path,omitempty"`
	ProposalID                 string         `json:"proposal_id,omitempty"`
	DetectedCorruption         string         `json:"detected_corruption,omitempty"`
	ProposalAvailable          *bool          `json:"proposal_available,omitempty"`
	CorrelationID              string         `json:"correlation_id,omitempty"`
	SuppliedExpiry             string         `json:"supplied_expiry,omitempty"`
	EffectiveTime              string         `json:"effective_time,omitempty"`
	OperationApplied           bool           `json:"operation_applied,omitempty"`
	SafeActions                []SafeAction   `json:"safe_actions"`
	Cause                      error          `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *Error) Is(target error) bool {
	var other *Error
	return e != nil && e.Code != "" && errors.As(target, &other) && other.Code == e.Code
}

func (e *Error) Clone() *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.SafeActions = append([]SafeAction(nil), e.SafeActions...)
	clone.MutationIndexes = append([]int(nil), e.MutationIndexes...)
	clone.SupportedVersions = append([]uint64(nil), e.SupportedVersions...)
	if e.AssertionIndex != nil {
		value := *e.AssertionIndex
		clone.AssertionIndex = &value
	}
	return &clone
}

type ErrorDefinition struct {
	Code        ErrorCode
	Retry       RetryClass
	SafeActions []SafeAction
}

func ErrorDefinitions() []ErrorDefinition {
	definitions := []ErrorDefinition{
		{CodeInvalidRequest, RetryNever, []SafeAction{ActionFixRequest, ActionAbort}},
		{CodeUnsupportedVersion, RetryNever, []SafeAction{ActionUseSupportedVersion, ActionUpgradeClient, ActionAbort}},
		{CodeFrameTooLarge, RetryNever, []SafeAction{ActionReduceRequest, ActionAbort}},
		{CodeKeyTooLarge, RetryNever, []SafeAction{ActionReduceKey, ActionAbort}},
		{CodeValueTooLarge, RetryNever, []SafeAction{ActionReduceValue, ActionAbort}},
		{CodeBatchTooLarge, RetryNever, []SafeAction{ActionSplitBatch, ActionAbort}},
		{CodeDuplicateKey, RetryNever, []SafeAction{ActionDeduplicateBatch, ActionAbort}},
		{CodeExpiryNotFuture, RetryAfterReconcile, []SafeAction{ActionChooseFutureExpiry, ActionAbort}},
		{CodeInvalidCursor, RetryNever, []SafeAction{ActionRestartScan, ActionAbort}},
		{CodeNotFound, RetryAfterReconcile, []SafeAction{ActionGet, ActionPutIfAbsent, ActionAbort}},
		{CodeAlreadyExists, RetryAfterReconcile, []SafeAction{ActionGet, ActionCompareAndSwap, ActionAbort}},
		{CodeRevisionMismatch, RetryAfterReconcile, []SafeAction{ActionGet, ActionRetryWithNewCondition, ActionAbort}},
		{CodeConditionFailed, RetryAfterReconcile, []SafeAction{ActionGet, ActionRebuildBatch, ActionAbort}},
		{CodeScanStale, RetryAfterRescan, []SafeAction{ActionRestartScan, ActionAbort}},
		{CodeRevisionAhead, RetryAfterRescan, []SafeAction{ActionScanPrefix, ActionRestartWatch, ActionAbort}},
		{CodeRevisionCompacted, RetryAfterRescan, []SafeAction{ActionScanPrefix, ActionRestartWatch, ActionAbort}},
		{CodeWatchLagged, RetryAfterRescan, []SafeAction{ActionScanPrefix, ActionRestartWatch, ActionAbort}},
		{CodeOwnerExists, RetryAfterOperator, []SafeAction{ActionStatus, ActionWait, ActionAbort}},
		{CodeServerBusy, RetryImmediate, []SafeAction{ActionRetryWithBackoff, ActionAbort}},
		{CodeServerShuttingDown, RetryAfterRestart, []SafeAction{ActionWaitForRestart, ActionAbort}},
		{CodeDeadlineExceeded, RetryAfterReconcile, []SafeAction{ActionStatus, ActionReconcile, ActionAbort}},
		{CodeServerUnavailable, RetryAfterRestart, []SafeAction{ActionStatusOffline, ActionStartServer, ActionAbort}},
		{CodePermissionDenied, RetryAfterOperator, []SafeAction{ActionFixPermissions, ActionAbort}},
		{CodeStorageDegraded, RetryAfterRestart, []SafeAction{ActionStatus, ActionVerify, ActionRestartAfterRecovery, ActionAbort}},
		{CodeCorruptIdentity, RetryAfterOperator, []SafeAction{ActionInspectOffline, ActionRestoreBackup, ActionAbort}},
		{CodeCorruptManifest, RetryAfterOperator, []SafeAction{ActionInspectOffline, ActionRecoverPropose, ActionRestoreBackup, ActionAbort}},
		{CodeCorruptSnapshot, RetryAfterOperator, []SafeAction{ActionInspectOffline, ActionRecoverPropose, ActionRestoreBackup, ActionAbort}},
		{CodeCorruptWAL, RetryAfterOperator, []SafeAction{ActionInspectOffline, ActionRecoverPropose, ActionRestoreBackup, ActionAbort}},
		{CodeUnknownFormat, RetryNever, []SafeAction{ActionUpgradeGapdb, ActionUseCompatibleBinary, ActionAbort}},
		{CodeDatabaseIDMismatch, RetryNever, []SafeAction{ActionSelectCorrectDatabase, ActionAbort}},
		{CodeRevisionRangeExhausted, RetryAfterRestart, []SafeAction{ActionVerifyStorage, ActionRestartAfterRecovery, ActionAbort}},
		{CodeIOError, RetryAfterOperator, []SafeAction{ActionCheckStorage, ActionVerify, ActionAbort}},
		{CodeAdminPreconditionFailed, RetryAfterReconcile, []SafeAction{ActionStatus, ActionRebuildRequest, ActionAbort}},
		{CodeSnapshotInProgress, RetryImmediate, []SafeAction{ActionStatus, ActionWait, ActionAbort}},
		{CodeCompactionNotSafe, RetryAfterReconcile, []SafeAction{ActionStatus, ActionCreateSnapshot, ActionAbort}},
		{CodeBackupDestinationExists, RetryNever, []SafeAction{ActionChooseNewDestination, ActionAbort}},
		{CodeBackupInvalid, RetryAfterOperator, []SafeAction{ActionVerifyBackup, ActionChooseOtherBackup, ActionAbort}},
		{CodeRecoveryRequired, RetryAfterOperator, []SafeAction{ActionRecoverPropose, ActionRestoreBackup, ActionAbort}},
		{CodeRecoveryActionMismatch, RetryAfterReconcile, []SafeAction{ActionRecoverPropose, ActionAbort}},
		{CodeAuditFailedAfterApply, RetryAfterReconcile, []SafeAction{ActionStatus, ActionVerify, ActionRepairAuditStorage, ActionAbort}},
		{CodeInternal, RetryAfterOperator, []SafeAction{ActionStatus, ActionVerify, ActionAbort}},
	}
	for i := range definitions {
		definitions[i].SafeActions = append([]SafeAction(nil), definitions[i].SafeActions...)
	}
	return definitions
}

func invalidField(field, reason string) error {
	return &Error{Code: CodeInvalidRequest, Message: "Request validation failed.", Retry: RetryNever, Field: field, Reason: reason, SafeActions: []SafeAction{ActionFixRequest, ActionAbort}}
}

type AckMode string

const (
	AckMemory  AckMode = "memory"
	AckDurable AckMode = "durable"
)

func (a AckMode) Valid() bool { return a == AckMemory || a == AckDurable }

type ConditionKind string

const (
	ConditionAny      ConditionKind = "any"
	ConditionAbsent   ConditionKind = "absent"
	ConditionRevision ConditionKind = "revision"
)

type MutationKind string

const (
	MutationPut    MutationKind = "put"
	MutationDelete MutationKind = "delete"
)

type ChangeKind string

const (
	ChangePut    ChangeKind = "put"
	ChangeDelete ChangeKind = "delete"
	ChangeExpire ChangeKind = "expire"
)

type LifecycleState string

const (
	LifecycleReady            LifecycleState = "ready"
	LifecycleDegradedReadOnly LifecycleState = "degraded_read_only"
	LifecycleDraining         LifecycleState = "draining"
	LifecycleInspectionOnly   LifecycleState = "inspection_only"
)

// Record is the complete public value model. ExpiresAt is an absolute UTC
// instant; at or after that instant the record is logically absent.
type Record struct {
	Key       string     `json:"key"`
	Value     []byte     `json:"value_base64"`
	Revision  Revision   `json:"revision"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

func NewRecord(key string, value []byte, revision Revision, expiresAt *time.Time) Record {
	return Record{Key: key, Value: cloneOpaqueBytes(value), Revision: revision, ExpiresAt: cloneTime(expiresAt)}
}

func (r Record) Clone() Record {
	r.Value = cloneOpaqueBytes(r.Value)
	r.ExpiresAt = cloneTime(r.ExpiresAt)
	return r
}

type Condition struct {
	Kind             ConditionKind `json:"kind"`
	ExpectedRevision Revision      `json:"expected_revision,omitempty"`
}

func (c Condition) Validate() error {
	switch c.Kind {
	case ConditionAny, ConditionAbsent:
		if c.ExpectedRevision != 0 {
			return invalidField("condition.expected_revision", "is valid only for revision conditions")
		}
	case ConditionRevision:
		if c.ExpectedRevision == 0 {
			return invalidField("condition.expected_revision", "must be greater than zero")
		}
	default:
		return invalidField("condition.kind", "must be any, absent, or revision")
	}
	return nil
}

type Mutation struct {
	Kind      MutationKind `json:"kind"`
	Key       string       `json:"key"`
	Condition Condition    `json:"condition"`
	Value     []byte       `json:"value_base64,omitempty"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
}

// Assertion is a read-only admission predicate evaluated against the same
// pre-batch logical view as mutation conditions. It is never persisted and
// never counts as a mutation.
type Assertion struct {
	Key       string    `json:"key"`
	Condition Condition `json:"condition"`
}

func (a Assertion) Validate(limits Limits) error {
	if err := validateKey(a.Key, limits); err != nil {
		return err
	}
	switch a.Condition.Kind {
	case ConditionAbsent:
		if a.Condition.ExpectedRevision != 0 {
			return invalidField("condition.expected_revision", "is valid only for revision assertions")
		}
	case ConditionRevision:
		if a.Condition.ExpectedRevision == 0 {
			return invalidField("condition.expected_revision", "must be greater than zero")
		}
	default:
		return invalidField("condition.kind", "assertions require absent or revision")
	}
	return nil
}

func NewPutMutation(key string, value []byte, condition Condition, expiresAt *time.Time) Mutation {
	return Mutation{Kind: MutationPut, Key: key, Condition: condition, Value: cloneOpaqueBytes(value), ExpiresAt: cloneTime(expiresAt)}
}

func NewDeleteMutation(key string, expectedRevision Revision) Mutation {
	return Mutation{Kind: MutationDelete, Key: key, Condition: Condition{Kind: ConditionRevision, ExpectedRevision: expectedRevision}}
}

func (m Mutation) Clone() Mutation {
	if m.Kind == MutationPut {
		m.Value = cloneOpaqueBytes(m.Value)
	} else {
		m.Value = cloneBytes(m.Value)
	}
	m.ExpiresAt = cloneTime(m.ExpiresAt)
	return m
}

func (m Mutation) Validate(limits Limits) error {
	if err := validateKey(m.Key, limits); err != nil {
		return err
	}
	if err := m.Condition.Validate(); err != nil {
		return err
	}
	switch m.Kind {
	case MutationPut:
		if len(m.Value) > limits.MaxValueBytes {
			return &Error{Code: CodeValueTooLarge, Message: "Value exceeds the configured limit.", Retry: RetryNever, Field: "value_base64", ReceivedBytes: len(m.Value), MaximumBytes: limits.MaxValueBytes, SafeActions: []SafeAction{ActionReduceValue, ActionAbort}}
		}
	case MutationDelete:
		if m.Condition.Kind != ConditionRevision {
			return invalidField("condition.kind", "delete requires a revision condition")
		}
		if m.Value != nil || m.ExpiresAt != nil {
			return invalidField("mutation", "delete cannot carry a value or expiry")
		}
	default:
		return invalidField("mutation.kind", "must be put or delete")
	}
	return nil
}

func (m Mutation) ValidateAt(limits Limits, effectiveNow time.Time) error {
	if err := m.Validate(limits); err != nil {
		return err
	}
	if m.ExpiresAt != nil && !m.ExpiresAt.After(effectiveNow) {
		return &Error{
			Code:           CodeExpiryNotFuture,
			Message:        "Expiry must be later than the effective current time.",
			Retry:          RetryAfterReconcile,
			SuppliedExpiry: m.ExpiresAt.UTC().Format(time.RFC3339Nano),
			EffectiveTime:  effectiveNow.UTC().Format(time.RFC3339Nano),
			SafeActions:    []SafeAction{ActionChooseFutureExpiry, ActionAbort},
		}
	}
	return nil
}

type Batch struct {
	Ack        AckMode     `json:"ack"`
	Assertions []Assertion `json:"assertions,omitempty"`
	Mutations  []Mutation  `json:"mutations"`
}

const (
	maxDiagnosticKeyBytes = 256
	batchBaseBytes        = 32
	batchOperationBytes   = 20
)

func (b Batch) Clone() Batch {
	clone := Batch{Ack: b.Ack, Assertions: append([]Assertion(nil), b.Assertions...), Mutations: make([]Mutation, len(b.Mutations))}
	for i, mutation := range b.Mutations {
		clone.Mutations[i] = mutation.Clone()
	}
	return clone
}

func (b Batch) Validate(limits Limits) error {
	if !b.Ack.Valid() {
		return invalidField("ack", "must be memory or durable")
	}
	if len(b.Mutations) == 0 {
		return invalidField("mutations", "must contain at least one mutation")
	}
	operationCount := len(b.Assertions) + len(b.Mutations)
	if operationCount < len(b.Assertions) || operationCount > limits.MaxBatchOperations {
		return &Error{Code: CodeBatchTooLarge, Message: "Batch exceeds the configured operation limit.", Retry: RetryNever, ReceivedOperations: operationCount, MaximumOperations: limits.MaxBatchOperations, SafeActions: []SafeAction{ActionSplitBatch, ActionAbort}}
	}
	type batchKey struct {
		assertion bool
		index     int
	}
	seen := make(map[string]batchKey, operationCount)
	approximateBytes := batchBaseBytes
	for i, assertion := range b.Assertions {
		if first, ok := seen[assertion.Key]; ok {
			index := i
			failure := &Error{Code: CodeDuplicateKey, Message: "A key occurs more than once in the batch.", Retry: RetryNever, Key: diagnosticKey(assertion.Key), AssertionIndex: &index, SafeActions: []SafeAction{ActionDeduplicateBatch, ActionAbort}}
			if !first.assertion {
				failure.MutationIndex = intPointer(first.index)
			}
			return failure
		}
		seen[assertion.Key] = batchKey{assertion: true, index: i}
		if err := assertion.Validate(limits); err != nil {
			return err
		}
		addition := batchOperationBytes + len(assertion.Key)
		if approximateBytes > limits.MaxBatchBytes || addition > limits.MaxBatchBytes-approximateBytes {
			return batchBytesError(approximateBytes+addition, limits.MaxBatchBytes)
		}
		approximateBytes += addition
	}
	for i, mutation := range b.Mutations {
		if first, ok := seen[mutation.Key]; ok {
			failure := &Error{Code: CodeDuplicateKey, Message: "A key occurs more than once in the batch.", Retry: RetryNever, Key: diagnosticKey(mutation.Key), MutationIndex: intPointer(i), SafeActions: []SafeAction{ActionDeduplicateBatch, ActionAbort}}
			if first.assertion {
				failure.AssertionIndex = intPointer(first.index)
			} else {
				failure.MutationIndexes = []int{first.index, i}
			}
			return failure
		}
		seen[mutation.Key] = batchKey{index: i}
		if err := mutation.Validate(limits); err != nil {
			return err
		}
		addition := batchOperationBytes + len(mutation.Key) + len(mutation.Value)
		if approximateBytes > limits.MaxBatchBytes || addition > limits.MaxBatchBytes-approximateBytes {
			return batchBytesError(approximateBytes+addition, limits.MaxBatchBytes)
		}
		approximateBytes += addition
	}
	return nil
}

func (b Batch) ValidateAt(limits Limits, effectiveNow time.Time) error {
	if err := b.Validate(limits); err != nil {
		return err
	}
	for _, mutation := range b.Mutations {
		if err := mutation.ValidateAt(limits, effectiveNow); err != nil {
			return err
		}
	}
	return nil
}

type MutationResult struct {
	Revision               Revision `json:"revision"`
	Ack                    AckMode  `json:"ack"`
	DurableThroughRevision Revision `json:"durable_through_revision"`
	MutationCount          int      `json:"mutation_count,omitempty"`
	AssertionCount         int      `json:"assertion_count,omitempty"`
}

type ScanPage struct {
	ObservedRevision Revision  `json:"observed_revision"`
	AsOf             time.Time `json:"as_of"`
	Records          []Record  `json:"records"`
	Cursor           string    `json:"cursor,omitempty"`
	Truncated        bool      `json:"truncated"`
}

func (p ScanPage) Clone() ScanPage {
	clone := p
	clone.AsOf = p.AsOf.UTC()
	clone.Records = make([]Record, len(p.Records))
	for i, record := range p.Records {
		clone.Records[i] = record.Clone()
	}
	return clone
}

type ChangeEvent struct {
	Revision Revision   `json:"revision"`
	Order    uint32     `json:"order"`
	Kind     ChangeKind `json:"kind"`
	Key      string     `json:"key"`
	Record   *Record    `json:"record,omitempty"`
}

func (e ChangeEvent) Clone() ChangeEvent {
	if e.Record != nil {
		record := e.Record.Clone()
		e.Record = &record
	}
	return e
}

type WatchEndReason string

const (
	WatchEndedByClient   WatchEndReason = "client_closed"
	WatchEndedByShutdown WatchEndReason = "server_shutdown"
	WatchEndedByError    WatchEndReason = "error"
)

type WatchTermination struct {
	Reason                WatchEndReason `json:"reason"`
	LastDeliveredRevision Revision       `json:"last_delivered_revision"`
	Error                 *Error         `json:"error,omitempty"`
}

type Status struct {
	Lifecycle              LifecycleState `json:"lifecycle"`
	DatabaseID             string         `json:"database_id"`
	CurrentRevision        Revision       `json:"current_revision"`
	DurableThroughRevision Revision       `json:"durable_through_revision"`
	ReservedRevisionEnd    Revision       `json:"reserved_revision_end"`
	SnapshotRevision       Revision       `json:"snapshot_revision"`
	ActiveWALStart         Revision       `json:"active_wal_start"`
	EarliestWatchRevision  Revision       `json:"earliest_watch_revision"`
	Limits                 Limits         `json:"limits"`
}

func validateKey(key string, limits Limits) error {
	if key == "" {
		return invalidField("key", "must not be empty")
	}
	if !utf8.ValidString(key) {
		return invalidField("key", "must be valid UTF-8")
	}
	if len(key) > limits.MaxKeyBytes {
		return &Error{Code: CodeKeyTooLarge, Message: "Key exceeds the configured limit.", Retry: RetryNever, Field: "key", ReceivedBytes: len(key), MaximumBytes: limits.MaxKeyBytes, SafeActions: []SafeAction{ActionReduceKey, ActionAbort}}
	}
	return nil
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

func cloneOpaqueBytes(value []byte) []byte {
	return append([]byte{}, value...)
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func intPointer(value int) *int { return &value }

func diagnosticKey(key string) string {
	if len(key) <= maxDiagnosticKeyBytes {
		return key
	}
	end := maxDiagnosticKeyBytes
	for end > 0 && !utf8.ValidString(key[:end]) {
		end--
	}
	return key[:end]
}

func batchBytesError(received, maximum int) error {
	return &Error{Code: CodeBatchTooLarge, Message: "Batch exceeds the configured byte limit.", Retry: RetryNever, ReceivedBytes: received, MaximumBytes: maximum, SafeActions: []SafeAction{ActionSplitBatch, ActionAbort}}
}

func (r Revision) String() string { return fmt.Sprintf("%d", uint64(r)) }
