package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/service"
)

type monitorReport struct {
	Path       string         `json:"path"`
	ActivePath string         `json:"active_path,omitempty"`
	Unit       string         `json:"unit"`
	PID        int            `json:"pid"`
	Config     monitorConfig  `json:"config"`
	Runtime    monitorRuntime `json:"runtime"`
	Assessment monitorVerdict `json:"assessment"`
}

type monitorConfig struct {
	Concurrency       string  `json:"concurrency"`
	MaxMetadataQPS    int     `json:"max_metadata_qps"`
	MaxCPUPercent     int     `json:"max_cpu_percent"`
	MemoryLimit       int64   `json:"memory_limit"`
	MemoryLimitAuto   bool    `json:"memory_limit_auto"`
	MaxHostCPUPercent float64 `json:"max_host_cpu_percent"`
	MaxLoad1          float64 `json:"max_load1"`
	NFSSlowLatencyMS  int64   `json:"nfs_slow_latency_ms"`
	NFSPauseLatencyMS int64   `json:"nfs_pause_latency_ms"`
}

type monitorRuntime struct {
	ProcessCPUPercent float64 `json:"process_cpu_percent"`
	ProcessRSS        int64   `json:"process_rss_bytes"`
	ProcessThreads    int     `json:"process_threads"`
	HostCPUPercent    float64 `json:"host_cpu_percent"`
	HostLoad1         float64 `json:"host_load1"`
	HostMemUsed       int64   `json:"host_mem_used_bytes"`
	HostMemTotal      int64   `json:"host_mem_total_bytes"`
	MemoryCurrent     int64   `json:"memory_current_bytes"`
	MemoryHigh        int64   `json:"memory_high_bytes"`
	MemoryMax         int64   `json:"memory_max_bytes"`
	CPUQuotaPercent   float64 `json:"cpu_quota_percent"`
	CPUQuotaKnown     bool    `json:"cpu_quota_known"`
	Active            bool    `json:"active"`
	Mode              string  `json:"mode,omitempty"`
	CurrentDir        string  `json:"current_dir,omitempty"`
	DoneDirs          int     `json:"done_dirs"`
	TotalDirs         int     `json:"total_dirs"`
	QPS               float64 `json:"qps"`
	QPSLimit          float64 `json:"qps_limit"`
	LastError         string  `json:"last_error,omitempty"`
}

type monitorVerdict struct {
	Level       string   `json:"level"`
	Summary     string   `json:"summary"`
	Reasons     []string `json:"reasons,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
}

func monitorCommand(ctx context.Context, cfg *config.Config, path string, status *service.StatusView) monitorReport {
	report := monitorReport{
		Path: path,
		Unit: monitorUnit(),
		Config: monitorConfig{
			Concurrency:       cfg.Performance.Concurrency.String(),
			MaxMetadataQPS:    cfg.Performance.MaxMetadataQPS,
			MaxCPUPercent:     cfg.Performance.MaxCPUPercent,
			MemoryLimit:       cfg.Performance.MemoryLimit.Resolve(),
			MemoryLimitAuto:   cfg.Performance.MemoryLimit.Auto,
			MaxHostCPUPercent: cfg.Performance.MaxHostCPUPercent,
			MaxLoad1:          cfg.Performance.MaxLoad1,
			NFSSlowLatencyMS:  cfg.Performance.NFSSlowLatency.Duration.Milliseconds(),
			NFSPauseLatencyMS: cfg.Performance.NFSPauseLatency.Duration.Milliseconds(),
		},
	}
	props := readSystemdProperties(ctx, report.Unit)
	report.PID = parseInt(props["MainPID"])
	if report.PID == 0 {
		report.PID = findDaemonPID()
	}
	if v, ok := props["MemoryCurrent"]; ok {
		report.Runtime.MemoryCurrent = parseUintBytes(v)
	}
	if v, ok := props["MemoryHigh"]; ok {
		report.Runtime.MemoryHigh = parseUintBytes(v)
	}
	if v, ok := props["MemoryMax"]; ok {
		report.Runtime.MemoryMax = parseUintBytes(v)
	}
	if v, ok := props["CPUQuotaPerSecUSec"]; ok {
		if quota, err := strconv.ParseFloat(v, 64); err == nil && quota > 0 {
			report.Runtime.CPUQuotaPercent = quota / 10000
			report.Runtime.CPUQuotaKnown = true
		}
	}
	report.Runtime.HostCPUPercent, report.Runtime.HostLoad1, report.Runtime.HostMemUsed, report.Runtime.HostMemTotal = readHostResources()
	if status != nil {
		if status.Active != nil {
			report.ActivePath = status.Root.Path
			report.Runtime.Active = true
			report.Runtime.Mode = string(status.Active.Mode)
			report.Runtime.CurrentDir = status.Active.CurrentDir
			report.Runtime.DoneDirs = status.ActiveDoneDirs
			report.Runtime.TotalDirs = status.Active.TotalDirs
			report.Runtime.QPS = status.Active.MetadataQPS
			report.Runtime.QPSLimit = status.Active.MetadataLimit
			report.Runtime.LastError = status.Active.Error
		} else if len(status.ChildActive) > 0 {
			child := status.ChildActive[0]
			report.ActivePath = child.RootPath
			report.Runtime.Active = true
			report.Runtime.Mode = string(child.Run.Mode)
			report.Runtime.CurrentDir = child.Run.CurrentDir
			report.Runtime.DoneDirs = child.DoneDirs
			report.Runtime.TotalDirs = child.Run.TotalDirs
			report.Runtime.QPS = child.Run.MetadataQPS
			report.Runtime.QPSLimit = child.Run.MetadataLimit
			report.Runtime.LastError = child.Run.Error
		} else {
			report.Runtime.QPS = status.MetadataQPS
			report.Runtime.QPSLimit = status.MetadataLimit
			if status.Last != nil {
				report.Runtime.LastError = status.Last.Error
			}
		}
	}
	if report.Runtime.QPSLimit == 0 {
		report.Runtime.QPSLimit = float64(cfg.Performance.MaxMetadataQPS)
	}
	if report.Runtime.Active {
		if scanPID := findScanPID(report.ActivePath); scanPID > 0 {
			report.PID = scanPID
		}
	}
	if report.PID > 0 {
		report.Runtime.ProcessRSS, report.Runtime.ProcessThreads = readProcStatus(report.PID)
		report.Runtime.ProcessCPUPercent = readProcessCPU(report.PID)
	}
	report.Assessment = assessMonitor(report)
	return report
}

func monitorUnit() string {
	if unit := os.Getenv("DIRSPACE_UNIT"); unit != "" {
		return unit
	}
	return "dirspace.service"
}

func readSystemdProperties(ctx context.Context, unit string) map[string]string {
	out := map[string]string{}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "show", unit, "--no-pager",
		"--property=MainPID,MemoryCurrent,MemoryHigh,MemoryMax,CPUQuotaPerSecUSec,TasksCurrent")
	data, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

func findDaemonPID() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		joined := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if strings.Contains(joined, "dirspace") && strings.Contains(joined, "daemon") {
			return pid
		}
	}
	return 0
}

func findScanPID(scanRoot string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		joined := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(joined, "dirspace") || strings.Contains(joined, "daemon") ||
			strings.Contains(joined, "monitor") || strings.Contains(joined, "status") {
			continue
		}
		if !strings.Contains(joined, " quick ") && !strings.Contains(joined, " scan ") &&
			!strings.Contains(joined, " refresh ") && !strings.Contains(joined, " rebuild ") {
			continue
		}
		if scanRoot != "" && !strings.Contains(joined, scanRoot) {
			continue
		}
		return pid
	}
	return 0
}

func readProcStatus(pid int) (rss int64, threads int) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			rss = parseInt64(fields[1]) * 1024
		case "Threads:":
			threads = int(parseInt64(fields[1]))
		}
	}
	return rss, threads
}

func readProcessCPU(pid int) float64 {
	first, ok := readProcCPUTicks(pid)
	if !ok {
		return 0
	}
	start := time.Now()
	time.Sleep(700 * time.Millisecond)
	second, ok := readProcCPUTicks(pid)
	if !ok {
		return 0
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	// Linux exposes CPU time in USER_HZ ticks, normally 100 per second.
	return float64(second-first) / 100.0 / elapsed * 100
}

func readProcCPUTicks(pid int) (uint64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	end := strings.LastIndex(string(data), ")")
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 13 {
		return 0, false
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	return utime + stime, true
}

func readHostResources() (cpuPercent, load1 float64, memUsed, memTotal int64) {
	if runtime.GOOS != "linux" {
		return 0, 0, 0, 0
	}
	cpuPercent = readHostCPU()
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 0 {
			load1, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		var available int64
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				memTotal = parseInt64(fields[1]) * 1024
			case "MemAvailable:":
				available = parseInt64(fields[1]) * 1024
			}
		}
		if memTotal > 0 && available > 0 {
			memUsed = memTotal - available
		}
	}
	return cpuPercent, load1, memUsed, memTotal
}

func readHostCPU() float64 {
	first, ok := readCPUStat()
	if !ok {
		return 0
	}
	time.Sleep(500 * time.Millisecond)
	second, ok := readCPUStat()
	if !ok || second.total <= first.total {
		return 0
	}
	totalDelta := second.total - first.total
	busyDelta := second.busy - first.busy
	if totalDelta == 0 {
		return 0
	}
	return float64(busyDelta) / float64(totalDelta) * 100
}

type cpuStat struct {
	busy  uint64
	total uint64
}

func readCPUStat() (cpuStat, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuStat{}, false
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuStat{}, false
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return cpuStat{}, false
		}
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	return cpuStat{busy: total - idle, total: total}, true
}

func assessMonitor(r monitorReport) monitorVerdict {
	v := monitorVerdict{Level: "unknown", Summary: "空闲，无法判断配置高低"}
	if !r.Runtime.Active {
		v.Reasons = append(v.Reasons, "当前没有扫描任务；配置是否合适要在扫描稳定运行后判断")
		return v
	}
	if r.Runtime.QPS <= 0 {
		v.Summary = "任务刚启动，样本不足，暂不判断"
		v.Reasons = append(v.Reasons, "等待 QPS、CPU 和内存稳定 5 到 10 分钟后再看")
		return v
	}
	cfg := r.Config
	rt := r.Runtime
	var high, low bool

	if cfg.MaxHostCPUPercent > 0 && rt.HostCPUPercent >= cfg.MaxHostCPUPercent {
		high = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("主机 CPU %.1f%%，已达到保护阈值 %.1f%%", rt.HostCPUPercent, cfg.MaxHostCPUPercent))
	}
	if cfg.MaxLoad1 > 0 && rt.HostLoad1 >= cfg.MaxLoad1 {
		high = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("Load1 %.2f，已达到阈值 %.2f", rt.HostLoad1, cfg.MaxLoad1))
	}
	if rt.MemoryHigh > 0 && rt.MemoryCurrent > 0 && float64(rt.MemoryCurrent) >= 0.85*float64(rt.MemoryHigh) {
		high = true
		v.Reasons = append(v.Reasons, "进程内存已接近 cgroup MemoryHigh")
	}
	if rt.MemoryMax > 0 && rt.MemoryCurrent > 0 && float64(rt.MemoryCurrent) >= 0.85*float64(rt.MemoryMax) {
		high = true
		v.Reasons = append(v.Reasons, "进程内存已接近 cgroup MemoryMax")
	}
	if cfg.MemoryLimit > 0 && rt.ProcessRSS > 0 && float64(rt.ProcessRSS) >= 0.85*float64(cfg.MemoryLimit) {
		high = true
		v.Reasons = append(v.Reasons, "进程 RSS 已接近应用内存上限")
	}
	if cfg.MaxCPUPercent > 0 && rt.ProcessCPUPercent >= 0.85*float64(cfg.MaxCPUPercent) &&
		(rt.QPSLimit <= 0 || rt.QPS < 0.8*rt.QPSLimit) {
		low = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("进程 CPU %.1f%% 已接近配置上限 %d%%，但 QPS 未到限速", rt.ProcessCPUPercent, cfg.MaxCPUPercent))
	}
	if rt.QPSLimit > 0 && rt.QPS >= 0.85*rt.QPSLimit && rt.HostCPUPercent < 70 &&
		(cfg.MaxLoad1 <= 0 || rt.HostLoad1 < cfg.MaxLoad1) {
		low = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("QPS %.0f 已接近限速 %.0f，但主机 CPU/Load 还有余量", rt.QPS, rt.QPSLimit))
	}
	if rt.QPSLimit > 0 && rt.QPS > 0 && rt.QPS < 0.5*rt.QPSLimit && rt.HostCPUPercent < 50 &&
		(cfg.MaxCPUPercent <= 0 || rt.ProcessCPUPercent < 0.5*float64(cfg.MaxCPUPercent)) {
		low = true
		v.Reasons = append(v.Reasons, "QPS 和进程 CPU 都明显低于配置上限")
	}
	if high {
		v.Level = "high"
		v.Summary = "配置可能偏高，当前主机/内存压力较大"
		v.Suggestions = append(v.Suggestions, "优先降低 max_metadata_qps、concurrency 或 CPU 上限；观察 NFS 延迟和业务侧体验")
	}
	if low && !high {
		v.Level = "low"
		v.Summary = "配置可能偏低，资源还有余量"
		v.Suggestions = append(v.Suggestions, "小步提高 max_metadata_qps 或 max_cpu_percent，每次观察 5 到 10 分钟")
	}
	if !high && !low {
		v.Level = "ok"
		v.Summary = "当前配置基本合适"
	}
	if len(v.Reasons) == 0 {
		v.Reasons = append(v.Reasons, "关键指标未触发高低阈值；继续观察 NFS 延迟和主机负载")
	}
	return v
}

func printMonitor(w io.Writer, r monitorReport) {
	fmt.Fprintf(w, "dirspace 资源与配置体检\n\n")
	fmt.Fprintf(w, "路径：%s\n", r.Path)
	if r.ActivePath != "" && r.ActivePath != r.Path {
		fmt.Fprintf(w, "活跃任务根：%s\n", r.ActivePath)
	}
	fmt.Fprintf(w, "服务：%s\nPID：%d\n\n", r.Unit, r.PID)
	fmt.Fprintln(w, "配置")
	fmt.Fprintf(w, "  并发：%s\n", r.Config.Concurrency)
	fmt.Fprintf(w, "  QPS：%d（自适应启动值更低）\n", r.Config.MaxMetadataQPS)
	if r.Config.MaxCPUPercent > 0 {
		fmt.Fprintf(w, "  CPU：%d%%（单核）\n", r.Config.MaxCPUPercent)
	} else {
		fmt.Fprintln(w, "  CPU：不限制")
	}
	fmt.Fprintf(w, "  内存：%s（%s）\n", config.FormatBytes(r.Config.MemoryLimit), autoLabel(r.Config.MemoryLimitAuto))
	if r.Config.MaxHostCPUPercent > 0 {
		fmt.Fprintf(w, "  主机保护：CPU %.0f%%", r.Config.MaxHostCPUPercent)
	} else {
		fmt.Fprintln(w, "  主机保护：CPU 关闭")
	}
	if r.Config.MaxHostCPUPercent > 0 {
		if r.Config.MaxLoad1 > 0 {
			fmt.Fprintf(w, "，Load1 %.2f\n", r.Config.MaxLoad1)
		} else {
			fmt.Fprintln(w, "，Load1 关闭")
		}
	} else if r.Config.MaxLoad1 > 0 {
		fmt.Fprintf(w, "  Load1 保护：%.2f\n", r.Config.MaxLoad1)
	}
	fmt.Fprintf(w, "  NFS：慢 %dms，暂停 %dms\n\n", r.Config.NFSSlowLatencyMS, r.Config.NFSPauseLatencyMS)

	fmt.Fprintln(w, "运行")
	fmt.Fprintf(w, "  进程 CPU：%.1f%%", r.Runtime.ProcessCPUPercent)
	if r.Config.MaxCPUPercent > 0 {
		fmt.Fprintf(w, " / 上限 %d%%", r.Config.MaxCPUPercent)
	}
	if r.Runtime.CPUQuotaKnown {
		fmt.Fprintf(w, " / cgroup quota %.0f%%", r.Runtime.CPUQuotaPercent)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  进程内存：%s", config.FormatBytes(r.Runtime.ProcessRSS))
	if r.Runtime.MemoryCurrent > 0 {
		fmt.Fprintf(w, " / cgroup 当前 %s", config.FormatBytes(r.Runtime.MemoryCurrent))
	}
	if r.Runtime.MemoryHigh > 0 {
		fmt.Fprintf(w, " / high %s", config.FormatBytes(r.Runtime.MemoryHigh))
	}
	if r.Runtime.MemoryMax > 0 {
		fmt.Fprintf(w, " / max %s", config.FormatBytes(r.Runtime.MemoryMax))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  主机：CPU %.1f%%，Load1 %.2f", r.Runtime.HostCPUPercent, r.Runtime.HostLoad1)
	if r.Runtime.HostMemTotal > 0 {
		fmt.Fprintf(w, "，内存 %s / %s", config.FormatBytes(r.Runtime.HostMemUsed), config.FormatBytes(r.Runtime.HostMemTotal))
	}
	fmt.Fprintln(w)
	if r.Runtime.Active {
		fmt.Fprintf(w, "  任务：%s，%d/%d 目录", r.Runtime.Mode, r.Runtime.DoneDirs, r.Runtime.TotalDirs)
		if r.Runtime.CurrentDir != "" {
			fmt.Fprintf(w, "，当前 %s", r.Runtime.CurrentDir)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  QPS：%.0f / 限速 %.0f\n", r.Runtime.QPS, r.Runtime.QPSLimit)
	} else {
		fmt.Fprintf(w, "  任务：空闲（最近 QPS %.0f / %.0f）\n", r.Runtime.QPS, r.Runtime.QPSLimit)
	}
	if r.Runtime.LastError != "" {
		fmt.Fprintf(w, "  最近错误：%s\n", r.Runtime.LastError)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "判断")
	for _, reason := range r.Assessment.Reasons {
		fmt.Fprintf(w, "  - %s\n", reason)
	}
	fmt.Fprintf(w, "  结论：%s\n", r.Assessment.Summary)
	for _, suggestion := range r.Assessment.Suggestions {
		fmt.Fprintf(w, "  建议：%s\n", suggestion)
	}
	fmt.Fprintln(w, "  提示：NFS 服务端延迟请配合 nfsiostat 5 / nfsstat -c 观察。")
}

func autoLabel(auto bool) string {
	if auto {
		return "自动"
	}
	return "固定"
}

func parseInt(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func parseUintBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "max" || s == "infinity" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
