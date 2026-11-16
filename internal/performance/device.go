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
