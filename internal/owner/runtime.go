// Package owner composes the recovered database state with its guarded
// administrative surface. The Unix-socket owner uses this seam instead of
// constructing persistence primitives independently.
package owner

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/spec-kitty/gapdb/internal/admin"
	"github.com/spec-kitty/gapdb/internal/engine"
	"github.com/spec-kitty/gapdb/internal/persist"
)

type Runtime struct {
	state *engine.DatabaseState
	admin *admin.Controller
	owner *persist.OwnerLease

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// OpenConfig is the recovered owner-process composition handed to the WP06
// server. Open constructs the sole writer and its administrative controller as
// one lifecycle unit.
type OpenConfig struct {
	Engine    engine.Config
	Admin     admin.ControllerConfig
	Ownership *persist.OwnerLock
}

func Open(config OpenConfig) (*Runtime, error) {
	if config.Ownership == nil {
		return nil, fmt.Errorf("owner: exclusive storage ownership is required")
	}
	lease, err := config.Ownership.Claim(config.Admin.Directory)
	if err != nil {
		_ = config.Ownership.Close()
		return nil, fmt.Errorf("owner: claim exclusive storage ownership: %w", err)
	}
	state, err := engine.New(config.Engine)
	if err != nil {
		_ = lease.Close()
		return nil, err
	}
	config.Admin.State = state
	controller, err := admin.NewController(config.Admin)
	if err != nil {
		_ = state.Close(context.Background())
		_ = lease.Close()
		return nil, err
	}
	return &Runtime{state: state, admin: controller, owner: lease, closeDone: make(chan struct{})}, nil
}

func (runtime *Runtime) State() *engine.DatabaseState { return runtime.state }
func (runtime *Runtime) Status() admin.RunningView    { return runtime.admin.Status() }
func (runtime *Runtime) SecureOwnerLock() error       { return runtime.owner.SecureHeldPath() }
func (runtime *Runtime) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runtime.closeOnce.Do(func() {
		go func() {
			runtime.closeErr = errors.Join(runtime.state.Close(context.Background()), runtime.owner.Close())
			close(runtime.closeDone)
		}()
	})
	select {
	case <-runtime.closeDone:
		return runtime.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (runtime *Runtime) Snapshot(ctx context.Context, request admin.SnapshotRequest) (engine.SnapshotResult, error) {
	return runtime.admin.Snapshot(ctx, request)
}
func (runtime *Runtime) Compact(request admin.CompactionRequest) (admin.CompactionView, error) {
	return runtime.admin.Compact(request)
}
func (runtime *Runtime) Backup(request admin.BackupRequest) (persist.BackupMetadata, error) {
	return runtime.admin.Backup(request)
}
func (runtime *Runtime) Restore(request admin.RestoreRequest) (persist.BackupMetadata, error) {
	return runtime.admin.Restore(request)
}
func (runtime *Runtime) InspectOffline() (admin.Inspection, error) {
	return runtime.admin.InspectOffline()
}
func (runtime *Runtime) VerifyOffline() (admin.Inspection, error) {
	return runtime.admin.VerifyOffline()
}
func (runtime *Runtime) ApplyRecovery(proposal admin.RecoveryProposal, options admin.ApplyOptions) (admin.ApplyResult, error) {
	return runtime.admin.ApplyRecovery(proposal, options)
}
