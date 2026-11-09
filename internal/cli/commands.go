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
	"github.com/YEDEDD/dirspace/internal/platform"
	"github.com/YEDEDD/dirspace/internal/service"
	"github.com/YEDEDD/dirspace/internal/storage"
)

// Version is overridden with -ldflags at release time.
var Version = "1.0.0"

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
	case "list", "status", "scan", "daemon", "refresh", "pause", "resume", "cancel",
		"cleanup", "backup", "doctor":
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
	default:
		return command{}, &usageError{fmt.Sprintf("未知命令: %s", name)}
	}
}

func validateCommandOptions(cmd command, opts *options) error {
	allowOnce := cmd.name == "daemon"
	allowRepair := cmd.name == "doctor"
	allowVacuum := cmd.name == "cleanup"
	allowDays := cmd.name == "cleanup"
	allowOutput := cmd.name == "backup"
	allowFull := cmd.name == "refresh" || cmd.name == "scan"
	allowResume := cmd.name == "scan"
	allowPath := cmd.name == "top" || cmd.name == "list" || cmd.name == "status" ||
		cmd.name == "scan" || cmd.name == "refresh" || cmd.name == "rebuild" ||
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
	if opts.output != "" && !allowOutput {
		return &usageError{"--output 仅用于 backup"}
	}
	if opts.full && !allowFull {
		return &usageError{"--full 仅用于 scan 或 refresh"}
	}
	if opts.resume && !allowResume {
		return &usageError{"--resume 仅用于 scan"}
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

	case "scan", "refresh":
		setLowPriority(cfg, log)
		progress := progressPrinter(stderr)
		scanOpts := service.ScanOptions{
			Resume:         opts.resume,
			ResumeCanceled: opts.resume,
			Progress:       progress,
		}
		if opts.full {
			scanOpts.Mode = model.ModeFull
		}
		fmt.Fprintf(stderr, "开始扫描 %s（并发 %d，元数据上限 %d QPS）...\n",
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
	if cfg.Performance.Concurrency < runtime.NumCPU() {
		runtime.GOMAXPROCS(cfg.Performance.Concurrency)
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, `dirspace %s — 目录空间扫描与缓存服务

用法:
  dirspace [全局参数] [命令] [命令参数]

命令:
  （无命令）          等同于 top 10
  top <N>             显示空间排名前 N 个一级目录（默认 10）
  list                显示全部一级目录
  status              显示缓存、任务、进度和数据库状态
  scan                执行一次扫描并建立基线（默认全量）
  refresh             执行一次刷新（当前版本为全量，保证准确性）
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
  --db <文件>             临时覆盖 SQLite 路径（不能位于扫描根目录或 NFS 上）
  --json                  输出 JSON
  --logical               使用逻辑大小 st_size 展示/统计
  --full                  强制全量（scan/refresh）
  --resume                断点续扫（scan）
  --once                  只运行一轮（daemon）
  --days <N>              清理早于 N 天的历史（cleanup）
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
  dirspace --path /data top 10
  dirspace scan --path /data
  dirspace refresh --path /data
  dirspace rebuild --path /data/cae365-test
  dirspace rebuild cae365-test --path /mnt
  dirspace pause --path /mnt
  dirspace backup --output /var/backups/dirspace.db

默认配置:
  %s
  %s

重要:
  查询命令只读 SQLite，不访问 NFS；扫描只读文件元数据，不读取文件内容。
`, Version, config.Default().Database.RetentionDays, config.DefaultConfigPath, config.DefaultDBPath)
}

// validateExplicitScanRoot applies the database-location guard to an explicit
// --path scan root, which is not covered by the configured roots in Validate.
func validateExplicitScanRoot(command, queryPath, explicitPath, dbPath string, args []string) error {
	if explicitPath == "" {
		return nil
	}
	switch command {
	case "scan", "refresh", "rebuild":
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
