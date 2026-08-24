package engine

import (
	"context"
	"fmt"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/persist"
)

// DurableAdminCut is authority selected by the sole writer after its real WAL
// barrier. The operation runs while mutation publication is paused.
type DurableAdminCut struct {
	DatabaseID persist.DatabaseID
	Revision   gapdb.Revision
}

type DurableAdminOperation func(DurableAdminCut) error

func (state *DatabaseState) DurableAdmin(ctx context.Context, operation DurableAdminOperation) error {
	if operation == nil {
		return invalidField("admin_operation", "must be configured")
	}
	reply := make(chan error, 1)
	cmd := command{kind: commandDurableAdmin, admin: operation, adminReply: reply}
	state.admissionMu.RLock()
	state.mu.RLock()
	lifecycle := state.lifecycle
	state.mu.RUnlock()
	if lifecycle != gapdb.LifecycleReady {
		state.admissionMu.RUnlock()
		return state.mutationUnavailable(lifecycle)
	}
	select {
	case state.queue <- cmd:
		state.admissionMu.RUnlock()
	case <-ctx.Done():
		state.admissionMu.RUnlock()
		return ctx.Err()
	default:
		depth := len(state.queue)
		state.admissionMu.RUnlock()
		return serverBusy(depth, cap(state.queue))
	}
	return <-reply
}

func (state *DatabaseState) executeDurableAdmin(operation DurableAdminOperation) (err error) {
	operationStarted := false
	defer func() {
		if recovered := recover(); recovered != nil {
			state.degrade()
			err = internalFailure("engine-durable-admin-panic", operationStarted, fmt.Errorf("durable admin panic: %v", recovered))
		}
	}()
	if err := state.log.Barrier(); err != nil {
		state.degrade()
		current, durable := state.revisions()
		return persistenceFailure("admin_wal_barrier", current, durable, false, err)
	}
	state.mu.Lock()
	state.durableThrough = state.log.DurableThrough()
	current := state.current
	durable := state.durableThrough
	state.mu.Unlock()
	if durable != current {
		state.degrade()
		return storageDegraded("admin_wal_barrier_evidence", current, durable, false, nil)
	}
	operationStarted = true
	return operation(DurableAdminCut{DatabaseID: state.databaseID, Revision: current})
}

// AdminStatus returns a bounded, synchronized description of live authority.
func (state *DatabaseState) AdminStatus() gapdb.Status {
	state.mu.RLock()
	status := gapdb.Status{Lifecycle: state.lifecycle, DatabaseID: state.databaseID.String(), CurrentRevision: state.current, DurableThroughRevision: state.durableThrough, ReservedRevisionEnd: state.reservedRevisionEnd, SnapshotRevision: state.snapshotRevision, ActiveWALStart: state.activeWALStart, Limits: state.limits}
	state.mu.RUnlock()
	status.EarliestWatchRevision = state.history.earliestAvailable()
	return status
}

func (state *DatabaseState) AdminCounts() (records, watches, queueDepth, queueCapacity int) {
	state.mu.RLock()
	records = len(state.records)
	state.mu.RUnlock()
	return records, int(state.activeWatches.Load()), len(state.queue), cap(state.queue)
}

func (state *DatabaseState) MarkAdminDegraded() { state.degrade() }
