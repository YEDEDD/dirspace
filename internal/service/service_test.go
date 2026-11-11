package service

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/storage"
	_ "modernc.org/sqlite"
)

type serviceFixture struct {
	svc   *Service
	store *storage.Store
	cfg   *config.Config
	base  string
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	base := t.TempDir()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(base, "db", "usage.db")
	cfg.Scan.RootPath = filepath.Join(base, "root")
	cfg.Performance.MaxMetadataQPS = 100000
	cfg.Performance.MaxCPUPercent = 0
	cfg.Performance.AdaptiveThrottle = false
	cfg.Performance.Concurrency = config.Concurrency{Max: 2}
	cfg.Performance.MemoryLimit.Bytes = 512 << 20
	cfg.Scan.Inventory.Enabled = false
	store, err := storage.Open(context.Background(), cfg.Database.Path, storage.Options{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &serviceFixture{
		svc:   New(store, cfg, log),
		store: store,
		cfg:   cfg,
		base:  base,
	}
}

func mkFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestQueryUsesCacheWithoutFilesystem(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	if _, err := f.svc.Scan(context.Background(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	// Move the root away. A query must still work from SQLite.
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.Query(context.Background(), root, 10)
	if err != nil {
		t.Fatalf("query after unmount-like rename: %v", err)
	}
	if len(view.Dirs) != 1 || view.Dirs[0].Name != "alpha" {
		t.Fatalf("unexpected view: %+v", view)
	}
	child, err := f.svc.Query(context.Background(), filepath.Join(root, "alpha"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !child.Single || child.Target == nil || child.Target.Name != "alpha" {
		t.Fatalf("child view: %+v", child)
	}
	if _, err := f.svc.Query(context.Background(), filepath.Join(f.base, "unknown"), 10); err == nil {
		t.Fatal("expected not-scanned error")
	}
}

func TestMultiRootIsolation(t *testing.T) {
	f := newServiceFixture(t)
	rootA := filepath.Join(f.base, "a")
	rootB := filepath.Join(f.base, "b")
	mkFile(t, filepath.Join(rootA, "same", "file"), 100)
	mkFile(t, filepath.Join(rootB, "same", "file"), 1<<20)
	ctx := context.Background()
	if _, err := f.svc.Scan(ctx, rootA, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Scan(ctx, rootB, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	viewA, err := f.svc.Query(ctx, rootA, 0)
	if err != nil {
		t.Fatal(err)
	}
	viewB, err := f.svc.Query(ctx, rootB, 0)
	if err != nil {
		t.Fatal(err)
	}
	if viewA.Root.ID == viewB.Root.ID {
		t.Fatal("roots not isolated")
	}
	if viewA.Dirs[0].AllocBytes == viewB.Dirs[0].AllocBytes {
		t.Fatalf("values leaked: %d", viewA.Dirs[0].AllocBytes)
	}
	if len(viewA.Dirs) != 1 || len(viewB.Dirs) != 1 {
		t.Fatalf("unexpected rows: %d %d", len(viewA.Dirs), len(viewB.Dirs))
	}
}

func TestRebuildAndStatus(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 100)
	mkFile(t, filepath.Join(root, "beta", "b"), 200)
	ctx := context.Background()
	if _, err := f.svc.Scan(ctx, root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.Status(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if status.DirCount != 2 || status.Last == nil {
		t.Fatalf("status: %+v", status)
	}
	mkFile(t, filepath.Join(root, "alpha", "new"), 4096)
	summary, err := f.svc.Rebuild(ctx, filepath.Join(root, "alpha"), "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalDirs != 1 {
		t.Fatalf("rebuild dirs=%d", summary.TotalDirs)
	}
	view, err := f.svc.Query(ctx, root, 0)
	if err != nil {
		t.Fatal(err)
	}
	var alpha, beta model.DirStat
	for _, d := range view.Dirs {
		if d.Name == "alpha" {
			alpha = d
		}
		if d.Name == "beta" {
			beta = d
		}
	}
	if alpha.FileCount != 2 {
		t.Fatalf("alpha not rebuilt: %+v", alpha)
	}
	if beta.FileCount != 1 {
		t.Fatalf("beta damaged: %+v", beta)
	}
}

func TestScanRootCannotBeSymlink(t *testing.T) {
	f := newServiceFixture(t)
	target := filepath.Join(f.base, "target")
	mkFile(t, filepath.Join(target, "alpha", "a"), 10)
	link := filepath.Join(f.base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Scan(context.Background(), link, ScanOptions{}); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestEffectiveStatus(t *testing.T) {
	f := newServiceFixture(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	if got := f.svc.EffectiveStatus(model.DataFresh, &old); got != model.DataStale {
		t.Fatalf("old data status=%s", got)
	}
	now := time.Now()
	if got := f.svc.EffectiveStatus(model.DataFresh, &now); got != model.DataFresh {
		t.Fatalf("fresh data status=%s", got)
	}
	if got := f.svc.EffectiveStatus(model.DataFailed, &now); got != model.DataFailed {
		t.Fatalf("failed status=%s", got)
	}
}

func TestScanRejectsDatabaseInsideRoot(t *testing.T) {
	base := t.TempDir()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(base, "usage.db")
	cfg.Scan.RootPath = base
	cfg.Performance.MaxMetadataQPS = 100000
	cfg.Performance.MaxCPUPercent = 0
	cfg.Performance.AdaptiveThrottle = false
	store, err := storage.Open(context.Background(), cfg.Database.Path, storage.Options{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := New(store, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := svc.Scan(context.Background(), base, ScanOptions{}); err == nil {
		t.Fatal("expected database-inside-root rejection")
	}
}

func TestIncrementalScanOnlyAddsNewTopDirs(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 4096)
	if _, err := f.svc.Scan(context.Background(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.Query(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	before := view.Dirs[0].AllocBytes

	mkFile(t, filepath.Join(root, "beta", "b"), 8192)
	summary, err := f.svc.Scan(context.Background(), root, ScanOptions{Mode: model.ModeIncremental})
	if err != nil {
		t.Fatalf("incremental scan: %v", err)
	}
	if summary.DoneDirs != 1 || summary.FailedDirs != 0 {
		t.Fatalf("unexpected incremental summary: %+v", summary)
	}
	view, err = f.svc.Query(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Dirs) != 2 {
		t.Fatalf("dirs=%+v", view.Dirs)
	}
	byName := map[string]model.DirStat{}
	for _, d := range view.Dirs {
		byName[d.Name] = d
	}
	if byName["alpha"].AllocBytes != before {
		t.Fatalf("existing alpha changed: before=%d after=%d", before, byName["alpha"].AllocBytes)
	}
	if byName["beta"].FileCount != 1 {
		t.Fatalf("beta not scanned: %+v", byName["beta"])
	}
}

func TestQuickOnlyBaselineSchedulesExactOnStart(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	if _, err := f.svc.Scan(context.Background(), root, ScanOptions{Mode: model.ModeQuick}); err != nil {
		t.Fatal(err)
	}
	sc := f.svc.nextSchedule(context.Background(), root)
	if sc.exact.After(time.Now().Add(2 * time.Second)) {
		t.Fatalf("exact should be due after quick-only baseline: %s", sc.exact)
	}
	if sc.quick.IsZero() {
		t.Fatal("quick schedule missing")
	}
}

func TestStatusRecoversAbandonedRun(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	run, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeFull, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Minute).UnixMilli()
	raw, err := sql.Open("sqlite", f.cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(),
		`UPDATE scan_runs SET heartbeat_at=? WHERE id=?`, old, run.ID); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.Status(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if view.Active != nil {
		t.Fatalf("abandoned run still active: %+v", view.Active)
	}
	if view.Root.Status == model.DataScanning {
		t.Fatalf("root still scanning after recovery")
	}
}

func TestInterruptedExactSchedulesResumeBeforeQuick(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	run, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeFull, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveDirResult(context.Background(), run.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 1024, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbortRun(context.Background(), run.ID, model.RunInterrupted, "test restart"); err != nil {
		t.Fatal(err)
	}
	sc := f.svc.nextSchedule(context.Background(), root)
	if sc.exact.After(time.Now().Add(2 * time.Second)) {
		t.Fatalf("interrupted exact not scheduled for resume: %s", sc.exact)
	}
	if !sc.quick.IsZero() && !sc.quick.After(time.Now()) {
		t.Fatalf("quick must not run before exact resume: %s", sc.quick)
	}
}

func TestQuickResumeKeepsCompletedDirs(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	f.cfg.Scan.QuickSamplePercent = 100
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	mkFile(t, filepath.Join(root, "beta", "b"), 2048)

	run, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeQuick, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveDirResult(context.Background(), run.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 4096, FileCount: 1, DirCount: 0,
		CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbortRun(context.Background(), run.ID, model.RunInterrupted, "test restart"); err != nil {
		t.Fatal(err)
	}
	// If alpha were rescanned, this extra file would appear in the summary.
	mkFile(t, filepath.Join(root, "alpha", "extra"), 8192)

	summary, err := f.svc.Scan(context.Background(), root, ScanOptions{
		Mode: model.ModeQuick, Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Resumed || summary.RunID != run.ID {
		t.Fatalf("quick run was not resumed: %+v", summary)
	}
	if summary.TotalDirs != 2 || summary.DoneDirs != 2 {
		t.Fatalf("unexpected dirs: total=%d done=%d", summary.TotalDirs, summary.DoneDirs)
	}
	if summary.FileCount != 2 {
		t.Fatalf("completed alpha was rescanned: files=%d want=2", summary.FileCount)
	}
}

func TestNewestQuickResumeNotBlockedByOlderExact(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	f.cfg.Scan.QuickSamplePercent = 100
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	mkFile(t, filepath.Join(root, "beta", "b"), 2048)

	exact, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeFull, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveDirResult(context.Background(), exact.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 1024, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbortRun(context.Background(), exact.ID, model.RunInterrupted, "old exact"); err != nil {
		t.Fatal(err)
	}

	quick, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeQuick, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveDirResult(context.Background(), quick.ID, model.DirOutcome{
		Name: "beta", AllocBytes: 2048, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbortRun(context.Background(), quick.ID, model.RunInterrupted, "new quick"); err != nil {
		t.Fatal(err)
	}

	sc := f.svc.nextSchedule(context.Background(), root)
	if sc.quick.After(time.Now().Add(2 * time.Second)) {
		t.Fatalf("newest quick run not scheduled for resume: %s", sc.quick)
	}
	summary, err := f.svc.Scan(context.Background(), root, ScanOptions{
		Mode: model.ModeQuick, Resume: true,
	})
	if err != nil {
		t.Fatalf("newest quick run should not be blocked by an older exact run: %v", err)
	}
	if !summary.Resumed || summary.RunID != quick.ID {
		t.Fatalf("wrong run resumed: %+v", summary)
	}
}

func TestInterruptedQuickSchedulesQuickResume(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	if _, err := f.svc.Scan(context.Background(), root, ScanOptions{Mode: model.ModeFull}); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeQuick, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveDirResult(context.Background(), run.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 1024, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbortRun(context.Background(), run.ID, model.RunInterrupted, "test restart"); err != nil {
		t.Fatal(err)
	}
	sc := f.svc.nextSchedule(context.Background(), root)
	if sc.quick.After(time.Now().Add(2 * time.Second)) {
		t.Fatalf("interrupted quick not scheduled for resume: %s", sc.quick)
	}
}

func TestQuickRefusesWhileExactIsActive(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	if _, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeFull, SizeBasis: model.BasisAllocated,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Scan(context.Background(), root, ScanOptions{Mode: model.ModeQuick}); err == nil {
		t.Fatal("quick scan should be refused while exact is active")
	}
}

func TestPartialReturnsCompleteStagedNames(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	run, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeQuick, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	longName := "turing-data-etcd-2-pvc-4c25346d-d5c0-4876-b39e-02174655b88e"
	if err := f.store.SaveDirResult(context.Background(), run.ID, model.DirOutcome{
		Name: longName, AllocBytes: 7 << 40, FileCount: 42, DirCount: 3,
		CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	view, err := f.svc.Partial(context.Background(), root, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.Run == nil || view.Run.ID != run.ID {
		t.Fatalf("unexpected run: %+v", view.Run)
	}
	if len(view.Dirs) != 1 || view.Dirs[0].Dir.Name != longName {
		t.Fatalf("partial dirs: %+v", view.Dirs)
	}
	if view.Dirs[0].Dir.AllocBytes != 7<<40 {
		t.Fatalf("partial bytes: %+v", view.Dirs[0])
	}
	if view.Dirs[0].Source != PartialSourceCurrent {
		t.Fatalf("partial source: %+v", view.Dirs[0])
	}
}

func TestPartialMergesCurrentWithLastPublished(t *testing.T) {
	f := newServiceFixture(t)
	root := f.cfg.Scan.RootPath
	mkFile(t, filepath.Join(root, "alpha", "a"), 1024)
	mkFile(t, filepath.Join(root, "beta", "b"), 2048)
	if _, err := f.svc.Scan(context.Background(), root, ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	run, err := f.store.StartRun(context.Background(), root, storage.StartOptions{
		Mode: model.ModeQuick, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []model.DirOutcome{
		{Name: "alpha", AllocBytes: 7 << 20, FileCount: 2, DirCount: 1, CompletedAt: time.Now()},
		{Name: "gamma", AllocBytes: 3 << 20, FileCount: 3, DirCount: 2, CompletedAt: time.Now()},
	} {
		if err := f.store.SaveDirResult(context.Background(), run.ID, out); err != nil {
			t.Fatal(err)
		}
	}

	view, err := f.svc.Partial(context.Background(), root, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Mixed || view.FreshDirs != 2 || view.CarriedDirs != 1 {
		t.Fatalf("unexpected merge counts: %+v", view)
	}
	found := map[string]PartialDir{}
	for _, item := range view.Dirs {
		found[item.Dir.Name] = item
	}
	if got := found["alpha"]; got.Source != PartialSourceCurrent || got.Dir.AllocBytes != 7<<20 {
		t.Fatalf("alpha should be current: %+v", got)
	}
	if got := found["beta"]; got.Source != PartialSourceLast || got.Dir.AllocBytes == 0 {
		t.Fatalf("beta should be carried: %+v", got)
	}
	if got := found["gamma"]; got.Source != PartialSourceCurrent || got.Dir.AllocBytes != 3<<20 {
		t.Fatalf("gamma should be current: %+v", got)
	}

	stagingOnly, err := f.svc.Partial(context.Background(), root, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if stagingOnly.CarriedDirs != 0 || stagingOnly.Mixed {
		t.Fatalf("staging only should not carry rows: %+v", stagingOnly)
	}
	for _, item := range stagingOnly.Dirs {
		if item.Dir.Name == "beta" {
			t.Fatalf("staging only unexpectedly contains beta: %+v", stagingOnly.Dirs)
		}
	}
}
