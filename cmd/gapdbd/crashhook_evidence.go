//go:build gapdb_crash_evidence

package main

import (
	"os"
	"strconv"
	"sync"
	"syscall"

	"github.com/spec-kitty/gapdb/internal/faultfs"
)

// processCrashHook exists only in evidence-tagged binaries. Release builds do
// not read these environment variables and contain no process-kill path.
type processCrashHook struct {
	mu         sync.Mutex
	point      faultfs.Point
	phase      faultfs.Phase
	occurrence uint64
	counts     map[faultfs.Event]uint64
}

func evidenceFS() faultfs.FS {
	point := faultfs.Point(os.Getenv("GAPDB_CRASH_POINT"))
	phase := faultfs.Phase(os.Getenv("GAPDB_CRASH_PHASE"))
	occurrence, err := strconv.ParseUint(os.Getenv("GAPDB_CRASH_OCCURRENCE"), 10, 64)
	if point == "" || (phase != faultfs.Before && phase != faultfs.After) || err != nil || occurrence == 0 {
		return nil
	}
	return faultfs.NewOS(&processCrashHook{point: point, phase: phase, occurrence: occurrence, counts: make(map[faultfs.Event]uint64)})
}

func (hook *processCrashHook) Visit(event faultfs.Event) error {
	hook.mu.Lock()
	hook.counts[event]++
	matched := event.Point == hook.point && event.Phase == hook.phase && hook.counts[event] == hook.occurrence
	hook.mu.Unlock()
	if matched {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	return nil
}
