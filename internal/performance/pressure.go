package performance

// PressureLevel is the confirmed controller pressure state. Watch means
// "stop increasing, keep scanning"; Warning means back off; Critical means
// pause or halve the rate.
type PressureLevel int

const (
	PressureNormal PressureLevel = iota
	PressureWatch
	PressureWarning
	PressureCritical
)

func (p PressureLevel) String() string {
	switch p {
	case PressureWatch:
		return "watch"
	case PressureWarning:
		return "warning"
	case PressureCritical:
		return "critical"
	default:
		return "normal"
	}
}

func levelFor(value, baseline, watchFloor, warnFloor, criticalFloor float64) PressureLevel {
	if value <= 0 {
		return PressureNormal
	}
	if baseline <= 0 {
		// Without a learned baseline use the class floor as the reference so
		// absolute diagnostics still produce a meaningful warning/critical.
		baseline = watchFloor
	}
	watch := 1.3 * baseline
	warn := 1.8 * baseline
	critical := 3.0 * baseline
	if watch < watchFloor {
		watch = watchFloor
	}
	if warn < warnFloor {
		warn = warnFloor
	}
	if critical < criticalFloor {
		critical = criticalFloor
	}
	switch {
	case value >= critical:
		return PressureCritical
	case value >= warn:
		return PressureWarning
	case value >= watch:
		return PressureWatch
	default:
		return PressureNormal
	}
}

func deviceAwait(m DeviceMetrics) float64 {
	await := m.ReadAwait
	if m.WriteAwait > await {
		await = m.WriteAwait
	}
	return await
}

// classifyDevice uses both baseline-relative latency and absolute floors.
// Queue depth is a second independent signal; %util is deliberately not a
// primary input because it is misleading on SSD, RAID and shared devices.
func classifyDevice(m DeviceMetrics, baselineAwait float64) (PressureLevel, string) {
	if !m.Supported || !m.Valid {
		return PressureNormal, ""
	}
	await := deviceAwait(m)
	var level PressureLevel
	var reason string
	switch m.Kind {
	case DeviceHDD:
		level = levelFor(await, baselineAwait, 10, 20, 50)
		if m.Queue >= 16 {
			level = PressureCritical
			reason = "HDD 队列严重积压"
		} else if m.Queue >= 8 {
			if level < PressureWarning {
				level = PressureWarning
			}
			reason = "HDD 队列开始积压"
		} else if m.Queue >= 4 && level < PressureWatch {
			level = PressureWatch
			reason = "HDD 队列进入观察"
		}
	case DeviceSSD:
		level = levelFor(await, baselineAwait, 2, 5, 20)
		if m.Queue >= 64 {
			level = PressureCritical
			reason = "SSD 队列严重积压"
		} else if m.Queue >= 32 {
			if level < PressureWarning {
				level = PressureWarning
			}
			reason = "SSD 队列开始积压"
		}
	case DeviceNVMe:
		level = levelFor(await, baselineAwait, 1, 2, 10)
		if m.Queue >= 128 {
			level = PressureCritical
			reason = "NVMe 队列严重积压"
		} else if m.Queue >= 64 {
			if level < PressureWarning {
				level = PressureWarning
			}
			reason = "NVMe 队列开始积压"
		}
	default:
		level = levelFor(await, baselineAwait, 10, 20, 50)
	}
	if reason == "" {
		switch level {
		case PressureCritical:
			reason = "设备延迟达到严重阈值"
		case PressureWarning:
			reason = "设备延迟达到警告阈值"
		case PressureWatch:
			reason = "设备延迟进入观察区"
		}
	}
	return level, reason
}

// assessDevice is the baseline-less absolute classifier used by older tests
// and diagnostics. It intentionally ignores %util as a primary signal.
func assessDevice(m DeviceMetrics) DeviceAssessment {
	level, reason := classifyDevice(m, 0)
	p := DevicePressureNone
	switch level {
	case PressureWatch, PressureWarning:
		p = DevicePressureWarn
	case PressureCritical:
		p = DevicePressureCritical
	}
	return DeviceAssessment{Level: p, Reason: reason}
}

func classifyLatency(seconds, baselineSeconds float64) (PressureLevel, string) {
	if seconds <= 0 {
		return PressureNormal, ""
	}
	level := levelFor(seconds, baselineSeconds, 0.010, 0.020, 0.050)
	switch level {
	case PressureCritical:
		return level, "元数据延迟达到严重阈值"
	case PressureWarning:
		return level, "元数据延迟达到警告阈值"
	case PressureWatch:
		return level, "元数据延迟进入观察区"
	default:
		return level, ""
	}
}

func maxPressure(a, b PressureLevel) PressureLevel {
	if b > a {
		return b
	}
	return a
}
