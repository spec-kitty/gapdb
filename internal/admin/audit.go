package admin

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/persist"
)

const AuditFilename = "audit.jsonl"

var auditCRC = crc32.MakeTable(crc32.Castagnoli)

type AuditEntry struct {
	SchemaVersion  uint16
	EventID        string
	Timestamp      time.Time
	DatabaseID     persist.DatabaseID
	Operation      string
	RequestID      string
	BeforeRevision gapdb.Revision
	AfterRevision  gapdb.Revision
	Outcome        string
	ErrorCode      gapdb.ErrorCode
	Paths          []string
	SafeActions    []gapdb.SafeAction
}
type auditBase struct {
	SchemaVersion  uint16             `json:"schema_version"`
	EventID        string             `json:"event_id"`
	Timestamp      string             `json:"timestamp"`
	DatabaseID     string             `json:"database_id"`
	Operation      string             `json:"operation"`
	RequestID      string             `json:"request_id"`
	BeforeRevision gapdb.Revision     `json:"before_revision"`
	AfterRevision  gapdb.Revision     `json:"after_revision"`
	Outcome        string             `json:"outcome"`
	ErrorCode      gapdb.ErrorCode    `json:"error_code,omitempty"`
	Paths          []string           `json:"paths"`
	SafeActions    []gapdb.SafeAction `json:"safe_actions"`
}
type auditWire struct {
	SchemaVersion  uint16             `json:"schema_version"`
	EventID        string             `json:"event_id"`
	Timestamp      string             `json:"timestamp"`
	DatabaseID     string             `json:"database_id"`
	Operation      string             `json:"operation"`
	RequestID      string             `json:"request_id"`
	BeforeRevision gapdb.Revision     `json:"before_revision"`
	AfterRevision  gapdb.Revision     `json:"after_revision"`
	Outcome        string             `json:"outcome"`
	ErrorCode      gapdb.ErrorCode    `json:"error_code,omitempty"`
	Paths          []string           `json:"paths"`
	SafeActions    []gapdb.SafeAction `json:"safe_actions"`
	CRC32C         string             `json:"crc32c"`
}
type AuditOptions struct {
	MaxLineBytes    int
	MaxFileBytes    int64
	KeepGenerations int
}

func encodeAudit(entry AuditEntry, maximum int) ([]byte, error) {
	if entry.SchemaVersion != 1 || entry.EventID == "" || entry.Timestamp.IsZero() || entry.DatabaseID.IsZero() || entry.Operation == "" || entry.RequestID == "" || entry.Outcome == "" {
		return nil, fmt.Errorf("required audit fields are missing")
	}
	for _, path := range entry.Paths {
		if filepath.IsAbs(path) || filepath.Base(path) != path {
			return nil, fmt.Errorf("audit path is not a safe relative base name")
		}
	}
	base := auditBase{entry.SchemaVersion, entry.EventID, entry.Timestamp.UTC().Format(time.RFC3339Nano), entry.DatabaseID.String(), entry.Operation, entry.RequestID, entry.BeforeRevision, entry.AfterRevision, entry.Outcome, entry.ErrorCode, append([]string(nil), entry.Paths...), append([]gapdb.SafeAction(nil), entry.SafeActions...)}
	without, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	checksum := crc32.Checksum(without, auditCRC)
	wire := auditWire{base.SchemaVersion, base.EventID, base.Timestamp, base.DatabaseID, base.Operation, base.RequestID, base.BeforeRevision, base.AfterRevision, base.Outcome, base.ErrorCode, base.Paths, base.SafeActions, fmt.Sprintf("%08x", checksum)}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if maximum <= 0 || len(encoded) > maximum {
		return nil, fmt.Errorf("audit line exceeds %d bytes", maximum)
	}
	return encoded, nil
}

func AppendAudit(fsys faultfs.FS, directory string, entry AuditEntry, options AuditOptions) error {
	if options.MaxLineBytes <= 0 {
		options.MaxLineBytes = 64 << 10
	}
	if options.MaxFileBytes < int64(options.MaxLineBytes) {
		options.MaxFileBytes = 4 << 20
	}
	if options.KeepGenerations < 1 {
		options.KeepGenerations = 1
	}
	line, err := encodeAudit(entry, options.MaxLineBytes)
	if err != nil {
		return auditIO("audit_encode", err)
	}
	path := filepath.Join(directory, AuditFilename)
	if info, statErr := fsys.Stat(faultfs.PointStat, path); statErr == nil && info.Size()+int64(len(line)) > options.MaxFileBytes {
		if err := rotateAudit(fsys, directory, options.KeepGenerations); err != nil {
			return err
		}
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return auditIO("audit_stat", statErr)
	}
	file, err := fsys.OpenFile(faultfs.PointAuditAppend, path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return auditIO("audit_open", err)
	}
	defer file.Close()
	for len(line) > 0 {
		n, writeErr := fsys.Write(faultfs.PointAuditAppend, file, line)
		if n < 0 || n > len(line) {
			return auditIO("audit_append", io.ErrShortWrite)
		}
		line = line[n:]
		if writeErr != nil {
			return auditIO("audit_append", writeErr)
		}
		if n == 0 {
			return auditIO("audit_append", io.ErrShortWrite)
		}
	}
	if err := fsys.Sync(faultfs.PointAuditSync, file); err != nil {
		return auditIO("audit_sync", err)
	}
	return nil
}

func AppendAuditAfterApply(fsys faultfs.FS, directory string, entry AuditEntry, options AuditOptions) error {
	if err := AppendAudit(fsys, directory, entry, options); err != nil {
		return &gapdb.Error{Code: gapdb.CodeAuditFailedAfterApply, Message: "The operation applied but its audit record could not be made durable.", Retry: gapdb.RetryAfterReconcile, OperationApplied: true, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionRepairAuditStorage, gapdb.ActionAbort}, Cause: err}
	}
	return nil
}

func VerifyAudit(reader io.Reader, maximum int) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), maximum)
	line := 0
	for scanner.Scan() {
		line++
		value := bytes.Clone(scanner.Bytes())
		var wire auditWire
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&wire); err != nil {
			return fmt.Errorf("audit line %d: %w", line, err)
		}
		checksum, err := hex.DecodeString(wire.CRC32C)
		if err != nil || len(checksum) != 4 {
			return fmt.Errorf("audit line %d: invalid crc", line)
		}
		base := auditBase{wire.SchemaVersion, wire.EventID, wire.Timestamp, wire.DatabaseID, wire.Operation, wire.RequestID, wire.BeforeRevision, wire.AfterRevision, wire.Outcome, wire.ErrorCode, wire.Paths, wire.SafeActions}
		encoded, _ := json.Marshal(base)
		if fmt.Sprintf("%08x", crc32.Checksum(encoded, auditCRC)) != wire.CRC32C {
			return fmt.Errorf("audit line %d: crc mismatch", line)
		}
		canonical, _ := json.Marshal(wire)
		if !bytes.Equal(canonical, value) {
			return fmt.Errorf("audit line %d: noncanonical", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func rotateAudit(fsys faultfs.FS, directory string, keep int) error {
	for generation := keep - 1; generation >= 1; generation-- {
		old := filepath.Join(directory, AuditFilename+"."+strconv.Itoa(generation))
		next := filepath.Join(directory, AuditFilename+"."+strconv.Itoa(generation+1))
		if _, err := fsys.Stat(faultfs.PointStat, old); err == nil {
			if generation+1 > keep {
				if err := fsys.Remove(faultfs.PointAuditAppend, old); err != nil {
					return auditIO("audit_rotate_remove", err)
				}
			} else if err := fsys.Rename(faultfs.PointAuditAppend, old, next); err != nil {
				return auditIO("audit_rotate", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return auditIO("audit_rotate_stat", err)
		}
	}
	current := filepath.Join(directory, AuditFilename)
	if _, err := fsys.Stat(faultfs.PointStat, current); err == nil {
		if err := fsys.Rename(faultfs.PointAuditAppend, current, current+".1"); err != nil {
			return auditIO("audit_rotate", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return auditIO("audit_rotate_stat", err)
	}
	if err := fsys.SyncDir(faultfs.PointAuditSync, directory); err != nil {
		return auditIO("audit_rotate_sync", err)
	}
	return nil
}
func auditIO(operation string, cause error) error {
	return &gapdb.Error{Code: gapdb.CodeIOError, Message: "An audit storage operation failed.", Retry: gapdb.RetryAfterOperator, Operation: operation, Path: AuditFilename, OSErrorCategory: "other", SafeActions: []gapdb.SafeAction{gapdb.ActionCheckStorage, gapdb.ActionVerify, gapdb.ActionAbort}, Cause: cause}
}
