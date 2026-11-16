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

// ControlMode limits what the adaptive controller may do.
type ControlMode string

const (
	ControlObserve ControlMode = "observe"
	ControlSafe    ControlMode = "safe"
	ControlAuto    ControlMode = "auto"
)

func parseControlMode(value string) ControlMode {
	switch ControlMode(value) {
	case ControlObserve, ControlSafe, ControlAuto:
		return ControlMode(value)
	default:
		return ControlSafe
	}
}

// Impact is a behavioural policy, not a QPS value.
type Impact string

const (
	ImpactLow      Impact = "low"
	ImpactBalanced Impact = "balanced"
	ImpactFast     Impact = "fast"
)

func parseImpact(value string) Impact {
	switch Impact(value) {
	case ImpactLow, ImpactBalanced, ImpactFast:
		return Impact(value)
	default:
		return ImpactBalanced
	}
}

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
	// Mode, Impact, WorkerLimit and DeviceSource are the P0 control inputs.
	// An empty Mode means safe; an empty Impact means balanced.
	Mode         string
	Impact       string
	WorkerLimit  int
	DeviceSource string
	StartQPS     int
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

	mode           ControlMode
	impact         Impact
	device         deviceSampler
	workerLimit    atomic.Int64
	devicePressure atomic.Int64
	deviceReason   atomic.Value // string
	lastObserveLog string
	targetQPS      float64
	calibrated     bool
	sampleCount    int
	autoCritical   int
	lastCritical   time.Time
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
	startQPS := limits.StartQPS
	if startQPS < 1 {
		startQPS = limits.BaseQPS
	}
	if limits.StartQPS < 1 && limits.Adaptive && startQPS >= 10 {
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
		cpu:      NewCPUGuardForCores(limits.MaxCPUPercent, runtime.NumCPU()),
		log:      log,
		mode:     parseControlMode(limits.Mode),
		impact:   parseImpact(limits.Impact),
	}
	controller.workerLimit.Store(int64(normalizeWorkerLimit(limits.WorkerLimit)))
	controller.targetQPS = float64(limits.BaseQPS)
	if limits.DeviceSource != "" {
		if sampler := newDeviceSampler(limits.DeviceSource); sampler != nil {
			controller.device = sampler
		}
	}
	controller.reason.Store("")
	controller.deviceReason.Store("")
	controller.log.Info("扫描控制器启动",
		"mode", controller.mode, "impact", controller.impact,
		"start_qps", round1(float64(startQPS)), "base_qps", limits.BaseQPS,
		"worker_limit", controller.workerLimit.Load())
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
	c.mode = parseControlMode(limits.Mode)
	c.impact = parseImpact(limits.Impact)
	if limits.WorkerLimit > 0 {
		c.workerLimit.Store(int64(normalizeWorkerLimit(limits.WorkerLimit)))
	}
	c.cpu.SetCores(runtime.NumCPU())
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
	limits := c.snapshotLimits()
	pressure := false
	critical := false
	reason := ""

	metrics := sampler.Sample()
	if metrics.Supported {
		if limits.MaxHostCPU > 0 && metrics.CPUPercent > limits.MaxHostCPU {
			pressure = true
			reason = "宿主机 CPU 高"
		}
		if limits.MaxLoad1 > 0 && metrics.Load1 > limits.MaxLoad1 {
			pressure = true
			if reason == "" {
				reason = "宿主机 Load 高"
			}
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
			reason = "元数据延迟高"
		}
	}

	// Device pressure is a first-class signal. It is deliberately not reduced
	// to "%util": on HDD, SSD, RAID and shared devices the useful signals are
	// latency and queue depth. The sampler returns Valid=false for the first
	// sample and for non-local mounts.
	if c.device != nil {
		dm := c.device.Sample()
		assessment := assessDevice(dm)
		c.devicePressure.Store(int64(assessment.Level))
		if assessment.Level != DevicePressureNone {
			c.deviceReason.Store(assessment.Reason)
			if c.mode == ControlObserve {
				c.logObserveDevice(assessment)
			} else {
				pressure = true
				if reason == "" {
					reason = assessment.Reason
				}
				if assessment.Level == DevicePressureCritical {
					critical = true
					if c.mode == ControlAuto {
						now := time.Now()
						if !c.lastCritical.IsZero() && now.Sub(c.lastCritical) <= 10*time.Minute {
							c.autoCritical++
						} else {
							c.autoCritical = 1
						}
						c.lastCritical = now
						if c.autoCritical >= 2 {
							c.mode = ControlSafe
							c.log.Warn("auto 模式连续遇到设备严重压力，回退到 safe", "device", dm.Name)
						}
					}
				}
			}
		} else {
			c.deviceReason.Store("")
		}
	}

	if !limits.Adaptive {
		return
	}
	if c.mode != ControlObserve {
		c.sampleCount++
		if !c.calibrated && c.sampleCount >= 2 && latency > 0 {
			capacity := float64(c.workerLimit.Load()) / latency
			if capacity > float64(limits.BaseQPS) {
				capacity = float64(limits.BaseQPS)
			}
			ratio := impactRatio(c.impact)
			c.targetQPS = capacity * ratio
			if c.targetQPS < float64(limits.MinQPS) {
				c.targetQPS = float64(limits.MinQPS)
			}
			c.calibrated = true
			c.log.Info("完成起始容量探测", "capacity_qps", round1(capacity),
				"target_qps", round1(c.targetQPS), "impact", c.impact)
		}
	}
	if c.mode == ControlObserve {
		c.logObserveDecision(pressure, critical, reason)
		// Observe must not change the scan relative to the pre-controller
		// behaviour: still run the legacy host/latency adaptation, but never
		// apply device pressure, worker reductions or device pauses.
		if pressure {
			if next := c.throttle.Rate() * 0.65; next < c.throttle.Rate() {
				if next < float64(limits.MinQPS) {
					next = float64(limits.MinQPS)
				}
				c.throttle.SetRate(next)
			}
			c.healthySamples = 0
		} else {
			c.healthySamples++
			if c.healthySamples >= 3 {
				if rate := c.throttle.Rate(); rate < float64(limits.BaseQPS) {
					next := rate * 1.2
					if next > float64(limits.BaseQPS) {
						next = float64(limits.BaseQPS)
					}
					c.throttle.SetRate(next)
				}
				c.healthySamples = 0
			}
		}
		return
	}
	if pressure {
		c.reduceWorkers(critical)
		if critical {
			c.PauseFor(limits.PauseCooldown, reason)
		}
		rate := c.throttle.Rate()
		next := rate * 0.65
		if critical {
			next = rate * 0.50
		}
		if next < float64(limits.MinQPS) {
			next = float64(limits.MinQPS)
		}
		if next < rate {
			c.throttle.SetRate(next)
			c.log.Warn("检测到资源压力，降低元数据扫描速率",
				"reason", reason, "qps", round1(next), "critical", critical,
				"host_load1", round1(metrics.Load1),
				"host_cpu", round1(metrics.CPUPercent), "metadata_latency_ms", round1(latency*1000))
		}
		c.healthySamples = 0
		return
	}

	if c.mode == ControlAuto && c.calibrated {
		rate := c.throttle.Rate()
		if rate > c.targetQPS {
			c.throttle.SetRate(c.targetQPS)
			return
		}
		if rate < c.targetQPS*0.9 {
			next := rate * 2
			if next > c.targetQPS {
				next = c.targetQPS
			}
			c.throttle.SetRate(next)
			c.log.Info("容量探测后快速爬坡", "qps", round1(next), "target", round1(c.targetQPS))
			return
		}
	}

	// Safe mode can lower an over-aggressive start after calibration, but it
	// never raises the rate within the same scan.
	if c.mode == ControlSafe && c.calibrated {
		if rate := c.throttle.Rate(); rate > c.targetQPS {
			c.throttle.SetRate(c.targetQPS)
		}
		c.healthySamples = 0
		return
	}
	if c.mode != ControlAuto {
		c.healthySamples = 0
		return
	}
	c.healthySamples++
	needed := 3
	switch c.impact {
	case ImpactFast:
		needed = 2
	case ImpactLow:
		needed = 5
	}
	if c.healthySamples >= needed {
		rate := c.throttle.Rate()
		if rate < float64(limits.BaseQPS) {
			next := rate * 1.2
			if next > float64(limits.BaseQPS) {
				next = float64(limits.BaseQPS)
			}
			c.throttle.SetRate(next)
			c.log.Info("资源恢复正常，逐步提高元数据扫描速率", "qps", round1(next))
		}
		c.healthySamples = 0
	}
}

func (c *Controller) reduceWorkers(critical bool) {
	current := c.workerLimit.Load()
	if current <= 1 {
		return
	}
	floor := int64(1)
	if initial := int64(normalizeWorkerLimit(c.snapshotLimits().WorkerLimit)); initial > 4 {
		floor = initial / 4
	}
	next := current * 3 / 4
	if critical {
		next = current / 2
	}
	if next < floor {
		next = floor
	}
	if next < current {
		c.workerLimit.Store(next)
		c.log.Warn("降低扫描有效并发", "from", current, "to", next, "critical", critical)
	}
}

func (c *Controller) logObserveDevice(a DeviceAssessment) {
	key := "device:" + a.Reason
	if c.lastObserveLog == key {
		return
	}
	c.lastObserveLog = key
	c.log.Info("observe：检测到设备压力，未改变扫描行为", "reason", a.Reason)
}

func (c *Controller) logObserveDecision(pressure, critical bool, reason string) {
	key := "decision:" + reason
	if c.lastObserveLog == key {
		return
	}
	c.lastObserveLog = key
	action := "保持"
	if critical {
		action = "建议暂停"
	} else if pressure {
		action = "建议降速"
	}
	c.log.Info("observe：控制器建议", "action", action, "reason", reason)
}

func impactRatio(impact Impact) float64 {
	switch impact {
	case ImpactLow:
		return 0.2
	case ImpactFast:
		return 0.8
	default:
		return 0.5
	}
}

func normalizeWorkerLimit(value int) int {
	if value < 1 {
		return 1
	}
	if value > 64 {
		return 64
	}
	return value
}

// RecommendedWorkers is the effective concurrency ceiling. It only decreases
// during a scan; a new scan starts from the configured worker cap again.
func (c *Controller) RecommendedWorkers() int {
	return int(c.workerLimit.Load())
}

// Mode returns the active control mode.
func (c *Controller) Mode() ControlMode { return c.mode }

// DevicePressure returns the last device pressure level.
func (c *Controller) DevicePressure() DevicePressure {
	return DevicePressure(c.devicePressure.Load())
}

// DeviceReason returns the last device pressure reason.
func (c *Controller) DeviceReason() string {
	value, _ := c.deviceReason.Load().(string)
	return value
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
