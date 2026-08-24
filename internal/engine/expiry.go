package engine

import (
	"container/heap"
	"context"
	"sort"
	"time"

	"gapdb/gapdb"
)

type ExpiryTimer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}

type systemExpiryTimer struct {
	timer *time.Timer
}

type dormantExpiryTimer struct{}

func (dormantExpiryTimer) C() <-chan time.Time { return nil }
func (dormantExpiryTimer) Reset(time.Duration) {}
func (dormantExpiryTimer) Stop()               {}

func newSystemExpiryTimer() *systemExpiryTimer {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	return &systemExpiryTimer{timer: timer}
}

func (timer *systemExpiryTimer) C() <-chan time.Time { return timer.timer.C }

func (timer *systemExpiryTimer) Reset(delay time.Duration) {
	if !timer.timer.Stop() {
		select {
		case <-timer.timer.C:
		default:
		}
	}
	timer.timer.Reset(delay)
}

func (timer *systemExpiryTimer) Stop() {
	if !timer.timer.Stop() {
		select {
		case <-timer.timer.C:
		default:
		}
	}
}

type expiryCandidate struct {
	at       time.Time
	key      string
	revision gapdb.Revision
	index    int
}

type expiryHeap []*expiryCandidate

func (items expiryHeap) Len() int { return len(items) }
func (items expiryHeap) Less(i, j int) bool {
	if items[i].at.Equal(items[j].at) {
		if items[i].key == items[j].key {
			return items[i].revision < items[j].revision
		}
		return items[i].key < items[j].key
	}
	return items[i].at.Before(items[j].at)
}
func (items expiryHeap) Swap(i, j int) {
	items[i], items[j] = items[j], items[i]
	items[i].index = i
	items[j].index = j
}
func (items *expiryHeap) Push(value any) {
	candidate := value.(*expiryCandidate)
	candidate.index = len(*items)
	*items = append(*items, candidate)
}
func (items *expiryHeap) Pop() any {
	old := *items
	last := old[len(old)-1]
	old[len(old)-1] = nil
	*items = old[:len(old)-1]
	last.index = -1
	return last
}

func (state *DatabaseState) scheduleRecordExpiry(record gapdb.Record) {
	if record.ExpiresAt == nil {
		state.removeScheduledExpiry(record.Key)
		return
	}
	if candidate, exists := state.expiryByKey[record.Key]; exists {
		candidate.at = record.ExpiresAt.UTC()
		candidate.revision = record.Revision
		heap.Fix(&state.expiries, candidate.index)
		return
	}
	candidate := &expiryCandidate{at: record.ExpiresAt.UTC(), key: record.Key, revision: record.Revision}
	state.expiryByKey[record.Key] = candidate
	heap.Push(&state.expiries, candidate)
}

func (state *DatabaseState) removeScheduledExpiry(key string) {
	candidate, exists := state.expiryByKey[key]
	if !exists {
		return
	}
	delete(state.expiryByKey, key)
	heap.Remove(&state.expiries, candidate.index)
}

func (state *DatabaseState) resetExpiryTimer() {
	if len(state.expiries) == 0 {
		state.expiryTimer.Stop()
		return
	}
	delay := state.expiries[0].at.Sub(state.clock.Now())
	if delay < 0 {
		delay = 0
	}
	state.expiryTimer.Reset(delay)
}

func (state *DatabaseState) ProcessDueExpiries(ctx context.Context) (CommitResult, error) {
	select {
	case <-ctx.Done():
		return CommitResult{}, ctx.Err()
	default:
	}
	response := make(chan commandResult, 1)
	command := command{kind: commandExpiry, response: response}
	state.admissionMu.RLock()
	if state.Lifecycle() != gapdb.LifecycleReady {
		state.admissionMu.RUnlock()
		return CommitResult{}, state.mutationUnavailable(state.Lifecycle())
	}
	select {
	case state.queue <- command:
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

func (state *DatabaseState) executeDueExpiriesSafely() (result commandResult) {
	progress := &commitProgress{}
	defer func() {
		if recovered := recover(); recovered != nil {
			state.degrade()
			result.err = internalFailure("engine-expiry-panic", progress.appliedPossible, nil)
		}
	}()
	result.result, result.err = state.executeDueExpiries(progress)
	return result
}

func (state *DatabaseState) executeDueExpiries(progress *commitProgress) (CommitResult, error) {
	now := state.clock.Now()
	due := make([]*expiryCandidate, 0)
	seen := make(map[string]struct{})
	encodedBytes := 32
	maximumBytes := state.limits.MaxBatchBytes
	if state.limits.MaxFrameBytes < maximumBytes {
		maximumBytes = state.limits.MaxFrameBytes
	}
	state.mu.RLock()
	for len(due) < state.limits.MaxBatchOperations && len(state.expiries) != 0 && !state.expiries[0].at.After(now) {
		candidate := heap.Pop(&state.expiries).(*expiryCandidate)
		if state.expiryByKey[candidate.key] == candidate {
			delete(state.expiryByKey, candidate.key)
		}
		record, exists := state.records[candidate.key]
		if !exists || record.Revision != candidate.revision || record.ExpiresAt == nil || !record.ExpiresAt.Equal(candidate.at) || !recordExpired(record, now) {
			continue
		}
		if _, duplicate := seen[candidate.key]; duplicate {
			continue
		}
		itemBytes := 20 + len(candidate.key)
		if encodedBytes+itemBytes > maximumBytes {
			state.expiryByKey[candidate.key] = candidate
			heap.Push(&state.expiries, candidate)
			break
		}
		seen[candidate.key] = struct{}{}
		encodedBytes += itemBytes
		due = append(due, candidate)
	}
	state.mu.RUnlock()
	if len(due) == 0 {
		return CommitResult{}, nil
	}
	sort.Slice(due, func(i, j int) bool { return due[i].key < due[j].key })
	batch := gapdb.Batch{Ack: gapdb.AckDurable, Mutations: make([]gapdb.Mutation, len(due))}
	for index, candidate := range due {
		batch.Mutations[index] = gapdb.NewDeleteMutation(candidate.key, candidate.revision)
	}
	return state.execute(batch, operationExpiry, progress)
}

func (state *DatabaseState) afterCommit(events []gapdb.ChangeEvent) {
	for _, event := range events {
		if event.Kind == gapdb.ChangePut && event.Record != nil {
			state.scheduleRecordExpiry(*event.Record)
		} else {
			state.removeScheduledExpiry(event.Key)
		}
	}
	state.publishWatchers(events)
}
