package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseBytesAndDuration(t *testing.T) {
	cases := map[string]int64{
		"512MB": 512 << 20,
		"1GiB":  1 << 30,
		"2GB":   2 << 30,
		"1024":  1024,
		"1.5KB": 1536,
	}
	for input, want := range cases {
		got, err := ParseBytes(input)
		if err != nil || got != want {
			t.Fatalf("ParseBytes(%q)=%d,%v want %d", input, got, err, want)
		}
	}
	if _, err := ParseBytes("nonsense"); err == nil {
		t.Fatal("expected error")
	}
	if got := FormatBytes(1536); got != "1.50KB" {
		t.Fatalf("FormatBytes=%s", got)
	}
	var wrapper struct {
		D Duration `yaml:"d"`
	}
	if err := yaml.Unmarshal([]byte("d: 30m"), &wrapper); err != nil || wrapper.D.Duration != 30*time.Minute {
		t.Fatalf("duration=%v err=%v", wrapper.D.Duration, err)
	}
}

func TestLoadAndValidate(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "config.yaml")
	content := `database:
  path: ` + filepath.Join(base, "db", "usage.db") + `
  retention_days: 12
scan:
  root_path: ` + filepath.Join(base, "data") + `
  interval: 45m
  full_scan_interval: 36h
  size_basis: logical
performance:
  concurrency: 3
  max_metadata_qps: 25
  memory_limit: 256MB
logging:
  level: debug
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scan.Interval.Duration != 45*time.Minute || cfg.Scan.FullScanInterval.Duration != 36*time.Hour {
		t.Fatalf("durations: %v %v", cfg.Scan.Interval, cfg.Scan.FullScanInterval)
	}
	if cfg.Scan.SizeBasis != "logical" || cfg.Performance.Concurrency != 3 {
		t.Fatalf("cfg=%+v", cfg)
	}
	if cfg.Performance.MaxMetadataQPS != 25 || cfg.Performance.MemoryLimit.Bytes != 256<<20 {
		t.Fatalf("perf=%+v", cfg.Performance)
	}
	if cfg.Performance.MinMetadataQPS > cfg.Performance.MaxMetadataQPS {
		t.Fatalf("min qps default not clamped")
	}
	if cfg.Database.RetentionDays != 12 {
		t.Fatalf("retention=%d", cfg.Database.RetentionDays)
	}
}

func TestValidationRejectsUnsafePaths(t *testing.T) {
	base := t.TempDir()
	cfg := Default()
	cfg.Database.Path = filepath.Join(base, "usage.db")
	cfg.Scan.RootPath = "/"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "不允许使用 /") {
		t.Fatalf("root / accepted: %v", err)
	}

	cfg = Default()
	cfg.Database.Path = filepath.Join(base, "data", "usage.db")
	cfg.Scan.RootPath = filepath.Join(base, "data")
	if err := cfg.Validate(); err == nil {
		t.Fatal("database inside root accepted")
	}

	cfg = Default()
	cfg.Database.Path = filepath.Join(base, "usage.db")
	cfg.Scan.RootPath = "relative/path"
	if err := cfg.Validate(); err == nil {
		t.Fatal("relative root accepted")
	}

	if _, err := NormalizeAbsolutePath("/a/../b"); err == nil {
		t.Fatal(".. accepted")
	}
	if _, err := NormalizeAbsolutePath("/"); err == nil {
		t.Fatal("root accepted")
	}
	if got, err := NormalizeAbsolutePath("/a//b/"); err != nil || got != "/a/b" {
		t.Fatalf("normalize=%q err=%v", got, err)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("scan:\n  unknown_option: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}
