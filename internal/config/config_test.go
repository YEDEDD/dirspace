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
	if cfg.Scan.Interval.Duration != 45*time.Minute {
		t.Fatalf("durations: %v", cfg.Scan.Interval)
	}
	if cfg.Scan.SizeBasis != "logical" || cfg.Performance.Concurrency.Limit() != 3 || cfg.Performance.Concurrency.Auto {
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

func TestProfilesAndOverrides(t *testing.T) {
	base := t.TempDir()
	write := func(content string) string {
		path := filepath.Join(base, "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	db := filepath.Join(base, "db", "usage.db")
	root := filepath.Join(base, "data")
	baseYAML := "database:\n  path: " + db + "\nscan:\n  root_path: " + root + "\n  interval: 1h\n"

	cfg, err := Load(write(baseYAML + "profile: low\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "low" || cfg.Performance.MaxMetadataQPS != 100 {
		t.Fatalf("low profile: %+v", cfg.Performance)
	}
	if !cfg.Performance.Concurrency.Auto || cfg.Performance.Concurrency.Max != 4 {
		t.Fatalf("low concurrency: %+v", cfg.Performance.Concurrency)
	}
	if cfg.Performance.MaxLoad1 != 0 || cfg.Scan.Interval.Duration != time.Hour {
		t.Fatalf("low defaults: load=%v interval=%v", cfg.Performance.MaxLoad1, cfg.Scan.Interval)
	}

	cfg, err = Load(write(baseYAML + "profile: fast\nperformance:\n  concurrency: auto\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Performance.Concurrency.Auto || cfg.Performance.Concurrency.Max != 16 {
		t.Fatalf("fast auto concurrency: %+v", cfg.Performance.Concurrency)
	}

	cfg, err = Load(write(baseYAML + "profile: normal\nperformance:\n  concurrency: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Performance.Concurrency.Auto || cfg.Performance.Concurrency.Limit() != 3 {
		t.Fatalf("fixed concurrency: %+v", cfg.Performance.Concurrency)
	}
	if cfg.Performance.Concurrency.Effective(1) != 3 || cfg.Performance.Concurrency.Effective(9) != 3 {
		t.Fatalf("fixed effective: %+v", cfg.Performance.Concurrency)
	}
	if got := (Concurrency{Auto: true, Max: 8}).Effective(3); got != 3 {
		t.Fatalf("auto effective=%d", got)
	}

	if _, err := Load(write(baseYAML + "profile: turbo\n")); err == nil {
		t.Fatal("invalid profile accepted")
	}
}

func TestDefaultIntervalIsWeekly(t *testing.T) {
	if got := Default().Scan.Interval.Duration; got != 168*time.Hour {
		t.Fatalf("default interval=%v", got)
	}
	if got := Default().Scan.QuickInterval.Duration; got != 6*time.Hour {
		t.Fatalf("default quick interval=%v", got)
	}
	if got := Default().Scan.QuickSamplePercent; got != 2 {
		t.Fatalf("default quick sample=%d", got)
	}
	if got := Default().Database.StaleRetentionDays; got != 7 {
		t.Fatalf("default stale retention=%d", got)
	}
	if !Default().Performance.MemoryLimit.Auto {
		t.Fatal("default memory limit should be auto")
	}
	if got := Default().Performance.MaxLoad1; got != 0 {
		t.Fatalf("default max_load1=%v", got)
	}
}

func TestAutoMemoryLimit(t *testing.T) {
	var wrapper struct {
		M Bytes `yaml:"m"`
	}
	if err := yaml.Unmarshal([]byte("m: auto"), &wrapper); err != nil {
		t.Fatal(err)
	}
	if !wrapper.M.Auto || wrapper.M.String() != "auto" {
		t.Fatalf("auto bytes: %+v", wrapper.M)
	}
	if got := wrapper.M.Resolve(); got < 32<<20 {
		t.Fatalf("resolved limit too small: %d", got)
	}
	cases := []struct {
		cgroup, total, want int64
	}{
		{cgroup: 200 << 20, want: 160 << 20},
		{cgroup: 20 << 20, want: 20 << 20},
		{cgroup: 4 << 30, want: 3435973836},
		{total: 1 << 30, want: 256 << 20},
		{total: 10 << 30, want: 1 << 30},
		{total: 100 << 30, want: 4 << 30},
		{want: 512 << 20},
	}
	for _, tc := range cases {
		if got := autoMemoryLimit(tc.cgroup, tc.total); got != tc.want {
			t.Fatalf("autoMemoryLimit(%d,%d)=%d want %d", tc.cgroup, tc.total, got, tc.want)
		}
	}
}
