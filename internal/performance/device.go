package performance

import "time"

// DeviceKind is the coarse storage class used to choose pressure thresholds.
// It is intentionally small: the controller only needs to know whether random
// metadata latency is dominated by seeks (HDD), flash latency (SSD/NVMe), or
// an unknown/remote device.
type DeviceKind string

const (
	DeviceUnknown DeviceKind = "unknown"
	DeviceHDD     DeviceKind = "hdd"
	DeviceSSD     DeviceKind = "ssd"
	DeviceNVMe    DeviceKind = "nvme"
)

// DeviceMetrics is a rate sample for one backing block device. Valid is false
// until two counter samples have been taken.
type DeviceMetrics struct {
	Supported  bool
	Valid      bool
	Name       string
	Kind       DeviceKind
	ReadIOPS   float64
	WriteIOPS  float64
	ReadAwait  float64 // milliseconds
	WriteAwait float64 // milliseconds
	Queue      float64 // average in-flight requests
	Util       float64 // percent, capped at 100
}

type deviceSampler interface {
	Sample() DeviceMetrics
}

// DevicePressure is the device part of the controller pressure state.
type DevicePressure int

const (
	DevicePressureNone DevicePressure = iota
	DevicePressureWarn
	DevicePressureCritical
)

// DeviceAssessment is a device-only pressure decision.
type DeviceAssessment struct {
	Level  DevicePressure
	Reason string
}

func assessDevice(m DeviceMetrics) DeviceAssessment {
	if !m.Supported || !m.Valid {
		return DeviceAssessment{}
	}
	kind := m.Kind
	if kind == "" {
		kind = DeviceUnknown
	}
	var warnAwait, criticalAwait, warnQueue, criticalQueue float64
	switch kind {
	case DeviceHDD:
		warnAwait, criticalAwait = 20, 50
		warnQueue, criticalQueue = 8, 24
	case DeviceSSD:
		warnAwait, criticalAwait = 5, 20
		warnQueue, criticalQueue = 16, 64
	case DeviceNVMe:
		warnAwait, criticalAwait = 2, 10
		warnQueue, criticalQueue = 32, 128
	default:
		warnAwait, criticalAwait = 10, 50
		warnQueue, criticalQueue = 8, 32
	}

	// %util is not a primary signal: RAID, SSD and shared devices can report
	// 100% while still having usable headroom. It only sharpens a decision
	// that is already supported by latency or queue depth.
	if m.Queue >= criticalQueue || m.ReadAwait >= criticalAwait || m.WriteAwait >= criticalAwait {
		return DeviceAssessment{
			Level:  DevicePressureCritical,
			Reason: "设备延迟或队列已达严重阈值",
		}
	}
	if m.Queue >= warnQueue || m.ReadAwait >= warnAwait || m.WriteAwait >= warnAwait {
		return DeviceAssessment{
			Level:  DevicePressureWarn,
			Reason: "设备延迟或队列已达警告阈值",
		}
	}
	if kind == DeviceHDD && m.Util >= 95 && m.Queue >= 4 {
		return DeviceAssessment{Level: DevicePressureWarn, Reason: "HDD 利用率高且队列开始积压"}
	}
	return DeviceAssessment{}
}

// ProbeDevice takes a short preflight sample of a local block device. It is
// used as an admission check before starting a scan. Network/FUSE sources and
// platforms without /proc/diskstats return no pressure.
func ProbeDevice(source string, wait time.Duration) DeviceAssessment {
	if wait <= 0 {
		wait = time.Second
	}
	sampler := newDeviceSampler(source)
	if sampler == nil {
		return DeviceAssessment{}
	}
	_ = sampler.Sample()
	time.Sleep(wait)
	return assessDevice(sampler.Sample())
}
