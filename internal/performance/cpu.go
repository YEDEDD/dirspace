package performance

import (
	"context"
	"sync"
	"time"

	"github.com/YEDEDD/dirspace/internal/platform"
)

// CPUGuard is a best-effort process CPU governor. It measures this process's
// user+system CPU over a recent window and sleeps when the configured
// percentage is exceeded.
//
// The guard is global: one caller sleeps while the others wait, so dozens of
// metadata workers cannot each sleep independently and still exceed the
// budget together. systemd CPUQuota remains the hard kernel limit.
type CPUGuard struct {
	mu       sync.Mutex
	sleepMu  sync.Mutex
	maxRatio float64
	cores    int
	lastCPU  time.Duration
	lastWall time.Time
}

// NewCPUGuard constructs a guard whose percentage is relative to one core.
// It is kept for tests and callers that do not know the machine width.
func NewCPUGuard(maxPercent int) *CPUGuard {
	return NewCPUGuardForCores(maxPercent, 1)
}

// NewCPUGuardForCores interprets maxPercent as a percentage of the total CPU
// capacity represented by cores. For example 50% on a 4-core host allows two
// cores of process CPU, not half of one core.
func NewCPUGuardForCores(maxPercent, cores int) *CPUGuard {
	guard := &CPUGuard{}
	guard.SetCores(cores)
	guard.SetMaxPercent(maxPercent)
	return guard
}

func (g *CPUGuard) SetCores(cores int) {
	if cores < 1 {
		cores = 1
	}
	g.mu.Lock()
	g.cores = cores
	g.maxRatio = 0
	g.mu.Unlock()
}

func (g *CPUGuard) SetMaxPercent(percent int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if percent <= 0 || percent > 100 {
		g.maxRatio = 0
		return
	}
	cores := g.cores
	if cores < 1 {
		cores = 1
	}
	g.maxRatio = float64(percent) / 100 * float64(cores)
	g.lastWall = time.Time{}
}

// Wait sleeps only when the process exceeded its CPU budget since the previous
// check. The sleep lock makes the budget global rather than per caller.
func (g *CPUGuard) Wait(ctx context.Context) error {
	g.sleepMu.Lock()
	defer g.sleepMu.Unlock()

	g.mu.Lock()
	maxRatio := g.maxRatio
	if maxRatio <= 0 {
		g.mu.Unlock()
		return nil
	}
	now := time.Now()
	if g.lastWall.IsZero() {
		cpu, err := platform.ProcessCPUTime()
		if err != nil {
			g.mu.Unlock()
			return nil
		}
		g.lastCPU = cpu
		g.lastWall = now
		g.mu.Unlock()
		return nil
	}
	if now.Sub(g.lastWall) < 250*time.Millisecond {
		g.mu.Unlock()
		return nil
	}
	cpu, err := platform.ProcessCPUTime()
	if err != nil {
		g.mu.Unlock()
		return nil
	}
	deltaCPU := cpu - g.lastCPU
	deltaWall := now.Sub(g.lastWall)
	g.lastCPU = cpu
	g.lastWall = now
	g.mu.Unlock()
	if deltaWall <= 0 || deltaCPU <= 0 {
		return nil
	}
	ratio := float64(deltaCPU) / float64(deltaWall)
	if ratio <= maxRatio {
		return nil
	}
	// Time needed for the same CPU work to average maxRatio.
	targetWall := time.Duration(float64(deltaCPU) / maxRatio)
	sleep := targetWall - deltaWall
	if sleep > 2*time.Second {
		sleep = 2 * time.Second
	}
	if sleep <= 0 {
		return nil
	}
	timer := time.NewTimer(sleep)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	return nil
}
