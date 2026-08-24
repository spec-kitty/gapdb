package clock

import (
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
}

type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Nondecreasing clamps source-clock rollback to the latest instant already
// observed by this wrapper. One wrapper should be shared for one owner process.
type Nondecreasing struct {
	mu          sync.Mutex
	source      Clock
	initialized bool
	latest      time.Time
}

func NewNondecreasing(source Clock) *Nondecreasing {
	if source == nil {
		panic("clock: nil source")
	}
	return &Nondecreasing{source: source}
}

func (c *Nondecreasing) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.source.Now().UTC()
	if !c.initialized || now.After(c.latest) {
		c.latest = now
		c.initialized = true
	}
	return c.latest
}

func IsExpired(expiresAt, effectiveNow time.Time) bool {
	return !effectiveNow.Before(expiresAt)
}

func FormatUTC(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
