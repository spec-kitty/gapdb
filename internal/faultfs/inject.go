package faultfs

import (
	"errors"
	"sync"
)

type Point string

const (
	PointLockOpen                 Point = "lock.open"
	PointIdentityTempCreate       Point = "identity.temp_create"
	PointIdentityTempWrite        Point = "identity.temp_write"
	PointIdentityTempSync         Point = "identity.temp_sync"
	PointIdentityRename           Point = "identity.rename"
	PointIdentityDirectorySync    Point = "identity.directory_sync"
	PointWALHeaderCreate          Point = "wal.header_create"
	PointWALHeaderWrite           Point = "wal.header_write"
	PointWALFrameWrite            Point = "wal.frame_write"
	PointWALBufferFlush           Point = "wal.buffer_flush"
	PointWALFileSync              Point = "wal.file_sync"
	PointWALTailTruncate          Point = "wal.tail_truncate"
	PointWALTailSync              Point = "wal.tail_sync"
	PointSnapshotTempCreate       Point = "snapshot.temp_create"
	PointSnapshotTempWrite        Point = "snapshot.temp_write"
	PointSnapshotTempSync         Point = "snapshot.temp_sync"
	PointSnapshotRename           Point = "snapshot.rename"
	PointSnapshotDirectorySync    Point = "snapshot.directory_sync"
	PointNextWALCreate            Point = "next_wal.create"
	PointNextWALSync              Point = "next_wal.sync"
	PointNextWALRename            Point = "next_wal.rename"
	PointManifestTempCreate       Point = "manifest.temp_create"
	PointManifestTempWrite        Point = "manifest.temp_write"
	PointManifestTempSync         Point = "manifest.temp_sync"
	PointManifestRename           Point = "manifest.rename"
	PointManifestDirectorySync    Point = "manifest.directory_sync"
	PointCompactionRemove         Point = "compaction.remove"
	PointCompactionDirectorySync  Point = "compaction.directory_sync"
	PointBackupFileCopy           Point = "backup.file_copy"
	PointBackupFileSync           Point = "backup.file_sync"
	PointBackupRename             Point = "backup.rename"
	PointPublicationDirectorySync Point = "publication.directory_sync"
	PointAuditAppend              Point = "audit.append"
	PointAuditSync                Point = "audit.sync"
	PointMapApply                 Point = "map.apply"
	PointResponsePublish          Point = "response.publish"
	PointOpen                     Point = "filesystem.open"
	PointStat                     Point = "filesystem.stat"
	PointReadDir                  Point = "filesystem.read_dir"
)

type Phase string

const (
	Before Phase = "before"
	After  Phase = "after"
)

type Event struct {
	Point Point
	Phase Phase
}

type HookError struct {
	Event Event
	Err   error
}

func (e *HookError) Error() string { return e.Err.Error() }

func (e *HookError) Unwrap() error { return e.Err }

func FailedAfter(err error) bool {
	var hookError *HookError
	return errors.As(err, &hookError) && hookError.Event.Phase == After
}

var ErrProcessStop = errors.New("faultfs: simulated process stop")

type Hook interface {
	Visit(Event) error
}

type Recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *Recorder) Visit(event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

type Rule struct {
	Point      Point
	Phase      Phase
	Occurrence uint64
	Seed       uint64
	Err        error
	Stop       bool
	Observe    func(Event)
}

type Injector struct {
	mu     sync.Mutex
	seed   uint64
	rules  []Rule
	counts map[Event]uint64
}

func NewInjector(seed uint64, rules ...Rule) *Injector {
	cloned := append([]Rule(nil), rules...)
	return &Injector{seed: seed, rules: cloned, counts: make(map[Event]uint64)}
}

func (i *Injector) Visit(event Event) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.counts[event]++
	occurrence := i.counts[event]
	for _, rule := range i.rules {
		if rule.Point != event.Point || rule.Phase != event.Phase || rule.Seed != i.seed || rule.Occurrence != occurrence {
			continue
		}
		if rule.Observe != nil {
			rule.Observe(event)
		}
		if rule.Stop {
			return ErrProcessStop
		}
		if rule.Err != nil {
			return rule.Err
		}
	}
	return nil
}
