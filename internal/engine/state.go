package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"gapdb/gapdb"
	"gapdb/internal/clock"
	"gapdb/internal/faultfs"
	"gapdb/internal/persist"
)

const defaultQueueCapacity = 64

type CommitLog interface {
	Append(persist.CommitFrame) error
	Barrier() error
	DurableThrough() gapdb.Revision
}

type Config struct {
	Limits                 gapdb.Limits
	QueueCapacity          int
	Clock                  clock.Clock
	ExpiryTimer            ExpiryTimer
	Log                    CommitLog
	Allocator              RevisionAllocator
	Records                []gapdb.Record
	CurrentRevision        gapdb.Revision
	DurableThroughRevision gapdb.Revision
	DatabaseID             persist.DatabaseID
	SnapshotRevision       gapdb.Revision
	RecoveredCommits       []persist.CommitFrame
	ReservedRevisionEnd    gapdb.Revision
	ActiveWALStart         gapdb.Revision
	Faults                 faultfs.FS
}

type DatabaseState struct {
	mu             sync.RWMutex
	records        map[string]gapdb.Record
	current        gapdb.Revision
	durableThrough gapdb.Revision
	lifecycle      gapdb.LifecycleState

	limits              gapdb.Limits
	clock               *clock.Nondecreasing
	log                 CommitLog
	allocator           RevisionAllocator
	faults              faultfs.FS
	databaseID          persist.DatabaseID
	snapshotRevision    gapdb.Revision
	reservedRevisionEnd gapdb.Revision
	activeWALStart      gapdb.Revision
	activeWatches       atomic.Int64
	expiries            expiryHeap
	expiryByKey         map[string]*expiryCandidate
	expiryTimer         ExpiryTimer
	history             historyLog
	watchers            map[uint64]*watcherState
	nextWatcherID       uint64

	admissionMu sync.RWMutex
	queue       chan command
	done        chan struct{}
	closeOnce   sync.Once
	closeErr    error
	closed      bool
}

func New(config Config) (*DatabaseState, error) {
	if config.Limits == (gapdb.Limits{}) {
		config.Limits = gapdb.DefaultOptions().Limits
	}
	if err := (gapdb.Options{Limits: config.Limits}).Validate(); err != nil {
		return nil, err
	}
	if config.QueueCapacity == 0 {
		config.QueueCapacity = defaultQueueCapacity
	}
	if config.QueueCapacity < 0 {
		return nil, invalidField("queue_capacity", "must be greater than zero")
	}
	if config.Clock == nil {
		config.Clock = clock.Real{}
	}
	if config.Log == nil {
		return nil, invalidField("log", "must be configured")
	}
	if config.Allocator == nil {
		return nil, invalidField("allocator", "must be configured")
	}
	if config.DurableThroughRevision > config.CurrentRevision {
		return nil, invalidField("durable_through_revision", "must not exceed current_revision")
	}
	if config.SnapshotRevision > config.CurrentRevision {
		return nil, invalidField("snapshot_revision", "must not exceed current_revision")
	}

	state := &DatabaseState{
		records:             make(map[string]gapdb.Record, len(config.Records)),
		current:             config.CurrentRevision,
		durableThrough:      config.DurableThroughRevision,
		lifecycle:           gapdb.LifecycleReady,
		limits:              config.Limits,
		clock:               clock.NewNondecreasing(config.Clock),
		log:                 config.Log,
		allocator:           config.Allocator,
		faults:              config.Faults,
		databaseID:          config.DatabaseID,
		snapshotRevision:    config.SnapshotRevision,
		reservedRevisionEnd: config.ReservedRevisionEnd,
		activeWALStart:      config.ActiveWALStart,
		expiryByKey:         make(map[string]*expiryCandidate),
		expiryTimer:         config.ExpiryTimer,
		watchers:            make(map[uint64]*watcherState),
		queue:               make(chan command, config.QueueCapacity),
		done:                make(chan struct{}),
	}
	if state.expiryTimer == nil {
		if _, realClock := config.Clock.(clock.Real); realClock {
			state.expiryTimer = newSystemExpiryTimer()
		} else {
			state.expiryTimer = dormantExpiryTimer{}
		}
	}
	state.history.initialize(config.Limits, config.SnapshotRevision)
	maximumCleanupBytes := config.Limits.MaxBatchBytes
	if config.Limits.MaxFrameBytes < maximumCleanupBytes {
		maximumCleanupBytes = config.Limits.MaxFrameBytes
	}
	for _, record := range config.Records {
		if err := validateKey(record.Key, config.Limits); err != nil {
			state.expiryTimer.Stop()
			return nil, fmt.Errorf("initial record: %w", err)
		}
		if record.Revision == 0 || record.Revision > config.CurrentRevision {
			state.expiryTimer.Stop()
			return nil, invalidField("records.revision", "must be nonzero and not exceed current_revision")
		}
		if len(record.Value) > config.Limits.MaxValueBytes {
			state.expiryTimer.Stop()
			return nil, valueTooLarge(len(record.Value), config.Limits.MaxValueBytes)
		}
		if record.ExpiresAt != nil {
			cleanupBytes := 32 + 20 + len(record.Key)
			if cleanupBytes > maximumCleanupBytes {
				state.expiryTimer.Stop()
				return nil, batchTooLarge(cleanupBytes, maximumCleanupBytes)
			}
		}
		if _, exists := state.records[record.Key]; exists {
			state.expiryTimer.Stop()
			return nil, invalidField("records.key", "must be unique")
		}
		state.records[record.Key] = record.Clone()
		state.scheduleRecordExpiry(record)
	}
	if err := state.history.rebuild(config.RecoveredCommits, config.CurrentRevision); err != nil {
		state.expiryTimer.Stop()
		return nil, err
	}
	state.resetExpiryTimer()
	go state.runWriter()
	return state, nil
}

func (state *DatabaseState) Get(key string) (gapdb.Record, error) {
	if err := validateKey(key, state.limits); err != nil {
		return gapdb.Record{}, err
	}
	now := state.clock.Now()
	state.mu.RLock()
	record, exists := state.records[key]
	current := state.current
	state.mu.RUnlock()
	if !exists || recordExpired(record, now) {
		return gapdb.Record{}, notFound(key, current)
	}
	return record.Clone(), nil
}

func (state *DatabaseState) CurrentRevision() gapdb.Revision {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.current
}

func (state *DatabaseState) DurableThroughRevision() gapdb.Revision {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.durableThrough
}

func (state *DatabaseState) Lifecycle() gapdb.LifecycleState {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.lifecycle
}

func (state *DatabaseState) Close(ctx context.Context) error {
	state.closeOnce.Do(func() {
		state.admissionMu.Lock()
		state.closed = true
		state.mu.Lock()
		if state.lifecycle == gapdb.LifecycleReady {
			state.lifecycle = gapdb.LifecycleDraining
		}
		state.mu.Unlock()
		close(state.queue)
		state.admissionMu.Unlock()
	})
	select {
	case <-state.done:
		return state.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (state *DatabaseState) submit(ctx context.Context, batch gapdb.Batch, single singleOperation) (CommitResult, error) {
	select {
	case <-ctx.Done():
		return CommitResult{}, ctx.Err()
	default:
	}
	response := make(chan commandResult, 1)
	cmd := command{batch: batch, single: single, response: response}

	state.admissionMu.RLock()
	state.mu.RLock()
	lifecycle := state.lifecycle
	state.mu.RUnlock()
	if lifecycle != gapdb.LifecycleReady {
		state.admissionMu.RUnlock()
		return CommitResult{}, state.mutationUnavailable(lifecycle)
	}
	select {
	case state.queue <- cmd:
		state.admissionMu.RUnlock()
	case <-ctx.Done():
		state.admissionMu.RUnlock()
		return CommitResult{}, ctx.Err()
	default:
		depth := len(state.queue)
		state.admissionMu.RUnlock()
		return CommitResult{}, serverBusy(depth, cap(state.queue))
	}
	result := <-response
	return result.result, result.err
}

func (state *DatabaseState) mutationUnavailable(lifecycle gapdb.LifecycleState) error {
	if lifecycle == gapdb.LifecycleDegradedReadOnly {
		current, durable := state.revisions()
		return storageDegraded("writer_disabled", current, durable, false, nil)
	}
	return &gapdb.Error{
		Code:        gapdb.CodeServerShuttingDown,
		Message:     "The owner is not accepting new mutations.",
		Retry:       gapdb.RetryAfterRestart,
		Lifecycle:   lifecycle,
		SafeActions: []gapdb.SafeAction{gapdb.ActionWaitForRestart, gapdb.ActionAbort},
	}
}

func (state *DatabaseState) revisions() (gapdb.Revision, gapdb.Revision) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.current, state.durableThrough
}

func (state *DatabaseState) degrade() {
	state.mu.Lock()
	state.lifecycle = gapdb.LifecycleDegradedReadOnly
	state.mu.Unlock()
}

func validateKey(key string, limits gapdb.Limits) error {
	if key == "" {
		return invalidField("key", "must not be empty")
	}
	if !utf8.ValidString(key) {
		return invalidField("key", "must be valid UTF-8")
	}
	if len(key) > limits.MaxKeyBytes {
		return &gapdb.Error{Code: gapdb.CodeKeyTooLarge, Message: "Key exceeds the configured limit.", Retry: gapdb.RetryNever, Field: "key", ReceivedBytes: len(key), MaximumBytes: limits.MaxKeyBytes, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceKey, gapdb.ActionAbort}}
	}
	return nil
}

func invalidField(field, reason string) error {
	return &gapdb.Error{Code: gapdb.CodeInvalidRequest, Message: "Request validation failed.", Retry: gapdb.RetryNever, Field: field, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionFixRequest, gapdb.ActionAbort}}
}

func valueTooLarge(received, maximum int) error {
	return &gapdb.Error{Code: gapdb.CodeValueTooLarge, Message: "Value exceeds the configured limit.", Retry: gapdb.RetryNever, Field: "value_base64", ReceivedBytes: received, MaximumBytes: maximum, SafeActions: []gapdb.SafeAction{gapdb.ActionReduceValue, gapdb.ActionAbort}}
}

func notFound(key string, current gapdb.Revision) error {
	copyCurrent := current
	return &gapdb.Error{Code: gapdb.CodeNotFound, Message: "The key is not present at the current effective time.", Retry: gapdb.RetryAfterReconcile, Key: key, CurrentRevision: &copyCurrent, SafeActions: []gapdb.SafeAction{gapdb.ActionGet, gapdb.ActionPutIfAbsent, gapdb.ActionAbort}}
}

func serverBusy(depth, maximum int) error {
	active := 1
	return &gapdb.Error{Code: gapdb.CodeServerBusy, Message: "The mutation queue is full.", Retry: gapdb.RetryImmediate, ActiveClients: &active, MaximumClients: &maximum, QueueDepth: &depth, SafeActions: []gapdb.SafeAction{gapdb.ActionRetryWithBackoff, gapdb.ActionAbort}}
}

func storageDegraded(stage string, current, durable gapdb.Revision, operationApplied bool, cause error) error {
	currentCopy, durableCopy := current, durable
	return &gapdb.Error{Code: gapdb.CodeStorageDegraded, Message: "Storage can no longer accept mutations safely.", Retry: gapdb.RetryAfterRestart, FailedStage: stage, CurrentRevision: &currentCopy, DurableThroughRevision: &durableCopy, OperationApplied: operationApplied, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionRestartAfterRecovery, gapdb.ActionAbort}, Cause: cause}
}

func recordExpired(record gapdb.Record, now time.Time) bool {
	return record.ExpiresAt != nil && clock.IsExpired(*record.ExpiresAt, now)
}

func asGapdbError(err error) *gapdb.Error {
	var apiErr *gapdb.Error
	if errors.As(err, &apiErr) {
		return apiErr.Clone()
	}
	return nil
}
