package engine

import (
	"sync"

	"gapdb/gapdb"
	"gapdb/internal/persist"
)

// RevisionAllocator supplies values from storage-reserved ranges. The writer is
// its only engine caller, which keeps revision assignment in commit order.
type RevisionAllocator interface {
	Next() (gapdb.Revision, error)
}

type ReserveFunc func(after gapdb.Revision) (persist.RevisionRange, error)

type RangeAllocator struct {
	mu        sync.Mutex
	next      gapdb.Revision
	end       gapdb.Revision
	exhausted bool
	reserve   ReserveFunc
}

func NewRangeAllocator(current gapdb.Revision, allocation persist.RevisionRange, reserve ReserveFunc) (*RangeAllocator, error) {
	if err := validateRange(allocation, current); err != nil {
		return nil, err
	}
	return &RangeAllocator{next: allocation.First, end: allocation.End, reserve: reserve}, nil
}

func (allocator *RangeAllocator) Next() (gapdb.Revision, error) {
	allocator.mu.Lock()
	defer allocator.mu.Unlock()

	if allocator.exhausted {
		return 0, revisionRangeExhausted(allocator.end, "the durable revision space is exhausted", nil)
	}
	if allocator.next > allocator.end {
		if allocator.reserve == nil {
			return 0, revisionRangeExhausted(allocator.end, "the durable revision reservation is exhausted", nil)
		}
		allocation, err := allocator.reserve(allocator.end)
		if err != nil {
			return 0, err
		}
		if err := validateRange(allocation, allocator.end); err != nil {
			return 0, err
		}
		allocator.next, allocator.end = allocation.First, allocation.End
	}
	revision := allocator.next
	if revision == gapdb.Revision(^uint64(0)) {
		allocator.exhausted = true
	} else {
		allocator.next++
	}
	return revision, nil
}

func validateRange(allocation persist.RevisionRange, after gapdb.Revision) error {
	if allocation.First == 0 || allocation.First <= after || allocation.End < allocation.First {
		return revisionRangeExhausted(after, "the supplied revision range is not strictly after the committed revision", nil)
	}
	return nil
}

func revisionRangeExhausted(end gapdb.Revision, reason string, cause error) error {
	reserved := end
	return &gapdb.Error{
		Code:                gapdb.CodeRevisionRangeExhausted,
		Message:             "A safe revision range could not be reserved.",
		Retry:               gapdb.RetryAfterRestart,
		Reason:              reason,
		ReservedRevisionEnd: &reserved,
		SafeActions: []gapdb.SafeAction{
			gapdb.ActionVerifyStorage,
			gapdb.ActionRestartAfterRecovery,
			gapdb.ActionAbort,
		},
		Cause: cause,
	}
}
