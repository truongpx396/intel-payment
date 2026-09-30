package app

import (
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// coalesceWindow is how often one (scope, limit, kind) may publish a warning or a block. A warning
// repeats on every request past a threshold; money intents never collapse, notifications do
// (bus-subjects.md: "billing.warn and billing.blocked are coalesced; intents are not").
const coalesceWindow = time.Minute

// coalescer suppresses repeats of an event within a window. It is per process and best effort: a
// second replica may publish once more, which the receiving side already tolerates.
type coalescer struct {
	clock  ports.Clock
	window time.Duration
	mu     sync.Mutex
	last   map[string]time.Time
}

func newCoalescer(c ports.Clock, w time.Duration) *coalescer {
	return &coalescer{clock: c, window: w, last: map[string]time.Time{}}
}

// allow reports whether key may publish now, and records it if so.
func (c *coalescer) allow(key string) bool {
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.last[key]; ok && now.Sub(at) < c.window {
		return false
	}
	c.last[key] = now
	if len(c.last) > 4096 { // bound the map: drop what has already aged out
		for k, at := range c.last {
			if now.Sub(at) >= c.window {
				delete(c.last, k)
			}
		}
	}
	return true
}
