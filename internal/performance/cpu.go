package performance

import (
	"context"
	"sync"
	"time"

	"github.com/YEDEDD/dirspace/internal/platform"
)

// CPUGuard is a best-effort process CPU governor. It measures this process's
// user+system CPU over a recent window and sleeps when the configured
// percentage is exceeded. systemd CPUQuota remains the hard kernel limit.
type CPUGuard struct {
	mu       sync.Mutex
	maxRatio float64
	lastCPU  time.Duration
	lastWall time.Time
}

func NewCPUGuard(maxPercent int) *CPUGuard {
	guard := &CPUGuard{}
	guard.SetMaxPercent(maxPercent)
	return guard
}

func (g *CPUGuard) SetMaxPercent(percent int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if percent <= 0 || percent >= 100 {
		g.maxRatio = 0
		return
	}
	g.maxRatio = float64(percent) / 100
	g.lastWall = time.Time{}
}

// Wait sleeps only when the process exceeded its CPU budget since the previous
// check. It is intentionally coarse to avoid adding a syscall per file.
func (g *CPUGuard) Wait(ctx context.Context) error {
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
