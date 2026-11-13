package performance

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestUpdateLimitsAppliesQPSAndThresholds(t *testing.T) {
	controller := NewController(Limits{
		BaseQPS:       100,
		MinQPS:        10,
		MaxCPUPercent: 20,
		Adaptive:      false,
		PauseLatency:  time.Second,
		PauseCooldown: 5 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	controller.UpdateLimits(Limits{
		BaseQPS:       400,
		MinQPS:        20,
		MaxCPUPercent: 50,
		Adaptive:      false,
		PauseLatency:  2 * time.Second,
		PauseCooldown: 10 * time.Second,
	})
	if got := controller.Rate(); got != 400 {
		t.Fatalf("rate after reload = %v, want 400", got)
	}
}
