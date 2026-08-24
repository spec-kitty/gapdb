package clock

import (
	"sync"
	"time"
)

// Manual is a concurrency-safe test clock. Set intentionally permits backward
// movement so tests can prove that Nondecreasing prevents expiry resurrection.
type Manual struct {
	mu  sync.RWMutex
	now time.Time
}

func NewManual(initial time.Time) *Manual {
	return &Manual{now: initial}
}

func (c *Manual) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *Manual) Set(value time.Time) {
	c.mu.Lock()
	c.now = value
	c.mu.Unlock()
}

func (c *Manual) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}
