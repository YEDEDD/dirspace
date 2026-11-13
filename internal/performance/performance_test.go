package performance

import (
	"context"
	"testing"
	"time"
)

func TestThrottleRate(t *testing.T) {
	throttle := NewThrottle(20, 1)
	start := time.Now()
	for i := 0; i < 6; i++ {
		if err := throttle.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 220*time.Millisecond {
		t.Fatalf("rate cap ineffective: %s", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("throttle too slow: %s", elapsed)
	}
}

func TestThrottleCancel(t *testing.T) {
	throttle := NewThrottle(1, 1)
	if err := throttle.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := throttle.Wait(ctx)
	if err == nil {
		t.Fatal("expected context error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel did not interrupt throttle")
	}
}

func TestControllerPauseAndResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := NewController(Limits{
		BaseQPS:        1000,
		MinQPS:         1,
		MaxCPUPercent:  0,
		MemoryLimit:    64 << 20,
		Adaptive:       false,
		SampleInterval: time.Hour,
	}, nil)
	controller.Start(ctx)
	controller.PauseFor(250*time.Millisecond, "test")
	start := time.Now()
	if err := controller.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("pause not respected: %s", elapsed)
	}
	paused, reason := controller.Paused()
	if paused || reason == "" {
		// pause is allowed to have expired by now; reason remains as the last one.
		if reason == "" {
			t.Fatal("missing pause reason")
		}
	}
	if err := controller.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCPUGuardDisabled(t *testing.T) {
	guard := NewCPUGuard(0)
	start := time.Now()
	for i := 0; i < 100; i++ {
		if err := guard.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("disabled CPU guard slept")
	}
}

func TestThrottleHighRateIsNotCappedAtOneMillisecond(t *testing.T) {
	throttle := NewThrottle(10000, 1)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 20; i++ {
		if err := throttle.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// 20 calls at 10000 QPS should take about 2ms. The old 1ms floor made
	// every retry wait at least 1ms and took roughly 20ms.
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("throttle too slow at 10000 QPS: %v", elapsed)
	}
}
