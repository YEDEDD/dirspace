// Package service coordinates scans, locks, cache queries, and maintenance.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/mountinfo"
	"github.com/YEDEDD/dirspace/internal/scanner"
	"github.com/YEDEDD/dirspace/internal/storage"
	"golang.org/x/sys/unix"
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
	ShardIndex     int
	ShardCount     int
	// SkipIfScannedAfter makes a scheduled scan abort if another process
	// published a successful scan after this time. It closes the race between
	// the scheduler's re-read and the scan acquiring the filesystem lock.
	SkipIfScannedAfter time.Time
}

// ErrScanSuperseded means another process completed a newer scan while this
// scheduled run was waiting for the global lock.
var ErrScanSuperseded = errors.New("已有更新的扫描结果，跳过本次调度")

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
	lock, err := acquireScanLock(rootPath)
	if err != nil {
		if errors.Is(err, ErrRootLocked) {
			return nil, fmt.Errorf("%w: %s", ErrRootLocked, rootPath)
		}
		return nil, err
	}
	defer lock.Close()
	if !opts.SkipIfScannedAfter.IsZero() {
		if root, rootErr := s.Store.GetRoot(ctx, rootPath); rootErr == nil &&
			root.LastSuccessAt != nil && root.LastSuccessAt.After(opts.SkipIfScannedAfter) {
			return nil, ErrScanSuperseded
		}
	}

	mode := opts.Mode
	if mode == "" {
		mode = model.ModeFull
	}
	if mode == model.ModeQuick {
		if root, rootErr := s.Store.GetRoot(ctx, rootPath); rootErr == nil {
			if active, activeErr := s.Store.ActiveRun(ctx, root.ID); activeErr == nil && active.Mode == model.ModeFull {
				return nil, fmt.Errorf("精确扫描正在进行（#%d），请等它完成后再执行快速估算", active.ID)
			}
			if s.Cfg.Scan.ResumeInterrupted {
				// Only the newest run may block a quick scan. An older
				// interrupted exact run must not block resuming a newer quick
				// run; the daemon resumes the newest run first.
				if latest, runErr := s.Store.LastRun(ctx, root.ID); runErr == nil && latest != nil &&
					latest.Mode == model.ModeFull {
					switch latest.Status {
					case model.RunInterrupted, model.RunRunning, model.RunPaused:
						return nil, fmt.Errorf("有未完成的精确扫描（#%d），请先执行 dirspace scan --resume 续扫", latest.ID)
					}
				}
			}
		}
	}
	resume := opts.Resume
	summary, err := s.scanner.Scan(ctx, scanner.Request{
		RootPath:       rootPath,
		Mode:           mode,
		Resume:         resume,
		ResumeCanceled: opts.ResumeCanceled,
		Progress:       opts.Progress,
		ShardIndex:     opts.ShardIndex,
		ShardCount:     opts.ShardCount,
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
	lock, err := acquireScanLock(rootPath)
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
	StaleDirs  int
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
		view.StaleDirs, _ = s.Store.CountStaleDirs(ctx, root.ID)
		status := root.Status
		if status == model.DataScanning {
			if _, activeErr := s.Store.ActiveRun(ctx, root.ID); errors.Is(activeErr, storage.ErrNotFound) {
				status = model.DataFresh
			}
		}
		view.DataStatus = s.EffectiveStatus(status, root.LastSuccessAt)
		view.Server = mountinfo.Source(root.Path)
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
	view.Server = mountinfo.Source(root.Path)
	view.FSType = mountinfo.FSType(root.Path)
	return view, nil
}

// IndexChildrenView is a directory-index drill-down result.
type IndexChildrenView struct {
	Path      string
	ScanRoot  string
	Parent    *model.DirIndexEntry
	Dirs      []model.DirIndexEntry
	SizeBasis model.SizeBasis
}

// Children reads direct children of path from the depth-limited directory
// index. It never accesses NFS. Path may be the scan root or any indexed
// descendant. If the path has no index entry yet, the caller should run a
// targeted drill/rebuild for that subtree.
func (s *Service) Children(ctx context.Context, path string, limit int) (*IndexChildrenView, error) {
	requested, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	root, err := s.findScannedRoot(ctx, requested)
	if err != nil {
		return nil, err
	}
	rel := ""
	if requested != root.Path {
		rel, err = filepath.Rel(root.Path, requested)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if strings.HasPrefix(rel, "../") || rel == ".." {
			return nil, &NotScannedError{Path: requested}
		}
	}
	var parent *model.DirIndexEntry
	if rel != "" {
		entry, getErr := s.Store.GetIndexEntry(ctx, root.ID, rel)
		if getErr != nil {
			if errors.Is(getErr, storage.ErrNotFound) {
				return nil, fmt.Errorf("目录 %s 尚未进入索引；请提高 scan.index_depth 后重新扫描，或对其所属的一级目录执行 dirspace rebuild", requested)
			}
			return nil, getErr
		}
		parent = entry
	}
	dirs, err := s.Store.Children(ctx, root.ID, rel, limit, s.sizeBasis())
	if err != nil {
		return nil, err
	}
	return &IndexChildrenView{
		Path: requested, ScanRoot: root.Path, Parent: parent,
		Dirs: dirs, SizeBasis: s.sizeBasis(),
	}, nil
}

// HistoryDiffView compares two completed snapshots by top-level directory.
type HistoryDiffView struct {
	Path  string
	Root  string
	Since time.Duration
	Base  *model.Run
	Head  *model.Run
	Rows  []storage.HistoryDiffRow
}

// Diff compares the newest completed full/quick run with the newest completed
// run of the same mode at or before the requested cutoff. It is a cache-only
// report; it does not rescan the filesystem.
func (s *Service) Diff(ctx context.Context, path string, since time.Duration) (*HistoryDiffView, error) {
	if since <= 0 {
		return nil, errors.New("diff 需要正的时间范围，例如 --since 7d")
	}
	requested, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	root, err := s.findScannedRoot(ctx, requested)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-since)
	base, head, rows, err := s.Store.HistoryDiff(ctx, root.ID, cutoff)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("没有可对比的完成快照：需要至少两次同模式完成扫描，且新快照在 %s 之后", since)
		}
		return nil, err
	}
	return &HistoryDiffView{Path: requested, Root: root.Path, Since: since, Base: base, Head: head, Rows: rows}, nil
}

// Merge combines disjoint shard databases for one root into targetDB.
func (s *Service) Merge(ctx context.Context, targetDB, rootPath string, inputs []string) (*model.Root, error) {
	rootPath, err := config.NormalizeAbsolutePath(rootPath)
	if err != nil {
		return nil, err
	}
	if err := validateExistingDir(rootPath); err != nil {
		return nil, err
	}
	if targetDB == "" {
		return nil, errors.New("merge 需要 --db <合并后的数据库>")
	}
	if len(inputs) == 0 {
		return nil, errors.New("merge 需要 --inputs <分片1,分片2,...>")
	}
	absTarget, err := filepath.Abs(targetDB)
	if err != nil {
		return nil, err
	}
	if config.PathWithin(rootPath, absTarget) {
		return nil, fmt.Errorf("合并数据库不能位于扫描根目录内: %s", absTarget)
	}
	return storage.MergeShards(ctx, absTarget, rootPath, inputs)
}

// FSUsage is the capacity of the filesystem containing Path.
type FSUsage struct {
	Path      string
	FSType    string
	Total     uint64
	Free      uint64
	Available uint64
	Used      uint64
	BlockSize int64
}

// FSUsage reads statfs for the filesystem containing path. It is a
// filesystem-level number; it cannot answer per-directory usage.
func (s *Service) FSUsage(ctx context.Context, path string) (*FSUsage, error) {
	target, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	if err := validateExistingDir(target); err != nil {
		return nil, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(target, &stat); err != nil {
		return nil, fmt.Errorf("读取文件系统容量失败 %s: %w", target, err)
	}
	blockSize := int64(stat.Bsize)
	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	free := uint64(stat.Bfree) * uint64(stat.Bsize)
	avail := uint64(stat.Bavail) * uint64(stat.Bsize)
	return &FSUsage{
		Path: target, FSType: mountinfo.FSType(target),
		Total: total, Free: free, Available: avail,
		Used: total - free, BlockSize: blockSize,
	}, nil
}

// SizeView is one cached directory size. Entry is nil when Path is the scan
// root and the value comes from the root aggregate.
type SizeView struct {
	Path      string
	Root      model.Root
	Entry     *model.DirIndexEntry
	SizeBasis model.SizeBasis
	Scanned   bool
}

// Size returns a cached directory aggregate. When exact is true and the path is
// not in the cache, it scans that path as its own root and returns the fresh
// root aggregate.
func (s *Service) Size(ctx context.Context, path string, exact bool) (*SizeView, error) {
	requested, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	root, rootErr := s.findScannedRoot(ctx, requested)
	if rootErr == nil {
		rel, relErr := filepath.Rel(root.Path, requested)
		if relErr != nil {
			return nil, relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if rel != "" && !strings.HasPrefix(rel, "../") {
			entry, entryErr := s.Store.GetIndexEntry(ctx, root.ID, rel)
			if entryErr == nil {
				return &SizeView{Path: requested, Root: *root, Entry: entry, SizeBasis: s.sizeBasis()}, nil
			}
			if !errors.Is(entryErr, storage.ErrNotFound) {
				return nil, entryErr
			}
			if !exact {
				return nil, fmt.Errorf("目录 %s 尚未进入索引；可使用 dirspace size --path %s --exact 单独扫描该目录，或开启 scan.index_all_dirs 并重新扫描所属根", requested, requested)
			}
		} else if rel == "" {
			return &SizeView{Path: requested, Root: *root, SizeBasis: s.sizeBasis()}, nil
		}
	} else if rootErr != nil {
		var notScanned *NotScannedError
		if !errors.As(rootErr, &notScanned) {
			return nil, rootErr
		}
	}
	if !exact {
		if rootErr != nil {
			return nil, rootErr
		}
		return nil, &NotScannedError{Path: requested}
	}

	// Scan the requested subtree as its own root. This is the on-demand exact
	// path: it does not need the parent root to be idle, because the lock is
	// scoped to this path.
	if _, err := s.Scan(ctx, requested, ScanOptions{Mode: model.ModeFull}); err != nil {
		return nil, err
	}
	root, err = s.Store.GetRoot(ctx, requested)
	if err != nil {
		return nil, err
	}
	return &SizeView{Path: requested, Root: *root, SizeBasis: s.sizeBasis(), Scanned: true}, nil
}

func (s *Service) findScannedRoot(ctx context.Context, path string) (*model.Root, error) {
	cur := path
	for {
		root, err := s.Store.GetRoot(ctx, cur)
		if err == nil {
			return root, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil, &NotScannedError{Path: path}
		}
		cur = parent
	}
}

// PartialSource identifies where a merged partial-view row came from.
type PartialSource string

const (
	PartialSourceCurrent PartialSource = "current"
	PartialSourceLast    PartialSource = "last"
)

// PartialDir is one first-level row in a partial view. Current rows come from
// the active run's staging table; Last rows are the published result that the
// active run has not replaced yet.
type PartialDir struct {
	Dir    model.DirStat
	Source PartialSource
}

// PartialView is the in-progress staging data of a scan run, optionally merged
// with the last published result for directories the active run has not
// completed. The merge is read-only and never changes top_dirs.
type PartialView struct {
	Path         string
	Root         model.Root
	Run          *model.Run
	Dirs         []PartialDir
	SizeBasis    model.SizeBasis
	FreshDirs    int
	CarriedDirs  int
	FreshBytes   int64
	CarriedBytes int64
	Mixed        bool
}

// Partial returns a merged view when includeLast is true: current staged rows
// override the last published rows by name, and unpublished names are carried
// from the previous successful scan. With includeLast false it returns only the
// active run's staging rows, preserving the old behavior.
func (s *Service) Partial(ctx context.Context, path string, limit int, includeLast bool) (*PartialView, error) {
	requested, err := config.NormalizeAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	root, err := s.Store.GetRoot(ctx, requested)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, &NotScannedError{Path: requested}
		}
		return nil, err
	}
	run, err := s.Store.ActiveRun(ctx, root.ID)
	if errors.Is(err, storage.ErrNotFound) {
		latest, lastErr := s.Store.LastRun(ctx, root.ID)
		if lastErr != nil {
			if errors.Is(lastErr, storage.ErrNotFound) {
				return nil, fmt.Errorf("当前没有扫描任务；请先执行 dirspace scan")
			}
			return nil, lastErr
		}
		switch latest.Status {
		case model.RunRunning, model.RunPaused:
			run = latest
		case model.RunInterrupted:
			if partialInterruptedStale(latest.FinishedAt, time.Now()) {
				finished := "未知时间"
				if latest.FinishedAt != nil {
					finished = latest.FinishedAt.Local().Format("2006-01-02 15:04:05")
				}
				return nil, fmt.Errorf("当前没有进行中的扫描；最近任务 #%d 已于 %s 中断，未发布正式结果，请用 dirspace top 查看已发布数据", latest.ID, finished)
			}
			run = latest
		default:
			return nil, fmt.Errorf("当前没有进行中的扫描；已完成的结果请用 dirspace top 查看")
		}
	} else if err != nil {
		return nil, err
	}
	staged, err := s.Store.StagedDirs(ctx, run.ID, 0, s.sizeBasis())
	if err != nil {
		return nil, err
	}
	accuracy := model.AccuracyExact
	switch run.Mode {
	case model.ModeQuick:
		accuracy = model.AccuracySampled
	case model.ModeIncremental:
		accuracy = model.AccuracyIncremental
	}
	for i := range staged {
		staged[i].RootID = root.ID
		staged[i].Accuracy = accuracy
		if staged[i].Truncated {
			staged[i].DataStatus = model.DataPartial
		} else {
			staged[i].DataStatus = model.DataFresh
		}
	}

	view := &PartialView{
		Path: requested, Root: *root, Run: run,
		SizeBasis: s.sizeBasis(),
	}
	byName := make(map[string]bool, len(staged))
	for _, d := range staged {
		byName[d.Name] = true
		view.Dirs = append(view.Dirs, PartialDir{Dir: d, Source: PartialSourceCurrent})
	}
	if includeLast {
		published, err := s.Store.ListDirs(ctx, root.ID, 0, s.sizeBasis())
		if err != nil {
			return nil, err
		}
		for _, d := range published {
			if byName[d.Name] {
				continue
			}
			view.Dirs = append(view.Dirs, PartialDir{Dir: d, Source: PartialSourceLast})
		}
	}
	sort.SliceStable(view.Dirs, func(i, j int) bool {
		left, right := partialBytes(view.Dirs[i].Dir, view.SizeBasis), partialBytes(view.Dirs[j].Dir, view.SizeBasis)
		if left != right {
			return left > right
		}
		return view.Dirs[i].Dir.Name < view.Dirs[j].Dir.Name
	})
	if limit > 0 && len(view.Dirs) > limit {
		view.Dirs = view.Dirs[:limit]
	}
	for _, item := range view.Dirs {
		switch item.Source {
		case PartialSourceCurrent:
			view.FreshDirs++
			view.FreshBytes += partialBytes(item.Dir, view.SizeBasis)
		case PartialSourceLast:
			view.CarriedDirs++
			view.CarriedBytes += partialBytes(item.Dir, view.SizeBasis)
		}
	}
	view.Mixed = view.FreshDirs > 0 && view.CarriedDirs > 0
	return view, nil
}

// partialInterruptedStale prevents an interrupted run from being presented as
// the current partial scan days later. A recent interruption is still useful
// for inspecting what completed before the failure.
func partialInterruptedStale(finished *time.Time, now time.Time) bool {
	if finished == nil {
		return false
	}
	return now.Sub(*finished) > time.Hour
}

func partialBytes(d model.DirStat, basis model.SizeBasis) int64 {
	if basis == model.BasisLogical {
		return d.LogicalBytes
	}
	return d.AllocBytes
}

// StatusView is the read-only state summary shown by "status".
type ChildActive struct {
	RootPath string
	Run      *model.Run
	DoneDirs int
}

type StatusView struct {
	Path              string
	Root              model.Root
	Target            *model.DirStat
	Active            *model.Run
	Last              *model.Run
	Control           model.Control
	DirCount          int
	StaleDirs         int
	ActiveDoneAlloc   int64
	ActiveDoneLogical int64
	ActiveDoneDirs    int
	DataStatus        model.DataStatus
	DBPath            string
	DBSize            int64
	Server            string
	FSType            string
	SizeBasis         model.SizeBasis
	MetadataOps       int64
	MetadataQPS       float64
	MetadataLimit     float64
	ChildActive       []ChildActive
}

// Status reads only local database and mount table state.
func (s *Service) Status(ctx context.Context, path string) (*StatusView, error) {
	// Status is also the operator's recovery command: a killed foreground
	// scanner leaves a running row behind, and the next status should not keep
	// showing it as live forever.
	s.Recover(ctx)
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
	if view.Active != nil {
		if staged, stagedErr := s.Store.StagedDirs(ctx, view.Active.ID, 0, model.BasisAllocated); stagedErr == nil {
			for _, d := range staged {
				view.ActiveDoneAlloc += d.AllocBytes
				view.ActiveDoneLogical += d.LogicalBytes
				view.ActiveDoneDirs++
			}
		}
	}
	view.Last, _ = s.Store.LastRun(ctx, root.ID)
	if view.Active != nil {
		view.MetadataOps = view.Active.MetadataOps
		view.MetadataQPS = view.Active.MetadataQPS
		view.MetadataLimit = view.Active.MetadataLimit
	} else if view.Last != nil {
		view.MetadataOps = view.Last.MetadataOps
		view.MetadataQPS = view.Last.MetadataQPS
		view.MetadataLimit = view.Last.MetadataLimit
	}
	if view.MetadataLimit == 0 {
		view.MetadataLimit = float64(s.Cfg.Performance.MaxMetadataQPS)
	}
	view.Control, _ = s.Store.GetControl(ctx, root.ID)
	if activeRuns, activeErr := s.Store.ActiveRuns(ctx); activeErr == nil {
		for _, run := range activeRuns {
			if run.RootID == root.ID || !config.PathWithin(requested, run.RootPath) {
				continue
			}
			view.ChildActive = append(view.ChildActive, ChildActive{
				RootPath: run.RootPath,
				Run:      run,
				DoneDirs: run.ProcessedDirs,
			})
		}
	}
	view.DirCount, _ = s.Store.CountDirs(ctx, root.ID)
	view.StaleDirs, _ = s.Store.CountStaleDirs(ctx, root.ID)
	view.DBSize = databaseSize(s.Store.Path())
	view.Server = mountinfo.Source(root.Path)
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
	freshness := s.Cfg.Scan.Interval.Duration
	if s.Cfg.Scan.QuickInterval.Duration > 0 && s.Cfg.Scan.QuickInterval.Duration < freshness {
		freshness = s.Cfg.Scan.QuickInterval.Duration
	}
	if time.Since(*last) > freshness {
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
