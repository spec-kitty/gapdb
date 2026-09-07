package engine

import (
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

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

// These engine-local fault points exercise predicate boundaries without
// changing the persistence fault vocabulary. They are exported from this
// internal package so the Unix transport qualification can drive the same
// production writer path through server.Config.FS.
const (
	FaultPointAssertionEvaluation         faultfs.Point = "engine.assertion_evaluation"
	FaultPointMutationConditionEvaluation faultfs.Point = "engine.mutation_condition_evaluation"
)

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

	// The writer owns the definitive predicate phase. Assertions and mutation
	// conditions share this one lock and the single effective time captured
	// above, so no queued writer can move authority between the two groups.
	predicateErr := func() error {
		state.mu.RLock()
		defer state.mu.RUnlock()
		if len(batch.Assertions) != 0 {
			if err := faultfs.Checkpoint(state.faults, FaultPointAssertionEvaluation, faultfs.Before); err != nil {
				return internalFailure("engine-assertion-evaluation", false, err)
			}
		}
		for index, assertion := range batch.Assertions {
			record, exists := state.records[assertion.Key]
			live := exists && !recordExpired(record, now)
			if err := assertionConditionError(assertion, record, live, index); err != nil {
				return err
			}
		}
		if len(batch.Assertions) != 0 {
			if err := faultfs.Checkpoint(state.faults, FaultPointAssertionEvaluation, faultfs.After); err != nil {
				return internalFailure("engine-assertion-evaluation", false, err)
			}
		}
		if err := faultfs.Checkpoint(state.faults, FaultPointMutationConditionEvaluation, faultfs.Before); err != nil {
			return internalFailure("engine-mutation-condition-evaluation", false, err)
		}
		for index, mutation := range batch.Mutations {
			record, exists := state.records[mutation.Key]
			live := exists && !recordExpired(record, now)
			if err := conditionError(mutation, record, live, state.current, index, single); err != nil {
				return err
			}
		}
		if err := faultfs.Checkpoint(state.faults, FaultPointMutationConditionEvaluation, faultfs.After); err != nil {
			return internalFailure("engine-mutation-condition-evaluation", false, err)
		}
		return nil
	}()
	if predicateErr != nil {
		return CommitResult{}, predicateErr
	}
	// Plan the immutable ordered-index replacement before allocating or
	// persisting a revision. The writer goroutine serializes mutations, while
	// the brief read lock in planOrderedKeys copies the current index for
	// O(n+k log k) delta merging without blocking readers during the merge.
	orderedKeys := state.planOrderedKeys(batch.Mutations)

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
	if orderedKeys.replacement != nil {
		state.orderedKeys = orderedKeys.replacement
	} else if len(orderedKeys.tail) != 0 {
		state.orderedKeys = append(state.orderedKeys, orderedKeys.tail...)
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
	return CommitResult{MutationResult: gapdb.MutationResult{Revision: revision, Ack: batch.Ack, DurableThroughRevision: durableThrough, MutationCount: len(batch.Mutations), AssertionCount: len(batch.Assertions)}, Events: cloneEvents(events)}, nil
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

func assertionConditionError(assertion gapdb.Assertion, record gapdb.Record, live bool, index int) error {
	switch assertion.Condition.Kind {
	case gapdb.ConditionAbsent:
		if !live {
			return nil
		}
	case gapdb.ConditionRevision:
		if live && record.Revision == assertion.Condition.ExpectedRevision {
			return nil
		}
	default:
		return invalidField("condition.kind", "assertions require absent or revision")
	}
	assertionIndex := index
	err := &gapdb.Error{
		Code:           gapdb.CodeConditionFailed,
		Message:        "Atomic batch assertion failed.",
		Retry:          gapdb.RetryAfterReconcile,
		Key:            boundedPredicateKey(assertion.Key),
		AssertionIndex: &assertionIndex,
		Condition:      string(assertion.Condition.Kind),
		SafeActions:    []gapdb.SafeAction{gapdb.ActionGet, gapdb.ActionRebuildBatch, gapdb.ActionAbort},
	}
	if assertion.Condition.Kind == gapdb.ConditionRevision {
		expected := assertion.Condition.ExpectedRevision
		err.ExpectedRevision = &expected
	}
	if live {
		actual := record.Revision
		err.ActualRevision = &actual
	} else {
		err.ActualState = "absent"
	}
	return err
}

func boundedPredicateKey(key string) string {
	const maximum = 256
	if len(key) <= maximum {
		return key
	}
	end := maximum
	for end > 0 && !utf8.ValidString(key[:end]) {
		end--
	}
	return key[:end]
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

type orderedKeyPlan struct {
	replacement []string
	tail        []string
}

func (state *DatabaseState) planOrderedKeys(mutations []gapdb.Mutation) orderedKeyPlan {
	state.mu.RLock()
	changed := false
	bulkMerge := false
	additions := make([]string, 0, len(mutations))
	last := ""
	if len(state.orderedKeys) != 0 {
		last = state.orderedKeys[len(state.orderedKeys)-1]
	}
	for _, mutation := range mutations {
		index := sort.SearchStrings(state.orderedKeys, mutation.Key)
		exists := index < len(state.orderedKeys) && state.orderedKeys[index] == mutation.Key
		if mutation.Kind == gapdb.MutationDelete && exists {
			changed = true
			bulkMerge = true
		}
		if mutation.Kind == gapdb.MutationPut && !exists {
			changed = true
			additions = append(additions, mutation.Key)
			if last != "" && mutation.Key <= last {
				bulkMerge = true
			}
		}
	}
	if !changed {
		state.mu.RUnlock()
		return orderedKeyPlan{}
	}
	if !bulkMerge {
		sort.Strings(additions)
		if len(state.orderedKeys)+len(additions) <= cap(state.orderedKeys) {
			state.mu.RUnlock()
			return orderedKeyPlan{tail: additions}
		}
		capacity := max(16, cap(state.orderedKeys)*2, len(state.orderedKeys)+len(additions))
		replacement := make([]string, len(state.orderedKeys), capacity)
		copy(replacement, state.orderedKeys)
		replacement = append(replacement, additions...)
		state.mu.RUnlock()
		return orderedKeyPlan{replacement: replacement}
	}
	base := append([]string(nil), state.orderedKeys...)
	state.mu.RUnlock()
	return orderedKeyPlan{replacement: mergeOrderedKeyDelta(base, mutations)}
}

func mergeOrderedKeyDelta(base []string, mutations []gapdb.Mutation) []string {
	removed := make(map[string]struct{}, len(mutations))
	additions := make([]string, 0, len(mutations))
	for _, mutation := range mutations {
		index := sort.SearchStrings(base, mutation.Key)
		exists := index < len(base) && base[index] == mutation.Key
		if mutation.Kind == gapdb.MutationDelete {
			if exists {
				removed[mutation.Key] = struct{}{}
			}
			continue
		}
		if !exists {
			additions = append(additions, mutation.Key)
		}
	}
	sort.Strings(additions)
	result := make([]string, 0, len(base)+len(additions)-len(removed))
	baseIndex, additionIndex := 0, 0
	for baseIndex < len(base) || additionIndex < len(additions) {
		if baseIndex < len(base) {
			if _, skip := removed[base[baseIndex]]; skip {
				baseIndex++
				continue
			}
		}
		if additionIndex == len(additions) || baseIndex < len(base) && base[baseIndex] < additions[additionIndex] {
			result = append(result, base[baseIndex])
			baseIndex++
			continue
		}
		result = append(result, additions[additionIndex])
		additionIndex++
	}
	return result
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
