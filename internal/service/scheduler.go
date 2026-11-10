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
		"exact_interval", s.Cfg.Scan.Interval.Duration.String(),
		"quick_interval", s.Cfg.Scan.QuickInterval.Duration.String())

	if !opts.Once && s.Cfg.Scan.StartupDelay.Duration > 0 {
		s.Log.Info("等待启动延迟", "delay", s.Cfg.Scan.StartupDelay.Duration.String())
		if err := sleepContext(ctx, s.Cfg.Scan.StartupDelay.Duration); err != nil {
			return nil
		}
	}

	if opts.Once {
		var errs []error
		for _, root := range roots {
			if err := s.runScheduledRoot(ctx, root, model.ModeFull); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	next := make(map[string]scanSchedule, len(roots))
	for _, root := range roots {
		next[root] = s.nextSchedule(ctx, root)
		s.Log.Info("已安排扫描", "root", root,
			"next_exact", next[root].exact.Format(time.RFC3339),
			"next_quick", next[root].quick.Format(time.RFC3339))
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		now := time.Now()
		earliest := now.Add(24 * time.Hour)
		var exactDue, quickDue []string
		for _, root := range roots {
			sc := next[root]
			if !sc.quick.IsZero() && !sc.quick.After(now) {
				quickDue = append(quickDue, root)
			}
			if !sc.exact.After(now) {
				exactDue = append(exactDue, root)
			}
			for _, when := range []time.Time{sc.exact, sc.quick} {
				if when.IsZero() {
					continue
				}
				if when.Before(earliest) {
					earliest = when
				}
			}
		}
		// Quick mode is cheap and gives users a fresh approximate view first.
		// Exact scans still run immediately afterwards when they are due.
		if len(quickDue) > 0 {
			sort.Strings(quickDue)
			for _, root := range quickDue {
				if err := ctx.Err(); err != nil {
					return nil
				}
				err := s.runScheduledRoot(ctx, root, model.ModeQuick)
				if err != nil && !errors.Is(err, context.Canceled) {
					s.Log.Error("快速估算失败", "root", root, "error", err.Error())
				}
				sc := next[root]
				sc.quick = time.Now().Add(s.Cfg.Scan.QuickInterval.Duration)
				next[root] = sc
				s.Log.Info("下次快速估算已安排", "root", root, "next", sc.quick.Format(time.RFC3339))
			}
			continue
		}
		if len(exactDue) > 0 {
			sort.Strings(exactDue)
			for _, root := range exactDue {
				if err := ctx.Err(); err != nil {
					return nil
				}
				err := s.runScheduledRoot(ctx, root, model.ModeFull)
				if err != nil && !errors.Is(err, context.Canceled) {
					s.Log.Error("定时扫描失败", "root", root, "error", err.Error())
				}
				sc := next[root]
				sc.exact = time.Now().Add(s.Cfg.Scan.Interval.Duration)
				if s.Cfg.Scan.QuickInterval.Duration > 0 {
					sc.quick = time.Now().Add(s.Cfg.Scan.QuickInterval.Duration)
				}
				next[root] = sc
				s.Log.Info("下次扫描已安排", "root", root,
					"next_exact", sc.exact.Format(time.RFC3339),
					"next_quick", sc.quick.Format(time.RFC3339))
			}
			continue
		}
		if err := sleepContext(ctx, time.Until(earliest)); err != nil {
			return nil
		}
	}
}

func (s *Service) runScheduledRoot(ctx context.Context, root string, mode model.ScanMode) error {
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
		Mode: mode,
		// Both quick and exact staging are durable. A daemon restart must
		// resume the unfinished run of this mode instead of starting a fresh
		// quick that would delete every completed first-level row.
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
	// Keep the local database bounded without requiring a second timer. The
	// cleanup only removes history/old runs; current cache rows are untouched.
	if history, runs, cleanupErr := s.CleanupHistory(ctx, 0, false); cleanupErr != nil {
		s.Log.Warn("自动清理历史失败", "error", cleanupErr.Error())
	} else if history > 0 || runs > 0 {
		s.Log.Info("已自动清理历史", "history_rows", history, "runs", runs)
	}
	return nil
}

type scanSchedule struct {
	exact time.Time
	quick time.Time
}

func (s *Service) nextSchedule(ctx context.Context, rootPath string) scanSchedule {
	var sc scanSchedule
	now := time.Now()
	root, err := s.Store.GetRoot(ctx, rootPath)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			s.Log.Warn("读取扫描根状态失败", "root", rootPath, "error", err.Error())
		}
		if s.Cfg.Scan.ScanOnStart {
			sc.exact, sc.quick = now, now
		} else {
			sc.exact = now.Add(s.Cfg.Scan.Interval.Duration)
			if s.Cfg.Scan.QuickInterval.Duration > 0 {
				sc.quick = now.Add(s.Cfg.Scan.QuickInterval.Duration)
			}
		}
		return sc
	}
	// Resume the newest unfinished run first. Older interrupted runs must not
	// preempt a newer quick run just because they are exact: with quick resume
	// implemented, continuing the newest work preserves the most staging and
	// avoids starting an exact scan that would invalidate that staging.
	quickInterrupted := false
	if latest, runErr := s.Store.LastRun(ctx, root.ID); runErr == nil && latest != nil {
		switch latest.Status {
		case model.RunInterrupted, model.RunRunning, model.RunPaused:
			if latest.Mode == model.ModeFull {
				sc.exact = now
				if s.Cfg.Scan.QuickInterval.Duration > 0 {
					sc.quick = now.Add(s.Cfg.Scan.QuickInterval.Duration)
				}
				return sc
			}
			if latest.Mode == model.ModeQuick {
				quickInterrupted = true
			}
		}
	}
	if root.LastSuccessAt == nil && s.Cfg.Scan.ScanOnStart {
		sc.exact, sc.quick = now, now
	} else {
		sc.exact = root.LastSuccessAt.Add(s.Cfg.Scan.Interval.Duration)
		if sc.exact.Before(now) {
			sc.exact = now
		}
		if s.Cfg.Scan.QuickInterval.Duration > 0 {
			sc.quick = root.LastSuccessAt.Add(s.Cfg.Scan.QuickInterval.Duration)
			if sc.quick.Before(now) {
				sc.quick = now
			}
		}
	}
	// Use the last exact run, not the newest quick run, to schedule the weekly
	// exact refresh.
	exactSeen := false
	if run, runErr := s.Store.LastRunByMode(ctx, root.ID, model.ModeFull); runErr == nil && run.FinishedAt != nil {
		exactSeen = true
		sc.exact = run.FinishedAt.Add(s.Cfg.Scan.Interval.Duration)
		if sc.exact.Before(now) {
			sc.exact = now
		}
	}
	if !exactSeen {
		// A quick-only baseline is useful immediately, but it is not the exact
		// weekly baseline. Run the exact pass on the next daemon start.
		sc.exact = now
	}
	if s.Cfg.Scan.QuickInterval.Duration > 0 {
		if run, runErr := s.Store.LastRunByMode(ctx, root.ID, model.ModeQuick); runErr == nil && run.FinishedAt != nil {
			sc.quick = run.FinishedAt.Add(s.Cfg.Scan.QuickInterval.Duration)
			if sc.quick.Before(now) {
				sc.quick = now
			}
		}
	}
	// An interrupted quick run must be resumed promptly, even when the next
	// exact scan is still in the future. The resume path reuses completed
	// first-level rows and only scans the directory that was interrupted.
	// This must come after the normal quick interval calculation because an
	// interrupted run also has finished_at set.
	if quickInterrupted && s.Cfg.Scan.QuickInterval.Duration > 0 {
		sc.quick = now
	}
	return sc
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
