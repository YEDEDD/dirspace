package performance

import "testing"

func TestAssessDeviceHDD(t *testing.T) {
	base := DeviceMetrics{Supported: true, Valid: true, Kind: DeviceHDD}
	if got := assessDevice(base).Level; got != DevicePressureNone {
		t.Fatalf("idle HDD = %v, want none", got)
	}
	warn := base
	warn.ReadAwait = 25
	if got := assessDevice(warn).Level; got != DevicePressureWarn {
		t.Fatalf("HDD await 25ms = %v, want warn", got)
	}
	critical := base
	critical.Queue = 30
	if got := assessDevice(critical).Level; got != DevicePressureCritical {
		t.Fatalf("HDD queue 30 = %v, want critical", got)
	}
}

func TestAssessDeviceSSDIgnoresUtil(t *testing.T) {
	m := DeviceMetrics{Supported: true, Valid: true, Kind: DeviceSSD, Util: 100, Queue: 2, ReadAwait: 1}
	if got := assessDevice(m).Level; got != DevicePressureNone {
		t.Fatalf("SSD util alone = %v, want none", got)
	}
	m.ReadAwait = 20
	if got := assessDevice(m).Level; got != DevicePressureCritical {
		t.Fatalf("SSD await 20ms = %v, want critical", got)
	}
}
