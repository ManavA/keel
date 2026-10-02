package agenttest

import (
	"sync"
	"time"

	"github.com/ManavA/keel/agent"
)

// Clock is an agent.Clock a test moves by hand. It is safe for concurrent
// use: an engine reads it from its heartbeat while the test advances it.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock builds a Clock reading start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now implements agent.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var _ agent.Clock = (*Clock)(nil)
