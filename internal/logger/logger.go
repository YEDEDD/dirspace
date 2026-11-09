// Package logger configures structured logs for journald.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New builds a text or JSON slog logger.
func New(level, format string, out io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("非法日志级别: %s", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "", "text":
		return slog.New(slog.NewTextHandler(out, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(out, opts)), nil
	default:
		return nil, fmt.Errorf("非法日志格式: %s", format)
	}
}
