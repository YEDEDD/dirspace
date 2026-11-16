package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlDefaultsAndParsing(t *testing.T) {
	cfg := Default()
	if cfg.Performance.ControlMode != "safe" {
		t.Fatalf("default control_mode=%q, want safe", cfg.Performance.ControlMode)
	}
	if cfg.Performance.Impact != "balanced" {
		t.Fatalf("default impact=%q, want balanced", cfg.Performance.Impact)
	}

	base := t.TempDir()
	path := filepath.Join(base, "config.yaml")
	content := `database:
  path: ` + filepath.Join(base, "db", "usage.db") + `
scan:
  root_path: ` + filepath.Join(base, "data") + `
performance:
  control_mode: observe
  impact: fast
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Performance.ControlMode != "observe" || loaded.Performance.Impact != "fast" {
		t.Fatalf("loaded=%+v", loaded.Performance)
	}
}

func TestStartQPSUsesModeAndImpact(t *testing.T) {
	cfg := Default()
	cfg.Performance.MaxMetadataQPS = 1000
	cfg.Performance.MinMetadataQPS = 10
	cfg.Performance.AdaptiveThrottle = true
	cfg.Performance.ControlMode = "safe"
	cfg.Performance.Impact = "balanced"
	if got := cfg.Performance.StartQPS(); got != 1000 {
		t.Fatalf("safe start=%d, want 1000", got)
	}
	cfg.Performance.ControlMode = "auto"
	cfg.Performance.Impact = "low"
	if got := cfg.Performance.StartQPS(); got != 200 {
		t.Fatalf("auto low start=%d, want 200", got)
	}
	cfg.Performance.Impact = "fast"
	if got := cfg.Performance.StartQPS(); got != 800 {
		t.Fatalf("auto fast start=%d, want 800", got)
	}
	cfg.Performance.ControlMode = "observe"
	if got := cfg.Performance.StartQPS(); got != 333 {
		t.Fatalf("observe start=%d, want 333", got)
	}
}

func TestControlRejectsUnknownValues(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "config.yaml")
	content := `database:
  path: ` + filepath.Join(base, "db", "usage.db") + `
scan:
  root_path: ` + filepath.Join(base, "data") + `
performance:
  control_mode: turbo
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "control_mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestAutoProfileCanResolvePerRootClass(t *testing.T) {
	cfg := DefaultForProfile("auto")
	cfg.applyAutoProfileForClass("local")
	localQPS := cfg.Performance.MaxMetadataQPS
	cfg.applyAutoProfileForClass("network")
	networkQPS := cfg.Performance.MaxMetadataQPS
	if localQPS != 4000 || networkQPS != 1000 {
		t.Fatalf("local=%d network=%d", localQPS, networkQPS)
	}
}
