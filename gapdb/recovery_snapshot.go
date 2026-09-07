package gapdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	RecoverySnapshotSchemaVersion = 1
	HardMaxRecoveryRecords        = 200_000
	HardMaxRecoveryBytes          = 256 << 20
)

var recoverySnapshotMagic = [8]byte{'G', 'D', 'B', 'R', 'E', 'C', '1', '\n'}

// RecoverySnapshotRequest is a read-only, stable-revision export intended for
// bounded recovery engines. It exposes values, never a storage handle.
type RecoverySnapshotRequest struct {
	Prefix           string   `json:"prefix"`
	ExpectedRevision Revision `json:"expected_revision"`
	MaxRecords       int      `json:"max_records"`
	MaxBytes         int      `json:"max_bytes"`
}

// RecoverySnapshotResult owns an immutable sorted record set observed under
// one database read lock.
type RecoverySnapshotResult struct {
	DatabaseID       string
	RequestID        string
	ObservedRevision Revision
	AsOf             time.Time
	Records          []Record
}

func (r RecoverySnapshotResult) Clone() RecoverySnapshotResult {
	clone := r
	clone.Records = make([]Record, len(r.Records))
	for i := range r.Records {
		clone.Records[i] = r.Records[i].Clone()
	}
	return clone
}

func validateRecoverySnapshotRequest(request RecoverySnapshotRequest, limits Limits) error {
	if request.Prefix == "" || len(request.Prefix) > limits.MaxKeyBytes {
		return invalidField("prefix", "must be a bounded non-empty key prefix")
	}
	if request.ExpectedRevision == 0 {
		return invalidField("expected_revision", "must be greater than zero")
	}
	if request.MaxRecords <= 0 || request.MaxRecords > HardMaxRecoveryRecords {
		return invalidField("max_records", "exceeds the recovery record bound")
	}
	if request.MaxBytes <= 0 || request.MaxBytes > HardMaxRecoveryBytes {
		return invalidField("max_bytes", "exceeds the recovery byte bound")
	}
	return nil
}

// ReadRecoverySnapshot reads one bounded binary record snapshot on a dedicated
// connection. The ordinary JSON protocol remains the request and error
// authority; only the successful record body uses the compact codec.
func (c *Client) ReadRecoverySnapshot(ctx context.Context, request RecoverySnapshotRequest) (RecoverySnapshotResult, error) {
	if err := validateRecoverySnapshotRequest(request, c.limits); err != nil {
		return RecoverySnapshotResult{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return RecoverySnapshotResult{}, err
	}
	defer conn.Close()
	req := wireRequest{clientSchemaVersion, c.requestID(ctx), "read_recovery_snapshot", request}
	if err := c.writeRequest(ctx, conn, req); err != nil {
		return RecoverySnapshotResult{}, err
	}
	c.applyDeadline(ctx, conn)
	payload, err := readClientFrame(conn, HardMaxRecoveryBytes)
	if err != nil {
		return RecoverySnapshotResult{}, &TransportError{Operation: req.Operation, SocketPath: c.socketPath, Ambiguous: true, Cause: err}
	}
	if !bytes.HasPrefix(payload, recoverySnapshotMagic[:]) {
		return RecoverySnapshotResult{}, c.decodeRecoverySnapshotFailure(payload, req)
	}
	result, err := DecodeRecoverySnapshot(payload, req.RequestID, request)
	if err != nil {
		return RecoverySnapshotResult{}, c.invalidResult(req.Operation, err)
	}
	return result, nil
}

func (c *Client) decodeRecoverySnapshotFailure(payload []byte, request wireRequest) error {
	if err := validateClientResponse(payload, c.limits); err != nil {
		return c.invalidResult(request.Operation, err)
	}
	var response wireResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || decoder.Decode(&struct{}{}) != io.EOF || response.SchemaVersion == nil || *response.SchemaVersion != clientSchemaVersion || response.OK == nil || *response.OK || response.Operation != request.Operation || response.RequestID != request.RequestID || response.Error == nil {
		return c.invalidResult(request.Operation, errors.New("recovery snapshot failure response is malformed"))
	}
	if err := validateRemoteError(response.Error); err != nil {
		return c.invalidResult(request.Operation, err)
	}
	return response.Error.Clone()
}

// EncodeRecoverySnapshot emits the canonical bounded success frame.
func EncodeRecoverySnapshot(result RecoverySnapshotResult, request RecoverySnapshotRequest) ([]byte, error) {
	if result.DatabaseID == "" || result.RequestID == "" || result.ObservedRevision != request.ExpectedRevision || result.AsOf.IsZero() || len(result.Records) > request.MaxRecords {
		return nil, errors.New("recovery snapshot identity is incomplete")
	}
	var payload bytes.Buffer
	payload.Grow(min(request.MaxBytes, 4<<20))
	payload.Write(recoverySnapshotMagic[:])
	writeUint16 := func(value string) error {
		if value == "" || len(value) > 65535 {
			return errors.New("recovery snapshot identity is unbounded")
		}
		_ = binary.Write(&payload, binary.BigEndian, uint16(len(value)))
		payload.WriteString(value)
		return nil
	}
	if err := writeUint16(result.DatabaseID); err != nil {
		return nil, err
	}
	if err := writeUint16(result.RequestID); err != nil {
		return nil, err
	}
	_ = binary.Write(&payload, binary.BigEndian, uint64(result.ObservedRevision))
	_ = binary.Write(&payload, binary.BigEndian, result.AsOf.UTC().UnixNano())
	_ = binary.Write(&payload, binary.BigEndian, uint32(len(result.Records)))
	previous := ""
	for _, record := range result.Records {
		if record.Key == "" || len(record.Key) > 65535 || record.Key <= previous || !bytes.HasPrefix([]byte(record.Key), []byte(request.Prefix)) || record.Revision == 0 || record.Revision > result.ObservedRevision || len(record.Value) > request.MaxBytes {
			return nil, errors.New("recovery snapshot record is invalid")
		}
		previous = record.Key
		_ = binary.Write(&payload, binary.BigEndian, uint16(len(record.Key)))
		payload.WriteString(record.Key)
		_ = binary.Write(&payload, binary.BigEndian, uint64(record.Revision))
		expires := int64(-1)
		if record.ExpiresAt != nil {
			expires = record.ExpiresAt.UTC().UnixNano()
		}
		_ = binary.Write(&payload, binary.BigEndian, expires)
		_ = binary.Write(&payload, binary.BigEndian, uint32(len(record.Value)))
		payload.Write(record.Value)
		if payload.Len()+sha256.Size > request.MaxBytes {
			return nil, &Error{Code: CodeFrameTooLarge, Message: "Recovery snapshot exceeds the requested byte limit.", Retry: RetryNever, ReceivedBytes: payload.Len() + sha256.Size, MaximumBytes: request.MaxBytes, SafeActions: []SafeAction{ActionReduceRequest, ActionAbort}}
		}
	}
	digest := sha256.Sum256(payload.Bytes())
	payload.Write(digest[:])
	return payload.Bytes(), nil
}

// DecodeRecoverySnapshot strictly decodes and validates one success frame.
func DecodeRecoverySnapshot(payload []byte, expectedRequestID string, request RecoverySnapshotRequest) (RecoverySnapshotResult, error) {
	if len(payload) < len(recoverySnapshotMagic)+sha256.Size || len(payload) > request.MaxBytes || !bytes.Equal(payload[:8], recoverySnapshotMagic[:]) {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot frame is invalid")
	}
	want := sha256.Sum256(payload[:len(payload)-sha256.Size])
	if !bytes.Equal(want[:], payload[len(payload)-sha256.Size:]) {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot digest differs")
	}
	reader := bytes.NewReader(payload[8 : len(payload)-sha256.Size])
	readString := func() (string, error) {
		var size uint16
		if err := binary.Read(reader, binary.BigEndian, &size); err != nil || size == 0 {
			return "", errors.New("recovery snapshot identity is malformed")
		}
		value := make([]byte, int(size))
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		return string(value), nil
	}
	databaseID, err := readString()
	if err != nil {
		return RecoverySnapshotResult{}, err
	}
	requestID, err := readString()
	if err != nil || requestID != expectedRequestID {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot request identity differs")
	}
	var revision uint64
	var asOfNS int64
	var count uint32
	if binary.Read(reader, binary.BigEndian, &revision) != nil || binary.Read(reader, binary.BigEndian, &asOfNS) != nil || binary.Read(reader, binary.BigEndian, &count) != nil || revision != uint64(request.ExpectedRevision) || count == 0 || int(count) > request.MaxRecords {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot header is malformed")
	}
	result := RecoverySnapshotResult{DatabaseID: databaseID, RequestID: requestID, ObservedRevision: Revision(revision), AsOf: time.Unix(0, asOfNS).UTC(), Records: make([]Record, int(count))}
	previous := ""
	for index := range result.Records {
		key, err := readString()
		var recordRevision uint64
		var expiresNS int64
		var valueSize uint32
		if err != nil || binary.Read(reader, binary.BigEndian, &recordRevision) != nil || binary.Read(reader, binary.BigEndian, &expiresNS) != nil || binary.Read(reader, binary.BigEndian, &valueSize) != nil || int(valueSize) > reader.Len() {
			return RecoverySnapshotResult{}, errors.New("recovery snapshot record is truncated")
		}
		value := make([]byte, int(valueSize))
		if _, err := io.ReadFull(reader, value); err != nil {
			return RecoverySnapshotResult{}, err
		}
		if key <= previous || !bytes.HasPrefix([]byte(key), []byte(request.Prefix)) || recordRevision == 0 || recordRevision > revision {
			return RecoverySnapshotResult{}, errors.New("recovery snapshot membership is invalid")
		}
		previous = key
		record := Record{Key: key, Value: value, Revision: Revision(recordRevision)}
		if expiresNS >= 0 {
			expires := time.Unix(0, expiresNS).UTC()
			record.ExpiresAt = &expires
		}
		result.Records[index] = record
	}
	if reader.Len() != 0 || result.AsOf.IsZero() {
		return RecoverySnapshotResult{}, fmt.Errorf("recovery snapshot has %d trailing bytes", reader.Len())
	}
	return result, nil
}
