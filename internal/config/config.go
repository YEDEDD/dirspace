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
	// DefaultDBPath is deliberately outside every network/FUSE mount.
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
	Auto  bool
}

func (b Bytes) String() string {
	if b.Auto {
		return "auto"
	}
	return FormatBytes(b.Bytes)
}

func (b *Bytes) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("byte size must be a scalar")
	}
	if node.Tag == "!!str" && strings.EqualFold(strings.TrimSpace(node.Value), "auto") {
		b.Auto = true
		b.Bytes = 0
		return nil
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

func (b Bytes) MarshalYAML() (any, error) {
	if b.Auto {
		return "auto", nil
	}
	return FormatBytes(b.Bytes), nil
}

// Resolve returns the configured byte limit or an automatic limit derived from
// the service cgroup. The cgroup branch stays below the hard cgroup limit even
// on small machines; the physical-memory fallback never goes below 256MB.
func (b Bytes) Resolve() int64 {
	if !b.Auto {
		return b.Bytes
	}
	return autoMemoryLimit(cgroupMemoryLimit(), memTotal())
}

// autoMemoryLimit keeps the application guard below the cgroup hard limit.
// A cgroup limit is authoritative; physical memory is only a fallback.
func autoMemoryLimit(cgroup, total int64) int64 {
	if cgroup > 0 {
		value := int64(float64(cgroup) * 0.8)
		if value < 32<<20 {
			value = cgroup
			if value > 32<<20 {
				value = 32 << 20
			}
		}
		return value
	}
	if total > 0 {
		value := int64(float64(total) * 0.10)
		if value < 256<<20 {
			value = 256 << 20
		}
		if value > 4<<30 {
			value = 4 << 30
		}
		return value
	}
	return 512 << 20
}

func cgroupMemoryLimit() int64 {
	for _, path := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" || strings.EqualFold(text, "max") {
			continue
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil || value <= 0 || value >= 1<<60 {
			continue
		}
		return value
	}
	return 0
}

func memTotal() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err == nil {
				return kb << 10
			}
		}
	}
	return 0
}

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

// Concurrency is a fixed worker count or "auto" with a maximum cap.
// The zero value is treated as auto with the profile's cap.
type Concurrency struct {
	Auto bool
	Max  int
}

func (c *Concurrency) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("concurrency must be an integer or \"auto\"")
	}
	if strings.EqualFold(strings.TrimSpace(node.Value), "auto") {
		c.Auto = true
		if c.Max < 1 {
			c.Max = 8
		}
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(node.Value))
	if err != nil {
		return fmt.Errorf("concurrency must be an integer or \"auto\": %w", err)
	}
	if n < 0 {
		return errors.New("concurrency cannot be negative")
	}
	if n == 0 {
		c.Auto = true
		if c.Max < 1 {
			c.Max = 8
		}
		return nil
	}
	c.Auto = false
	c.Max = n
	return nil
}

func (c Concurrency) MarshalYAML() (any, error) {
	if c.Auto {
		return "auto", nil
	}
	return c.Max, nil
}

// String is safe for human-readable logs.
func (c Concurrency) String() string {
	if c.Auto {
		return fmt.Sprintf("auto (max %d)", c.Limit())
	}
	return strconv.Itoa(c.Limit())
}

// Limit returns the hard worker cap.
func (c Concurrency) Limit() int {
	if c.Max < 1 {
		return 1
	}
	return c.Max
}

// Effective returns the worker count for the number of ready top-level
// directories. Fixed values are still clamped by the caller; auto values use
// only as many workers as there is work for.
func (c Concurrency) Effective(ready int) int {
	limit := c.Limit()
	if c.Auto && ready > 0 && ready < limit {
		return ready
	}
	return limit
}

// DatabaseConfig controls the local SQLite cache.
type DatabaseConfig struct {
	Path               string   `yaml:"path"`
	BusyTimeout        Duration `yaml:"busy_timeout"`
	RetentionDays      int      `yaml:"retention_days"`
	StaleRetentionDays int      `yaml:"stale_retention_days"`
}

// ScanConfig controls what is scanned and when.
type ScanConfig struct {
	RootPath string `yaml:"root_path"`
	// SizeBasis is "allocated" (default, close to du) or "logical".
	SizeBasis  string   `yaml:"size_basis"`
	ExtraRoots []string `yaml:"extra_roots"`
	Interval   Duration `yaml:"interval"`
	// QuickInterval enables approximate sampled refreshes between exact full
	// scans. Zero disables quick mode.
	QuickInterval Duration `yaml:"quick_interval"`
	// QuickSamplePercent is the percentage of files sampled by quick mode.
	QuickSamplePercent int `yaml:"quick_sample_percent"`
	// QuickCensusFiles makes the first N files in every directory a full
	// census. Small directories stay exact while large directories sample the
	// tail; lower values reduce metadata calls at the cost of accuracy.
	QuickCensusFiles int `yaml:"quick_census_files"`
	// ReadBatchSize is the number of directory entries requested per ReadDir.
	// Larger batches reduce READDIR calls on huge directories.
	ReadBatchSize int `yaml:"read_batch_size"`
	// IndexDepth controls how many levels of the directory tree are cached in
	// the drill-down index. Zero disables the index. Deep levels are kept only
	// for the top IndexTopK children of each indexed parent.
	IndexDepth int `yaml:"index_depth"`
	// IndexTopK is the maximum number of children retained per indexed parent.
	IndexTopK int `yaml:"index_top_k"`
	// IndexAllDirs stores every directory in the drill-down index instead of
	// only the top IndexTopK children up to IndexDepth. This makes arbitrary
	// nested paths queryable at the cost of a larger local database.
	IndexAllDirs bool `yaml:"index_all_dirs"`
	// QuickMaxFilesPerTop caps one quick-mode top-level directory. Zero means
	// unlimited. A capped directory is marked partial and reports a lower bound.
	QuickMaxFilesPerTop int64 `yaml:"quick_max_files_per_top"`
	// RequireMountPoint makes a scan fail if RootPath is not a mount target.
	// It protects against scanning the local root filesystem when a shared mount
	// is missing. Off by default so ordinary local directories still work.
	RequireMountPoint bool `yaml:"require_mount_point"`
	// ScanOnStart makes the daemon establish a baseline immediately when a root
	// has never been scanned. Existing roots still wait for their interval.
	ScanOnStart bool `yaml:"scan_on_start"`
	// StartupDelay spreads startup work on busy hosts.
	StartupDelay Duration `yaml:"startup_delay"`
	// ResumeInterrupted lets the daemon continue an interrupted full scan.
	ResumeInterrupted bool `yaml:"resume_interrupted"`
	// Inventory is experimental. It costs extra local DB writes and does not
	// remove the need to inspect filesystem metadata for every file.
	Inventory InventoryConfig `yaml:"inventory"`
}

type InventoryConfig struct {
	Enabled   bool `yaml:"enabled"`
	BatchSize int  `yaml:"batch_size"`
}

// PerformanceConfig holds advanced overrides. Most users should select a
// profile instead of editing these values.
type PerformanceConfig struct {
	Concurrency      Concurrency `yaml:"concurrency"`
	MaxMetadataQPS   int         `yaml:"max_metadata_qps"`
	MinMetadataQPS   int         `yaml:"min_metadata_qps"`
	MaxCPUPercent    int         `yaml:"max_cpu_percent"`
	MemoryLimit      Bytes       `yaml:"memory_limit"`
	AdaptiveThrottle bool        `yaml:"adaptive_throttle"`
	LowPriority      bool        `yaml:"low_priority"`
	SampleInterval   Duration    `yaml:"sample_interval"`
	// Host thresholds. Values <= 0 disable the corresponding check.
	MaxHostCPUPercent    float64  `yaml:"max_host_cpu_percent"`
	MaxLoad1             float64  `yaml:"max_load1"`
	SlowMetadataLatency  Duration `yaml:"slow_metadata_latency"`
	PauseMetadataLatency Duration `yaml:"pause_metadata_latency"`
	PauseCooldown        Duration `yaml:"pause_cooldown"`
	MaxMetadataErrors    int      `yaml:"max_metadata_errors"`
	ProgressInterval     Duration `yaml:"progress_interval"`
	HeartbeatInterval    Duration `yaml:"heartbeat_interval"`
}

// performanceYAML decodes both the generic latency keys and the legacy NFS
// names. Older configuration files keep working while new files stay
// filesystem-agnostic.
type performanceYAML struct {
	Concurrency        Concurrency `yaml:"concurrency"`
	MaxMetadataQPS     int         `yaml:"max_metadata_qps"`
	MinMetadataQPS     int         `yaml:"min_metadata_qps"`
	MaxCPUPercent      int         `yaml:"max_cpu_percent"`
	MemoryLimit        Bytes       `yaml:"memory_limit"`
	AdaptiveThrottle   bool        `yaml:"adaptive_throttle"`
	LowPriority        bool        `yaml:"low_priority"`
	SampleInterval     Duration    `yaml:"sample_interval"`
	MaxHostCPUPercent  float64     `yaml:"max_host_cpu_percent"`
	MaxLoad1           float64     `yaml:"max_load1"`
	SlowLatency        *Duration   `yaml:"slow_metadata_latency"`
	PauseLatency       *Duration   `yaml:"pause_metadata_latency"`
	PauseCooldown      *Duration   `yaml:"pause_cooldown"`
	LegacySlowLatency  *Duration   `yaml:"nfs_slow_latency"`
	LegacyPauseLatency *Duration   `yaml:"nfs_pause_latency"`
	LegacyPauseCool    *Duration   `yaml:"nfs_pause_cooldown"`
	MaxMetadataErrors  int         `yaml:"max_metadata_errors"`
	ProgressInterval   Duration    `yaml:"progress_interval"`
	HeartbeatInterval  Duration    `yaml:"heartbeat_interval"`
}

// UnmarshalYAML accepts both generic and legacy latency names. Generic names
// win when both are present.
func (p *PerformanceConfig) UnmarshalYAML(value *yaml.Node) error {
	known := map[string]struct{}{
		"concurrency": {}, "max_metadata_qps": {}, "min_metadata_qps": {},
		"max_cpu_percent": {}, "memory_limit": {}, "adaptive_throttle": {},
		"low_priority": {}, "sample_interval": {}, "max_host_cpu_percent": {},
		"max_load1": {}, "slow_metadata_latency": {}, "pause_metadata_latency": {},
		"pause_cooldown": {}, "nfs_slow_latency": {}, "nfs_pause_latency": {},
		"nfs_pause_cooldown": {}, "max_metadata_errors": {}, "progress_interval": {},
		"heartbeat_interval": {},
	}
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			key := value.Content[i].Value
			if _, ok := known[key]; !ok {
				return fmt.Errorf("未知配置项 performance.%s", key)
			}
		}
	}
	aux := performanceYAML{
		Concurrency:       p.Concurrency,
		MaxMetadataQPS:    p.MaxMetadataQPS,
		MinMetadataQPS:    p.MinMetadataQPS,
		MaxCPUPercent:     p.MaxCPUPercent,
		MemoryLimit:       p.MemoryLimit,
		AdaptiveThrottle:  p.AdaptiveThrottle,
		LowPriority:       p.LowPriority,
		SampleInterval:    p.SampleInterval,
		MaxHostCPUPercent: p.MaxHostCPUPercent,
		MaxLoad1:          p.MaxLoad1,
		MaxMetadataErrors: p.MaxMetadataErrors,
		ProgressInterval:  p.ProgressInterval,
		HeartbeatInterval: p.HeartbeatInterval,
	}
	if err := value.Decode(&aux); err != nil {
		return err
	}
	*p = PerformanceConfig{
		Concurrency:          aux.Concurrency,
		MaxMetadataQPS:       aux.MaxMetadataQPS,
		MinMetadataQPS:       aux.MinMetadataQPS,
		MaxCPUPercent:        aux.MaxCPUPercent,
		MemoryLimit:          aux.MemoryLimit,
		AdaptiveThrottle:     aux.AdaptiveThrottle,
		LowPriority:          aux.LowPriority,
		SampleInterval:       aux.SampleInterval,
		MaxHostCPUPercent:    aux.MaxHostCPUPercent,
		MaxLoad1:             aux.MaxLoad1,
		SlowMetadataLatency:  p.SlowMetadataLatency,
		PauseMetadataLatency: p.PauseMetadataLatency,
		PauseCooldown:        p.PauseCooldown,
		MaxMetadataErrors:    aux.MaxMetadataErrors,
		ProgressInterval:     aux.ProgressInterval,
		HeartbeatInterval:    aux.HeartbeatInterval,
	}
	if aux.SlowLatency != nil {
		p.SlowMetadataLatency = *aux.SlowLatency
	} else if aux.LegacySlowLatency != nil {
		p.SlowMetadataLatency = *aux.LegacySlowLatency
	}
	if aux.PauseLatency != nil {
		p.PauseMetadataLatency = *aux.PauseLatency
	} else if aux.LegacyPauseLatency != nil {
		p.PauseMetadataLatency = *aux.LegacyPauseLatency
	}
	if aux.PauseCooldown != nil {
		p.PauseCooldown = *aux.PauseCooldown
	} else if aux.LegacyPauseCool != nil {
		p.PauseCooldown = *aux.LegacyPauseCool
	}
	return nil
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Config struct {
	Profile     string            `yaml:"profile"`
	Database    DatabaseConfig    `yaml:"database"`
	Scan        ScanConfig        `yaml:"scan"`
	Performance PerformanceConfig `yaml:"performance"`
	Logging     LoggingConfig     `yaml:"logging"`
	Source      string            `yaml:"-"`
}

const (
	// DefaultProfile is the safe default for mixed production use.
	DefaultProfile = "normal"
)

func normalizeProfile(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "low":
		return "low"
	case "fast":
		return "fast"
	case "", "normal":
		return DefaultProfile
	default:
		return ""
	}
}

// Default returns the normal profile. Use DefaultForProfile to select a
// conservative or throughput-oriented baseline.
func Default() *Config { return DefaultForProfile(DefaultProfile) }

// DefaultForProfile returns the baseline for low, normal, or fast.
// Individual YAML values are decoded over this baseline, so advanced users can
// override any field without losing the rest of the profile.
func DefaultForProfile(name string) *Config {
	profile := normalizeProfile(name)
	if profile == "" {
		profile = DefaultProfile
	}
	cfg := &Config{
		Profile: profile,
		Database: DatabaseConfig{
			Path:               DefaultDBPath,
			BusyTimeout:        Duration{5 * time.Second},
			RetentionDays:      90,
			StaleRetentionDays: 7,
		},
		Scan: ScanConfig{
			RootPath:            "/mnt",
			SizeBasis:           "allocated",
			Interval:            Duration{168 * time.Hour},
			QuickInterval:       Duration{24 * time.Hour},
			QuickSamplePercent:  2,
			QuickCensusFiles:    16,
			ReadBatchSize:       1024,
			IndexDepth:          3,
			IndexTopK:           20,
			IndexAllDirs:        true,
			QuickMaxFilesPerTop: 0,
			ScanOnStart:         true,
			StartupDelay:        Duration{15 * time.Second},
			ResumeInterrupted:   true,
			Inventory: InventoryConfig{
				Enabled:   false,
				BatchSize: 1000,
			},
		},
		Performance: PerformanceConfig{
			Concurrency:          Concurrency{Auto: true, Max: 8},
			MaxMetadataQPS:       300,
			MinMetadataQPS:       30,
			MaxCPUPercent:        20,
			MemoryLimit:          Bytes{Auto: true},
			AdaptiveThrottle:     true,
			LowPriority:          true,
			SampleInterval:       Duration{5 * time.Second},
			MaxHostCPUPercent:    85,
			MaxLoad1:             0,
			SlowMetadataLatency:  Duration{500 * time.Millisecond},
			PauseMetadataLatency: Duration{5 * time.Second},
			PauseCooldown:        Duration{30 * time.Second},
			MaxMetadataErrors:    8,
			ProgressInterval:     Duration{10 * time.Second},
			HeartbeatInterval:    Duration{5 * time.Second},
		},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}
	switch profile {
	case "low":
		cfg.Performance.Concurrency = Concurrency{Auto: true, Max: 4}
		cfg.Performance.MaxMetadataQPS = 100
		cfg.Performance.MinMetadataQPS = 10
		cfg.Performance.MaxCPUPercent = 10
		cfg.Performance.MemoryLimit = Bytes{Auto: true}
	case "fast":
		cfg.Performance.Concurrency = Concurrency{Auto: true, Max: 16}
		cfg.Performance.MaxMetadataQPS = 1000
		cfg.Performance.MinMetadataQPS = 100
		cfg.Performance.MaxCPUPercent = 40
		cfg.Performance.MemoryLimit = Bytes{Auto: true}
		cfg.Performance.SlowMetadataLatency = Duration{time.Second}
		cfg.Performance.PauseMetadataLatency = Duration{10 * time.Second}
	}
	return cfg
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

// Load reads a config file over the selected profile defaults and validates it.
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
		var head struct {
			Profile string `yaml:"profile"`
		}
		if err := yaml.Unmarshal(data, &head); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", resolved, err)
		}
		if strings.TrimSpace(head.Profile) != "" {
			profile := normalizeProfile(head.Profile)
			if profile == "" {
				return nil, fmt.Errorf("profile 非法: %s（可选 low、normal、fast）", head.Profile)
			}
			cfg = DefaultForProfile(profile)
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
	if c.Database.StaleRetentionDays < 1 {
		return errors.New("database.stale_retention_days 必须至少为 1")
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
	if c.Scan.QuickInterval.Duration < 0 {
		return errors.New("scan.quick_interval 不能为负数（0 表示关闭快速估算）")
	}
	if c.Scan.QuickInterval.Duration > 0 && c.Scan.QuickInterval.Duration < 10*time.Second {
		return errors.New("scan.quick_interval 不能小于 10s")
	}
	if c.Scan.QuickSamplePercent < 0 || c.Scan.QuickSamplePercent > 100 {
		return errors.New("scan.quick_sample_percent 必须在 0 到 100 之间")
	}
	if c.Scan.QuickCensusFiles < 0 || c.Scan.QuickCensusFiles > 4096 {
		return errors.New("scan.quick_census_files 必须在 0 到 4096 之间")
	}
	if c.Scan.ReadBatchSize < 32 || c.Scan.ReadBatchSize > 8192 {
		return errors.New("scan.read_batch_size 必须在 32 到 8192 之间")
	}
	if c.Scan.IndexDepth < 0 || c.Scan.IndexDepth > 16 {
		return errors.New("scan.index_depth 必须在 0 到 16 之间（0 表示关闭目录索引）")
	}
	if c.Scan.IndexTopK < 1 || c.Scan.IndexTopK > 1000 {
		return errors.New("scan.index_top_k 必须在 1 到 1000 之间")
	}
	if c.Scan.QuickMaxFilesPerTop < 0 {
		return errors.New("scan.quick_max_files_per_top 不能为负数（0 表示不限制）")
	}
	if c.Scan.StartupDelay.Duration < 0 {
		return errors.New("scan.startup_delay 不能为负数")
	}
	if c.Scan.Inventory.BatchSize < 100 || c.Scan.Inventory.BatchSize > 2000 {
		return errors.New("scan.inventory.batch_size 必须在 100 到 2000 之间")
	}

	profile := normalizeProfile(c.Profile)
	if profile == "" {
		return fmt.Errorf("profile 非法: %s（可选 low、normal、fast）", c.Profile)
	}
	c.Profile = profile

	p := &c.Performance
	if p.Concurrency.Max < 1 || p.Concurrency.Max > 64 {
		return errors.New("performance.concurrency 上限必须在 1 到 64 之间")
	}
	if p.MaxMetadataQPS < 1 || p.MaxMetadataQPS > 1_000_000 {
		return errors.New("performance.max_metadata_qps 必须在 1 到 1000000 之间")
	}
	if p.MinMetadataQPS < 1 {
		return errors.New("performance.min_metadata_qps 必须至少为 1")
	}
	// A profile supplies min QPS, but an explicit lower max QPS must still be
	// usable. Clamp the floor to the ceiling instead of rejecting the config.
	if p.MinMetadataQPS > p.MaxMetadataQPS {
		p.MinMetadataQPS = p.MaxMetadataQPS
	}
	if p.MaxCPUPercent < 1 || p.MaxCPUPercent > 100 {
		return errors.New("performance.max_cpu_percent 必须在 1 到 100 之间")
	}
	if !p.MemoryLimit.Auto && p.MemoryLimit.Bytes < 32<<20 {
		return errors.New("performance.memory_limit 不能小于 32MB（或设为 auto）")
	}
	if p.SampleInterval.Duration < time.Second {
		return errors.New("performance.sample_interval 不能小于 1s")
	}
	if p.SlowMetadataLatency.Duration < 0 || p.PauseMetadataLatency.Duration < 0 {
		return errors.New("performance.slow_metadata_latency/pause_metadata_latency 不能为负数")
	}
	if p.PauseMetadataLatency.Duration <= p.SlowMetadataLatency.Duration {
		return errors.New("performance.pause_metadata_latency 必须大于 slow_metadata_latency")
	}
	if p.PauseCooldown.Duration < time.Second {
		return errors.New("performance.pause_cooldown 不能小于 1s")
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
	if mountinfo.IsNetworkFS(c.Database.Path) {
		return fmt.Errorf("数据库不能位于网络或 FUSE 挂载目录: %s", c.Database.Path)
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
