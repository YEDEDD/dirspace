package performance

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
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
	latencyRing [256]float64
	latencyNext int
	latencyUsed int
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

	deviceBaseline    float64
	deviceBaselineSet bool
	latencyBaseline   float64
	pressureLevel     PressureLevel
	candidateLevel    PressureLevel
	candidateCount    int
	lastPressureAt    time.Time
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
	rawLevel := PressureNormal
	reason := ""

	metrics := sampler.Sample()
	if metrics.Supported {
		if limits.MaxHostCPU > 0 && metrics.CPUPercent > limits.MaxHostCPU {
			rawLevel = maxPressure(rawLevel, PressureWarning)
			reason = "宿主机 CPU 高"
		}
		if limits.MaxLoad1 > 0 && metrics.Load1 > limits.MaxLoad1 {
			rawLevel = maxPressure(rawLevel, PressureWarning)
			if reason == "" {
				reason = "宿主机 Load 高"
			}
		}
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	if limits.MemoryLimit > 0 && mem.HeapAlloc > uint64(float64(limits.MemoryLimit)*0.85) {
		rawLevel = maxPressure(rawLevel, PressureCritical)
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
	latency := c.latencyP95Locked()
	if latency <= 0 {
		latency = c.latencyEWMA
	}
	c.mu.Unlock()
	if latency > 0 {
		if c.latencyBaseline <= 0 || (c.sampleCount < 6 && latency < c.latencyBaseline) {
			c.latencyBaseline = latency
		}
		level, why := classifyLatency(latency, c.latencyBaseline)
		if level > rawLevel {
			rawLevel = level
			if why != "" {
				reason = why
			}
		}
	}
	if limits.SlowLatency > 0 && latency > limits.SlowLatency.Seconds() {
		rawLevel = maxPressure(rawLevel, PressureCritical)
		if reason == "" {
			reason = "元数据延迟超过绝对阈值"
		}
	}

	if c.device != nil {
		dm := c.device.Sample()
		c.devicePressure.Store(int64(assessmentFromLevel(PressureNormal)))
		if dm.Supported && dm.Valid {
			await := deviceAwait(dm)
			if !c.deviceBaselineSet || (c.sampleCount < 6 && await > 0 && (c.deviceBaseline <= 0 || await < c.deviceBaseline)) {
				c.deviceBaseline = await
				c.deviceBaselineSet = true
			}
			level, why := classifyDevice(dm, c.deviceBaseline)
			c.devicePressure.Store(int64(assessmentFromLevel(level)))
			if level > rawLevel {
				rawLevel = level
				if why != "" {
					reason = why
				}
			}
			if c.mode == ControlObserve && level >= PressureWarning {
				c.logObserveDevice(DeviceAssessment{Level: assessmentFromLevel(level), Reason: why})
			}
			if level == PressureCritical && c.mode == ControlAuto {
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

	if !limits.Adaptive {
		return
	}
	if c.sampleCount < 1000000 {
		c.sampleCount++
	}

	level := c.confirmPressure(rawLevel, reason)
	if c.mode == ControlObserve {
		c.logObserveDecision(level, reason)
		return
	}

	switch level {
	case PressureCritical:
		if rawLevel >= PressureWarning {
			c.reduceWorkers(true)
			c.PauseFor(limits.PauseCooldown, reason)
			if rate := c.throttle.Rate(); rate > float64(limits.MinQPS) {
				next := rate * 0.50
				if next < float64(limits.MinQPS) {
					next = float64(limits.MinQPS)
				}
				if time.Since(c.lastPressureAt) >= 10*time.Second {
					c.throttle.SetRate(next)
					c.lastPressureAt = time.Now()
				}
			}
		}
		c.healthySamples = 0
		return
	case PressureWarning:
		if rawLevel >= PressureWarning {
			c.reduceWorkers(false)
			if time.Since(c.lastPressureAt) >= 30*time.Second {
				if rate := c.throttle.Rate(); rate > float64(limits.MinQPS) {
					next := rate * 0.75
					if next < float64(limits.MinQPS) {
						next = float64(limits.MinQPS)
					}
					c.throttle.SetRate(next)
					c.lastPressureAt = time.Now()
					c.log.Warn("压力警告，降低元数据扫描速率",
						"reason", reason, "qps", round1(next))
				}
			}
		}
		c.healthySamples = 0
		return
	case PressureWatch:
		// Stop increasing, but keep the current rate. Watch is a sustained
		// signal to hold, not to immediately back off.
		c.healthySamples = 0
		return
	}

	// Capacity calibration: once per scan, after a short low-rate probe.
	if c.mode != ControlObserve && !c.calibrated && c.sampleCount >= 2 && latency > 0 {
		capacity := float64(c.workerLimit.Load()) / latency
		if capacity > float64(limits.BaseQPS) {
			capacity = float64(limits.BaseQPS)
		}
		ratio := impactRatio(c.impact)
		if c.mode == ControlSafe {
			ratio = 1
		}
		c.targetQPS = capacity * ratio
		if c.targetQPS < float64(limits.MinQPS) {
			c.targetQPS = float64(limits.MinQPS)
		}
		c.calibrated = true
		c.log.Info("完成起始容量探测", "capacity_qps", round1(capacity),
			"target_qps", round1(c.targetQPS), "impact", c.impact)
	}

	if c.mode == ControlAuto && c.calibrated {
		rate := c.throttle.Rate()
		if rate > c.targetQPS {
			c.throttle.SetRate(c.targetQPS)
			c.healthySamples = 0
			return
		}
		if rate < c.targetQPS*0.9 {
			next := rate * 2
			if next > c.targetQPS {
				next = c.targetQPS
			}
			c.throttle.SetRate(next)
			c.log.Info("容量探测后快速爬坡", "qps", round1(next), "target", round1(c.targetQPS))
			c.healthySamples = 0
			return
		}
	}

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

func assessmentFromLevel(level PressureLevel) DevicePressure {
	switch level {
	case PressureWatch, PressureWarning:
		return DevicePressureWarn
	case PressureCritical:
		return DevicePressureCritical
	default:
		return DevicePressureNone
	}
}

// confirmPressure applies hysteresis and multi-sample confirmation. Watch and
// Warning need two samples to enter; recovery to normal needs six samples
// (about 30 seconds at the default 5s interval). A lower raw state can
// downgrade one level after three samples.
func (c *Controller) confirmPressure(raw PressureLevel, reason string) PressureLevel {
	if raw == c.pressureLevel {
		c.candidateLevel = raw
		c.candidateCount = 0
		return c.pressureLevel
	}
	if c.candidateLevel != raw {
		c.candidateLevel = raw
		c.candidateCount = 1
	} else {
		c.candidateCount++
	}
	need := 2
	if raw == PressureNormal {
		need = 6
	} else if raw < c.pressureLevel {
		need = 3
	}
	if c.candidateCount >= need {
		c.pressureLevel = raw
		c.candidateCount = 0
		c.candidateLevel = raw
		if raw >= PressureWarning {
			c.deviceReason.Store(reason)
		} else if raw == PressureNormal {
			c.deviceReason.Store("")
		}
	}
	return c.pressureLevel
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

func (c *Controller) logObserveDecision(level PressureLevel, reason string) {
	key := "decision:" + level.String() + ":" + reason
	if c.lastObserveLog == key {
		return
	}
	c.lastObserveLog = key
	action := "保持"
	switch level {
	case PressureWatch:
		action = "停止升速"
	case PressureWarning:
		action = "建议降速"
	case PressureCritical:
		action = "建议暂停"
	}
	c.log.Info("observe：控制器建议", "level", level.String(), "action", action, "reason", reason)
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
		c.latencyRing[c.latencyNext] = seconds
		c.latencyNext = (c.latencyNext + 1) % len(c.latencyRing)
		if c.latencyUsed < len(c.latencyRing) {
			c.latencyUsed++
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

// LatencyP95 returns the p95 of the recent metadata samples in milliseconds.
func (c *Controller) LatencyP95() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latencyP95Locked() * 1000
}

func (c *Controller) latencyP95Locked() float64 {
	if c.latencyUsed == 0 {
		return 0
	}
	values := make([]float64, c.latencyUsed)
	copy(values, c.latencyRing[:c.latencyUsed])
	sort.Float64s(values)
	idx := int(float64(len(values)-1) * 0.95)
	return values[idx]
}

// Latency returns the current metadata EWMA in milliseconds.
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
