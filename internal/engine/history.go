package engine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/persist"
)

type eventGroup struct {
	revision gapdb.Revision
	events   []gapdb.ChangeEvent
	bytes    int
}

type historyLog struct {
	mu               sync.RWMutex
	commits          []eventGroup
	eventCount       int
	byteCount        int
	maxEvents        int
	maxBytes         int
	compactedThrough gapdb.Revision
}

func (history *historyLog) initialize(limits gapdb.Limits, snapshotRevision gapdb.Revision) {
	history.maxEvents = limits.MaxHistoryEvents
	history.maxBytes = limits.MaxHistoryBytes
	history.compactedThrough = snapshotRevision
}

func (history *historyLog) rebuild(commits []persist.CommitFrame, current gapdb.Revision) error {
	previous := history.compactedThrough
	for _, commit := range commits {
		if commit.Revision <= previous || commit.Revision > current {
			return invalidField("recovered_commits", fmt.Sprintf("revision %d is outside strictly increasing recovery bounds after revision %d", commit.Revision, previous))
		}
		events := make([]gapdb.ChangeEvent, len(commit.Effects))
		for index, effect := range commit.Effects {
			event := gapdb.ChangeEvent{Revision: commit.Revision, Order: uint32(index), Kind: effect.Kind, Key: effect.Key}
			if effect.Kind == gapdb.ChangePut {
				record := gapdb.NewRecord(effect.Key, effect.Value, commit.Revision, effect.ExpiresAt)
				event.Record = &record
			}
			events[index] = event
		}
		history.append(events)
		previous = commit.Revision
	}
	if previous != current {
		return invalidField("recovered_commits", fmt.Sprintf("coverage ends at revision %d instead of current revision %d", previous, current))
	}
	return nil
}

func (history *historyLog) append(events []gapdb.ChangeEvent) {
	if len(events) == 0 {
		return
	}
	group := eventGroup{revision: events[0].Revision, events: cloneEvents(events)}
	for _, event := range group.events {
		group.bytes += historyEventBytes(event)
	}
	history.mu.Lock()
	history.commits = append(history.commits, group)
	history.eventCount += len(group.events)
	history.byteCount += group.bytes
	for len(history.commits) != 0 && (history.eventCount > history.maxEvents || history.byteCount > history.maxBytes) {
		evicted := history.commits[0]
		history.commits = history.commits[1:]
		history.eventCount -= len(evicted.events)
		history.byteCount -= evicted.bytes
		if evicted.revision > history.compactedThrough {
			history.compactedThrough = evicted.revision
		}
	}
	history.mu.Unlock()
}

func historyEventBytes(event gapdb.ChangeEvent) int {
	bytes := 24 + len(event.Key)
	if event.Record != nil {
		bytes += len(event.Record.Key) + len(event.Record.Value)
		if event.Record.ExpiresAt != nil {
			bytes += 8
		}
	}
	return bytes
}

func (history *historyLog) replayGroups(prefix string, after gapdb.Revision) ([]eventGroup, error) {
	history.mu.RLock()
	defer history.mu.RUnlock()
	if after < history.compactedThrough {
		earliest := history.earliestAvailableLocked()
		requested := after
		return nil, &gapdb.Error{Code: gapdb.CodeRevisionCompacted, Message: "The requested watch revision is no longer retained.", Retry: gapdb.RetryAfterRescan, RequestedRevision: &requested, EarliestRevision: &earliest, SafeActions: []gapdb.SafeAction{gapdb.ActionScanPrefix, gapdb.ActionRestartWatch, gapdb.ActionAbort}}
	}
	groups := make([]eventGroup, 0)
	for _, commit := range history.commits {
		if commit.revision <= after {
			continue
		}
		group := eventGroup{revision: commit.revision}
		for _, event := range commit.events {
			if strings.HasPrefix(event.Key, prefix) {
				group.events = append(group.events, event.Clone())
			}
		}
		if len(group.events) != 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

func (history *historyLog) replayGroupsForWatch(prefix string, after gapdb.Revision, limit int64) ([]eventGroup, int64, bool, error) {
	history.mu.RLock()
	defer history.mu.RUnlock()
	if after < history.compactedThrough {
		earliest := history.earliestAvailableLocked()
		requested := after
		return nil, 0, false, &gapdb.Error{Code: gapdb.CodeRevisionCompacted, Message: "The requested watch revision is no longer retained.", Retry: gapdb.RetryAfterRescan, RequestedRevision: &requested, EarliestRevision: &earliest, SafeActions: []gapdb.SafeAction{gapdb.ActionScanPrefix, gapdb.ActionRestartWatch, gapdb.ActionAbort}}
	}
	groups := make([]eventGroup, 0)
	var total int64
	for _, commit := range history.commits {
		if commit.revision <= after {
			continue
		}
		matching := 0
		for _, event := range commit.events {
			if strings.HasPrefix(event.Key, prefix) {
				matching++
			}
		}
		count := int64(matching)
		if count == 0 {
			continue
		}
		if count > limit || total > limit-count {
			return nil, 0, false, nil
		}
		group := eventGroup{revision: commit.revision, events: make([]gapdb.ChangeEvent, 0, matching)}
		for _, event := range commit.events {
			if strings.HasPrefix(event.Key, prefix) {
				group.events = append(group.events, event.Clone())
			}
		}
		groups = append(groups, group)
		total += count
	}
	return groups, total, true, nil
}

func (history *historyLog) earliestAvailableLocked() gapdb.Revision {
	if len(history.commits) != 0 {
		return history.commits[0].revision
	}
	return history.compactedThrough
}

func (history *historyLog) earliestAvailable() gapdb.Revision {
	history.mu.RLock()
	defer history.mu.RUnlock()
	return history.earliestAvailableLocked()
}

func (state *DatabaseState) ReplayHistory(prefix string, after gapdb.Revision) ([]gapdb.ChangeEvent, error) {
	if err := validatePrefix(prefix, state.limits); err != nil {
		return nil, err
	}
	current := state.CurrentRevision()
	if after > current {
		return nil, revisionAhead(after, current)
	}
	groups, err := state.history.replayGroups(prefix, after)
	if err != nil {
		return nil, err
	}
	var events []gapdb.ChangeEvent
	for _, group := range groups {
		events = append(events, cloneEvents(group.events)...)
	}
	return events, nil
}

func (state *DatabaseState) EarliestWatchRevision() gapdb.Revision {
	return state.history.earliestAvailable()
}
