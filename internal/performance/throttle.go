package performance

import (
	"context"
	"math"
	"sync"
	"time"
)

// Throttle is a strict, dynamically adjustable token bucket for NFS metadata
// operations. Capacity is one token, so the configured QPS is a real cap rather
// than a burst size.
type Throttle struct {
	mu     sync.Mutex
	rate   float64
	min    float64
	tokens float64
	last   time.Time
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
	return &Throttle{
		rate:   float64(rate),
		min:    float64(min),
		tokens: 1,
		last:   time.Now(),
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
			t.tokens = math.Min(1, t.tokens+elapsed*t.rate)
			t.last = now
		}
		if t.tokens >= 1 {
			t.tokens -= 1
			t.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - t.tokens) / t.rate * float64(time.Second))
		t.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
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
	if t.tokens > 1 {
		t.tokens = 1
	}
}

// Rate returns the current sustained rate.
func (t *Throttle) Rate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rate
}
