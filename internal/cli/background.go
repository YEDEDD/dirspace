package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/platform"
	"github.com/mattn/go-isatty"
)

// maybeDetach turns interactive long-running scans into a detached child. The
// default remains foreground when stdout is not a terminal, so scripts and
// tests keep the old synchronous behavior.
func maybeDetach(args []string, cmd command, opts *options, cfg *config.Config, stdout, stderr io.Writer) (bool, int) {
	if opts.foreground || opts.json {
		return false, 0
	}
	long := cmd.name == "quick" || cmd.name == "scan" || cmd.name == "refresh" || cmd.name == "rebuild" ||
		(cmd.name == "size" && opts.exact)
	if !long {
		return false, 0
	}
	if !opts.background && !terminalWriter(stdout) {
		return false, 0
	}
	if opts.background && opts.json {
		return false, 0
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "无法定位 dirspace 可执行文件: %v\n", err)
		return true, exitRuntime
	}
	logDir := filepath.Join(filepath.Dir(cfg.Database.Path), "manual")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		fmt.Fprintf(stderr, "创建后台日志目录失败: %v\n", err)
		return true, exitRuntime
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s.log", cmd.name, time.Now().Format("20060102-150405")))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		fmt.Fprintf(stderr, "创建后台日志失败: %v\n", err)
		return true, exitRuntime
	}
	childArgs := detachArgs(args)
	child := exec.Command(exe, childArgs...)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = os.Environ()
	platform.Detach(child)
	if err := child.Start(); err != nil {
		logFile.Close()
		fmt.Fprintf(stderr, "启动后台扫描失败: %v\n", err)
		return true, exitRuntime
	}
	pid := child.Process.Pid
	_ = child.Process.Release()
	_ = logFile.Close()
	fmt.Fprintf(stdout, "已转入后台运行（PID %d）\n", pid)
	fmt.Fprintf(stdout, "日志：%s\n", logPath)
	fmt.Fprintf(stdout, "查看进度：dirspace status --path %s\n", scanPathForMessage(opts))
	return true, exitOK
}

func terminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

func detachArgs(args []string) []string {
	out := make([]string, 0, len(args)+1)
	for _, arg := range args {
		if arg == "--background" || strings.HasPrefix(arg, "--background=") {
			continue
		}
		out = append(out, arg)
	}
	return append(out, "--foreground")
}

func scanPathForMessage(opts *options) string {
	if opts.path == "" {
		return "/mnt"
	}
	return opts.path
}
