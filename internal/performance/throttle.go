package performance

import (
	"context"
	"math"
	"sync"
	"time"
)

// Throttle is a dynamically adjustable token bucket for metadata operations.
// It keeps a 10ms burst window so high QPS limits are not defeated by timer
// granularity; the sustained rate remains the configured QPS.
type Throttle struct {
	mu       sync.Mutex
	rate     float64
	min      float64
	capacity float64
	tokens   float64
	last     time.Time
}

// bucketCapacity gives the bucket a small burst window (10ms of tokens) so
// high QPS settings are not limited by the kernel/Go timer granularity. The
// sustained rate still follows rate; only a short burst is absorbed.
func bucketCapacity(rate float64) float64 {
	capacity := rate / 100
	if capacity < 1 {
		capacity = 1
	}
	if capacity > 256 {
		capacity = 256
	}
	return capacity
}

// NewThrottle creates a throttle. rate and min are requests per second.
func NewThrottle(rate, min int) *Throttle {
	if rate < 1 {
		rate = 1
	}
	if min < 1 {
		min = 1
	}
	if min > rate {
		min = rate
	}
	rateF := float64(rate)
	capacity := bucketCapacity(rateF)
	return &Throttle{
		rate:     rateF,
		min:      float64(min),
		capacity: capacity,
		tokens:   capacity,
		last:     time.Now(),
	}
}

// Wait blocks until one metadata request may be issued or the context ends.
func (t *Throttle) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		t.mu.Lock()
		elapsed := now.Sub(t.last).Seconds()
		if elapsed > 0 {
			t.tokens = math.Min(t.capacity, t.tokens+elapsed*t.rate)
			t.last = now
		}
		if t.tokens >= 1 {
			t.tokens -= 1
			t.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - t.tokens) / t.rate * float64(time.Second))
		t.mu.Unlock()
		// Do not round short waits up to a millisecond: at 4000 QPS the
		// next token is only 250µs away, and a 1ms floor silently caps the
		// bucket at about 1000 QPS. Keep a small floor only to avoid
		// zero-duration timers at extreme configured rates.
		if wait < 20*time.Microsecond {
			wait = 20 * time.Microsecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// SetRate changes the sustained request rate.
func (t *Throttle) SetRate(rate float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rate < t.min {
		rate = t.min
	}
	t.rate = rate
	t.capacity = bucketCapacity(rate)
	if t.tokens > t.capacity {
		t.tokens = t.capacity
	}
}

// SetMin changes the lower bound used by adaptive throttling.
func (t *Throttle) SetMin(min float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if min < 1 {
		min = 1
	}
	t.min = min
	if t.rate < t.min {
		t.rate = t.min
	}
}

// Rate returns the current sustained rate.
func (t *Throttle) Rate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rate
}
