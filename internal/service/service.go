// Package service coordinates scans, locks, cache queries, and maintenance.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/mountinfo"
	"github.com/YEDEDD/dirspace/internal/scanner"
	"github.com/YEDEDD/dirspace/internal/storage"
)

// ErrNoActiveScan is returned by pause/resume/cancel when no scan is running.
var ErrNoActiveScan = errors.New("该根目录没有正在运行的扫描任务")

// ErrNotPaused is returned when resuming a scan that is not paused.
var ErrNotPaused = errors.New("扫描任务当前未暂停")

// NotScannedError tells the user how to establish a baseline.
type NotScannedError struct {
	Path string
}

func (e *NotScannedError) Error() string {
	return fmt.Sprintf("路径尚未统计: %s，请先执行: dirspace scan --path %s", e.Path, e.Path)
}

// Service is the application layer shared by CLI and daemon.
type Service struct {
	Store *storage.Store
	Cfg   *config.Config
	Log   *slog.Logger

	scanner *scanner.Scanner
}

func New(store *storage.Store, cfg *config.Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{Store: store, Cfg: cfg, Log: log, scanner: scanner.New(store, cfg, log)}
}

// ScanOptions controls baseline/refresh scans.
type ScanOptions struct {
	Mode           model.ScanMode
	Resume         bool
	ResumeCanceled bool
	Progress       func(model.Progress)
}

// Scan runs a full or refresh scan for an exact scan root.
func (s *Service) Scan(ctx context.Context, path string, opts ScanOptions) (*model.ScanSummary, error) {
	rootPath, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	if err := validateExistingDir(rootPath); err != nil {
		return nil, err
	}
	if config.PathWithin(rootPath, s.Store.Path()) {
		return nil, fmt.Errorf("数据库不能位于扫描根目录内: %s 位于 %s", s.Store.Path(), rootPath)
	}
	lock, err := acquireRootLock(s.Store.Dir(), rootPath)
	if err != nil {
		if errors.Is(err, ErrRootLocked) {
			return nil, fmt.Errorf("%w: %s", ErrRootLocked, rootPath)
		}
		return nil, err
	}
	defer lock.Close()

	mode := opts.Mode
	if mode == "" {
		// This release intentionally performs a full metadata walk for every
		// refresh. NFSv3 directory mtimes are not a safe change detector, and a
		// file-level inventory still requires one metadata lookup per file, so a
		// fake "incremental" mode would silently reduce accuracy. The mode is
		// kept in the schema for a future safe implementation.
		mode = model.ModeFull
	}
	resume := opts.Resume
	summary, err := s.scanner.Scan(ctx, scanner.Request{
		RootPath:       rootPath,
		Mode:           mode,
		Resume:         resume,
		ResumeCanceled: opts.ResumeCanceled,
		Progress:       opts.Progress,
	})
	_ = s.Store.Checkpoint(context.WithoutCancel(ctx))
	return summary, err
}

// Rebuild rescans exactly one first-level directory under a known root.
// With an empty name, path itself must be a direct child of a known root.
func (s *Service) Rebuild(ctx context.Context, path, name string) (*model.ScanSummary, error) {
	if path == "" {
		return nil, errors.New("rebuild 需要 --path 或目录名")
	}
	p, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	var rootPath string
	if name == "" {
		rootPath = filepath.Dir(p)
		name = filepath.Base(p)
	} else {
		rootPath = p
	}
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, fmt.Errorf("非法一级目录名称: %q", name)
	}
	if _, err := s.Store.GetRoot(ctx, rootPath); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &NotScannedError{Path: rootPath}
		}
		return nil, err
	}
	target := filepath.Join(rootPath, name)
	if err := validateExistingDir(target); err != nil {
		return nil, fmt.Errorf("待重算目录不可用: %w", err)
	}
	if config.PathWithin(rootPath, s.Store.Path()) {
		return nil, fmt.Errorf("数据库不能位于扫描根目录内: %s 位于 %s", s.Store.Path(), rootPath)
	}
	lock, err := acquireRootLock(s.Store.Dir(), rootPath)
	if err != nil {
		if errors.Is(err, ErrRootLocked) {
			return nil, fmt.Errorf("%w: %s", ErrRootLocked, rootPath)
		}
		return nil, err
	}
	defer lock.Close()
	summary, err := s.scanner.Scan(ctx, scanner.Request{
		RootPath:  rootPath,
		TargetDir: name,
		Mode:      model.ModeRebuild,
	})
	if summary != nil {
		summary.TargetDir = name
	}
	_ = s.Store.Checkpoint(context.WithoutCancel(ctx))
	return summary, err
}

// View is the read-only result of a cache query.
type View struct {
	Path       string
	ScanRoot   string
	Root       model.Root
	Dirs       []model.DirStat
	Target     *model.DirStat
	Single     bool
	DataStatus model.DataStatus
	SizeBasis  model.SizeBasis
	Server     string
	FSType     string
}

// Query reads only SQLite. It never stats or scans the requested NFS path.
func (s *Service) Query(ctx context.Context, path string, limit int) (*View, error) {
	requested, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	view := &View{Path: requested, SizeBasis: s.sizeBasis()}
	if root, err := s.Store.GetRoot(ctx, requested); err == nil {
		dirs, err := s.Store.ListDirs(ctx, root.ID, limit, s.sizeBasis())
		if err != nil {
			return nil, err
		}
		view.Root = *root
		view.ScanRoot = root.Path
		view.Dirs = dirs
		view.DataStatus = s.EffectiveStatus(root.Status, root.LastSuccessAt)
		view.Server = mountinfo.Server(root.Path)
		view.FSType = mountinfo.FSType(root.Path)
		return view, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	parent := filepath.Dir(requested)
	dirName := filepath.Base(requested)
	root, err := s.Store.GetRoot(ctx, parent)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &NotScannedError{Path: requested}
		}
		return nil, err
	}
	target, err := s.Store.GetDir(ctx, root.ID, dirName)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &NotScannedError{Path: requested}
		}
		return nil, err
	}
	view.Root = *root
	view.ScanRoot = root.Path
	view.Target = target
	view.Single = true
	view.Dirs = []model.DirStat{*target}
	view.DataStatus = s.EffectiveStatus(target.DataStatus, target.LastSuccessAt)
	view.Server = mountinfo.Server(root.Path)
	view.FSType = mountinfo.FSType(root.Path)
	return view, nil
}

// StatusView is the read-only state summary shown by "status".
type StatusView struct {
	Path       string
	Root       model.Root
	Target     *model.DirStat
	Active     *model.Run
	Last       *model.Run
	Control    model.Control
	DirCount   int
	DataStatus model.DataStatus
	DBPath     string
	DBSize     int64
	Server     string
	FSType     string
	SizeBasis  model.SizeBasis
}

// Status reads only local database and mount table state.
func (s *Service) Status(ctx context.Context, path string) (*StatusView, error) {
	requested, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	root, err := s.Store.GetRoot(ctx, requested)
	var target *model.DirStat
	if errors.Is(err, storage.ErrNotFound) {
		parent := filepath.Dir(requested)
		root, err = s.Store.GetRoot(ctx, parent)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, &NotScannedError{Path: requested}
			}
			return nil, err
		}
		target, err = s.Store.GetDir(ctx, root.ID, filepath.Base(requested))
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if requested != root.Path && target == nil {
		return nil, &NotScannedError{Path: requested}
	}
	view := &StatusView{
		Path:      requested,
		Root:      *root,
		Target:    target,
		DBPath:    s.Store.Path(),
		SizeBasis: s.sizeBasis(),
	}
	view.Active, _ = s.Store.ActiveRun(ctx, root.ID)
	view.Last, _ = s.Store.LastRun(ctx, root.ID)
	view.Control, _ = s.Store.GetControl(ctx, root.ID)
	view.DirCount, _ = s.Store.CountDirs(ctx, root.ID)
	view.DBSize = databaseSize(s.Store.Path())
	view.Server = mountinfo.Server(root.Path)
	view.FSType = mountinfo.FSType(root.Path)
	if target != nil {
		view.DataStatus = s.EffectiveStatus(target.DataStatus, target.LastSuccessAt)
	} else {
		view.DataStatus = s.EffectiveStatus(root.Status, root.LastSuccessAt)
	}
	return view, nil
}

// Pause asks the running scanner for a root to pause.
func (s *Service) Pause(ctx context.Context, path string) error {
	root, run, err := s.controlTarget(ctx, path)
	if err != nil {
		return err
	}
	control, _ := s.Store.GetControl(ctx, root.ID)
	if control.State == model.ControlPause {
		return nil
	}
	if run.Status != model.RunRunning && run.Status != model.RunPaused {
		return ErrNoActiveScan
	}
	return s.Store.SetControl(ctx, root.ID, model.ControlPause)
}

// Resume asks a paused scanner to continue.
func (s *Service) Resume(ctx context.Context, path string) error {
	root, run, err := s.controlTarget(ctx, path)
	if err != nil {
		return err
	}
	control, _ := s.Store.GetControl(ctx, root.ID)
	if run.Status != model.RunPaused && control.State != model.ControlPause {
		return ErrNotPaused
	}
	return s.Store.SetControl(ctx, root.ID, model.ControlRunning)
}

// Cancel asks the running scanner to stop and discard its in-flight result.
func (s *Service) Cancel(ctx context.Context, path string) error {
	root, _, err := s.controlTarget(ctx, path)
	if err != nil {
		return err
	}
	return s.Store.SetControl(ctx, root.ID, model.ControlCancel)
}

func (s *Service) controlTarget(ctx context.Context, path string) (*model.Root, *model.Run, error) {
	requested, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, nil, err
	}
	root, err := s.Store.GetRoot(ctx, requested)
	if errors.Is(err, storage.ErrNotFound) {
		root, err = s.Store.GetRoot(ctx, filepath.Dir(requested))
	}
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, &NotScannedError{Path: requested}
		}
		return nil, nil, err
	}
	run, err := s.Store.ActiveRun(ctx, root.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoActiveScan, root.Path)
	}
	if err != nil {
		return nil, nil, err
	}
	return root, run, nil
}

// EffectiveStatus combines the recorded state with the configured freshness
// interval. It is used for labels, never to mutate data.
func (s *Service) EffectiveStatus(current model.DataStatus, last *time.Time) model.DataStatus {
	switch current {
	case model.DataFailed, model.DataPartial, model.DataScanning, model.DataNever, model.DataStale:
		return current
	}
	if last == nil || last.IsZero() {
		return model.DataNever
	}
	if time.Since(*last) > s.Cfg.Scan.Interval.Duration {
		return model.DataStale
	}
	return model.DataFresh
}

func (s *Service) sizeBasis() model.SizeBasis {
	if strings.EqualFold(s.Cfg.Scan.SizeBasis, "logical") {
		return model.BasisLogical
	}
	return model.BasisAllocated
}

func (s *Service) Recover(ctx context.Context) int {
	n, err := s.Store.RecoverStaleRuns(ctx, 90*time.Second)
	if err != nil {
		s.Log.Warn("恢复异常退出的扫描任务失败", "error", err.Error())
		return 0
	}
	return n
}

func databaseSize(path string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

func validateExistingDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("路径不可用: %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("路径不能是符号链接: %s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("路径不是目录: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("目录不可读: %s: %w", path, err)
	}
	return f.Close()
}
