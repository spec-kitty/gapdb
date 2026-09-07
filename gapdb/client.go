package gapdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const clientSchemaVersion = 1

type ClientOptions struct {
	Timeout              time.Duration
	Limits               Limits
	ReuseUnaryConnection bool
}

// TransportError is a local connection or framing error. Ambiguous reports
// that the request may have reached the owner and must be reconciled.
type TransportError struct {
	Operation, SocketPath string
	Ambiguous             bool
	Cause                 error
}

func (e *TransportError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("gapdb transport %s %s: %v", e.Operation, e.SocketPath, e.Cause)
}
func (e *TransportError) Unwrap() error { return e.Cause }

type Client struct {
	socketPath string
	timeout    time.Duration
	limits     Limits
	requests   atomic.Uint64
	watchIDs   atomic.Uint64
	mu         sync.Mutex
	closed     bool
	watches    map[uint64]*watchReservation
	reuseUnary bool
	unaryMu    sync.Mutex
	unaryConn  net.Conn
}

type watchReservation struct {
	cancel context.CancelFunc
	conn   net.Conn
}

type requestIDKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func Dial(socketPath string, options ClientOptions) (*Client, error) {
	if socketPath == "" {
		return nil, &TransportError{Operation: "dial", Cause: errors.New("socket path is required")}
	}
	limits := options.Limits
	if limits == (Limits{}) {
		limits = DefaultOptions().Limits
	}
	if err := (Options{Limits: limits}).Validate(); err != nil {
		return nil, err
	}
	client := &Client{socketPath: socketPath, timeout: options.Timeout, limits: limits, watches: make(map[uint64]*watchReservation), reuseUnary: options.ReuseUnaryConnection}
	conn, err := client.dial(context.Background())
	if err != nil {
		return nil, err
	}
	_ = conn.Close()
	return client, nil
}
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	reservations := c.watches
	c.watches = make(map[uint64]*watchReservation)
	c.mu.Unlock()
	for _, reservation := range reservations {
		reservation.cancel()
		if reservation.conn != nil {
			_ = reservation.conn.Close()
		}
	}
	c.unaryMu.Lock()
	unaryConn := c.unaryConn
	c.unaryConn = nil
	c.unaryMu.Unlock()
	if unaryConn != nil {
		_ = unaryConn.Close()
	}
	return nil
}
func (c *Client) SocketPath() string {
	if c == nil {
		return ""
	}
	return c.socketPath
}

type putArguments struct {
	Key       string     `json:"key"`
	Value     []byte     `json:"value_base64"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Ack       AckMode    `json:"ack"`
}
type casArguments struct {
	Key              string     `json:"key"`
	ExpectedRevision Revision   `json:"expected_revision"`
	Value            []byte     `json:"value_base64"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	Ack              AckMode    `json:"ack"`
}
type deleteArguments struct {
	Key              string   `json:"key"`
	ExpectedRevision Revision `json:"expected_revision"`
	Ack              AckMode  `json:"ack"`
}

func (c *Client) Get(ctx context.Context, key string) (Record, error) {
	if err := (Mutation{Kind: MutationPut, Key: key, Condition: Condition{Kind: ConditionAny}}).Validate(c.limits); err != nil {
		return Record{}, err
	}
	r, err := c.unary(ctx, "get", struct {
		Key string `json:"key"`
	}{key})
	if err != nil {
		return Record{}, err
	}
	var result struct {
		Record Record `json:"record"`
	}
	if err := json.Unmarshal(r.Result, &result); err != nil || result.Record.Revision == 0 {
		return Record{}, c.invalidResult("get", err)
	}
	return result.Record.Clone(), nil
}
func (c *Client) Put(ctx context.Context, key string, value []byte, expiry *time.Time, ack AckMode) (MutationResult, error) {
	if err := NewPutMutation(key, value, Condition{Kind: ConditionAny}, expiry).Validate(c.limits); err != nil {
		return MutationResult{}, err
	}
	return c.mutate(ctx, "put", putArguments{key, append([]byte{}, value...), cloneTime(expiry), ack})
}
func (c *Client) PutIfAbsent(ctx context.Context, key string, value []byte, expiry *time.Time, ack AckMode) (MutationResult, error) {
	if err := NewPutMutation(key, value, Condition{Kind: ConditionAbsent}, expiry).Validate(c.limits); err != nil {
		return MutationResult{}, err
	}
	return c.mutate(ctx, "put_if_absent", putArguments{key, append([]byte{}, value...), cloneTime(expiry), ack})
}
func (c *Client) CompareAndSwap(ctx context.Context, key string, expected Revision, value []byte, expiry *time.Time, ack AckMode) (MutationResult, error) {
	if err := NewPutMutation(key, value, Condition{Kind: ConditionRevision, ExpectedRevision: expected}, expiry).Validate(c.limits); err != nil {
		return MutationResult{}, err
	}
	return c.mutate(ctx, "compare_and_swap", casArguments{key, expected, append([]byte{}, value...), cloneTime(expiry), ack})
}
func (c *Client) DeleteIfRevision(ctx context.Context, key string, expected Revision, ack AckMode) (MutationResult, error) {
	if err := NewDeleteMutation(key, expected).Validate(c.limits); err != nil {
		return MutationResult{}, err
	}
	return c.mutate(ctx, "delete_if_revision", deleteArguments{key, expected, ack})
}
func (c *Client) AtomicBatch(ctx context.Context, batch Batch) (MutationResult, error) {
	copy := batch.Clone()
	if err := copy.Validate(c.limits); err != nil {
		return MutationResult{}, err
	}
	result, err := c.mutate(ctx, "atomic_batch", copy)
	if err != nil {
		return MutationResult{}, err
	}
	if result.MutationCount != len(copy.Mutations) || result.AssertionCount != len(copy.Assertions) {
		return MutationResult{}, c.invalidResult("atomic_batch", errors.New("batch evidence counts do not match the request"))
	}
	return result, nil
}
func (c *Client) mutate(ctx context.Context, op string, args any) (MutationResult, error) {
	if acknowledgement, ok := acknowledgementOf(args); !ok || !acknowledgement.Valid() {
		return MutationResult{}, invalidField("ack", "must be memory or durable")
	}
	r, err := c.unary(ctx, op, args)
	if err != nil {
		return MutationResult{}, err
	}
	var result MutationResult
	if err := json.Unmarshal(r.Result, &result); err != nil || result.Revision == 0 || !result.Ack.Valid() {
		return MutationResult{}, c.invalidResult(op, err)
	}
	return result, nil
}
func (c *Client) ScanPrefix(ctx context.Context, prefix string, limit int, cursor string) (ScanPage, error) {
	if limit <= 0 || limit > c.limits.MaxScanRecords {
		return ScanPage{}, invalidField("limit", "is outside the configured scan limit")
	}
	if len(prefix) > c.limits.MaxKeyBytes {
		return ScanPage{}, &Error{Code: CodeKeyTooLarge, Message: "Prefix exceeds the configured key limit.", Retry: RetryNever, Field: "prefix", ReceivedBytes: len(prefix), MaximumBytes: c.limits.MaxKeyBytes, SafeActions: []SafeAction{ActionReduceKey, ActionAbort}}
	}
	args := struct {
		Prefix string `json:"prefix"`
		Limit  int    `json:"limit"`
		Cursor string `json:"cursor,omitempty"`
	}{prefix, limit, cursor}
	r, err := c.unary(ctx, "scan_prefix", args)
	if err != nil {
		return ScanPage{}, err
	}
	var page ScanPage
	if err := json.Unmarshal(r.Result, &page); err != nil {
		return ScanPage{}, c.invalidResult("scan_prefix", err)
	}
	return page.Clone(), nil
}

type HealthResult struct {
	Lifecycle         LifecycleState `json:"lifecycle"`
	Healthy           bool           `json:"healthy"`
	FailingSubsystems []string       `json:"failing_subsystems"`
	SafeActions       []SafeAction   `json:"safe_actions"`
}
type StatusResult struct {
	Lifecycle              LifecycleState `json:"lifecycle"`
	DatabaseID             string         `json:"database_id"`
	CurrentRevision        Revision       `json:"current_revision"`
	DurableThroughRevision Revision       `json:"durable_through_revision"`
	ReservedRevisionEnd    Revision       `json:"reserved_revision_end"`
	SnapshotRevision       Revision       `json:"snapshot_revision"`
	ActiveWALStart         Revision       `json:"active_wal_start"`
	EarliestWatchRevision  Revision       `json:"earliest_watch_revision"`
	RecordCount            int            `json:"record_count"`
	WatchCount             int            `json:"watch_count"`
	QueueDepth             int            `json:"queue_depth"`
	ActiveClients          int            `json:"active_clients"`
	Limits                 Limits         `json:"limits"`
	OwnerPID               int            `json:"owner_pid"`
	OwnerStartedAt         string         `json:"owner_started_at"`
	SnapshotInProgress     bool           `json:"snapshot_in_progress"`
	BackupInProgress       bool           `json:"backup_in_progress"`
}
type StatsResult struct {
	SchemaVersion         uint16 `json:"schema_version"`
	LiveRecords           int    `json:"live_records"`
	ExpiredRecords        int    `json:"expired_records"`
	ApproximateValueBytes int64  `json:"approximate_value_bytes"`
	Commits               uint64 `json:"commits"`
	ConditionFailures     uint64 `json:"condition_failures"`
	WALBytes              int64  `json:"wal_bytes"`
	SyncCount             uint64 `json:"sync_count"`
	SyncErrors            uint64 `json:"sync_errors"`
	Watchers              int    `json:"watchers"`
	LaggedWatches         uint64 `json:"lagged_watches"`
	Clients               int    `json:"clients"`
	QueueDepth            int    `json:"queue_depth"`
	SnapshotDurationNanos int64  `json:"snapshot_duration_nanos"`
	RecoveryDurationNanos int64  `json:"recovery_duration_nanos"`
}
type ConfigValues struct {
	Limits                Limits `json:"limits"`
	MutationQueueCapacity int    `json:"mutation_queue_capacity"`
	WALBufferBytes        int    `json:"wal_buffer_bytes"`
	AuditMaxLineBytes     int    `json:"audit_max_line_bytes"`
	AuditMaxFileBytes     int64  `json:"audit_max_file_bytes"`
	AuditKeepGenerations  int    `json:"audit_keep_generations"`
	MaxAdminPaths         int    `json:"max_admin_paths"`
}
type ConfigResult struct {
	Effective       ConfigValues `json:"effective"`
	Source          string       `json:"source"`
	SafeCeilings    Limits       `json:"safe_ceilings"`
	RestartRequired bool         `json:"restart_required"`
}
type VerifyResult struct {
	Mode               string   `json:"mode"`
	Verified           bool     `json:"verified"`
	DatabaseID         string   `json:"database_id"`
	ManifestGeneration uint64   `json:"manifest_generation"`
	CurrentRevision    Revision `json:"current_revision"`
	SnapshotRevision   Revision `json:"snapshot_revision"`
	ActiveWALStart     Revision `json:"active_wal_start"`
	CheckedFiles       []string `json:"checked_files"`
}
type SnapshotResult struct {
	Revision    Revision      `json:"revision"`
	Filename    string        `json:"filename"`
	Checksum    string        `json:"checksum"`
	NewWALStart Revision      `json:"new_wal_start"`
	Duration    time.Duration `json:"duration"`
}
type CompactionResult struct {
	BeforeRevision  Revision `json:"before_revision"`
	AfterRevision   Revision `json:"after_revision"`
	Removed         []string `json:"removed"`
	RemovedCount    int      `json:"removed_count"`
	SkippedCount    int      `json:"skipped_count"`
	PathsTruncated  bool     `json:"paths_truncated"`
	DirectorySynced bool     `json:"directory_synced"`
}
type BackupFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type BackupResult struct {
	DatabaseID       string       `json:"database_id"`
	Revision         Revision     `json:"revision"`
	ManifestChecksum string       `json:"manifest_checksum"`
	Files            []BackupFile `json:"files"`
	ByteCount        int64        `json:"byte_count"`
	Verified         bool         `json:"verified"`
}

func (c *Client) Status(ctx context.Context) (StatusResult, error) {
	r, err := c.unary(ctx, "status", struct{}{})
	if err != nil {
		return StatusResult{}, err
	}
	var value StatusResult
	if err := json.Unmarshal(r.Result, &value); err != nil {
		return StatusResult{}, c.invalidResult("status", err)
	}
	return value, nil
}
func (c *Client) Health(ctx context.Context) (HealthResult, error) {
	var result HealthResult
	err := c.admin(ctx, "health", struct{}{}, &result)
	return result, err
}
func (c *Client) Stats(ctx context.Context) (StatsResult, error) {
	var result StatsResult
	err := c.admin(ctx, "stats", struct{}{}, &result)
	return result, err
}
func (c *Client) DescribeConfig(ctx context.Context) (ConfigResult, error) {
	var result ConfigResult
	err := c.admin(ctx, "describe_config", struct{}{}, &result)
	return result, err
}
func (c *Client) Verify(ctx context.Context, mode string) (VerifyResult, error) {
	var result VerifyResult
	err := c.admin(ctx, "verify", struct {
		Mode string `json:"mode,omitempty"`
	}{mode}, &result)
	return result, err
}
func (c *Client) CreateSnapshot(ctx context.Context, id string, revision Revision) (SnapshotResult, error) {
	var result SnapshotResult
	err := c.admin(ctx, "create_snapshot", struct {
		ExpectedDatabaseID string   `json:"expected_database_id"`
		ExpectedRevision   Revision `json:"expected_revision"`
	}{id, revision}, &result)
	return result, err
}
func (c *Client) Compact(ctx context.Context, id string, through Revision) (CompactionResult, error) {
	var result CompactionResult
	err := c.admin(ctx, "compact", struct {
		ExpectedDatabaseID string   `json:"expected_database_id"`
		ThroughRevision    Revision `json:"through_revision"`
	}{id, through}, &result)
	return result, err
}
func (c *Client) Backup(ctx context.Context, id string, revision Revision, destination string) (BackupResult, error) {
	var result BackupResult
	err := c.admin(ctx, "backup", struct {
		ExpectedDatabaseID string   `json:"expected_database_id"`
		ExpectedRevision   Revision `json:"expected_revision"`
		Destination        string   `json:"destination"`
	}{id, revision, destination}, &result)
	return result, err
}
func (c *Client) admin(ctx context.Context, op string, args, result any) error {
	r, err := c.unary(ctx, op, args)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(r.Result, result); err != nil {
		return c.invalidResult(op, err)
	}
	return nil
}

type Watch struct {
	RegistrationRevision Revision
	Events               <-chan ChangeEvent
	Ended                <-chan WatchTermination
	cancel               context.CancelFunc
	once                 sync.Once
	client               *Client
	id                   uint64
}

func (w *Watch) Close() {
	if w != nil {
		w.once.Do(func() {
			if w.client != nil {
				w.client.cancelWatch(w.id)
			} else {
				w.cancel()
			}
		})
	}
}
func (c *Client) Watch(ctx context.Context, prefix string, after Revision) (*Watch, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	wctx, cancel := context.WithCancel(ctx)
	id := c.watchIDs.Add(1)
	reservation := &watchReservation{cancel: cancel}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return nil, &TransportError{Operation: "watch", SocketPath: c.socketPath, Cause: errors.New("client is closed")}
	}
	c.watches[id] = reservation
	c.mu.Unlock()
	rollback := func() {
		c.cancelWatch(id)
	}
	conn, err := c.dialReserved(wctx, id, reservation)
	if err != nil {
		rollback()
		return nil, err
	}
	req := wireRequest{clientSchemaVersion, c.requestID(ctx), "watch", struct {
		Prefix string   `json:"prefix"`
		After  Revision `json:"after_revision"`
	}{prefix, after}}
	if err := c.writeRequest(ctx, conn, req); err != nil {
		rollback()
		return nil, err
	}
	response, err := c.readResponse(wctx, conn, "watch", req.RequestID, true)
	if err != nil {
		rollback()
		return nil, err
	}
	if response.Stream == "ended" && response.Error != nil {
		rollback()
		return nil, response.Error.Clone()
	}
	if response.Stream != "started" || response.RegistrationRevision == nil {
		rollback()
		return nil, c.invalidResult("watch", nil)
	}
	c.mu.Lock()
	_, registered := c.watches[id]
	closed := c.closed
	c.mu.Unlock()
	if closed || !registered || wctx.Err() != nil {
		rollback()
		return nil, &TransportError{Operation: "watch", SocketPath: c.socketPath, Cause: errors.New("client is closed")}
	}
	events := make(chan ChangeEvent)
	ended := make(chan WatchTermination, 1)
	watch := &Watch{RegistrationRevision: *response.RegistrationRevision, Events: events, Ended: ended, cancel: cancel, client: c, id: id}
	go c.receiveWatch(wctx, conn, req, events, ended, after, id)
	return watch, nil
}
func (c *Client) receiveWatch(ctx context.Context, conn net.Conn, req wireRequest, events chan<- ChangeEvent, ended chan<- WatchTermination, after Revision, watchID uint64) {
	defer close(events)
	defer close(ended)
	defer conn.Close()
	defer c.removeWatch(watchID)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	last := after
	for {
		response, err := c.readResponse(ctx, conn, "watch", req.RequestID, false)
		if err != nil {
			if ctx.Err() != nil {
				ended <- WatchTermination{Reason: WatchEndedByClient, LastDeliveredRevision: last}
			} else {
				ended <- WatchTermination{Reason: WatchEndedByError, LastDeliveredRevision: last, Error: unavailableWatchError(c.socketPath, err)}
			}
			return
		}
		switch response.Stream {
		case "event":
			if response.Event == nil {
				ended <- WatchTermination{Reason: WatchEndedByError, LastDeliveredRevision: last, Error: unavailableWatchError(c.socketPath, errors.New("event missing"))}
				return
			}
			event := response.Event.Clone()
			select {
			case events <- event:
				last = event.Revision
			case <-ctx.Done():
				ended <- WatchTermination{Reason: WatchEndedByClient, LastDeliveredRevision: last}
				return
			}
		case "ended":
			termination := WatchTermination{Reason: response.Reason, LastDeliveredRevision: last, Error: response.Error}
			if response.Error != nil && response.Error.LastDeliveredRevision != nil {
				termination.LastDeliveredRevision = *response.Error.LastDeliveredRevision
			}
			if termination.Reason == "" {
				termination.Reason = WatchEndedByError
			}
			ended <- termination
			return
		default:
			ended <- WatchTermination{Reason: WatchEndedByError, LastDeliveredRevision: last, Error: unavailableWatchError(c.socketPath, errors.New("invalid stream frame"))}
			return
		}
	}
}

type wireRequest struct {
	SchemaVersion uint64 `json:"schema_version"`
	RequestID     string `json:"request_id,omitempty"`
	Operation     string `json:"operation"`
	Arguments     any    `json:"arguments"`
}
type wireResponse struct {
	SchemaVersion        *uint64         `json:"schema_version"`
	OK                   *bool           `json:"ok"`
	RequestID            string          `json:"request_id,omitempty"`
	DatabaseID           string          `json:"database_id"`
	Operation            string          `json:"operation"`
	Result               json.RawMessage `json:"result,omitempty"`
	Stream               string          `json:"stream,omitempty"`
	RegistrationRevision *Revision       `json:"registration_revision,omitempty"`
	Event                *ChangeEvent    `json:"event,omitempty"`
	Reason               WatchEndReason  `json:"reason,omitempty"`
	Error                *Error          `json:"error,omitempty"`
}

func (c *Client) unary(ctx context.Context, op string, args any) (wireResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.reuseUnary {
		return c.reusableUnary(ctx, op, args)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return wireResponse{}, err
	}
	defer conn.Close()
	req := wireRequest{clientSchemaVersion, c.requestID(ctx), op, args}
	if err := c.writeRequest(ctx, conn, req); err != nil {
		return wireResponse{}, err
	}
	return c.readResponse(ctx, conn, op, req.RequestID, true)
}

// reusableUnary serializes request/response pairs on one connection. The
// owner protocol already permits multiple unary requests per connection; the
// serialization prevents response ambiguity without widening mutation retry
// semantics. Any transport or framing failure poisons and closes the
// connection. A mutation is never replayed automatically.
func (c *Client) reusableUnary(ctx context.Context, op string, args any) (wireResponse, error) {
	c.unaryMu.Lock()
	defer c.unaryMu.Unlock()
	conn := c.unaryConn
	if conn == nil {
		var err error
		conn, err = c.dial(ctx)
		if err != nil {
			return wireResponse{}, err
		}
		c.unaryConn = conn
	}
	req := wireRequest{clientSchemaVersion, c.requestID(ctx), op, args}
	if err := c.writeRequest(ctx, conn, req); err != nil {
		c.discardUnaryConnection(conn)
		return wireResponse{}, err
	}
	response, err := c.readResponse(ctx, conn, op, req.RequestID, true)
	var transport *TransportError
	if errors.As(err, &transport) {
		c.discardUnaryConnection(conn)
	}
	return response, err
}

// discardUnaryConnection is called only while unaryMu is held.
func (c *Client) discardUnaryConnection(conn net.Conn) {
	if c.unaryConn == conn {
		c.unaryConn = nil
	}
	_ = conn.Close()
}
func (c *Client) requestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return fmt.Sprintf("go-%d", c.requests.Add(1))
}
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c == nil {
		return nil, &TransportError{Operation: "dial", SocketPath: c.SocketPath(), Cause: errors.New("client is closed")}
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, &TransportError{Operation: "dial", SocketPath: c.SocketPath(), Cause: errors.New("client is closed")}
	}
	conn, err := (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, &TransportError{Operation: "dial", SocketPath: c.socketPath, Cause: err}
	}
	return conn, nil
}

func (c *Client) dialReserved(ctx context.Context, id uint64, expected *watchReservation) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, &TransportError{Operation: "dial", SocketPath: c.socketPath, Cause: err}
	}
	c.mu.Lock()
	reservation, present := c.watches[id]
	if c.closed || !present || reservation != expected {
		c.mu.Unlock()
		_ = conn.Close()
		return nil, &TransportError{Operation: "watch", SocketPath: c.socketPath, Cause: errors.New("client is closed")}
	}
	reservation.conn = conn
	c.mu.Unlock()
	return conn, nil
}

func (c *Client) removeWatch(id uint64) {
	c.mu.Lock()
	delete(c.watches, id)
	c.mu.Unlock()
}

func (c *Client) cancelWatch(id uint64) {
	c.mu.Lock()
	reservation := c.watches[id]
	delete(c.watches, id)
	c.mu.Unlock()
	if reservation != nil {
		reservation.cancel()
		if reservation.conn != nil {
			_ = reservation.conn.Close()
		}
	}
}
func (c *Client) writeRequest(ctx context.Context, conn net.Conn, req wireRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(payload) > c.limits.MaxFrameBytes {
		return &Error{Code: CodeFrameTooLarge, Message: "Protocol frame exceeds the configured limit.", Retry: RetryNever, ReceivedBytes: len(payload), MaximumBytes: c.limits.MaxFrameBytes, SafeActions: []SafeAction{ActionReduceRequest, ActionAbort}}
	}
	c.applyDeadline(ctx, conn)
	if err := writeClientFrame(conn, payload); err != nil {
		return &TransportError{Operation: req.Operation, SocketPath: c.socketPath, Ambiguous: true, Cause: err}
	}
	return nil
}
func (c *Client) readResponse(ctx context.Context, conn net.Conn, op, id string, deadline bool) (wireResponse, error) {
	if deadline {
		c.applyDeadline(ctx, conn)
	} else if value, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(value)
	} else {
		_ = conn.SetReadDeadline(time.Time{})
	}
	payload, err := readClientFrame(conn, c.limits.MaxFrameBytes)
	if err != nil {
		return wireResponse{}, &TransportError{Operation: op, SocketPath: c.socketPath, Ambiguous: true, Cause: err}
	}
	if err := validateClientResponse(payload); err != nil {
		return wireResponse{}, c.invalidResult(op, err)
	}
	var response wireResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&response)
	if decodeErr == nil && decoder.Decode(&struct{}{}) != io.EOF {
		decodeErr = errors.New("response has trailing JSON")
	}
	preAdmissionBusy := response.Error != nil && response.Error.Code == CodeServerBusy
	if decodeErr != nil || response.SchemaVersion == nil || *response.SchemaVersion != clientSchemaVersion || response.OK == nil || response.DatabaseID == "" || (!preAdmissionBusy && (response.Operation != op || response.RequestID != id)) {
		return wireResponse{}, c.invalidResult(op, decodeErr)
	}
	if !*response.OK {
		if response.Error == nil {
			return wireResponse{}, c.invalidResult(op, errors.New("error missing"))
		}
		if err := validateRemoteError(response.Error); err != nil {
			return wireResponse{}, c.invalidResult(op, err)
		}
		if op == "watch" && response.Stream == "ended" {
			return response, nil
		}
		return wireResponse{}, response.Error.Clone()
	}
	return response, nil
}
func (c *Client) applyDeadline(ctx context.Context, conn net.Conn) {
	deadline, ok := ctx.Deadline()
	if c.timeout > 0 {
		candidate := time.Now().Add(c.timeout)
		if !ok || candidate.Before(deadline) {
			deadline, ok = candidate, true
		}
	}
	if ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Time{})
	}
}
func (c *Client) invalidResult(op string, cause error) error {
	if cause == nil {
		cause = errors.New("server response does not match request")
	}
	return &TransportError{Operation: op, SocketPath: c.socketPath, Ambiguous: true, Cause: cause}
}
func readClientFrame(reader io.Reader, maximum int) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint32(prefix[:]))
	if length <= 0 || length > maximum {
		return nil, fmt.Errorf("invalid frame length %d", length)
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(reader, payload)
	return payload, err
}
func writeClientFrame(writer io.Writer, payload []byte) error {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	for _, value := range [][]byte{prefix[:], payload} {
		for len(value) > 0 {
			n, err := writer.Write(value)
			if n < 0 || n > len(value) {
				return io.ErrShortWrite
			}
			value = value[n:]
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
		}
	}
	return nil
}
func unavailableWatchError(socket string, err error) *Error {
	return &Error{Code: CodeServerUnavailable, Message: "The watch transport became unavailable.", Retry: RetryAfterRestart, SocketPath: socket, ConnectionReason: err.Error(), SafeActions: []SafeAction{ActionStatusOffline, ActionStartServer, ActionAbort}, Cause: err}
}

func acknowledgementOf(arguments any) (AckMode, bool) {
	switch value := arguments.(type) {
	case putArguments:
		return value.Ack, true
	case casArguments:
		return value.Ack, true
	case deleteArguments:
		return value.Ack, true
	case Batch:
		return value.Ack, true
	default:
		return "", false
	}
}

func validateRemoteError(remote *Error) error {
	if remote == nil || remote.Message == "" {
		return errors.New("remote error is incomplete")
	}
	for _, definition := range ErrorDefinitions() {
		if definition.Code != remote.Code {
			continue
		}
		if definition.Retry != remote.Retry || len(definition.SafeActions) != len(remote.SafeActions) {
			return errors.New("remote error classification does not match its code")
		}
		for index := range definition.SafeActions {
			if definition.SafeActions[index] != remote.SafeActions[index] {
				return errors.New("remote error actions do not match its code")
			}
		}
		return nil
	}
	return errors.New("remote error code is unknown")
}
