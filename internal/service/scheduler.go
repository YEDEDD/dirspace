package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/storage"
)

// DaemonOptions configures the long-running scheduler.
type DaemonOptions struct {
	Once bool
}

// RunDaemon scans configured roots serially, using one shared low-load
// controller per scan. Serial execution is intentional: it prevents two roots
// from multiplying NFS metadata pressure.
func (s *Service) RunDaemon(ctx context.Context, opts DaemonOptions) error {
	roots := s.Cfg.AllRoots()
	if len(roots) == 0 {
		return errors.New("没有配置任何扫描根目录")
	}
	s.Recover(ctx)
	s.Log.Info("dirspace 后台调度启动", "roots", len(roots),
		"interval", s.Cfg.Scan.Interval.Duration.String(), "scan_mode", "full")

	if !opts.Once && s.Cfg.Scan.StartupDelay.Duration > 0 {
		s.Log.Info("等待启动延迟", "delay", s.Cfg.Scan.StartupDelay.Duration.String())
		if err := sleepContext(ctx, s.Cfg.Scan.StartupDelay.Duration); err != nil {
			return nil
		}
	}

	if opts.Once {
		var errs []error
		for _, root := range roots {
			if err := s.runScheduledRoot(ctx, root); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	next := make(map[string]time.Time, len(roots))
	for _, root := range roots {
		next[root] = s.nextRun(ctx, root)
		s.Log.Info("已安排扫描", "root", root, "next", next[root].Format(time.RFC3339))
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		now := time.Now()
		earliest := now.Add(24 * time.Hour)
		var due []string
		for _, root := range roots {
			when := next[root]
			if !when.After(now) {
				due = append(due, root)
			} else if when.Before(earliest) {
				earliest = when
			}
		}
		if len(due) > 0 {
			sort.Strings(due)
			for _, root := range due {
				if err := ctx.Err(); err != nil {
					return nil
				}
				err := s.runScheduledRoot(ctx, root)
				if err != nil && !errors.Is(err, context.Canceled) {
					s.Log.Error("定时扫描失败", "root", root, "error", err.Error())
				}
				next[root] = time.Now().Add(s.Cfg.Scan.Interval.Duration)
				s.Log.Info("下次扫描已安排", "root", root, "next", next[root].Format(time.RFC3339))
			}
			continue
		}
		if err := sleepContext(ctx, time.Until(earliest)); err != nil {
			return nil
		}
	}
}

func (s *Service) runScheduledRoot(ctx context.Context, root string) error {
	progress := func(p model.Progress) {
		state := ""
		if p.Paused {
			state = "（已暂停）"
		}
		s.Log.Info("扫描进度"+state, "root", p.RootPath, "current", p.CurrentDir,
			"done_dirs", p.DoneDirs, "total_dirs", p.TotalDirs,
			"files", p.Files, "bytes", p.Bytes, "elapsed", p.Elapsed.Round(time.Second).String())
	}
	summary, err := s.Scan(ctx, root, ScanOptions{
		Resume:         s.Cfg.Scan.ResumeInterrupted,
		ResumeCanceled: false, // a user cancel is not an abnormal exit
		Progress:       progress,
	})
	if err != nil {
		return fmt.Errorf("扫描 %s 失败: %w", root, err)
	}
	s.Log.Info("定时扫描完成", "root", root, "status", summary.DataStatus,
		"accuracy", summary.Accuracy, "dirs", summary.DoneDirs,
		"files", summary.FileCount, "bytes", summary.TotalAlloc,
		"duration", summary.Duration.Round(time.Millisecond).String())
	return nil
}

func (s *Service) nextRun(ctx context.Context, rootPath string) time.Time {
	now := time.Now()
	root, err := s.Store.GetRoot(ctx, rootPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			if s.Cfg.Scan.ScanOnStart {
				return now
			}
			return now.Add(s.Cfg.Scan.Interval.Duration)
		}
		return now.Add(s.Cfg.Scan.Interval.Duration)
	}
	if root.LastSuccessAt == nil {
		if s.Cfg.Scan.ScanOnStart {
			return now
		}
		return now.Add(s.Cfg.Scan.Interval.Duration)
	}
	next := root.LastSuccessAt.Add(s.Cfg.Scan.Interval.Duration)
	if !next.After(now) {
		return now
	}
	return next
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
