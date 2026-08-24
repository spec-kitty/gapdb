package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"gapdb/gapdb"
)

const schemaVersion = 1

type successEnvelope struct {
	SchemaVersion uint16       `json:"schema_version"`
	OK            bool         `json:"ok"`
	Operation     string       `json:"operation"`
	RequestID     string       `json:"request_id,omitempty"`
	Result        any          `json:"result"`
	Limits        gapdb.Limits `json:"limits"`
}

type errorEnvelope struct {
	SchemaVersion uint16       `json:"schema_version"`
	OK            bool         `json:"ok"`
	Operation     string       `json:"operation"`
	RequestID     string       `json:"request_id,omitempty"`
	Error         *gapdb.Error `json:"error"`
	Evidence      any          `json:"evidence,omitempty"`
	Limits        gapdb.Limits `json:"limits"`
}

func writeSuccess(writer io.Writer, operation, requestID string, result any) int {
	value := successEnvelope{schemaVersion, true, operation, requestID, result, gapdb.DefaultOptions().Limits}
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return 6
	}
	return 0
}

func writeError(writer io.Writer, operation, requestID string, err error) int {
	structured := structuredError(err, operation)
	value := errorEnvelope{schemaVersion, false, operation, requestID, structured, nil, gapdb.DefaultOptions().Limits}
	if encodeErr := json.NewEncoder(writer).Encode(value); encodeErr != nil {
		return 6
	}
	return exitCode(structured.Code)
}

func writeErrorEvidence(writer io.Writer, operation, requestID string, err error, evidence any) int {
	structured := structuredError(err, operation)
	value := errorEnvelope{schemaVersion, false, operation, requestID, structured, evidence, gapdb.DefaultOptions().Limits}
	if encodeErr := json.NewEncoder(writer).Encode(value); encodeErr != nil {
		return 6
	}
	return exitCode(structured.Code)
}

func structuredError(err error, operation string) *gapdb.Error {
	var structured *gapdb.Error
	if errors.As(err, &structured) {
		return structured.Clone()
	}
	var transport *gapdb.TransportError
	if errors.As(err, &transport) {
		return &gapdb.Error{Code: gapdb.CodeServerUnavailable, Message: "The Gapdb owner is unavailable.", Retry: gapdb.RetryAfterRestart, Operation: operation, SocketPath: transport.SocketPath, ConnectionReason: "transport_unavailable", SafeActions: []gapdb.SafeAction{gapdb.ActionStatusOffline, gapdb.ActionStartServer, gapdb.ActionAbort}, Cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &gapdb.Error{Code: gapdb.CodeDeadlineExceeded, Message: "The operation deadline elapsed.", Retry: gapdb.RetryAfterReconcile, Operation: operation, Deadline: time.Now().UTC().Format(time.RFC3339Nano), SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionReconcile, gapdb.ActionAbort}, Cause: err}
	}
	return &gapdb.Error{Code: gapdb.CodeInternal, Message: "The command failed safely.", Retry: gapdb.RetryAfterOperator, CorrelationID: "gapctl-local", SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionAbort}, Cause: err}
}

func invalidRequest(field, reason string) *gapdb.Error {
	return &gapdb.Error{Code: gapdb.CodeInvalidRequest, Message: "Command validation failed.", Retry: gapdb.RetryNever, Field: field, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionFixRequest, gapdb.ActionAbort}}
}

func exitCode(code gapdb.ErrorCode) int {
	switch code {
	case gapdb.CodeInvalidRequest, gapdb.CodeUnsupportedVersion, gapdb.CodeFrameTooLarge, gapdb.CodeKeyTooLarge, gapdb.CodeValueTooLarge, gapdb.CodeBatchTooLarge, gapdb.CodeDuplicateKey, gapdb.CodeExpiryNotFuture, gapdb.CodeInvalidCursor:
		return 2
	case gapdb.CodeNotFound, gapdb.CodeAlreadyExists, gapdb.CodeRevisionMismatch, gapdb.CodeConditionFailed, gapdb.CodeScanStale, gapdb.CodeRevisionAhead, gapdb.CodeRevisionCompacted, gapdb.CodeWatchLagged:
		return 3
	case gapdb.CodeOwnerExists, gapdb.CodeServerBusy, gapdb.CodeServerShuttingDown, gapdb.CodeDeadlineExceeded, gapdb.CodeServerUnavailable, gapdb.CodePermissionDenied:
		return 4
	case gapdb.CodeStorageDegraded, gapdb.CodeCorruptIdentity, gapdb.CodeCorruptManifest, gapdb.CodeCorruptSnapshot, gapdb.CodeCorruptWAL, gapdb.CodeUnknownFormat, gapdb.CodeDatabaseIDMismatch, gapdb.CodeRevisionRangeExhausted, gapdb.CodeIOError, gapdb.CodeAdminPreconditionFailed, gapdb.CodeSnapshotInProgress, gapdb.CodeCompactionNotSafe, gapdb.CodeBackupDestinationExists, gapdb.CodeBackupInvalid, gapdb.CodeRecoveryRequired, gapdb.CodeRecoveryActionMismatch:
		return 5
	case gapdb.CodeInternal, gapdb.CodeAuditFailedAfterApply:
		return 6
	default:
		return 6
	}
}
