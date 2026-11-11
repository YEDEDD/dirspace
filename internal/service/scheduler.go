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
	sort.Strings(roots)
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
			if err := s.runScheduledRoot(ctx, root, model.ModeFull, time.Time{}); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	// Schedules are deliberately reconstructed from the database on every
	// wake-up instead of being cached in memory. A manual CLI scan, or another
	// daemon, can complete between two ticks; caching `next` would then run a
	// duplicate scan. Reconcile first, then acquire the shared filesystem lock,
	// then reconcile again after taking the lock.
	retryAt := make(map[string]time.Time)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		s.Recover(ctx)
		now := time.Now()
		due, active, earliest, err := s.reconcileSchedule(ctx, roots, now)
		if err != nil {
			s.Log.Warn("重新计算扫描计划失败", "error", err.Error())
			if err := sleepContext(ctx, daemonPollInterval); err != nil {
				return nil
			}
			continue
		}
		if active {
			if err := sleepContext(ctx, daemonPollInterval); err != nil {
				return nil
			}
			continue
		}
		if due == nil {
			wait := time.Until(earliest)
			if wait > daemonIdlePoll {
				wait = daemonIdlePoll
			}
			if wait < daemonPollInterval {
				wait = daemonPollInterval
			}
			if err := sleepContext(ctx, wait); err != nil {
				return nil
			}
			continue
		}
		if retry, ok := retryAt[due.root]; ok && retry.After(now) {
			if err := sleepContext(ctx, time.Until(retry)); err != nil {
				return nil
			}
			continue
		}
		if due.mode == model.ModeFull {
			s.Log.Info("精确扫描到期，等待文件系统锁", "root", due.root)
		} else {
			s.Log.Info("快速估算到期，等待文件系统锁", "root", due.root)
		}
		// The scan itself takes the same lock. This short probe only tells us
		// whether another process is currently scanning this filesystem; if it
		// is, leave the due time unchanged and retry shortly.
		probe, lockErr := acquireScanLock(due.root)
		if lockErr != nil {
			if errors.Is(lockErr, ErrRootLocked) {
				if err := sleepContext(ctx, daemonPollInterval); err != nil {
					return nil
				}
				continue
			}
			s.Log.Warn("检查扫描锁失败", "root", due.root, "error", lockErr.Error())
			retryAt[due.root] = time.Now().Add(daemonRetryInterval)
			continue
		}
		_ = probe.Close()

		// Re-read after the probe: a manual scan may have completed while we
		// were deciding, in which case the due time is now in the future.
		recheck, active, _, err := s.reconcileSchedule(ctx, roots, time.Now())
		if err != nil || active || recheck == nil || recheck.root != due.root || recheck.mode != due.mode {
			if err != nil {
				s.Log.Warn("扫描前复查失败", "error", err.Error())
			}
			if err := sleepContext(ctx, daemonPollInterval); err != nil {
				return nil
			}
			continue
		}
		startedAt := time.Now()
		err = s.runScheduledRoot(ctx, due.root, due.mode, startedAt)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if errors.Is(err, model.ErrCanceled) {
				// A user cancel is not an abnormal exit. Do not immediately
				// restart the same scan; wait for the next regular interval.
				wait := s.Cfg.Scan.QuickInterval.Duration
				if due.mode == model.ModeFull {
					wait = s.Cfg.Scan.Interval.Duration
				}
				if wait <= 0 {
					wait = daemonRetryInterval
				}
				retryAt[due.root] = time.Now().Add(wait)
				continue
			}
			if errors.Is(err, ErrRootLocked) {
				// Another process won the lock between the probe and Scan.
				if err := sleepContext(ctx, daemonPollInterval); err != nil {
					return nil
				}
				continue
			}
			if errors.Is(err, ErrScanSuperseded) {
				continue
			}
			s.Log.Error("定时扫描失败", "root", due.root, "error", err.Error())
			retryAt[due.root] = time.Now().Add(daemonRetryInterval)
			continue
		}
		delete(retryAt, due.root)
	}
}

const (
	daemonPollInterval  = 15 * time.Second
	daemonIdlePoll      = 5 * time.Minute
	daemonRetryInterval = 5 * time.Minute
)

type dueScan struct {
	root string
	mode model.ScanMode
}

// reconcileSchedule derives all due work from the database. It never trusts a
// cached in-memory clock, so external scans are observed at the next tick.
func (s *Service) reconcileSchedule(ctx context.Context, roots []string, now time.Time) (due *dueScan, active bool, earliest time.Time, err error) {
	earliest = now.Add(24 * time.Hour)
	var exactRoot, quickRoot string
	for _, root := range roots {
		rootState, getErr := s.Store.GetRoot(ctx, root)
		if getErr == nil {
			if run, activeErr := s.Store.ActiveRun(ctx, rootState.ID); activeErr == nil && run != nil {
				active = true
			}
		}
		sc := s.nextSchedule(ctx, root)
		if !sc.exact.After(now) && exactRoot == "" {
			exactRoot = root
		}
		if !sc.quick.IsZero() && !sc.quick.After(now) && quickRoot == "" {
			quickRoot = root
		}
		for _, when := range []time.Time{sc.exact, sc.quick} {
			if when.IsZero() || when.Before(now) {
				continue
			}
			if when.Before(earliest) {
				earliest = when
			}
		}
	}
	if active {
		return nil, true, earliest, nil
	}
	// Exact is a superset of quick. If both are due, run exact and let the next
	// reconcile derive a fresh quick interval from the exact completion.
	if exactRoot != "" {
		return &dueScan{root: exactRoot, mode: model.ModeFull}, false, earliest, nil
	}
	if quickRoot != "" {
		return &dueScan{root: quickRoot, mode: model.ModeQuick}, false, earliest, nil
	}
	return nil, false, earliest, nil
}

func (s *Service) runScheduledRoot(ctx context.Context, root string, mode model.ScanMode, skipIfScannedAfter time.Time) error {
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
		Resume:             s.Cfg.Scan.ResumeInterrupted,
		ResumeCanceled:     false, // a user cancel is not an abnormal exit
		Progress:           progress,
		SkipIfScannedAfter: skipIfScannedAfter,
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
	if root.LastSuccessAt == nil {
		if s.Cfg.Scan.ScanOnStart {
			sc.exact, sc.quick = now, now
		} else {
			sc.exact = now.Add(s.Cfg.Scan.Interval.Duration)
			if s.Cfg.Scan.QuickInterval.Duration > 0 {
				sc.quick = now.Add(s.Cfg.Scan.QuickInterval.Duration)
			}
		}
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
	if run, runErr := s.Store.LastRunByMode(ctx, root.ID, model.ModeFull); runErr == nil && run.FinishedAt != nil &&
		(run.Status == model.RunCompleted || run.Status == model.RunPartial) {
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
