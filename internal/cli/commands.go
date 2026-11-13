package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/logger"
	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/mountinfo"
	"github.com/YEDEDD/dirspace/internal/platform"
	"github.com/YEDEDD/dirspace/internal/service"
	"github.com/YEDEDD/dirspace/internal/storage"
)

// Version is overridden with -ldflags at release time.
var Version = "1.13.1"

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

type command struct {
	name string
	args []string
}

func parseCommand(positional []string) (command, error) {
	if len(positional) == 0 {
		return command{name: "top", args: []string{"10"}}, nil
	}
	name := positional[0]
	rest := positional[1:]
	switch name {
	case "top":
		if len(rest) == 0 {
			return command{name: "top", args: []string{"10"}}, nil
		}
		if len(rest) > 1 {
			return command{}, &usageError{"top 只接受一个数字参数"}
		}
		if _, err := parsePositiveInt(rest[0], "top 参数"); err != nil {
			return command{}, err
		}
		return command{name: name, args: rest}, nil
	case "children":
		if len(rest) > 1 {
			return command{}, &usageError{"children 只接受一个可选数量参数"}
		}
		if len(rest) == 1 {
			if _, err := parsePositiveInt(rest[0], "children 数量"); err != nil {
				return command{}, err
			}
		}
		return command{name: name, args: rest}, nil
	case "list", "partial", "status", "size", "fs-usage", "merge", "monitor", "scan", "quick", "daemon", "refresh", "pause", "resume", "cancel",
		"cleanup", "backup", "doctor", "diff":
		if len(rest) > 0 {
			return command{}, &usageError{fmt.Sprintf("%s 不接受位置参数: %s", name, strings.Join(rest, " "))}
		}
		return command{name: name}, nil
	case "rebuild":
		if len(rest) > 1 {
			return command{}, &usageError{"rebuild 只接受一个一级目录名称"}
		}
		return command{name: name, args: rest}, nil
	case "help":
		return command{name: "help", args: rest}, nil
	case "version":
		return command{name: "version"}, nil
	case "config":
		if len(rest) == 0 {
			return command{name: "config", args: []string{"explain"}}, nil
		}
		if len(rest) == 1 && rest[0] == "explain" {
			return command{name: "config", args: rest}, nil
		}
		return command{}, &usageError{"config 只接受 explain"}
	default:
		return command{}, &usageError{fmt.Sprintf("未知命令: %s", name)}
	}
}

func validateCommandOptions(cmd command, opts *options) error {
	allowOnce := cmd.name == "daemon"
	allowRepair := cmd.name == "doctor"
	allowVacuum := cmd.name == "cleanup"
	allowDays := cmd.name == "cleanup"
	allowSince := cmd.name == "diff"
	allowOutput := cmd.name == "backup"
	allowFull := cmd.name == "refresh" || cmd.name == "scan"
	allowNew := cmd.name == "scan"
	allowResume := cmd.name == "scan"
	allowPath := cmd.name == "top" || cmd.name == "list" || cmd.name == "children" || cmd.name == "partial" || cmd.name == "status" || cmd.name == "size" || cmd.name == "fs-usage" || cmd.name == "merge" || cmd.name == "monitor" || cmd.name == "diff" ||
		cmd.name == "scan" || cmd.name == "quick" || cmd.name == "refresh" || cmd.name == "rebuild" ||
		cmd.name == "pause" || cmd.name == "resume" || cmd.name == "cancel"
	if opts.once && !allowOnce {
		return &usageError{"--once 仅用于 daemon"}
	}
	if opts.repair && !allowRepair {
		return &usageError{"--repair 仅用于 doctor"}
	}
	if opts.vacuum && !allowVacuum {
		return &usageError{"--vacuum 仅用于 cleanup"}
	}
	if opts.days > 0 && !allowDays {
		return &usageError{"--days 仅用于 cleanup"}
	}
	if opts.sinceSet && !allowSince {
		return &usageError{"--since 仅用于 diff"}
	}
	if opts.output != "" && !allowOutput {
		return &usageError{"--output 仅用于 backup"}
	}
	if opts.full && !allowFull {
		return &usageError{"--full 仅用于 scan 或 refresh"}
	}
	if opts.newOnly && !allowNew {
		return &usageError{"--new 仅用于 scan"}
	}
	if opts.full && opts.newOnly {
		return &usageError{"--full 与 --new 不能同时使用"}
	}
	if opts.resume && !allowResume {
		return &usageError{"--resume 仅用于 scan"}
	}
	if opts.stagingOnly && cmd.name != "partial" {
		return &usageError{"--staging-only 仅用于 partial"}
	}
	if opts.exact && cmd.name != "size" {
		return &usageError{"--exact 仅用于 size"}
	}
	longCommand := cmd.name == "quick" || cmd.name == "scan" || cmd.name == "refresh" || cmd.name == "rebuild" ||
		(cmd.name == "size" && opts.exact)
	if opts.foreground && !longCommand {
		return &usageError{"--foreground 仅用于 quick/scan/refresh/rebuild/size --exact"}
	}
	if opts.background && !longCommand {
		return &usageError{"--background 仅用于 quick/scan/refresh/rebuild/size --exact"}
	}
	if opts.foreground && opts.background {
		return &usageError{"--foreground 与 --background 不能同时使用"}
	}
	if opts.background && opts.json {
		return &usageError{"--background 不能和 --json 一起使用"}
	}
	if opts.shard != "" && cmd.name != "scan" && cmd.name != "refresh" {
		return &usageError{"--shard 仅用于 scan 或 refresh"}
	}
	if opts.shard != "" && opts.newOnly {
		return &usageError{"--shard 不能和 --new 一起使用"}
	}
	if len(opts.inputs) > 0 && cmd.name != "merge" {
		return &usageError{"--inputs 仅用于 merge"}
	}
	if opts.path != "" && !allowPath {
		return &usageError{"--path 仅用于查询、扫描或任务控制命令；daemon 多根请配置 extra_roots"}
	}
	if opts.json && cmd.name == "daemon" {
		return &usageError{"daemon 不产生 JSON 输出"}
	}
	return nil
}

// Execute runs the CLI and returns a process exit code.
func Execute(args []string, stdout, stderr io.Writer) int {
	return ExecuteContext(context.Background(), args, stdout, stderr)
}

// ExecuteContext is testable and propagates cancellation.
func ExecuteContext(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n\n", err)
		fmt.Fprintln(stderr, "使用 dirspace --help 查看帮助。")
		return exitUsage
	}
	if opts.help {
		printHelp(stdout)
		return exitOK
	}
	if opts.version {
		fmt.Fprintf(stdout, "dirspace %s\n", Version)
		return exitOK
	}
	cmd, err := parseCommand(opts.positional)
	if err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n\n", err)
		fmt.Fprintln(stderr, "使用 dirspace --help 查看帮助。")
		return exitUsage
	}
	if cmd.name == "version" {
		fmt.Fprintf(stdout, "dirspace %s\n", Version)
		return exitOK
	}
	if cmd.name == "help" {
		printHelp(stdout)
		return exitOK
	}
	if err := validateCommandOptions(cmd, opts); err != nil {
		fmt.Fprintf(stderr, "参数错误: %v\n", err)
		return exitUsage
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return exitRuntime
	}
	if opts.dbPath != "" {
		dbPath, err := config.NormalizeAbsolutePath(opts.dbPath)
		if err != nil {
			fmt.Fprintf(stderr, "参数错误: --db 非法: %v\n", err)
			return exitUsage
		}
		cfg.Database.Path = dbPath
	}
	if opts.logLevel != "" {
		cfg.Logging.Level = opts.logLevel
	}
	if opts.logical {
		cfg.Scan.SizeBasis = "logical"
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return exitRuntime
	}
	if detached, code := maybeDetach(args, cmd, opts, cfg, stdout, stderr); detached {
		return code
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Repair intentionally does not open the possibly-corrupt database first.
	if cmd.name == "doctor" && opts.repair {
		moved, err := service.RepairDatabase(ctx, cfg.Database.Path)
		if err != nil {
			fmt.Fprintf(stderr, "修复数据库失败: %v\n", err)
			return exitRuntime
		}
		fmt.Fprintf(stdout, "已移走损坏数据库: %s\n", moved)
		fmt.Fprintf(stdout, "已创建新的空缓存: %s\n", cfg.Database.Path)
		fmt.Fprintln(stdout, "请重新执行首次扫描。")
		return exitOK
	}

	log, err := logger.New(cfg.Logging.Level, cfg.Logging.Format, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "日志配置错误: %v\n", err)
		return exitRuntime
	}

	// config explain is read-only and must work even when the database is
	// missing or temporarily locked.
	if cmd.name == "config" {
		if opts.json {
			if err := writeJSON(stdout, buildConfigJSON(cfg)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printConfig(stdout, cfg)
		}
		return exitOK
	}

	defaultPath := cfg.Scan.RootPath
	queryPath := opts.path
	if queryPath == "" {
		queryPath = defaultPath
	}
	if err := validateExplicitScanRoot(cmd.name, queryPath, opts.path, cfg.Database.Path, cmd.args); err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return exitRuntime
	}

	store, err := storage.Open(ctx, cfg.Database.Path, storage.Options{BusyTimeout: cfg.Database.BusyTimeout.Duration})
	if err != nil {
		fmt.Fprintf(stderr, "数据库错误: %v\n", err)
		fmt.Fprintln(stderr, "若数据库损坏，可先停止服务，再执行: dirspace doctor --repair --db "+cfg.Database.Path)
		return exitRuntime
	}
	defer store.Close()
	svc := service.New(store, cfg, log)
	switch cmd.name {
	case "monitor":
		status, statusErr := svc.Status(ctx, queryPath)
		if statusErr != nil {
			if _, ok := statusErr.(*service.NotScannedError); !ok {
				printError(stderr, statusErr)
				return exitRuntime
			}
		}
		report := monitorCommand(ctx, cfg, queryPath, status)
		if opts.json {
			if err := writeJSON(stdout, report); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printMonitor(stdout, report)
		}
		return exitOK

	case "top", "list":
		limit := 0
		if cmd.name == "top" {
			limit, err = parsePositiveInt(cmd.args[0], "top 参数")
			if err != nil {
				fmt.Fprintf(stderr, "参数错误: %v\n", err)
				return exitUsage
			}
		}
		view, err := svc.Query(ctx, queryPath, limit)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildQueryJSON(svc, view)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printQuery(stdout, svc, view)
		}
		return exitOK

	case "children":
		limit := 0
		if len(cmd.args) == 1 {
			limit, err = parsePositiveInt(cmd.args[0], "children 数量")
			if err != nil {
				fmt.Fprintf(stderr, "参数错误: %v\n", err)
				return exitUsage
			}
		}
		view, err := svc.Children(ctx, queryPath, limit)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildChildrenJSON(view)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printChildren(stdout, view)
		}
		return exitOK

	case "merge":
		if opts.dbPath == "" {
			fmt.Fprintln(stderr, "参数错误: merge 需要 --db <合并后的数据库>")
			return exitUsage
		}
		if len(opts.inputs) == 0 {
			fmt.Fprintln(stderr, "参数错误: merge 需要 --inputs <分片1,分片2,...>")
			return exitUsage
		}
		root, err := svc.Merge(ctx, opts.dbPath, queryPath, opts.inputs)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		view, err := svc.Size(ctx, root.Path, false)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildSizeJSON(view)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			fmt.Fprintf(stdout, "合并完成：%d 个分片 -> %s\n\n", len(opts.inputs), opts.dbPath)
			printSize(stdout, view)
			fmt.Fprintln(stdout, "注意：当前合并按分片求和，跨分片硬链接尚未去重；存在跨分片硬链接时不能宣称精确。")
		}
		return exitOK

	case "fs-usage":
		usage, err := svc.FSUsage(ctx, queryPath)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildFSUsageJSON(usage)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printFSUsage(stdout, usage)
		}
		return exitOK

	case "size":
		view, err := svc.Size(ctx, queryPath, opts.exact)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildSizeJSON(view)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printSize(stdout, view)
		}
		return exitOK

	case "partial":
		view, err := svc.Partial(ctx, queryPath, 0, !opts.stagingOnly)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildPartialJSON(view)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printPartial(stdout, view)
		}
		return exitOK

	case "status":
		svc.Recover(ctx)
		view, err := svc.Status(ctx, queryPath)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildStatusJSON(view)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printStatus(stdout, view)
		}
		return exitOK

	case "quick":
		setLowPriority(cfg, log)
		progress := progressPrinter(stderr)
		fmt.Fprintf(stderr, "开始快速估算 %s（抽样 %d%%，并发 %s，元数据上限 %d QPS）...\n",
			queryPath, cfg.Scan.QuickSamplePercent, cfg.Performance.Concurrency, cfg.Performance.MaxMetadataQPS)
		summary, err := svc.Scan(ctx, queryPath, service.ScanOptions{
			Mode:     model.ModeQuick,
			Progress: progress,
		})
		if summary != nil {
			if opts.json {
				_ = writeJSON(stdout, buildScanJSON(summary))
			} else {
				printScanSummary(stdout, summary)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "快速估算失败: %v\n", err)
			return exitRuntime
		}
		return exitOK

	case "scan", "refresh":
		setLowPriority(cfg, log)
		progress := progressPrinter(stderr)
		scanOpts := service.ScanOptions{
			Resume:         opts.resume,
			ResumeCanceled: opts.resume,
			Progress:       progress,
		}
		if opts.shard != "" {
			index, count, err := parseShard(opts.shard)
			if err != nil {
				fmt.Fprintf(stderr, "参数错误: %v\n", err)
				return exitUsage
			}
			scanOpts.ShardIndex, scanOpts.ShardCount = index, count
		}
		if opts.full {
			scanOpts.Mode = model.ModeFull
		}
		if opts.newOnly {
			scanOpts.Mode = model.ModeIncremental
			scanOpts.Resume = false
			scanOpts.ResumeCanceled = false
		}
		fmt.Fprintf(stderr, "开始扫描 %s（并发 %s，元数据上限 %d QPS）...\n",
			queryPath, cfg.Performance.Concurrency, cfg.Performance.MaxMetadataQPS)
		summary, err := svc.Scan(ctx, queryPath, scanOpts)
		if summary != nil {
			if opts.json {
				_ = writeJSON(stdout, buildScanJSON(summary))
			} else {
				printScanSummary(stdout, summary)
			}
		}
		if err != nil {
			if errors.Is(err, model.ErrCanceled) {
				fmt.Fprintln(stderr, "扫描已按请求取消，正式缓存未被覆盖。")
			} else if errors.Is(err, context.Canceled) {
				fmt.Fprintln(stderr, "扫描被中断，已保留上次有效缓存；可执行 dirspace scan --resume 续扫。")
			} else {
				fmt.Fprintf(stderr, "扫描失败: %v\n", err)
			}
			return exitRuntime
		}
		return exitOK

	case "diff":
		view, err := svc.Diff(ctx, queryPath, opts.since)
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			if err := writeJSON(stdout, buildDiffJSON(view)); err != nil {
				fmt.Fprintf(stderr, "输出 JSON 失败: %v\n", err)
				return exitRuntime
			}
		} else {
			printDiff(stdout, view)
		}
		return exitOK

	case "rebuild":
		setLowPriority(cfg, log)
		var name string
		if len(cmd.args) == 1 {
			name = cmd.args[0]
		}
		path := queryPath
		if name == "" && opts.path == "" {
			fmt.Fprintln(stderr, "参数错误: rebuild 需要一级目录名称，或使用 --path /root/child")
			return exitUsage
		}
		summary, err := svc.Rebuild(ctx, path, name)
		if summary != nil {
			if opts.json {
				_ = writeJSON(stdout, buildScanJSON(summary))
			} else {
				printScanSummary(stdout, summary)
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "单目录重算失败: %v\n", err)
			return exitRuntime
		}
		return exitOK

	case "daemon":
		setLowPriority(cfg, log)
		if err := svc.RunDaemon(ctx, service.DaemonOptions{Once: opts.once}); err != nil {
			fmt.Fprintf(stderr, "后台调度失败: %v\n", err)
			return exitRuntime
		}
		return exitOK

	case "pause", "resume", "cancel":
		var err error
		switch cmd.name {
		case "pause":
			err = svc.Pause(ctx, queryPath)
		case "resume":
			err = svc.Resume(ctx, queryPath)
		case "cancel":
			err = svc.Cancel(ctx, queryPath)
		}
		if err != nil {
			printError(stderr, err)
			return exitRuntime
		}
		if opts.json {
			_ = writeJSON(stdout, map[string]any{"ok": true, "command": cmd.name, "path": queryPath})
		} else {
			label := map[string]string{"pause": "已发送暂停请求", "resume": "已发送恢复请求", "cancel": "已发送取消请求"}[cmd.name]
			fmt.Fprintf(stdout, "%s：%s\n", label, queryPath)
		}
		return exitOK

	case "cleanup":
		days := opts.days
		if days <= 0 {
			days = cfg.Database.RetentionDays
		}
		history, runs, err := svc.CleanupHistory(ctx, days, opts.vacuum)
		if err != nil {
			fmt.Fprintf(stderr, "清理历史失败: %v\n", err)
			return exitRuntime
		}
		if opts.json {
			_ = writeJSON(stdout, map[string]any{
				"ok": true, "history_rows": history, "scan_runs": runs,
				"older_than_days": days, "vacuum": opts.vacuum,
			})
		} else {
			fmt.Fprintf(stdout, "已清理 %d 条历史统计、%d 个历史扫描任务（早于 %d 天）。\n", history, runs, days)
			if opts.vacuum {
				fmt.Fprintln(stdout, "已执行 VACUUM。")
			}
		}
		return exitOK

	case "backup":
		destination := opts.output
		if destination == "" {
			destination = cfg.Database.Path + ".backup-" + time.Now().Format("20060102-150405")
		}
		if err := svc.Backup(ctx, destination); err != nil {
			fmt.Fprintf(stderr, "备份失败: %v\n", err)
			return exitRuntime
		}
		abs, _ := filepath.Abs(destination)
		if opts.json {
			_ = writeJSON(stdout, map[string]any{"ok": true, "backup": abs})
		} else {
			fmt.Fprintf(stdout, "数据库备份完成：%s\n", abs)
		}
		return exitOK

	case "doctor":
		lines, err := svc.QuickCheck(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "数据库检查失败: %v\n", err)
			return exitRuntime
		}
		ok := len(lines) == 1 && strings.EqualFold(lines[0], "ok")
		if opts.json {
			_ = writeJSON(stdout, map[string]any{"ok": ok, "result": lines})
		} else if ok {
			fmt.Fprintf(stdout, "数据库完整性检查通过：%s\n", cfg.Database.Path)
		} else {
			fmt.Fprintln(stdout, "数据库完整性检查发现问题：")
			for _, line := range lines {
				fmt.Fprintf(stdout, "  %s\n", line)
			}
			fmt.Fprintln(stdout, "请在停止服务后使用 --repair 重建缓存（会保留损坏文件副本）。")
		}
		if !ok {
			return exitRuntime
		}
		return exitOK
	}
	return exitUsage
}

func printError(w io.Writer, err error) {
	fmt.Fprintf(w, "错误: %v\n", err)
	var notScanned *service.NotScannedError
	if errors.As(err, &notScanned) {
		fmt.Fprintln(w, "提示: 先执行首次扫描建立基线，例如: dirspace scan --path "+notScanned.Path)
	}
	if errors.Is(err, service.ErrRootLocked) {
		fmt.Fprintln(w, "提示: 同一路径不允许并发扫描，请等待或查看 dirspace status。")
	}
	if errors.Is(err, service.ErrNoActiveScan) {
		fmt.Fprintln(w, "提示: 可用 dirspace status 查看任务状态。")
	}
}

func progressPrinter(w io.Writer) func(model.Progress) {
	return func(p model.Progress) {
		if p.Paused {
			fmt.Fprintf(w, "[扫描] 已暂停 %d/%d 目录，%s 文件，%s，当前：%s\n",
				p.DoneDirs, p.TotalDirs, formatCount(p.Files), formatSize(p.Bytes), p.CurrentDir)
			return
		}
		fmt.Fprintf(w, "[扫描] %d/%d 目录，%s 文件，%s，当前：%s\n",
			p.DoneDirs, p.TotalDirs, formatCount(p.Files), formatSize(p.Bytes), p.CurrentDir)
	}
}

func setLowPriority(cfg *config.Config, log interface {
	Debug(string, ...any)
}) {
	if !cfg.Performance.LowPriority {
		return
	}
	if err := platform.SetLowPriority(); err != nil {
		log.Debug("设置低优先级失败", "error", err.Error())
	}
	// Bound Go's runnable threads as an additional guard. systemd CPUQuota is
	// still the hard limit.
	if cfg.Performance.Concurrency.Limit() < runtime.NumCPU() {
		runtime.GOMAXPROCS(cfg.Performance.Concurrency.Limit())
	}
}

func buildConfigJSON(cfg *config.Config) map[string]any {
	startQPS := cfg.Performance.MaxMetadataQPS
	if cfg.Performance.AdaptiveThrottle && startQPS >= 10 {
		startQPS /= 3
		if startQPS < cfg.Performance.MinMetadataQPS {
			startQPS = cfg.Performance.MinMetadataQPS
		}
	}
	return map[string]any{
		"source":                 cfg.Source,
		"profile":                cfg.Profile,
		"root_path":              cfg.Scan.RootPath,
		"database_path":          cfg.Database.Path,
		"interval":               formatInterval(cfg.Scan.Interval.Duration),
		"quick_interval":         formatInterval(cfg.Scan.QuickInterval.Duration),
		"quick_sample_percent":   cfg.Scan.QuickSamplePercent,
		"quick_census_files":     cfg.Scan.QuickCensusFiles,
		"read_batch_size":        cfg.Scan.ReadBatchSize,
		"index_depth":            cfg.Scan.IndexDepth,
		"index_top_k":            cfg.Scan.IndexTopK,
		"index_all_dirs":         cfg.Scan.IndexAllDirs,
		"retention_days":         cfg.Database.RetentionDays,
		"stale_retention_days":   cfg.Database.StaleRetentionDays,
		"concurrency":            cfg.Performance.Concurrency.String(),
		"concurrency_auto":       cfg.Performance.Concurrency.Auto,
		"concurrency_max":        cfg.Performance.Concurrency.Limit(),
		"max_metadata_qps":       cfg.Performance.MaxMetadataQPS,
		"start_metadata_qps":     startQPS,
		"min_metadata_qps":       cfg.Performance.MinMetadataQPS,
		"max_cpu_percent":        cfg.Performance.MaxCPUPercent,
		"memory_limit":           cfg.Performance.MemoryLimit.Resolve(),
		"memory_limit_auto":      cfg.Performance.MemoryLimit.Auto,
		"memory_limit_config":    cfg.Performance.MemoryLimit.String(),
		"max_host_cpu_percent":   cfg.Performance.MaxHostCPUPercent,
		"max_load1":              cfg.Performance.MaxLoad1,
		"slow_metadata_latency":  cfg.Performance.SlowMetadataLatency.Duration.String(),
		"pause_metadata_latency": cfg.Performance.PauseMetadataLatency.Duration.String(),
		"require_mount_point":    cfg.Scan.RequireMountPoint,
		"is_mount_point":         mountinfo.IsMountPoint(cfg.Scan.RootPath),
		"is_within_mount":        mountinfo.IsWithinMount(cfg.Scan.RootPath),
		"mount_type":             mountinfo.FSType(cfg.Scan.RootPath),
		"mount_server":           mountinfo.Source(cfg.Scan.RootPath),
	}
}

func onOff(value bool) string {
	if value {
		return "开"
	}
	return "关"
}

func printConfig(w io.Writer, cfg *config.Config) {
	source := cfg.Source
	if source == "" {
		source = "内置默认值"
	}
	fmt.Fprintf(w, "配置来源：%s\n", source)
	fmt.Fprintf(w, "profile：%s\n", cfg.Profile)
	fmt.Fprintf(w, "扫描根：%s\n", cfg.Scan.RootPath)
	fmt.Fprintf(w, "数据库：%s\n", cfg.Database.Path)
	fmt.Fprintf(w, "精确全量间隔：%s；快速估算间隔：%s（抽样 %d%%）\n",
		formatInterval(cfg.Scan.Interval.Duration), formatInterval(cfg.Scan.QuickInterval.Duration),
		cfg.Scan.QuickSamplePercent)
	fmt.Fprintf(w, "目录索引：深度 %d，每层 top %d（0 表示关闭）；全目录索引：%s\n",
		cfg.Scan.IndexDepth, cfg.Scan.IndexTopK, onOff(cfg.Scan.IndexAllDirs))
	if cfg.Scan.QuickMaxFilesPerTop > 0 {
		fmt.Fprintf(w, "quick 单目录文件上限：%d（超出标记 partial）\n", cfg.Scan.QuickMaxFilesPerTop)
	} else {
		fmt.Fprintln(w, "quick 单目录文件上限：不限制")
	}
	fmt.Fprintf(w, "历史保留：%d 天；已删除目录保留：%d 天\n",
		cfg.Database.RetentionDays, cfg.Database.StaleRetentionDays)
	fmt.Fprintf(w, "并发：%s（一级目录间并行；大目录内部使用剩余并发）\n", cfg.Performance.Concurrency)
	fmt.Fprintf(w, "QPS：%d（自适应，起步约 %d）\n", cfg.Performance.MaxMetadataQPS, startQPS(cfg))
	fmt.Fprintf(w, "CPU：%d%%；内存：%s（%s）\n", cfg.Performance.MaxCPUPercent,
		config.FormatBytes(cfg.Performance.MemoryLimit.Resolve()), cfg.Performance.MemoryLimit.String())
	fmt.Fprintf(w, "主机保护：CPU %g%%，Load1 %g（0 表示关闭）\n",
		cfg.Performance.MaxHostCPUPercent, cfg.Performance.MaxLoad1)
	fmt.Fprintf(w, "慢元数据延迟：慢 %s，暂停 %s\n",
		cfg.Performance.SlowMetadataLatency.Duration, cfg.Performance.PauseMetadataLatency.Duration)
	mountType := mountinfo.FSType(cfg.Scan.RootPath)
	if mountType == "" {
		mountType = "未知（根路径未挂载或无 mountinfo）"
	}
	mountGuard := "关"
	if cfg.Scan.RequireMountPoint {
		mountGuard = "开"
	}
	mountOK := !cfg.Scan.RequireMountPoint || mountinfo.IsWithinMount(cfg.Scan.RootPath)
	fmt.Fprintf(w, "挂载保护：%s（require_mount_point=%t，判定=%s）\n",
		mountGuard, cfg.Scan.RequireMountPoint, map[bool]string{true: "通过", false: "失败"}[mountOK])
	fmt.Fprintf(w, "挂载类型：%s（%s）", mountType, fsClassText(mountinfo.ClassForPath(cfg.Scan.RootPath)))
	if server := mountinfo.Source(cfg.Scan.RootPath); server != "" {
		fmt.Fprintf(w, "，服务端：%s", server)
	}
	fmt.Fprintln(w)
}

func fsClassText(class string) string {
	switch class {
	case "local":
		return "本地文件系统"
	case "network":
		return "网络/共享文件系统"
	case "fuse":
		return "FUSE 文件系统"
	default:
		return "未知"
	}
}

func formatInterval(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	}
	return d.String()
}

func startQPS(cfg *config.Config) int {
	qps := cfg.Performance.MaxMetadataQPS
	if cfg.Performance.AdaptiveThrottle && qps >= 10 {
		qps /= 3
		if qps < cfg.Performance.MinMetadataQPS {
			qps = cfg.Performance.MinMetadataQPS
		}
	}
	return qps
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, `dirspace %s — 目录空间扫描与缓存服务

用法:
  dirspace [全局参数] [命令] [命令参数]

命令:
  （无命令）          等同于 top 10
  top <N>             显示空间排名前 N 个一级目录（默认 10）
  list                显示全部一级目录
  children [N]        显示目录索引中某个目录的直接子目录（--path 指定；N 可选）
  diff                对比最近两次同模式完成快照（--since 7d）
  size                查询一个目录的已缓存大小；--exact 未命中时精扫该目录
  fs-usage            查询整个挂载点的容量、已用和可用空间（statfs）
  merge               合并多个分片数据库（--db 目标库，--inputs 分片列表）
  partial             显示本轮已完成目录，未完成的沿用上次结果（--staging-only 只看本轮）
  status              显示缓存、任务、进度和数据库状态
  monitor             一屏查看进程/主机资源、生效配置和高低判断
  config explain      显示最终生效的配置和推导值
  scan                执行一次精确全量扫描（默认）
  quick               执行一次快速抽样估算（有误差，标记为 sampled）
  refresh             执行一次精确刷新（全量，保证准确性）
  rebuild [名称]      只重新统计一个一级目录
  daemon              后台定时调度（systemd 使用）
  pause               暂停当前扫描
  resume              恢复当前扫描
  cancel              取消当前扫描
  cleanup             清理历史统计（默认保留 %d 天）
  backup              备份 SQLite 缓存
  doctor [--repair]   检查或重建损坏的数据库
  version             显示版本
  help                显示帮助

全局参数（可放在命令前后）:
  --path <绝对路径>       查询/扫描/控制的目标路径，默认取配置 scan.root_path
  --config <文件>         配置文件路径
  --db <文件>             临时覆盖 SQLite 路径（不能位于扫描根目录或网络/FUSE 上）
  --json                  输出 JSON
  --logical               使用逻辑大小 st_size 展示/统计
  --full                  强制全量（scan/refresh）
  --new                   只扫描新增或失效的一级目录（scan，需要已有基线）
  --resume                断点续扫（scan）
  --staging-only          只显示本轮 staging，不与上次结果合并（partial）
  --exact                 未命中缓存时精确扫描目标目录作为独立根（size）
  --foreground            长任务在前台运行，不自动转后台
  --background            强制长任务转后台（脚本可用）
  --shard INDEX/COUNT     只扫描目标子树的一部分，结果需 merge（scan/refresh）
  --inputs <文件列表>     逗号分隔的分片数据库（merge）
  --once                  只运行一轮（daemon）
  --days <N>              清理早于 N 天的历史（cleanup）
  --since <范围>          diff 对比范围，例如 7d、24h（默认 7d）
  --vacuum                清理后 VACUUM（cleanup）
  --output <文件>         备份目标（backup）
  --repair                重建损坏的数据库（doctor）
  --log-level <级别>      debug/info/warn/error
  --help, -h              帮助
  --version, -v           版本

示例:
  dirspace
  dirspace top 20
  dirspace list --json
  dirspace children --path /data/cache-a 100
  dirspace diff --path /mnt --since 7d
  dirspace size --path /data/cache-a
  dirspace size --path /data/cache-a --exact
  dirspace monitor --path /mnt
  dirspace fs-usage --path /data
  dirspace merge --path /data --db /var/lib/dirspace/merged.db --inputs shard-0.db,shard-1.db
  dirspace partial --path /mnt
  dirspace partial --path /mnt --staging-only
  dirspace --path /data top 10
  dirspace scan --path /data
  dirspace scan --new --path /data
  dirspace quick --path /data
  dirspace refresh --path /data
  dirspace rebuild --path /data/cache-a
  dirspace rebuild cache-a --path /data
  dirspace pause --path /mnt
  dirspace backup --output /var/backups/dirspace.db

默认配置:
  profile=%s, exact_interval=%s, quick_interval=%s, retention=%d 天
  %s
  %s

重要:
  查询命令只读 SQLite，不访问扫描存储；扫描只读文件元数据，不读取文件内容。
`, Version, config.Default().Database.RetentionDays, config.DefaultProfile,
		formatInterval(config.Default().Scan.Interval.Duration),
		formatInterval(config.Default().Scan.QuickInterval.Duration),
		config.Default().Database.RetentionDays,
		config.DefaultConfigPath, config.DefaultDBPath)
}

// validateExplicitScanRoot applies the database-location guard to an explicit
// --path scan root, which is not covered by the configured roots in Validate.
func validateExplicitScanRoot(command, queryPath, explicitPath, dbPath string, args []string) error {
	if explicitPath == "" {
		return nil
	}
	switch command {
	case "scan", "quick", "refresh", "rebuild":
	default:
		return nil
	}
	root, err := config.NormalizeAbsolutePath(queryPath)
	if err != nil {
		return nil // Service will report the path error with its usual wording.
	}
	if command == "rebuild" && len(args) == 0 {
		// Without a name, --path is the target child and the scan root is its parent.
		root = filepath.Dir(root)
	}
	if config.PathWithin(root, dbPath) {
		return fmt.Errorf("数据库不能位于扫描根目录内: %s 位于 %s", dbPath, root)
	}
	return nil
}
