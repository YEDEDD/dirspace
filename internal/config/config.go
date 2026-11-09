// Package config loads and validates dirspace configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/YEDEDD/dirspace/internal/mountinfo"
)

const (
	// DefaultConfigPath is the system-wide configuration file used in production.
	DefaultConfigPath = "/etc/dirspace/config.yaml"
	// DefaultDBPath is deliberately outside every NFS mount.
	DefaultDBPath = "/var/lib/dirspace/usage.db"
)

// Duration is a YAML-friendly duration (for example "30m", "24h", "500ms").
// A bare integer is interpreted as seconds.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	if node.Tag == "!!int" {
		seconds, err := strconv.ParseInt(node.Value, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", node.Value, err)
		}
		d.Duration = time.Duration(seconds) * time.Second
		return nil
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q (use forms like 30m, 24h, 500ms): %w", node.Value, err)
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.Duration.String(), nil }

// Bytes is a YAML-friendly byte size (for example "512MB", "1GB").
// Bare integers are interpreted as bytes. Binary units are used.
type Bytes struct {
	Bytes int64
}

func (b *Bytes) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("byte size must be a scalar")
	}
	if node.Tag == "!!int" {
		n, err := strconv.ParseInt(node.Value, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid byte size %q", node.Value)
		}
		b.Bytes = n
		return nil
	}
	n, err := ParseBytes(node.Value)
	if err != nil {
		return err
	}
	b.Bytes = n
	return nil
}

func (b Bytes) MarshalYAML() (any, error) { return FormatBytes(b.Bytes), nil }

// ParseBytes parses values such as 512MB, 1GiB, 1024.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, errors.New("empty byte size")
	}
	multiplier := int64(1)
	units := []struct {
		suffix string
		value  int64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"TI", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"GI", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"MI", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"KI", 1 << 10},
		{"B", 1},
	}
	number := s
	for _, unit := range units {
		if strings.HasSuffix(s, unit.suffix) {
			multiplier = unit.value
			number = strings.TrimSpace(strings.TrimSuffix(s, unit.suffix))
			break
		}
	}
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	result := value * float64(multiplier)
	if result > float64(1<<62) {
		return 0, fmt.Errorf("byte size %q is too large", s)
	}
	return int64(result), nil
}

// FormatBytes renders a byte count using binary units.
func FormatBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
		tib = 1 << 40
	)
	switch {
	case n >= tib:
		return fmt.Sprintf("%.2fTB", float64(n)/tib)
	case n >= gib:
		return fmt.Sprintf("%.2fGB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.2fMB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.2fKB", float64(n)/kib)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// DatabaseConfig controls the local SQLite cache.
type DatabaseConfig struct {
	Path          string   `yaml:"path"`
	BusyTimeout   Duration `yaml:"busy_timeout"`
	RetentionDays int      `yaml:"retention_days"`
}

// ScanConfig controls what is scanned and when.
type ScanConfig struct {
	RootPath string `yaml:"root_path"`
	// SizeBasis is "allocated" (default, close to du) or "logical".
	SizeBasis        string   `yaml:"size_basis"`
	ExtraRoots       []string `yaml:"extra_roots"`
	Interval         Duration `yaml:"interval"`
	FullScanInterval Duration `yaml:"full_scan_interval"`
	// ScanOnStart makes the daemon establish a baseline immediately when a root
	// has never been scanned. Existing roots still wait for their interval.
	ScanOnStart bool `yaml:"scan_on_start"`
	// StartupDelay spreads startup work on busy hosts.
	StartupDelay Duration `yaml:"startup_delay"`
	// ResumeInterrupted lets the daemon continue an interrupted full scan.
	ResumeInterrupted bool `yaml:"resume_interrupted"`
	// Inventory is opt-in. It costs extra local DB writes and does not remove
	// the need to inspect NFS metadata for every file.
	Inventory InventoryConfig `yaml:"inventory"`
}

type InventoryConfig struct {
	Enabled   bool `yaml:"enabled"`
	BatchSize int  `yaml:"batch_size"`
}

// PerformanceConfig is intentionally conservative by default.
type PerformanceConfig struct {
	Concurrency      int      `yaml:"concurrency"`
	MaxMetadataQPS   int      `yaml:"max_metadata_qps"`
	MinMetadataQPS   int      `yaml:"min_metadata_qps"`
	MaxCPUPercent    int      `yaml:"max_cpu_percent"`
	MemoryLimit      Bytes    `yaml:"memory_limit"`
	AdaptiveThrottle bool     `yaml:"adaptive_throttle"`
	LowPriority      bool     `yaml:"low_priority"`
	SampleInterval   Duration `yaml:"sample_interval"`
	// Host thresholds. Values <= 0 disable the corresponding check.
	MaxHostCPUPercent float64  `yaml:"max_host_cpu_percent"`
	MaxLoad1          float64  `yaml:"max_load1"`
	NFSSlowLatency    Duration `yaml:"nfs_slow_latency"`
	NFSPauseLatency   Duration `yaml:"nfs_pause_latency"`
	NFSPauseCooldown  Duration `yaml:"nfs_pause_cooldown"`
	MaxMetadataErrors int      `yaml:"max_metadata_errors"`
	ProgressInterval  Duration `yaml:"progress_interval"`
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Config struct {
	Database    DatabaseConfig    `yaml:"database"`
	Scan        ScanConfig        `yaml:"scan"`
	Performance PerformanceConfig `yaml:"performance"`
	Logging     LoggingConfig     `yaml:"logging"`
	Source      string            `yaml:"-"`
}

// Default returns a deliberately low-load configuration.
func Default() *Config {
	return &Config{
		Database: DatabaseConfig{
			Path:          DefaultDBPath,
			BusyTimeout:   Duration{5 * time.Second},
			RetentionDays: 90,
		},
		Scan: ScanConfig{
			RootPath:          "/mnt",
			SizeBasis:         "allocated",
			Interval:          Duration{30 * time.Minute},
			FullScanInterval:  Duration{24 * time.Hour},
			ScanOnStart:       true,
			StartupDelay:      Duration{15 * time.Second},
			ResumeInterrupted: true,
			Inventory: InventoryConfig{
				Enabled:   false,
				BatchSize: 1000,
			},
		},
		Performance: PerformanceConfig{
			Concurrency:       2,
			MaxMetadataQPS:    50,
			MinMetadataQPS:    5,
			MaxCPUPercent:     10,
			MemoryLimit:       Bytes{512 << 20},
			AdaptiveThrottle:  true,
			LowPriority:       true,
			SampleInterval:    Duration{5 * time.Second},
			MaxHostCPUPercent: 85,
			MaxLoad1:          8,
			NFSSlowLatency:    Duration{500 * time.Millisecond},
			NFSPauseLatency:   Duration{5 * time.Second},
			NFSPauseCooldown:  Duration{30 * time.Second},
			MaxMetadataErrors: 8,
			ProgressInterval:  Duration{10 * time.Second},
			HeartbeatInterval: Duration{5 * time.Second},
		},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}
}

// ResolvePath finds the configuration file to use.
// Explicit path must exist. Otherwise DIRSPACE_CONFIG, /etc/..., then
// ./configs/config.yaml are tried. An empty result means "use defaults".
func ResolvePath(explicit string) (string, error) {
	candidates := make([]string, 0, 3)
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		if env := os.Getenv("DIRSPACE_CONFIG"); env != "" {
			candidates = append(candidates, env)
		}
		candidates = append(candidates, DefaultConfigPath, filepath.Join("configs", "config.yaml"))
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil {
			if info.IsDir() {
				return "", fmt.Errorf("配置文件路径是目录: %s", candidate)
			}
			abs, absErr := filepath.Abs(candidate)
			if absErr != nil {
				return "", absErr
			}
			return abs, nil
		}
		if explicit != "" && err != nil {
			return "", fmt.Errorf("配置文件不存在或不可读: %s: %w", candidate, err)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("无法读取配置文件 %s: %w", candidate, err)
		}
	}
	if explicit != "" {
		return "", fmt.Errorf("配置文件不存在: %s", explicit)
	}
	return "", nil
}

// Load reads a config file over the defaults and validates it.
func Load(path string) (*Config, error) {
	cfg := Default()
	resolved, err := ResolvePath(path)
	if err != nil {
		return nil, err
	}
	if resolved != "" {
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		decoder.KnownFields(true)
		if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", resolved, err)
		}
		cfg.Source = resolved
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// AllRoots returns the primary root followed by additional roots, de-duplicated.
func (c *Config) AllRoots() []string {
	seen := make(map[string]struct{})
	roots := make([]string, 0, 1+len(c.Scan.ExtraRoots))
	for _, root := range append([]string{c.Scan.RootPath}, c.Scan.ExtraRoots...) {
		if root == "" {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	return roots
}

// Validate rejects settings that could cause unsafe or nonsensical scans.
func (c *Config) Validate() error {
	if c.Database.Path == "" {
		return errors.New("database.path 不能为空")
	}
	dbPath, err := NormalizeAbsolutePath(c.Database.Path)
	if err != nil {
		return fmt.Errorf("database.path 非法: %w", err)
	}
	c.Database.Path = dbPath
	if c.Database.BusyTimeout.Duration <= 0 {
		return errors.New("database.busy_timeout 必须大于 0")
	}
	if c.Database.RetentionDays < 1 {
		return errors.New("database.retention_days 必须至少为 1")
	}

	if c.Scan.RootPath == "" {
		return errors.New("scan.root_path 不能为空")
	}
	root, err := NormalizeAbsolutePath(c.Scan.RootPath)
	if err != nil {
		return fmt.Errorf("scan.root_path 非法: %w", err)
	}
	if root == "/" {
		return errors.New("出于安全考虑，不允许将 / 作为扫描根目录")
	}
	c.Scan.RootPath = root
	switch strings.ToLower(c.Scan.SizeBasis) {
	case "", "allocated", "block", "blocks":
		c.Scan.SizeBasis = "allocated"
	case "logical", "size", "apparent":
		c.Scan.SizeBasis = "logical"
	default:
		return fmt.Errorf("scan.size_basis 非法: %s（可选 allocated 或 logical）", c.Scan.SizeBasis)
	}
	seen := map[string]struct{}{root: {}}
	extra := make([]string, 0, len(c.Scan.ExtraRoots))
	for _, raw := range c.Scan.ExtraRoots {
		p, err := NormalizeAbsolutePath(raw)
		if err != nil {
			return fmt.Errorf("scan.extra_roots 中的路径 %q 非法: %w", raw, err)
		}
		if p == "/" {
			return fmt.Errorf("出于安全考虑，不允许将 / 作为扫描根目录")
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		extra = append(extra, p)
	}
	c.Scan.ExtraRoots = extra
	if c.Scan.Interval.Duration < 10*time.Second {
		return errors.New("scan.interval 不能小于 10s")
	}
	if c.Scan.FullScanInterval.Duration < c.Scan.Interval.Duration {
		return errors.New("scan.full_scan_interval 不能小于 scan.interval")
	}
	if c.Scan.StartupDelay.Duration < 0 {
		return errors.New("scan.startup_delay 不能为负数")
	}
	if c.Scan.Inventory.BatchSize < 100 || c.Scan.Inventory.BatchSize > 2000 {
		return errors.New("scan.inventory.batch_size 必须在 100 到 2000 之间")
	}

	p := &c.Performance
	if p.Concurrency < 1 || p.Concurrency > 64 {
		return errors.New("performance.concurrency 必须在 1 到 64 之间")
	}
	if p.MaxMetadataQPS < 1 || p.MaxMetadataQPS > 1_000_000 {
		return errors.New("performance.max_metadata_qps 必须在 1 到 1000000 之间")
	}
	if p.MinMetadataQPS < 1 || p.MinMetadataQPS > p.MaxMetadataQPS {
		return errors.New("performance.min_metadata_qps 必须在 1 到 max_metadata_qps 之间")
	}
	if p.MaxCPUPercent < 1 || p.MaxCPUPercent > 100 {
		return errors.New("performance.max_cpu_percent 必须在 1 到 100 之间")
	}
	if p.MemoryLimit.Bytes < 32<<20 {
		return errors.New("performance.memory_limit 不能小于 32MB")
	}
	if p.SampleInterval.Duration < time.Second {
		return errors.New("performance.sample_interval 不能小于 1s")
	}
	if p.NFSSlowLatency.Duration < 0 || p.NFSPauseLatency.Duration < 0 {
		return errors.New("performance.nfs_slow_latency/nfs_pause_latency 不能为负数")
	}
	if p.NFSPauseLatency.Duration <= p.NFSSlowLatency.Duration {
		return errors.New("performance.nfs_pause_latency 必须大于 nfs_slow_latency")
	}
	if p.NFSPauseCooldown.Duration < time.Second {
		return errors.New("performance.nfs_pause_cooldown 不能小于 1s")
	}
	if p.ProgressInterval.Duration < time.Second {
		return errors.New("performance.progress_interval 不能小于 1s")
	}
	if p.HeartbeatInterval.Duration < time.Second {
		return errors.New("performance.heartbeat_interval 不能小于 1s")
	}
	if p.MaxHostCPUPercent < 0 || p.MaxHostCPUPercent > 100 {
		return errors.New("performance.max_host_cpu_percent 必须在 0 到 100 之间")
	}
	if p.MaxLoad1 < 0 {
		return errors.New("performance.max_load1 不能为负数")
	}
	if p.MaxMetadataErrors < 1 {
		return errors.New("performance.max_metadata_errors 必须至少为 1")
	}

	for _, r := range c.AllRoots() {
		if pathWithin(r, c.Database.Path) {
			return fmt.Errorf("数据库不能位于扫描根目录内: db=%s root=%s", c.Database.Path, r)
		}
	}
	if mountinfo.IsNFS(c.Database.Path) {
		return fmt.Errorf("数据库不能位于 NFS 挂载目录: %s", c.Database.Path)
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level 非法: %s", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format 非法: %s", c.Logging.Format)
	}
	return nil
}

// PathWithin reports whether child is equal to or below parent.
func PathWithin(parent, child string) bool { return pathWithin(parent, child) }

// pathWithin reports whether child is equal to or below parent.
func pathWithin(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// NormalizeAbsolutePath cleans an absolute path without resolving symlinks or
// replacing the user's path. It rejects relative paths, NUL bytes, ".." segments
// and the filesystem root.
func NormalizeAbsolutePath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("路径为空")
	}
	if strings.ContainsRune(raw, 0) {
		return "", errors.New("路径包含 NUL 字节")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("必须使用绝对路径: %s", raw)
	}
	for _, part := range strings.Split(raw, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("路径不允许包含 ..: %s", raw)
		}
	}
	clean := filepath.Clean(raw)
	if clean == "/" {
		return "", errors.New("不允许使用 / 作为路径")
	}
	return clean, nil
}
