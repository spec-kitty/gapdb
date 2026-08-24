package engine

import (
	"context"
	"fmt"
	"time"

	"gapdb/gapdb"
	"gapdb/internal/persist"
)

type SnapshotCut struct {
	DatabaseID persist.DatabaseID
	Revision   gapdb.Revision
	AsOf       time.Time
	Records    []gapdb.Record
}

type SnapshotInstaller interface {
	Install(SnapshotCut) (CommitLog, error)
}
type SnapshotInstallerFunc func(SnapshotCut) (CommitLog, error)

func (function SnapshotInstallerFunc) Install(cut SnapshotCut) (CommitLog, error) {
	return function(cut)
}

type SnapshotResult struct {
	Revision    gapdb.Revision
	RecordCount int
	Duration    time.Duration
}
type snapshotResult struct {
	result SnapshotResult
	err    error
}

func (state *DatabaseState) Snapshot(ctx context.Context, installer SnapshotInstaller) (SnapshotResult, error) {
	if installer == nil {
		return SnapshotResult{}, invalidField("snapshot_installer", "must be configured")
	}
	reply := make(chan snapshotResult, 1)
	cmd := command{kind: commandSnapshot, snapshot: installer, snapshotReply: reply}
	state.admissionMu.RLock()
	state.mu.RLock()
	lifecycle := state.lifecycle
	state.mu.RUnlock()
	if lifecycle != gapdb.LifecycleReady {
		state.admissionMu.RUnlock()
		return SnapshotResult{}, state.mutationUnavailable(lifecycle)
	}
	select {
	case state.queue <- cmd:
		state.admissionMu.RUnlock()
	case <-ctx.Done():
		state.admissionMu.RUnlock()
		return SnapshotResult{}, ctx.Err()
	default:
		depth := len(state.queue)
		state.admissionMu.RUnlock()
		return SnapshotResult{}, serverBusy(depth, cap(state.queue))
	}
	response := <-reply
	return response.result, response.err
}

func (state *DatabaseState) executeSnapshot(installer SnapshotInstaller) (result snapshotResult) {
	started := time.Now()
	installStarted := false
	defer func() {
		if recovered := recover(); recovered != nil {
			state.degrade()
			if installStarted {
				result.result = SnapshotResult{Revision: state.CurrentRevision(), Duration: time.Since(started)}
			}
			result.err = internalFailure("engine-snapshot-panic", installStarted, fmt.Errorf("snapshot panic: %v", recovered))
		}
	}()
	if err := state.log.Barrier(); err != nil {
		state.degrade()
		current, durable := state.revisions()
		result.err = persistenceFailure("snapshot_wal_barrier", current, durable, false, err)
		return result
	}
	state.mu.Lock()
	current := state.current
	state.durableThrough = state.log.DurableThrough()
	if state.durableThrough != current {
		durable := state.durableThrough
		state.lifecycle = gapdb.LifecycleDegradedReadOnly
		state.mu.Unlock()
		result.err = storageDegraded("snapshot_wal_barrier_evidence", current, durable, false, nil)
		return result
	}
	state.mu.Unlock()
	// This handler owns the only mutation executor, so the map is immutable
	// until it returns. Iterate without holding mu: direct readers can continue,
	// and record cloning prevents the installer from aliasing live values.
	cut := SnapshotCut{DatabaseID: state.databaseID, Revision: current, AsOf: state.clock.Now(), Records: make([]gapdb.Record, 0, len(state.records))}
	for _, record := range state.records {
		cut.Records = append(cut.Records, record.Clone())
	}
	// Once Install begins, an implementation panic has no trustworthy
	// authority boundary: snapshot, WAL, or CURRENT may already have changed.
	installStarted = true
	next, err := installer.Install(cut)
	if err != nil {
		if apiErr := asGapdbError(err); apiErr != nil && apiErr.OperationApplied {
			state.degrade()
			result.result = SnapshotResult{Revision: current, RecordCount: len(cut.Records), Duration: time.Since(started)}
		}
		result.err = err
		return result
	}
	if next == nil {
		state.degrade()
		result.err = internalFailure("engine-snapshot-nil-log", true, fmt.Errorf("snapshot installer returned nil WAL"))
		return result
	}
	if next.DurableThrough() != current {
		state.degrade()
		result.err = storageDegraded("snapshot_new_wal_evidence", current, current, true, nil)
		return result
	}
	state.log = next
	state.mu.Lock()
	state.snapshotRevision = current
	state.activeWALStart = current + 1
	state.mu.Unlock()
	result.result = SnapshotResult{Revision: current, RecordCount: len(cut.Records), Duration: time.Since(started)}
	return result
}
