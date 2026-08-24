package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/persist"
)

type command struct {
	kind          commandKind
	batch         gapdb.Batch
	single        singleOperation
	response      chan<- commandResult
	watch         watchRequest
	watchResponse chan<- watchRegistrationResult
	watchID       uint64
	doneResponse  chan<- struct{}
	snapshot      SnapshotInstaller
	snapshotReply chan<- snapshotResult
	admin         DurableAdminOperation
	adminReply    chan<- error
}

type commandKind uint8

const (
	commandMutation commandKind = iota
	commandExpiry
	commandWatchRegister
	commandWatchUnregister
	commandSnapshot
	commandDurableAdmin
)

type commandResult struct {
	result CommitResult
	err    error
}

type commitProgress struct {
	appliedPossible bool
}

func (state *DatabaseState) runWriter() {
	defer close(state.done)
	for {
		select {
		case command, open := <-state.queue:
			if !open {
				state.expiryTimer.Stop()
				state.shutdownWatchers()
				if err := state.log.Barrier(); err != nil {
					state.closeErr = err
					state.degrade()
				}
				return
			}
			state.handleCommand(command)
			state.resetExpiryTimer()
		case <-state.expiryTimer.C():
			if state.Lifecycle() == gapdb.LifecycleReady {
				_ = state.executeDueExpiriesSafely()
				state.resetExpiryTimer()
			} else {
				state.expiryTimer.Stop()
			}
		}
	}
}

func (state *DatabaseState) handleCommand(command command) {
	switch command.kind {
	case commandMutation:
		command.response <- state.executeSafely(command)
	case commandExpiry:
		command.response <- state.executeDueExpiriesSafely()
	case commandWatchRegister:
		command.watchResponse <- state.registerWatch(command.watch)
	case commandWatchUnregister:
		state.unregisterWatch(command.watchID, gapdb.WatchEndedByClient)
		close(command.doneResponse)
	case commandSnapshot:
		command.snapshotReply <- state.executeSnapshot(command.snapshot)
	case commandDurableAdmin:
		command.adminReply <- state.executeDurableAdmin(command.admin)
	}
}

func (state *DatabaseState) executeSafely(command command) (result commandResult) {
	progress := &commitProgress{}
	defer func() {
		if recovered := recover(); recovered != nil {
			state.degrade()
			result.err = internalFailure("engine-writer-panic", progress.appliedPossible, fmt.Errorf("writer panic: %v", recovered))
		}
	}()
	result.result, result.err = state.execute(command.batch, command.single, progress)
	return result
}

func (state *DatabaseState) execute(batch gapdb.Batch, single singleOperation, progress *commitProgress) (CommitResult, error) {
	state.mu.RLock()
	lifecycle := state.lifecycle
	state.mu.RUnlock()
	if lifecycle != gapdb.LifecycleReady && lifecycle != gapdb.LifecycleDraining {
		return CommitResult{}, state.mutationUnavailable(lifecycle)
	}
	now := state.clock.Now()
	if err := validateBatchAt(batch, state.limits, now); err != nil {
		return CommitResult{}, err
	}

	state.mu.RLock()
	for index, mutation := range batch.Mutations {
		record, exists := state.records[mutation.Key]
		live := exists && !recordExpired(record, now)
		if err := conditionError(mutation, record, live, state.current, index, single); err != nil {
			state.mu.RUnlock()
			return CommitResult{}, err
		}
	}
	state.mu.RUnlock()

	// Reviewer invariant: this is the engine's sole revision-allocation call site.
	revision, err := state.allocator.Next()
	if err != nil {
		state.degrade()
		return CommitResult{}, err
	}
	current, _ := state.revisions()
	if revision == 0 || revision <= current {
		state.degrade()
		return CommitResult{}, internalFailure("engine-invalid-allocator-revision", false, fmt.Errorf("allocator returned revision %d after committed revision %d", revision, current))
	}
	frame := persist.CommitFrame{Revision: revision, Effects: make([]persist.Effect, len(batch.Mutations))}
	for index, mutation := range batch.Mutations {
		frame.Effects[index] = effectFor(mutation, single)
	}
	if err := appendWithProgress(state.log, frame, progress); err != nil {
		state.degrade()
		current, durable := state.revisions()
		return CommitResult{}, persistenceFailure("wal_append", current, durable, progress.appliedPossible, err)
	}
	current, knownDurable := state.revisions()
	durableThrough := state.log.DurableThrough()
	if durableThrough < knownDurable || durableThrough > revision {
		state.degrade()
		return CommitResult{}, storageDegraded("wal_durability_evidence", current, knownDurable, true, nil)
	}
	if batch.Ack == gapdb.AckDurable {
		if err := state.log.Barrier(); err != nil {
			state.degrade()
			current, durable := state.revisions()
			return CommitResult{}, persistenceFailure("wal_barrier", current, durable, true, err)
		}
		durableThrough = state.log.DurableThrough()
		if durableThrough != revision {
			state.degrade()
			current, durable := state.revisions()
			return CommitResult{}, storageDegraded("wal_barrier_evidence", current, durable, true, nil)
		}
	}
	if err := faultfs.Checkpoint(state.faults, faultfs.PointMapApply, faultfs.Before); err != nil {
		state.degrade()
		current, durable := state.revisions()
		return CommitResult{}, persistenceFailure("map_apply", current, durable, true, err)
	}

	events := make([]gapdb.ChangeEvent, len(batch.Mutations))
	state.mu.Lock()
	for index, mutation := range batch.Mutations {
		events[index] = state.apply(revision, uint32(index), mutation, single)
	}
	state.current = revision
	if durableThrough > state.durableThrough {
		state.durableThrough = durableThrough
	}
	state.history.append(events)
	state.mu.Unlock()
	if err := faultfs.Checkpoint(state.faults, faultfs.PointMapApply, faultfs.After); err != nil {
		state.degrade()
		current, durable := state.revisions()
		return CommitResult{}, persistenceFailure("map_apply", current, durable, true, err)
	}
	state.afterCommit(events)
	return CommitResult{MutationResult: gapdb.MutationResult{Revision: revision, Ack: batch.Ack, DurableThroughRevision: durableThrough, MutationCount: len(batch.Mutations)}, Events: cloneEvents(events)}, nil
}

func appendWithProgress(log CommitLog, frame persist.CommitFrame, progress *commitProgress) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// A panic provides no trustworthy completion boundary. Once Append
			// started, callers must reconcile conservatively.
			progress.appliedPossible = true
			panic(recovered)
		}
	}()
	err = log.Append(frame)
	if err == nil || appendErrorMayHaveApplied(err) {
		progress.appliedPossible = true
	}
	return err
}

func appendErrorMayHaveApplied(err error) bool {
	if apiErr := asGapdbError(err); apiErr != nil {
		if apiErr.OperationApplied {
			return true
		}
		if apiErr.Code == gapdb.CodeStorageDegraded && apiErr.FailedStage == "wal_unavailable" {
			return false
		}
	}
	var hookError *faultfs.HookError
	if errors.As(err, &hookError) && hookError.Event.Point == faultfs.PointWALFrameWrite && hookError.Event.Phase == faultfs.Before {
		return false
	}
	// Ordinary write errors can follow a nonzero partial write. Without a
	// definitive before-write marker, replayability is possible.
	return true
}

func persistenceFailure(stage string, current, durable gapdb.Revision, operationApplied bool, cause error) error {
	if apiErr := asGapdbError(cause); apiErr != nil && apiErr.Code == gapdb.CodeStorageDegraded {
		currentCopy, durableCopy := current, durable
		apiErr.CurrentRevision = &currentCopy
		apiErr.DurableThroughRevision = &durableCopy
		apiErr.OperationApplied = apiErr.OperationApplied || operationApplied
		if apiErr.FailedStage == "" {
			apiErr.FailedStage = stage
		}
		return apiErr
	}
	return storageDegraded(stage, current, durable, operationApplied, cause)
}

func internalFailure(correlationID string, operationApplied bool, cause error) error {
	return &gapdb.Error{
		Code:             gapdb.CodeInternal,
		Message:          "The writer stopped after an internal invariant failure.",
		Retry:            gapdb.RetryAfterOperator,
		CorrelationID:    correlationID,
		OperationApplied: operationApplied,
		SafeActions:      []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionAbort},
		Cause:            cause,
	}
}

func conditionError(mutation gapdb.Mutation, record gapdb.Record, live bool, current gapdb.Revision, index int, single singleOperation) error {
	if single == operationExpiry && mutation.Condition.Kind == gapdb.ConditionRevision && record.Revision == mutation.Condition.ExpectedRevision {
		return nil
	}
	switch mutation.Condition.Kind {
	case gapdb.ConditionAny:
		return nil
	case gapdb.ConditionAbsent:
		if !live {
			return nil
		}
		if single == operationPutIfAbsent {
			actual := record.Revision
			return &gapdb.Error{Code: gapdb.CodeAlreadyExists, Message: "The key already exists.", Retry: gapdb.RetryAfterReconcile, Key: mutation.Key, ActualRevision: &actual, SafeActions: []gapdb.SafeAction{gapdb.ActionGet, gapdb.ActionCompareAndSwap, gapdb.ActionAbort}}
		}
		return batchConditionError(mutation, record, live, index)
	case gapdb.ConditionRevision:
		if live && record.Revision == mutation.Condition.ExpectedRevision {
			return nil
		}
		if single == operationCompareAndSwap || single == operationDeleteIfRevision {
			if !live {
				return notFound(mutation.Key, current)
			}
			expected, actual := mutation.Condition.ExpectedRevision, record.Revision
			return &gapdb.Error{Code: gapdb.CodeRevisionMismatch, Message: "The record revision does not match the condition.", Retry: gapdb.RetryAfterReconcile, Key: mutation.Key, ExpectedRevision: &expected, ActualRevision: &actual, SafeActions: []gapdb.SafeAction{gapdb.ActionGet, gapdb.ActionRetryWithNewCondition, gapdb.ActionAbort}}
		}
		return batchConditionError(mutation, record, live, index)
	}
	return invalidField("condition.kind", "must be any, absent, or revision")
}

func batchConditionError(mutation gapdb.Mutation, record gapdb.Record, live bool, index int) error {
	mutationIndex := index
	err := &gapdb.Error{Code: gapdb.CodeConditionFailed, Message: "A batch mutation condition failed.", Retry: gapdb.RetryAfterReconcile, Key: mutation.Key, MutationIndex: &mutationIndex, Condition: string(mutation.Condition.Kind), SafeActions: []gapdb.SafeAction{gapdb.ActionGet, gapdb.ActionRebuildBatch, gapdb.ActionAbort}}
	if live {
		actual := record.Revision
		err.ActualRevision = &actual
	} else {
		err.ActualState = "absent"
	}
	return err
}

func effectFor(mutation gapdb.Mutation, operation singleOperation) persist.Effect {
	if mutation.Kind == gapdb.MutationDelete {
		if operation == operationExpiry {
			return persist.Effect{Kind: gapdb.ChangeExpire, Key: mutation.Key}
		}
		return persist.Effect{Kind: gapdb.ChangeDelete, Key: mutation.Key}
	}
	return persist.Effect{Kind: gapdb.ChangePut, Key: mutation.Key, Value: append([]byte{}, mutation.Value...), ExpiresAt: cloneTime(mutation.ExpiresAt)}
}

func (state *DatabaseState) apply(revision gapdb.Revision, order uint32, mutation gapdb.Mutation, operation singleOperation) gapdb.ChangeEvent {
	if mutation.Kind == gapdb.MutationDelete {
		delete(state.records, mutation.Key)
		if operation == operationExpiry {
			return gapdb.ChangeEvent{Revision: revision, Order: order, Kind: gapdb.ChangeExpire, Key: mutation.Key}
		}
		return gapdb.ChangeEvent{Revision: revision, Order: order, Kind: gapdb.ChangeDelete, Key: mutation.Key}
	}
	record := gapdb.NewRecord(mutation.Key, mutation.Value, revision, mutation.ExpiresAt)
	state.records[mutation.Key] = record
	clone := record.Clone()
	return gapdb.ChangeEvent{Revision: revision, Order: order, Kind: gapdb.ChangePut, Key: mutation.Key, Record: &clone}
}

func cloneEvents(events []gapdb.ChangeEvent) []gapdb.ChangeEvent {
	clone := make([]gapdb.ChangeEvent, len(events))
	for index, event := range events {
		clone[index] = event.Clone()
	}
	return clone
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
