package performance

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeDeviceSampler struct{ metrics DeviceMetrics }

func (f fakeDeviceSampler) Sample() DeviceMetrics { return f.metrics }

func testController(mode string, device DeviceMetrics) *Controller {
	c := NewController(Limits{
		BaseQPS:        1000,
		MinQPS:         10,
		WorkerLimit:    8,
		Adaptive:       true,
		SampleInterval: time.Hour,
		Mode:           mode,
		Impact:         "balanced",
		PauseCooldown:  time.Minute,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.device = fakeDeviceSampler{metrics: device}
	return c
}

func TestObserveDoesNotApplyDevicePressure(t *testing.T) {
	c := testController("observe", DeviceMetrics{
		Supported: true, Valid: true, Kind: DeviceHDD, ReadAwait: 100, Queue: 50,
	})
	c.sample(&hostSampler{})
	if got := c.Rate(); got < 333 || got > 1000 {
		t.Fatalf("observe rate out of legacy range: %v", got)
	}
	if paused, _ := c.Paused(); paused {
		t.Fatal("observe paused the scan")
	}
	if got := c.DevicePressure(); got != DevicePressureCritical {
		t.Fatalf("device pressure = %v, want critical", got)
	}
}

func TestSafeDoesNotRaiseRate(t *testing.T) {
	c := testController("safe", DeviceMetrics{Supported: true, Valid: true, Kind: DeviceHDD})
	for i := 0; i < 5; i++ {
		c.sample(&hostSampler{})
	}
	if got := c.Rate(); got != 333 {
		t.Fatalf("safe raised rate to %v", got)
	}
}

func TestAutoRaisesRate(t *testing.T) {
	c := testController("auto", DeviceMetrics{Supported: true, Valid: true, Kind: DeviceHDD})
	for i := 0; i < 3; i++ {
		c.sample(&hostSampler{})
	}
	if got := c.Rate(); got <= 333 {
		t.Fatalf("auto did not raise rate: %v", got)
	}
}

func TestCriticalDeviceReducesQPSAndWorkers(t *testing.T) {
	c := testController("safe", DeviceMetrics{
		Supported: true, Valid: true, Kind: DeviceHDD, ReadAwait: 100, Queue: 50,
	})
	c.deviceBaseline, c.deviceBaselineSet = 10, true
	c.sample(&hostSampler{})
	c.sample(&hostSampler{})
	if got := c.Rate(); got >= 333 {
		t.Fatalf("critical device did not reduce QPS: %v", got)
	}
	if got := c.RecommendedWorkers(); got >= 8 {
		t.Fatalf("critical device did not reduce workers: %d", got)
	}
	if paused, _ := c.Paused(); !paused {
		t.Fatal("critical device did not pause")
	}
}

func TestControllerContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := testController("safe", DeviceMetrics{})
	c.Start(ctx)
	cancel()
}

func TestAutoCapacityProbeCapsAtBase(t *testing.T) {
	c := NewController(Limits{
		BaseQPS: 1000, MinQPS: 10, WorkerLimit: 8, Adaptive: true,
		SampleInterval: time.Hour, Mode: "auto", Impact: "fast",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 8 workers at 2ms can theoretically do 4000 QPS, but BaseQPS is the
	// configured safety cap.
	c.device = fakeDeviceSampler{metrics: DeviceMetrics{Supported: true, Valid: true, Kind: DeviceHDD}}
	c.ObserveMetadata(2*time.Millisecond, nil)
	c.sample(&hostSampler{})
	c.sample(&hostSampler{})
	if target := c.targetQPS; target > 1000 || target <= 0 {
		t.Fatalf("target=%v, want 1..1000", target)
	}
	if got := c.Rate(); got <= 333 || got > 1000 {
		t.Fatalf("rate=%v, want >333 and <=1000", got)
	}
}

func TestAutoFallsBackToSafeAfterRepeatedCritical(t *testing.T) {
	c := testController("auto", DeviceMetrics{
		Supported: true, Valid: true, Kind: DeviceHDD, ReadAwait: 100, Queue: 50,
	})
	c.deviceBaseline, c.deviceBaselineSet = 10, true
	c.sample(&hostSampler{})
	c.sample(&hostSampler{})
	if got := c.Mode(); got != ControlSafe {
		t.Fatalf("mode=%v, want safe", got)
	}
}

func TestWatchHoldsWithoutBackingOff(t *testing.T) {
	c := testController("auto", DeviceMetrics{
		Supported: true, Valid: true, Kind: DeviceHDD, ReadAwait: 14, Queue: 2,
	})
	c.deviceBaseline, c.deviceBaselineSet = 10, true
	c.sample(&hostSampler{})
	c.sample(&hostSampler{})
	if c.pressureLevel != PressureWatch {
		t.Fatalf("pressure=%v, want watch", c.pressureLevel)
	}
	if got := c.Rate(); got != 333 {
		t.Fatalf("watch changed rate to %v", got)
	}
}

func TestWarningNeedsTwoSamples(t *testing.T) {
	c := testController("safe", DeviceMetrics{
		Supported: true, Valid: true, Kind: DeviceHDD, ReadAwait: 25, Queue: 2,
	})
	c.deviceBaseline, c.deviceBaselineSet = 10, true
	c.sample(&hostSampler{})
	if got := c.Rate(); got != 333 {
		t.Fatalf("first warning sample changed rate to %v", got)
	}
	c.sample(&hostSampler{})
	if got := c.Rate(); got >= 333 {
		t.Fatalf("confirmed warning did not reduce rate: %v", got)
	}
}

func TestRecoveryNeedsSixNormalSamples(t *testing.T) {
	c := testController("safe", DeviceMetrics{
		Supported: true, Valid: true, Kind: DeviceHDD, ReadAwait: 25, Queue: 2,
	})
	c.deviceBaseline, c.deviceBaselineSet = 10, true
	c.sample(&hostSampler{})
	c.sample(&hostSampler{})
	if c.pressureLevel != PressureWarning {
		t.Fatalf("pressure=%v, want warning", c.pressureLevel)
	}
	c.device = fakeDeviceSampler{metrics: DeviceMetrics{
		Supported: true, Valid: true, Kind: DeviceHDD, ReadAwait: 5, Queue: 1,
	}}
	for i := 0; i < 5; i++ {
		c.sample(&hostSampler{})
	}
	if c.pressureLevel == PressureNormal {
		t.Fatal("recovered before six normal samples")
	}
	c.sample(&hostSampler{})
	if c.pressureLevel != PressureNormal {
		t.Fatalf("pressure=%v after six normal samples, want normal", c.pressureLevel)
	}
}

func TestLatencyP95(t *testing.T) {
	c := testController("safe", DeviceMetrics{})
	for _, d := range []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 100 * time.Millisecond} {
		c.ObserveMetadata(d, nil)
	}
	if got := c.LatencyP95(); got < 3 {
		t.Fatalf("p95=%vms, want >=3", got)
	}
}
