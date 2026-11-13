package performance

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Limits are the scanner's resource guardrails.
type Limits struct {
	BaseQPS          int
	MinQPS           int
	MaxCPUPercent    int
	MemoryLimit      int64
	Adaptive         bool
	SampleInterval   time.Duration
	MaxHostCPU       float64
	MaxLoad1         float64
	SlowLatency      time.Duration
	PauseLatency     time.Duration
	PauseCooldown    time.Duration
	MaxMetadataError int
}

// Controller combines the metadata QPS token bucket, process CPU governor,
// memory pressure check, and adaptive NFS/host-load feedback.
type Controller struct {
	limitsMu sync.RWMutex
	limits   Limits
	throttle *Throttle
	cpu      *CPUGuard
	log      *slog.Logger

	mu          sync.Mutex
	latencyEWMA float64 // seconds
	errorTimes  []time.Time

	pauseUntil     atomic.Int64
	memoryPressure atomic.Bool
	metadataOps    atomic.Int64
	paused         atomic.Bool
	reason         atomic.Value // string
	lastPauseLog   int64
	lastMemoryGC   time.Time
	healthySamples int
}

func NewController(limits Limits, log *slog.Logger) *Controller {
	if limits.MinQPS < 1 {
		limits.MinQPS = 1
	}
	if limits.MinQPS > limits.BaseQPS {
		limits.MinQPS = limits.BaseQPS
	}
	if limits.SampleInterval < time.Second {
		limits.SampleInterval = 5 * time.Second
	}
	if limits.MaxMetadataError < 1 {
		limits.MaxMetadataError = 8
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	startQPS := limits.BaseQPS
	if limits.Adaptive && startQPS >= 10 {
		// Start below the ceiling and let the sampler ramp up only while the
		// filesystem stays healthy. This avoids hitting a shared NFS server at
		// full rate in the first seconds of a scan.
		startQPS = startQPS / 3
		if startQPS < limits.MinQPS {
			startQPS = limits.MinQPS
		}
	}
	controller := &Controller{
		limits:   limits,
		throttle: NewThrottle(startQPS, limits.MinQPS),
		cpu:      NewCPUGuard(limits.MaxCPUPercent),
		log:      log,
	}
	controller.reason.Store("")
	if limits.MemoryLimit > 0 {
		debug.SetMemoryLimit(limits.MemoryLimit)
	}
	return controller
}

func (c *Controller) snapshotLimits() Limits {
	c.limitsMu.RLock()
	defer c.limitsMu.RUnlock()
	return c.limits
}

// UpdateLimits applies new resource limits to a running scan. It updates QPS,
// CPU, memory, host-load and latency thresholds. The sampler interval is fixed
// for the lifetime of a scan; the worker/semaphore limit is handled separately.
func (c *Controller) UpdateLimits(limits Limits) {
	if limits.MinQPS < 1 {
		limits.MinQPS = 1
	}
	if limits.MinQPS > limits.BaseQPS {
		limits.MinQPS = limits.BaseQPS
	}
	if limits.SampleInterval < time.Second {
		limits.SampleInterval = 5 * time.Second
	}
	if limits.MaxMetadataError < 1 {
		limits.MaxMetadataError = 8
	}
	c.limitsMu.Lock()
	old := c.limits
	c.limits = limits
	c.limitsMu.Unlock()

	c.throttle.SetMin(float64(limits.MinQPS))
	rate := c.throttle.Rate()
	if rate > float64(limits.BaseQPS) {
		rate = float64(limits.BaseQPS)
	}
	if rate < float64(limits.MinQPS) {
		rate = float64(limits.MinQPS)
	}
	if !limits.Adaptive {
		rate = float64(limits.BaseQPS)
	}
	c.throttle.SetRate(rate)
	c.cpu.SetMaxPercent(limits.MaxCPUPercent)
	if limits.MemoryLimit > 0 {
		debug.SetMemoryLimit(limits.MemoryLimit)
	}
	c.log.Info("已热更新性能配置",
		"old_qps", old.BaseQPS, "new_qps", limits.BaseQPS,
		"old_cpu", old.MaxCPUPercent, "new_cpu", limits.MaxCPUPercent,
		"current_qps", round1(rate))
}

// Start launches the background sampler until ctx is cancelled.
func (c *Controller) Start(ctx context.Context) {
	go c.loop(ctx)
}

func (c *Controller) loop(ctx context.Context) {
	ticker := time.NewTicker(c.snapshotLimits().SampleInterval)
	defer ticker.Stop()
	sampler := &hostSampler{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sample(sampler)
		}
	}
}

func (c *Controller) sample(sampler *hostSampler) {
	pressure := false
	reason := ""
	limits := c.snapshotLimits()

	metrics := sampler.Sample()
	if metrics.Supported {
		if limits.MaxHostCPU > 0 && metrics.CPUPercent > limits.MaxHostCPU {
			pressure = true
			reason = "宿主机 CPU 高"
		}
		if limits.MaxLoad1 > 0 && metrics.Load1 > limits.MaxLoad1 {
			pressure = true
			reason = "宿主机 Load 高"
		}
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	if limits.MemoryLimit > 0 && mem.HeapAlloc > uint64(float64(limits.MemoryLimit)*0.85) {
		pressure = true
		reason = "进程内存接近上限"
		c.memoryPressure.Store(true)
		if time.Since(c.lastMemoryGC) > 10*time.Second {
			c.lastMemoryGC = time.Now()
			runtime.GC()
			c.log.Warn("内存达到阈值，已触发 GC 并暂停元数据请求",
				"heap_alloc_mb", mem.HeapAlloc>>20, "limit_mb", limits.MemoryLimit>>20)
		}
	} else {
		c.memoryPressure.Store(false)
	}

	c.mu.Lock()
	latency := c.latencyEWMA
	c.mu.Unlock()
	if limits.SlowLatency > 0 && latency > limits.SlowLatency.Seconds() {
		pressure = true
		if reason == "" {
			reason = "NFS 元数据延迟高"
		}
	}

	if !limits.Adaptive {
		return
	}
	rate := c.throttle.Rate()
	if pressure {
		next := rate * 0.65
		if next < float64(limits.MinQPS) {
			next = float64(limits.MinQPS)
		}
		if next < rate {
			c.throttle.SetRate(next)
			c.log.Warn("检测到资源压力，降低元数据扫描速率",
				"reason", reason, "qps", round1(next), "host_load1", round1(metrics.Load1),
				"host_cpu", round1(metrics.CPUPercent), "nfs_latency_ms", round1(latency*1000))
		}
		c.healthySamples = 0
	} else {
		c.healthySamples++
		if c.healthySamples >= 3 && rate < float64(limits.BaseQPS) {
			next := rate * 1.2
			if next > float64(limits.BaseQPS) {
				next = float64(limits.BaseQPS)
			}
			c.throttle.SetRate(next)
			c.log.Info("资源恢复正常，逐步提高元数据扫描速率", "qps", round1(next))
			c.healthySamples = 0
		}
	}
}

// Wait blocks for one NFS metadata operation. It respects pause, memory
// pressure, the QPS token bucket, and the CPU governor.
func (c *Controller) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		until := c.pauseUntil.Load()
		if until > 0 && time.Now().UnixNano() < until {
			c.paused.Store(true)
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if until > 0 {
			c.pauseUntil.CompareAndSwap(until, 0)
			c.paused.Store(false)
		}
		if c.memoryPressure.Load() {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if err := c.throttle.Wait(ctx); err != nil {
			return err
		}
		return c.cpu.Wait(ctx)
	}
}

// ObserveMetadata records the latency and network errors of one metadata call.
func (c *Controller) ObserveMetadata(d time.Duration, err error) {
	limits := c.snapshotLimits()
	c.metadataOps.Add(1)
	if d > 0 {
		seconds := d.Seconds()
		c.mu.Lock()
		if c.latencyEWMA == 0 {
			c.latencyEWMA = seconds
		} else {
			c.latencyEWMA = c.latencyEWMA*0.95 + seconds*0.05
		}
		c.mu.Unlock()
		if limits.PauseLatency > 0 && d >= limits.PauseLatency {
			c.PauseFor(limits.PauseCooldown, "NFS 元数据延迟超过暂停阈值")
		}
	}
	if err == nil || !isNetworkError(err) {
		return
	}
	now := time.Now()
	c.mu.Lock()
	cutoff := now.Add(-30 * time.Second)
	kept := c.errorTimes[:0]
	for _, t := range c.errorTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.errorTimes = append(kept, now)
	count := len(c.errorTimes)
	c.mu.Unlock()
	if count >= limits.MaxMetadataError {
		c.PauseFor(limits.PauseCooldown, "NFS 元数据请求连续异常")
	}
}

// PauseFor pauses metadata calls for a cooldown window.
func (c *Controller) PauseFor(d time.Duration, reason string) {
	if d <= 0 {
		return
	}
	until := time.Now().Add(d).UnixNano()
	if until > c.pauseUntil.Load() {
		c.pauseUntil.Store(until)
	}
	c.reason.Store(reason)
	now := time.Now().UnixNano()
	c.mu.Lock()
	shouldLog := now-c.lastPauseLog > int64(5*time.Second)
	if shouldLog {
		c.lastPauseLog = now
	}
	c.mu.Unlock()
	if shouldLog {
		c.log.Warn("自动暂停 NFS 元数据扫描", "reason", reason, "cooldown", d.String())
	}
}

// Paused reports the current pause state and reason.
func (c *Controller) Paused() (bool, string) {
	paused := c.paused.Load() || c.pauseUntil.Load() > time.Now().UnixNano() || c.memoryPressure.Load()
	reason, _ := c.reason.Load().(string)
	return paused, reason
}

// Rate is the current metadata QPS.
func (c *Controller) Rate() float64 { return c.throttle.Rate() }

// MetadataOps returns the number of metadata operations observed by this
// process. It is used by status to report the actual scan rate.
func (c *Controller) MetadataOps() int64 { return c.metadataOps.Load() }

// Latency returns the current NFS metadata EWMA in milliseconds.
func (c *Controller) Latency() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latencyEWMA * 1000
}

func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	for _, target := range []error{
		syscall.ETIMEDOUT, syscall.ESTALE, syscall.EIO, syscall.ENOTCONN,
		syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
