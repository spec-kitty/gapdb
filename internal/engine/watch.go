package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"gapdb/gapdb"
)

type WatchSubscription struct {
	RegistrationRevision gapdb.Revision
	Events               <-chan gapdb.ChangeEvent
	Ended                <-chan gapdb.WatchTermination

	state    *DatabaseState
	id       uint64
	stop     chan struct{}
	finished chan struct{}
	once     sync.Once
}

type watchRequest struct {
	prefix string
	after  gapdb.Revision
	ctx    context.Context
}

type watchRegistrationResult struct {
	watch *WatchSubscription
	err   error
}

type watchEnd struct {
	reason  gapdb.WatchEndReason
	current gapdb.Revision
	err     *gapdb.Error
}

type watcherState struct {
	id            uint64
	prefix        string
	backlog       []eventGroup
	live          chan eventGroup
	end           chan watchEnd
	queuedEvents  atomic.Int64
	lastDelivered atomic.Uint64
	limit         int64
	registration  gapdb.Revision
	backlogFits   bool
	stop          chan struct{}
	finished      chan struct{}
}

func (state *DatabaseState) Watch(ctx context.Context, prefix string, after gapdb.Revision) (*WatchSubscription, error) {
	if err := validatePrefix(prefix, state.limits); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	response := make(chan watchRegistrationResult, 1)
	command := command{kind: commandWatchRegister, watch: watchRequest{prefix: prefix, after: after, ctx: ctx}, watchResponse: response}
	state.admissionMu.RLock()
	lifecycle := state.Lifecycle()
	if state.closed || lifecycle == gapdb.LifecycleDraining || lifecycle == gapdb.LifecycleInspectionOnly {
		state.admissionMu.RUnlock()
		return nil, state.mutationUnavailable(lifecycle)
	}
	select {
	case state.queue <- command:
		state.admissionMu.RUnlock()
	case <-ctx.Done():
		state.admissionMu.RUnlock()
		return nil, ctx.Err()
	default:
		depth := len(state.queue)
		state.admissionMu.RUnlock()
		return nil, serverBusy(depth, cap(state.queue))
	}
	result := <-response
	return result.watch, result.err
}

func (state *DatabaseState) registerWatch(request watchRequest) watchRegistrationResult {
	current := state.CurrentRevision()
	if request.after > current {
		return watchRegistrationResult{err: revisionAhead(request.after, current)}
	}
	groups, backlogEvents, backlogFits, err := state.history.replayGroupsForWatch(request.prefix, request.after, int64(state.limits.WatchBufferEvents))
	if err != nil {
		return watchRegistrationResult{err: err}
	}
	if len(state.watchers) >= state.limits.MaxWatchClients {
		active, maximum, depth := len(state.watchers), state.limits.MaxWatchClients, len(state.queue)
		return watchRegistrationResult{err: &gapdb.Error{Code: gapdb.CodeServerBusy, Message: "The configured watch limit is active.", Retry: gapdb.RetryImmediate, ActiveClients: &active, MaximumClients: &maximum, QueueDepth: &depth, SafeActions: []gapdb.SafeAction{gapdb.ActionRetryWithBackoff, gapdb.ActionAbort}}}
	}
	state.nextWatcherID++
	id := state.nextWatcherID
	watcher := &watcherState{
		id:           id,
		prefix:       request.prefix,
		backlog:      groups,
		live:         make(chan eventGroup, state.limits.WatchBufferEvents),
		end:          make(chan watchEnd, 1),
		limit:        int64(state.limits.WatchBufferEvents),
		registration: current,
		stop:         make(chan struct{}),
		finished:     make(chan struct{}),
	}
	watcher.lastDelivered.Store(uint64(request.after))
	watcher.backlogFits = backlogFits
	if backlogFits {
		watcher.queuedEvents.Store(backlogEvents)
		state.watchers[id] = watcher
		state.activeWatches.Add(1)
	} else {
		watcher.backlog = nil
		watcher.end <- watchLagEnd(request.after, current)
	}
	events := make(chan gapdb.ChangeEvent)
	ended := make(chan gapdb.WatchTermination, 1)
	subscription := &WatchSubscription{RegistrationRevision: current, Events: events, Ended: ended, state: state, id: id, stop: watcher.stop, finished: watcher.finished}
	go watcher.pump(events, ended)
	go func() {
		select {
		case <-request.ctx.Done():
			subscription.Close()
		case <-watcher.finished:
		}
	}()
	return watchRegistrationResult{watch: subscription}
}

func (watch *WatchSubscription) Close() {
	if watch == nil {
		return
	}
	watch.once.Do(func() {
		close(watch.stop)
		watch.state.requestWatchUnregister(watch.id)
	})
}

func (state *DatabaseState) requestWatchUnregister(id uint64) {
	state.admissionMu.RLock()
	lifecycle := state.Lifecycle()
	if state.closed || lifecycle == gapdb.LifecycleDraining || lifecycle == gapdb.LifecycleInspectionOnly {
		state.admissionMu.RUnlock()
		return
	}
	done := make(chan struct{})
	state.queue <- command{kind: commandWatchUnregister, watchID: id, doneResponse: done}
	state.admissionMu.RUnlock()
	<-done
}

func (watcher *watcherState) pump(events chan<- gapdb.ChangeEvent, ended chan<- gapdb.WatchTermination) {
	defer close(watcher.finished)
	defer close(events)
	defer close(ended)
	finish := func(end watchEnd) {
		termination := gapdb.WatchTermination{Reason: end.reason, LastDeliveredRevision: gapdb.Revision(watcher.lastDelivered.Load())}
		if end.err != nil {
			termination.Error = end.err.Clone()
			last := termination.LastDeliveredRevision
			termination.Error.LastDeliveredRevision = &last
		}
		ended <- termination
	}
	deliver := func(group eventGroup) (watchEnd, bool) {
		select {
		case end := <-watcher.end:
			return end, false
		case <-watcher.stop:
			return watchEnd{reason: gapdb.WatchEndedByClient}, false
		default:
		}
		for _, event := range group.events {
			select {
			case events <- event.Clone():
			case <-watcher.stop:
				return watchEnd{reason: gapdb.WatchEndedByClient}, false
			}
		}
		watcher.lastDelivered.Store(uint64(group.revision))
		return watchEnd{}, true
	}
	for _, group := range watcher.backlog {
		if end, ok := deliver(group); !ok {
			finish(end)
			return
		}
		watcher.queuedEvents.Add(-int64(len(group.events)))
	}
	watcher.backlog = nil
	if watcher.backlogFits {
		watcher.lastDelivered.Store(uint64(watcher.registration))
	}
	for {
		select {
		case group := <-watcher.live:
			if end, ok := deliver(group); !ok {
				finish(end)
				return
			}
			watcher.queuedEvents.Add(-int64(len(group.events)))
		case end := <-watcher.end:
			finish(end)
			return
		case <-watcher.stop:
			finish(watchEnd{reason: gapdb.WatchEndedByClient})
			return
		}
	}
}

func (state *DatabaseState) publishWatchers(events []gapdb.ChangeEvent) {
	for id, watcher := range state.watchers {
		group := eventGroup{revision: events[0].Revision}
		for _, event := range events {
			if len(event.Key) >= len(watcher.prefix) && event.Key[:len(watcher.prefix)] == watcher.prefix {
				group.events = append(group.events, event.Clone())
			}
		}
		if len(group.events) == 0 {
			continue
		}
		count := int64(len(group.events))
		if !reserveWatchEvents(watcher, count) {
			state.lagWatcher(id, watcher, events[0].Revision)
			continue
		}
		select {
		case watcher.live <- group:
		default:
			watcher.queuedEvents.Add(-count)
			state.lagWatcher(id, watcher, events[0].Revision)
		}
	}
}

func reserveWatchEvents(watcher *watcherState, count int64) bool {
	for {
		queued := watcher.queuedEvents.Load()
		if count > watcher.limit || queued > watcher.limit-count {
			return false
		}
		if watcher.queuedEvents.CompareAndSwap(queued, queued+count) {
			return true
		}
	}
}

func (state *DatabaseState) lagWatcher(id uint64, watcher *watcherState, current gapdb.Revision) {
	last := gapdb.Revision(watcher.lastDelivered.Load())
	delete(state.watchers, id)
	state.activeWatches.Add(-1)
	watcher.end <- watchLagEnd(last, current)
}

func watchLagEnd(last, current gapdb.Revision) watchEnd {
	lastCopy, currentCopy := last, current
	error := &gapdb.Error{Code: gapdb.CodeWatchLagged, Message: "The watch could not keep up with committed changes.", Retry: gapdb.RetryAfterRescan, LastDeliveredRevision: &lastCopy, CurrentRevision: &currentCopy, SafeActions: []gapdb.SafeAction{gapdb.ActionScanPrefix, gapdb.ActionRestartWatch, gapdb.ActionAbort}}
	return watchEnd{reason: gapdb.WatchEndedByError, current: current, err: error}
}

func (state *DatabaseState) unregisterWatch(id uint64, reason gapdb.WatchEndReason) {
	watcher, exists := state.watchers[id]
	if !exists {
		return
	}
	delete(state.watchers, id)
	state.activeWatches.Add(-1)
	watcher.end <- watchEnd{reason: reason, current: state.CurrentRevision()}
}

func (state *DatabaseState) shutdownWatchers() {
	for id, watcher := range state.watchers {
		delete(state.watchers, id)
		state.activeWatches.Add(-1)
		watcher.end <- watchEnd{reason: gapdb.WatchEndedByShutdown, current: state.CurrentRevision()}
	}
}

func revisionAhead(requested, current gapdb.Revision) error {
	requestedCopy, currentCopy := requested, current
	return &gapdb.Error{Code: gapdb.CodeRevisionAhead, Message: "The requested watch revision is ahead of the database.", Retry: gapdb.RetryAfterRescan, RequestedRevision: &requestedCopy, CurrentRevision: &currentCopy, SafeActions: []gapdb.SafeAction{gapdb.ActionScanPrefix, gapdb.ActionRestartWatch, gapdb.ActionAbort}}
}
