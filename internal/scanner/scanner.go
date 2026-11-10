// Package scanner walks NFS directory trees with strict metadata throttling.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/mountinfo"
	"github.com/YEDEDD/dirspace/internal/performance"
	"github.com/YEDEDD/dirspace/internal/platform"
	"github.com/YEDEDD/dirspace/internal/storage"
)

// Scanner orchestrates baseline, incremental, and single-directory scans.
type Scanner struct {
	store *storage.Store
	cfg   *config.Config
	log   *slog.Logger
}

func New(store *storage.Store, cfg *config.Config, log *slog.Logger) *Scanner {
	if log == nil {
		log = slog.Default()
	}
	return &Scanner{store: store, cfg: cfg, log: log}
}

// Request describes one scan operation. RootPath is always the cache root.
// TargetDir, when set, rescans only that immediate child and updates it without
// touching sibling rows.
type Request struct {
	RootPath       string
	TargetDir      string
	Mode           model.ScanMode
	Resume         bool
	ResumeCanceled bool
	Progress       func(model.Progress)
}

type topEntry struct {
	name string
}

type scanControl struct {
	paused   atomic.Bool
	canceled atomic.Bool
	runLost  atomic.Bool
}

type progressCounters struct {
	files   atomic.Int64
	alloc   atomic.Int64
	logical atomic.Int64
	done    atomic.Int64
	current atomic.Value // string
}

func (c *progressCounters) setCurrent(name string) { c.current.Store(name) }
func (c *progressCounters) getCurrent() string {
	value, _ := c.current.Load().(string)
	return value
}

type scanSession struct {
	rootPath     string
	runID        int64
	rootID       int64
	single       bool
	controller   *performance.Controller
	hardlinks    *hardlinkRegistry
	inventory    *inventoryWriter
	counters     *progressCounters
	control      *scanControl
	intraWorkers int
}

type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

func canceled(c *scanControl, cause error) error {
	if c != nil && (c.canceled.Load() || errors.Is(cause, model.ErrCanceled)) {
		return model.ErrCanceled
	}
	return cause
}

func fatal(err error) error {
	if err == nil {
		return nil
	}
	return &fatalError{err: err}
}

func (sess *scanSession) wait(ctx context.Context) error {
	for {
		if sess.control.canceled.Load() {
			return model.ErrCanceled
		}
		if sess.control.paused.Load() {
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		return sess.controller.Wait(ctx)
	}
}

// Scan runs one scan. It returns a summary even when the run is canceled or
// fails; the caller should inspect both summary and error.
func (s *Scanner) Scan(ctx context.Context, req Request) (*model.ScanSummary, error) {
	rootPath, err := config.NormalizeAbsolutePath(req.RootPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("扫描根目录不可用: %s: %w", rootPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("扫描根目录不能是符号链接: %s", rootPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("扫描根目录不是目录: %s", rootPath)
	}
	if s.cfg.Scan.RequireMountPoint && !mountinfo.IsMountPoint(rootPath) {
		return nil, fmt.Errorf("扫描根不是挂载点: %s（挂载未就绪，或请关闭 scan.require_mount_point）", rootPath)
	}
	if req.TargetDir != "" {
		if err := validateDirName(req.TargetDir); err != nil {
			return nil, err
		}
	}
	if req.TargetDir != "" {
		req.Mode = model.ModeRebuild
	}
	if req.Mode == "" {
		req.Mode = model.ModeFull
	}
	basis := model.BasisAllocated
	if strings.EqualFold(s.cfg.Scan.SizeBasis, "logical") {
		basis = model.BasisLogical
	}

	run, err := s.store.StartRun(ctx, rootPath, storage.StartOptions{
		Mode:           req.Mode,
		SizeBasis:      basis,
		Resume:         req.Resume,
		ResumeCanceled: req.ResumeCanceled,
	})
	if err != nil {
		return nil, fmt.Errorf("创建扫描任务失败: %w", err)
	}
	started := time.Now()
	if run.StartedAt.Before(started) {
		started = run.StartedAt
	}
	if run.Resumed {
		// Root-direct files are cheap enough to rescan on resume and may have
		// changed while the process was stopped. Drop their old claims so the
		// fresh pass is not forced to skip them.
		if err := s.store.DeleteHardlinkTopDir(ctx, run.ID, "."); err != nil {
			s.abort(run, model.RunFailed, "清理断点根目录硬链接状态失败: "+err.Error())
			return nil, err
		}
		if err := s.store.DeleteIncompleteHardlinks(ctx, run.ID); err != nil {
			s.abort(run, model.RunFailed, "清理断点硬链接状态失败: "+err.Error())
			return nil, err
		}
		s.log.Info("恢复中断的扫描任务", "run_id", run.ID, "root", rootPath)
	}

	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()
	control := &scanControl{}
	counters := &progressCounters{}
	counters.current.Store("")

	controller := performance.NewController(performance.Limits{
		BaseQPS:          s.cfg.Performance.MaxMetadataQPS,
		MinQPS:           s.cfg.Performance.MinMetadataQPS,
		MaxCPUPercent:    s.cfg.Performance.MaxCPUPercent,
		MemoryLimit:      s.cfg.Performance.MemoryLimit.Bytes,
		Adaptive:         s.cfg.Performance.AdaptiveThrottle,
		SampleInterval:   s.cfg.Performance.SampleInterval.Duration,
		MaxHostCPU:       s.cfg.Performance.MaxHostCPUPercent,
		MaxLoad1:         s.cfg.Performance.MaxLoad1,
		SlowLatency:      s.cfg.Performance.NFSSlowLatency.Duration,
		PauseLatency:     s.cfg.Performance.NFSPauseLatency.Duration,
		PauseCooldown:    s.cfg.Performance.NFSPauseCooldown.Duration,
		MaxMetadataError: s.cfg.Performance.MaxMetadataErrors,
	}, s.log)
	controller.Start(scanCtx)

	oldProcs := runtime.GOMAXPROCS(0)
	procs := s.cfg.Performance.Concurrency.Limit()
	if procs < 1 {
		procs = 1
	}
	runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(oldProcs)

	monitorCtx, stopMonitor := context.WithCancel(scanCtx)
	defer stopMonitor()
	go s.monitorControl(monitorCtx, run, control, cancelScan)

	entries, err := s.discover(scanCtx, rootPath, req.TargetDir, controller, control)
	if err != nil {
		summary := s.finishAbort(run, req, control, err)
		return summary, canceled(control, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	completed := map[string]model.DirOutcome{}
	if run.Resumed {
		completed, err = s.store.CompletedDirs(scanCtx, run.ID)
		if err != nil {
			summary := s.finishAbort(run, req, control, err)
			return summary, canceled(control, err)
		}
		current := make(map[string]bool, len(entries))
		for _, entry := range entries {
			current[entry.name] = true
		}
		for name := range completed {
			if current[name] {
				continue
			}
			// The directory was staged before the interruption but is gone now;
			// drop its staging and hardlink claims so CompleteRun marks the old
			// published row stale instead of promoting obsolete data.
			if err := s.store.DeleteDirResult(scanCtx, run.ID, name); err != nil {
				summary := s.finishAbort(run, req, control, err)
				return summary, canceled(control, err)
			}
			if err := s.store.DeleteHardlinkTopDir(scanCtx, run.ID, name); err != nil {
				summary := s.finishAbort(run, req, control, err)
				return summary, canceled(control, err)
			}
			delete(completed, name)
		}
	}
	for _, out := range completed {
		counters.done.Add(1)
		counters.files.Add(out.FileCount)
		counters.alloc.Add(out.AllocBytes)
		counters.logical.Add(out.LogicalBytes)
	}
	pending := make([]topEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := completed[entry.name]; ok {
			continue
		}
		pending = append(pending, entry)
	}
	if err := s.store.SetTotalDirs(scanCtx, run.ID, len(entries)); err != nil {
		summary := s.finishAbort(run, req, control, err)
		return summary, canceled(control, err)
	}
	run.TotalDirs = len(entries)

	var inv *inventoryWriter
	if s.cfg.Scan.Inventory.Enabled {
		inv = newInventoryWriter(scanCtx, cancelScan, s.store, run.ID, run.RootID,
			s.cfg.Scan.Inventory.BatchSize, s.log)
	}
	sess := &scanSession{
		rootPath:   rootPath,
		runID:      run.ID,
		rootID:     run.RootID,
		single:     req.TargetDir != "",
		controller: controller,
		hardlinks:  &hardlinkRegistry{store: s.store, runID: run.ID},
		inventory:  inv,
		counters:   counters,
		control:    control,
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(scanCtx)
	defer stopHeartbeat()
	go s.heartbeatLoop(heartbeatCtx, run, req, counters, control, started, cancelScan)

	// Files and the directory block directly under the scan root belong to the
	// root aggregate, not to any first-level child. They are captured before
	// workers start so hard-link attribution is deterministic.
	if req.TargetDir == "" {
		selfOut, selfErr := s.scanRootSelf(scanCtx, sess)
		if selfErr != nil {
			summary := s.finishAbort(run, req, control, selfErr)
			return summary, canceled(control, selfErr)
		}
		if err := s.store.SaveRootSelf(scanCtx, run.ID, selfOut); err != nil {
			summary := s.finishAbort(run, req, control, err)
			return summary, canceled(control, err)
		}
	}

	jobs := make(chan topEntry)
	var wg sync.WaitGroup
	var fatalMu sync.Mutex
	var fatalErr error
	setFatal := func(err error) {
		if err == nil {
			return
		}
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
		cancelScan()
	}
	workerCount := s.cfg.Performance.Concurrency.Effective(len(pending))
	if workerCount > len(pending) && len(pending) > 0 {
		workerCount = len(pending)
	}
	if workerCount < 1 {
		workerCount = 1
	}
	intraLimit := s.cfg.Performance.Concurrency.Limit()
	intraWorkers := intraLimit / workerCount
	if intraWorkers < 1 {
		intraWorkers = 1
	}
	sess.intraWorkers = intraWorkers
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range jobs {
				if scanCtx.Err() != nil {
					return
				}
				counters.setCurrent(entry.name)
				out, scanErr := s.scanTop(scanCtx, sess, entry.name)
				out.Name = entry.name
				if scanErr != nil {
					var fe *fatalError
					if errors.As(scanErr, &fe) {
						setFatal(scanErr)
						return
					}
					if errors.Is(scanErr, model.ErrCanceled) || scanCtx.Err() != nil {
						return
					}
					out.Err = scanErr
				}
				if out.CompletedAt.IsZero() {
					out.CompletedAt = time.Now()
				}
				if err := s.store.SaveDirResult(scanCtx, run.ID, out); err != nil {
					setFatal(fatal(fmt.Errorf("保存目录 %s 结果失败: %w", entry.name, err)))
					return
				}
				counters.done.Add(1)
				if out.Err != nil {
					s.log.Warn("目录统计失败，保留上次有效数据",
						"dir", entry.name, "error", out.Err.Error())
				} else {
					s.log.Debug("目录统计完成", "dir", entry.name,
						"alloc_bytes", out.AllocBytes, "files", out.FileCount,
						"duration", out.Duration.Round(time.Millisecond).String())
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, entry := range pending {
			select {
			case jobs <- entry:
			case <-scanCtx.Done():
				return
			}
		}
	}()
	wg.Wait()

	if inv != nil {
		if err := inv.Close(scanCtx); err != nil && !control.canceled.Load() {
			setFatal(err)
		}
	}
	stopHeartbeat()
	stopMonitor()

	fatalMu.Lock()
	runErr := fatalErr
	fatalMu.Unlock()

	if control.canceled.Load() {
		summary := s.finishAbort(run, req, control, model.ErrCanceled)
		return summary, canceled(control, model.ErrCanceled)
	}
	if control.runLost.Load() {
		err := errors.New("扫描任务状态丢失或被其他进程终止")
		summary := s.finishAbort(run, req, control, err)
		return summary, canceled(control, err)
	}
	if runErr != nil {
		summary := s.finishAbort(run, req, control, runErr)
		return summary, canceled(control, runErr)
	}
	if ctx.Err() != nil {
		summary := s.finishAbort(run, req, control, fmt.Errorf("扫描被中断: %w", ctx.Err()))
		return summary, canceled(control, ctx.Err())
	}

	summary, err := s.store.CompleteRun(ctx, run.ID, req.TargetDir != "", func() bool {
		return control.canceled.Load()
	})
	if err != nil {
		summary := s.finishAbort(run, req, control, fmt.Errorf("提交统计结果失败: %w", err))
		return summary, canceled(control, err)
	}
	if req.TargetDir != "" {
		if targetErr := s.applyRebuildSummary(ctx, summary, run.RootID, req.TargetDir); targetErr != nil {
			s.log.Info("单目录重算失败", "root", rootPath, "dir", req.TargetDir,
				"error", targetErr.Error())
			return summary, targetErr
		}
	}
	s.log.Info("扫描完成", "root", rootPath, "status", summary.DataStatus,
		"accuracy", summary.Accuracy, "dirs", summary.TotalDirs,
		"failed", summary.FailedDirs, "bytes", summary.TotalAlloc,
		"files", summary.FileCount, "duration", summary.Duration.Round(time.Millisecond).String())
	return summary, nil
}

// applyRebuildSummary reports the rebuilt directory rather than the root
// aggregate, which may include sibling and root-direct data.
func (s *Scanner) applyRebuildSummary(ctx context.Context, summary *model.ScanSummary, rootID int64, name string) error {
	target, err := s.store.GetDir(ctx, rootID, name)
	if err != nil {
		return fmt.Errorf("读取重算结果失败: %w", err)
	}
	summary.TotalAlloc = target.AllocBytes
	summary.TotalLogical = target.LogicalBytes
	summary.FileCount = target.FileCount
	summary.DirCount = target.DirCount
	summary.DataStatus = target.DataStatus
	summary.Accuracy = target.Accuracy
	summary.TotalDirs = 1
	summary.DoneDirs = 1
	summary.FailedDirs = 0
	if target.DataStatus == model.DataFailed || target.DataStatus == model.DataStale {
		summary.Accuracy = model.AccuracyApproximate
		summary.FailedDirs = 1
		if target.LastError != "" {
			summary.Errors = []string{target.Name + ": " + target.LastError}
			return fmt.Errorf("目录 %s 重算未成功: %s", target.Name, target.LastError)
		}
		return fmt.Errorf("目录 %s 重算未成功", target.Name)
	}
	return nil
}

func (s *Scanner) discover(ctx context.Context, rootPath, target string, controller *performance.Controller, control *scanControl) ([]topEntry, error) {
	if target != "" {
		if err := validateDirName(target); err != nil {
			return nil, err
		}
		full := filepath.Join(rootPath, target)
		if err := (&scanSession{controller: controller, control: control}).wait(ctx); err != nil {
			return nil, err
		}
		start := time.Now()
		info, err := os.Lstat(full)
		controller.ObserveMetadata(time.Since(start), err)
		if err != nil {
			return nil, fmt.Errorf("目录不可用: %s: %w", full, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("不扫描符号链接: %s", full)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("不是目录: %s", full)
		}
		return []topEntry{{name: target}}, nil
	}

	f, err := platform.OpenDir(rootPath)
	if err != nil {
		return nil, fmt.Errorf("打开扫描根目录失败: %s: %w", rootPath, err)
	}
	defer f.Close()
	var entries []topEntry
	for {
		if err := (&scanSession{controller: controller, control: control}).wait(ctx); err != nil {
			return nil, err
		}
		start := time.Now()
		batch, readErr := f.ReadDir(512)
		controller.ObserveMetadata(time.Since(start), readErr)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("读取扫描根目录失败: %w", readErr)
		}
		for _, entry := range batch {
			typ := entry.Type()
			if typ&os.ModeSymlink != 0 {
				continue
			}
			if typ.IsDir() {
				entries = append(entries, topEntry{name: entry.Name()})
				continue
			}
			if typ != 0 {
				continue
			}
			// d_type may be DT_UNKNOWN on some NFS mounts; lstat to classify.
			if err := (&scanSession{controller: controller, control: control}).wait(ctx); err != nil {
				return nil, err
			}
			statStart := time.Now()
			info, statErr := os.Lstat(filepath.Join(rootPath, entry.Name()))
			controller.ObserveMetadata(time.Since(statStart), statErr)
			if statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("读取目录项 %s 失败: %w", entry.Name(), statErr)
			}
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				entries = append(entries, topEntry{name: entry.Name()})
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return entries, nil
}

func (s *Scanner) monitorControl(ctx context.Context, run *model.Run, control *scanControl, cancel context.CancelFunc) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	paused := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			state, err := s.store.CheckControl(ctx, run.RootID)
			if err != nil {
				s.log.Warn("读取扫描控制状态失败", "error", err.Error())
				continue
			}
			switch state {
			case model.ControlCancel:
				control.canceled.Store(true)
				cancel()
				s.log.Warn("收到取消请求", "root", run.RootPath)
				return
			case model.ControlPause:
				if !paused {
					paused = true
					control.paused.Store(true)
					_ = s.store.SetRunStatus(context.WithoutCancel(ctx), run.ID, model.RunPaused, "用户暂停")
					s.log.Info("扫描已暂停", "root", run.RootPath)
				}
			default:
				if paused {
					paused = false
					control.paused.Store(false)
					_ = s.store.SetRunStatus(context.WithoutCancel(ctx), run.ID, model.RunRunning, "")
					s.log.Info("扫描已恢复", "root", run.RootPath)
				}
			}
		}
	}
}

func (s *Scanner) heartbeatLoop(ctx context.Context, run *model.Run, req Request, counters *progressCounters, control *scanControl, started time.Time, cancel context.CancelFunc) {
	heartbeat := s.cfg.Performance.HeartbeatInterval.Duration
	progressEvery := s.cfg.Performance.ProgressInterval.Duration
	if heartbeat < time.Second {
		heartbeat = 5 * time.Second
	}
	if progressEvery < heartbeat {
		progressEvery = heartbeat
	}
	hbTicker := time.NewTicker(heartbeat)
	defer hbTicker.Stop()
	progressTicker := time.NewTicker(progressEvery)
	defer progressTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hbTicker.C:
			ok, err := s.store.Heartbeat(context.WithoutCancel(ctx), run.ID, counters.getCurrent(),
				counters.done.Load(), counters.files.Load(), counters.alloc.Load())
			if err != nil {
				s.log.Warn("更新扫描心跳失败", "error", err.Error())
			} else if !ok {
				s.log.Warn("扫描任务已不在运行状态，停止扫描")
				control.runLost.Store(true)
				cancel()
				return
			}
		case <-progressTicker.C:
			if req.Progress == nil {
				continue
			}
			req.Progress(model.Progress{
				RootPath:   run.RootPath,
				CurrentDir: counters.getCurrent(),
				TotalDirs:  run.TotalDirs,
				DoneDirs:   int(counters.done.Load()),
				Files:      counters.files.Load(),
				Bytes:      counters.alloc.Load(),
				Paused:     control.paused.Load(),
				Elapsed:    time.Since(started),
			})
		}
	}
}

func (s *Scanner) finishAbort(run *model.Run, req Request, control *scanControl, cause error) *model.ScanSummary {
	status := model.RunFailed
	switch {
	case control.canceled.Load() || errors.Is(cause, model.ErrCanceled):
		status = model.RunCanceled
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		status = model.RunInterrupted
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reason := "扫描失败"
	switch status {
	case model.RunCanceled:
		reason = model.ErrCanceled.Error()
	case model.RunInterrupted:
		reason = "扫描被中断"
	default:
		if cause != nil {
			reason = cause.Error()
		}
	}
	if err := s.store.AbortRun(ctx, run.ID, status, reason); err != nil {
		s.log.Error("记录扫描终止状态失败", "error", err.Error())
	}
	summary := &model.ScanSummary{
		RunID:      run.ID,
		Mode:       run.Mode,
		SizeBasis:  run.SizeBasis,
		StartedAt:  run.StartedAt,
		FinishedAt: time.Now(),
		Duration:   time.Since(run.StartedAt),
		DataStatus: model.DataStale,
		Accuracy:   model.AccuracyUnknown,
		TotalDirs:  run.TotalDirs,
		DoneDirs:   run.ProcessedDirs,
		Errors:     []string{reason},
	}
	if current, err := s.store.GetRun(ctx, run.ID); err == nil {
		summary.TotalDirs = current.TotalDirs
		summary.DoneDirs = current.ProcessedDirs
		summary.Resumed = current.Resumed
	}
	if root, err := s.store.GetRootByID(ctx, run.RootID); err == nil {
		summary.Root = *root
		summary.DataStatus = root.Status
		summary.Accuracy = root.Accuracy
		summary.TotalAlloc = root.TotalAlloc
		summary.TotalLogical = root.TotalLogical
		summary.FileCount = root.FileCount
		summary.DirCount = root.DirCount
	}
	return summary
}

func (s *Scanner) abort(run *model.Run, status model.RunStatus, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.store.AbortRun(ctx, run.ID, status, reason)
}

func validateDirName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("非法目录名称: %q", name)
	}
	if strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("目录名称不能包含路径分隔符: %q", name)
	}
	return nil
}
