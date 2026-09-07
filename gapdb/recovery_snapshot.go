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
	"math"
	"strings"
	"time"
	"unicode/utf8"
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

func ValidateRecoverySnapshotRequest(request RecoverySnapshotRequest, limits Limits) error {
	if request.Prefix == "" || len(request.Prefix) > limits.MaxKeyBytes || !utf8.ValidString(request.Prefix) {
		return invalidField("prefix", "must be a bounded non-empty key prefix")
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
	if err := ValidateRecoverySnapshotRequest(request, c.limits); err != nil {
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
	payload, err := readClientFrame(conn, request.MaxBytes)
	if err != nil {
		return RecoverySnapshotResult{}, &TransportError{Operation: req.Operation, SocketPath: c.socketPath, Ambiguous: true, Cause: err}
	}
	if !bytes.HasPrefix(payload, recoverySnapshotMagic[:]) {
		return RecoverySnapshotResult{}, c.decodeRecoverySnapshotFailure(payload, req)
	}
	result, err := decodeRecoverySnapshot(payload, req.RequestID, request, true)
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
	if err := ValidateRecoverySnapshotRequest(request, Limits{MaxKeyBytes: HardMaxKeyBytes}); err != nil {
		return nil, err
	}
	if result.DatabaseID == "" || result.RequestID == "" || result.ObservedRevision != request.ExpectedRevision || result.AsOf.IsZero() || len(result.Records) > request.MaxRecords || uint64(len(result.Records)) > math.MaxUint32 {
		return nil, errors.New("recovery snapshot identity is incomplete")
	}
	asOfNS, ok := recoveryTimeNanoseconds(result.AsOf)
	if !ok {
		return nil, errors.New("recovery snapshot time is unrepresentable")
	}
	var payload bytes.Buffer
	payload.Grow(min(request.MaxBytes, 4<<20))
	payload.Write(recoverySnapshotMagic[:])
	writeUint16 := func(value string) error {
		if value == "" || len(value) > math.MaxUint16 || !utf8.ValidString(value) {
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
	_ = binary.Write(&payload, binary.BigEndian, asOfNS)
	_ = binary.Write(&payload, binary.BigEndian, uint32(len(result.Records)))
	if payload.Len()+sha256.Size > request.MaxBytes {
		return nil, recoverySnapshotTooLarge(payload.Len()+sha256.Size, request.MaxBytes)
	}
	previous := ""
	for _, record := range result.Records {
		if record.Key == "" || len(record.Key) > HardMaxKeyBytes || !utf8.ValidString(record.Key) || record.Key <= previous || !strings.HasPrefix(record.Key, request.Prefix) || record.Revision == 0 || record.Revision > result.ObservedRevision || len(record.Value) > request.MaxBytes || uint64(len(record.Value)) > math.MaxUint32 {
			return nil, errors.New("recovery snapshot record is invalid")
		}
		previous = record.Key
		_ = binary.Write(&payload, binary.BigEndian, uint32(len(record.Key)))
		payload.WriteString(record.Key)
		_ = binary.Write(&payload, binary.BigEndian, uint64(record.Revision))
		expires := int64(-1)
		if record.ExpiresAt != nil {
			var representable bool
			expires, representable = recoveryTimeNanoseconds(*record.ExpiresAt)
			if !representable || expires < 0 || !record.ExpiresAt.After(result.AsOf) {
				return nil, errors.New("recovery snapshot expiry is invalid")
			}
		}
		_ = binary.Write(&payload, binary.BigEndian, expires)
		_ = binary.Write(&payload, binary.BigEndian, uint32(len(record.Value)))
		payload.Write(record.Value)
		if payload.Len()+sha256.Size > request.MaxBytes {
			return nil, recoverySnapshotTooLarge(payload.Len()+sha256.Size, request.MaxBytes)
		}
	}
	digest := sha256.Sum256(payload.Bytes())
	payload.Write(digest[:])
	return payload.Bytes(), nil
}

// DecodeRecoverySnapshot strictly decodes and validates one success frame.
func DecodeRecoverySnapshot(payload []byte, expectedRequestID string, request RecoverySnapshotRequest) (RecoverySnapshotResult, error) {
	return decodeRecoverySnapshot(payload, expectedRequestID, request, false)
}

// decodeRecoverySnapshot may take ownership only of a private client frame.
// The public decoder retains its defensive-copy contract for caller-owned
// input, while the client avoids copying the full validated snapshot again.
func decodeRecoverySnapshot(payload []byte, expectedRequestID string, request RecoverySnapshotRequest, takeOwnership bool) (RecoverySnapshotResult, error) {
	if err := ValidateRecoverySnapshotRequest(request, Limits{MaxKeyBytes: HardMaxKeyBytes}); err != nil {
		return RecoverySnapshotResult{}, err
	}
	if len(payload) < len(recoverySnapshotMagic)+sha256.Size || len(payload) > request.MaxBytes || !bytes.Equal(payload[:8], recoverySnapshotMagic[:]) {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot frame is invalid")
	}
	want := sha256.Sum256(payload[:len(payload)-sha256.Size])
	if !bytes.Equal(want[:], payload[len(payload)-sha256.Size:]) {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot digest differs")
	}
	body := payload[8 : len(payload)-sha256.Size]
	reader := bytes.NewReader(body)
	readIdentity := func() (string, error) {
		var size uint16
		if err := binary.Read(reader, binary.BigEndian, &size); err != nil || size == 0 {
			return "", errors.New("recovery snapshot identity is malformed")
		}
		value := make([]byte, int(size))
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", err
		}
		if !utf8.Valid(value) {
			return "", errors.New("recovery snapshot identity is not UTF-8")
		}
		return string(value), nil
	}
	databaseID, err := readIdentity()
	if err != nil {
		return RecoverySnapshotResult{}, err
	}
	requestID, err := readIdentity()
	if err != nil || requestID != expectedRequestID {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot request identity differs")
	}
	var revision uint64
	var asOfNS int64
	var count uint32
	if binary.Read(reader, binary.BigEndian, &revision) != nil || binary.Read(reader, binary.BigEndian, &asOfNS) != nil || binary.Read(reader, binary.BigEndian, &count) != nil || revision != uint64(request.ExpectedRevision) || uint64(count) > uint64(request.MaxRecords) || uint64(count) > uint64(maxInt()) {
		return RecoverySnapshotResult{}, errors.New("recovery snapshot header is malformed")
	}
	result := RecoverySnapshotResult{DatabaseID: databaseID, RequestID: requestID, ObservedRevision: Revision(revision), AsOf: time.Unix(0, asOfNS).UTC(), Records: make([]Record, int(count))}
	previous := ""
	for index := range result.Records {
		var keySize uint32
		if binary.Read(reader, binary.BigEndian, &keySize) != nil || keySize == 0 || uint64(keySize) > uint64(HardMaxKeyBytes) || uint64(keySize) > uint64(reader.Len()) || uint64(keySize) > uint64(maxInt()) {
			return RecoverySnapshotResult{}, errors.New("recovery snapshot key is malformed")
		}
		keyBytes := make([]byte, int(keySize))
		if _, err := io.ReadFull(reader, keyBytes); err != nil || !utf8.Valid(keyBytes) {
			return RecoverySnapshotResult{}, errors.New("recovery snapshot key is malformed")
		}
		key := string(keyBytes)
		var recordRevision uint64
		var expiresNS int64
		var valueSize uint32
		if binary.Read(reader, binary.BigEndian, &recordRevision) != nil || binary.Read(reader, binary.BigEndian, &expiresNS) != nil || binary.Read(reader, binary.BigEndian, &valueSize) != nil || uint64(valueSize) > uint64(reader.Len()) || uint64(valueSize) > uint64(maxInt()) {
			return RecoverySnapshotResult{}, errors.New("recovery snapshot record is truncated")
		}
		valueOffset := len(body) - reader.Len()
		value := body[valueOffset : valueOffset+int(valueSize) : valueOffset+int(valueSize)]
		if _, err := reader.Seek(int64(valueSize), io.SeekCurrent); err != nil {
			return RecoverySnapshotResult{}, err
		}
		if !takeOwnership {
			value = bytes.Clone(value)
		}
		if key <= previous || !strings.HasPrefix(key, request.Prefix) || recordRevision == 0 || recordRevision > revision {
			return RecoverySnapshotResult{}, errors.New("recovery snapshot membership is invalid")
		}
		previous = key
		record := Record{Key: key, Value: value, Revision: Revision(recordRevision)}
		if expiresNS >= 0 {
			expires := time.Unix(0, expiresNS).UTC()
			if !expires.After(result.AsOf) {
				return RecoverySnapshotResult{}, errors.New("recovery snapshot contains an expired record")
			}
			record.ExpiresAt = &expires
		}
		result.Records[index] = record
	}
	if reader.Len() != 0 || result.AsOf.IsZero() {
		return RecoverySnapshotResult{}, fmt.Errorf("recovery snapshot has %d trailing bytes", reader.Len())
	}
	return result, nil
}

func recoverySnapshotTooLarge(received, maximum int) error {
	return &Error{Code: CodeFrameTooLarge, Message: "Recovery snapshot exceeds the requested byte limit.", Retry: RetryNever, ReceivedBytes: received, MaximumBytes: maximum, SafeActions: []SafeAction{ActionReduceRequest, ActionAbort}}
}

func recoveryTimeNanoseconds(value time.Time) (int64, bool) {
	nanoseconds := value.UTC().UnixNano()
	return nanoseconds, time.Unix(0, nanoseconds).UTC().Equal(value.UTC())
}

func maxInt() int { return int(^uint(0) >> 1) }
