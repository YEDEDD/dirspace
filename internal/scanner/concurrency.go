package scanner

import (
	"context"
	"sync"
)

// maxScanWorkers is the largest worker count accepted by config validation.
// Worker goroutines are cheap; the shared semaphore is what actually limits
// metadata concurrency, so a reload can move the limit without rebuilding the
// pools.
const maxScanWorkers = 64

// scanConcurrency is a context-aware semaphore whose limit can be changed
// while workers are waiting. Reducing the limit does not interrupt tasks that
// already own a slot; new tasks wait until the active count drops below it.
type scanConcurrency struct {
	mu     sync.Mutex
	limit  int
	active int
	wake   chan struct{}
}

func newScanConcurrency(limit int) *scanConcurrency {
	if limit < 1 {
		limit = 1
	}
	if limit > maxScanWorkers {
		limit = maxScanWorkers
	}
	return &scanConcurrency{limit: limit, wake: make(chan struct{})}
}

// Acquire blocks until a slot is free or ctx ends.
func (c *scanConcurrency) Acquire(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.active < c.limit {
			c.active++
			c.mu.Unlock()
			return nil
		}
		wake := c.wake
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

// Release returns one slot and wakes one waiter when capacity is available.
// Only SetLimit broadcasts; waking every waiter on every metadata operation
// would cause a thundering herd with dozens of tree workers.
func (c *scanConcurrency) Release() {
	c.mu.Lock()
	if c.active > 0 {
		c.active--
	}
	if c.active < c.limit {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	c.mu.Unlock()
}

// SetLimit changes the maximum number of concurrent tasks. Existing tasks are
// allowed to finish when the limit is lowered.
func (c *scanConcurrency) SetLimit(limit int) {
	if limit < 1 {
		limit = 1
	}
	if limit > maxScanWorkers {
		limit = maxScanWorkers
	}
	c.mu.Lock()
	c.limit = limit
	c.broadcastLocked()
	c.mu.Unlock()
}

func (c *scanConcurrency) Limit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

func (c *scanConcurrency) Active() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *scanConcurrency) broadcastLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}
