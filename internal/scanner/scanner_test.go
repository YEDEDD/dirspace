package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YEDEDD/dirspace/internal/config"
	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/platform"
	"github.com/YEDEDD/dirspace/internal/storage"
)

type scannerFixture struct {
	scanner *Scanner
	store   *storage.Store
	cfg     *config.Config
	root    string
}

func newScannerFixture(t *testing.T) *scannerFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(base, "db", "usage.db")
	store, err := storage.Open(context.Background(), db, storage.Options{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	cfg.Scan.SizeBasis = "allocated"
	cfg.Scan.Inventory.Enabled = false
	cfg.Performance.Concurrency = config.Concurrency{Max: 2}
	cfg.Performance.MaxMetadataQPS = 100000
	cfg.Performance.MinMetadataQPS = 1
	cfg.Performance.MaxCPUPercent = 0
	cfg.Performance.AdaptiveThrottle = false
	cfg.Performance.MemoryLimit.Bytes = 512 << 20
	cfg.Performance.ProgressInterval.Duration = time.Second
	cfg.Performance.HeartbeatInterval.Duration = time.Second
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &scannerFixture{
		scanner: New(store, cfg, log),
		store:   store,
		cfg:     cfg,
		root:    root,
	}
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanAggregatesAndHardlinks(t *testing.T) {
	f := newScannerFixture(t)
	writeFile(t, filepath.Join(f.root, "alpha", "one.bin"), 1<<20)
	writeFile(t, filepath.Join(f.root, "alpha", "two.txt"), 11)
	writeFile(t, filepath.Join(f.root, "beta", "sub", "three.txt"), 7)
	if err := os.MkdirAll(filepath.Join(f.root, "gamma", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(f.root, "alpha", "one.bin"), filepath.Join(f.root, "beta", "link.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, "alpha", "one.bin"), filepath.Join(f.root, "beta", "sym")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if summary.DataStatus != model.DataFresh || summary.Accuracy != model.AccuracyExact {
		t.Fatalf("unexpected summary: status=%s accuracy=%s", summary.DataStatus, summary.Accuracy)
	}
	if summary.TotalDirs != 3 || summary.DoneDirs != 3 || summary.FailedDirs != 0 {
		t.Fatalf("unexpected dir counts: %+v", summary)
	}
	root, err := f.store.GetRoot(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.ListDirs(ctx, root.ID, 0, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d", len(rows))
	}
	byName := map[string]model.DirStat{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	alpha := byName["alpha"]
	beta := byName["beta"]
	gamma := byName["gamma"]
	if alpha.FileCount != 2 || beta.FileCount != 2 || gamma.FileCount != 0 {
		t.Fatalf("file counts: alpha=%d beta=%d gamma=%d", alpha.FileCount, beta.FileCount, gamma.FileCount)
	}
	if beta.DirCount != 1 || beta.SymlinkCount != 1 {
		t.Fatalf("beta dirs/symlinks: %+v", beta)
	}
	// A hard link can be attributed to whichever directory the concurrent
	// scanner reaches first, but its allocated bytes must appear exactly once.
	shared := alpha.AllocBytes + beta.AllocBytes
	if shared < 1<<20 {
		t.Fatalf("hardlinked file lost: alpha=%d beta=%d", alpha.AllocBytes, beta.AllocBytes)
	}
	if alpha.AllocBytes >= 1<<20 && beta.AllocBytes >= 1<<20 {
		t.Fatalf("hardlinked bytes counted twice: alpha=%d beta=%d", alpha.AllocBytes, beta.AllocBytes)
	}
	if gamma.AllocBytes < 0 {
		t.Fatal("negative size")
	}
	total := alpha.AllocBytes + beta.AllocBytes + gamma.AllocBytes
	// Each directory contributes its own directory block, including the scan
	// root. On APFS the root block is usually 0; on ext4/XFS it is 4096 bytes.
	rootInfo, statErr := os.Stat(f.root)
	if statErr != nil {
		t.Fatal(statErr)
	}
	rootMeta, exact := platform.Stat(rootInfo)
	if !exact {
		t.Fatal("root metadata is not exact")
	}
	if want := total + rootMeta.Allocated; root.TotalAlloc != want {
		t.Fatalf("root total %d != top dirs %d + root block %d", root.TotalAlloc, total, rootMeta.Allocated)
	}
	if root.TotalAlloc < 1<<20 || root.TotalAlloc > (1<<20)+200<<10 {
		t.Fatalf("unexpected root total: %d", root.TotalAlloc)
	}
	if root.FileCount != 4 {
		t.Fatalf("root file count=%d", root.FileCount)
	}
}

func TestScanRequiresMountPoint(t *testing.T) {
	f := newScannerFixture(t)
	f.cfg.Scan.RequireMountPoint = true
	ctx := context.Background()
	if _, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull}); err == nil {
		t.Fatal("expected non-mount root to be rejected")
	}
}

func TestScanSingleTopDirUsesIntraWorkers(t *testing.T) {
	f := newScannerFixture(t)
	f.cfg.Performance.Concurrency = config.Concurrency{Max: 4}
	for i := 0; i < 200; i++ {
		writeFile(t, filepath.Join(f.root, "alpha", fmt.Sprintf("file-%03d", i)), 16)
	}
	ctx := context.Background()
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalDirs != 1 || summary.FileCount != 200 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	root, err := f.store.GetRoot(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.store.GetDir(ctx, root.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if row.FileCount != 200 {
		t.Fatalf("file count=%d, want 200", row.FileCount)
	}
}

func TestScanEmptyRoot(t *testing.T) {
	f := newScannerFixture(t)
	ctx := context.Background()
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if summary.TotalDirs != 0 {
		t.Fatalf("unexpected empty summary: %+v", summary)
	}
	if summary.TotalAlloc < 0 {
		t.Fatalf("negative total: %d", summary.TotalAlloc)
	}
	if summary.DataStatus != model.DataFresh {
		t.Fatalf("status=%s", summary.DataStatus)
	}
	root, err := f.store.GetRoot(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if root.TotalAlloc < 0 || root.Status != model.DataFresh {
		t.Fatalf("root=%+v", root)
	}
}

func TestRebuildSingleDirPreservesSiblings(t *testing.T) {
	f := newScannerFixture(t)
	writeFile(t, filepath.Join(f.root, "alpha", "a"), 1000)
	writeFile(t, filepath.Join(f.root, "beta", "b"), 2000)
	ctx := context.Background()
	if _, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull}); err != nil {
		t.Fatal(err)
	}
	root, _ := f.store.GetRoot(ctx, f.root)
	beforeBeta, _ := f.store.GetDir(ctx, root.ID, "beta")
	writeFile(t, filepath.Join(f.root, "alpha", "new"), 5000)
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, TargetDir: "alpha", Mode: model.ModeRebuild})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if summary.Mode != model.ModeRebuild || summary.TotalDirs != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	afterAlpha, _ := f.store.GetDir(ctx, root.ID, "alpha")
	afterBeta, _ := f.store.GetDir(ctx, root.ID, "beta")
	if afterAlpha.FileCount != 2 {
		t.Fatalf("alpha not updated: %+v", afterAlpha)
	}
	if afterBeta.AllocBytes != beforeBeta.AllocBytes || afterBeta.FileCount != beforeBeta.FileCount {
		t.Fatalf("sibling changed: before=%+v after=%+v", beforeBeta, afterBeta)
	}
}

func TestMissingTopDirMarksStale(t *testing.T) {
	f := newScannerFixture(t)
	writeFile(t, filepath.Join(f.root, "alpha", "a"), 1234)
	writeFile(t, filepath.Join(f.root, "beta", "b"), 10)
	ctx := context.Background()
	if _, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull}); err != nil {
		t.Fatal(err)
	}
	root, _ := f.store.GetRoot(ctx, f.root)
	before, _ := f.store.GetDir(ctx, root.ID, "alpha")
	if err := os.RemoveAll(filepath.Join(f.root, "alpha")); err != nil {
		t.Fatal(err)
	}
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if summary.DataStatus != model.DataPartial {
		t.Fatalf("status=%s", summary.DataStatus)
	}
	after, err := f.store.GetDir(ctx, root.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if after.AllocBytes != before.AllocBytes || after.DataStatus != model.DataStale {
		t.Fatalf("missing dir not preserved: %+v", after)
	}
}

func TestPauseResumeCancel(t *testing.T) {
	f := newScannerFixture(t)
	for i := 0; i < 400; i++ {
		writeFile(t, filepath.Join(f.root, "slow", "f", fmt.Sprintf("file-%04d", i)), 10)
	}
	f.cfg.Performance.MaxMetadataQPS = 20
	f.scanner = New(f.store, f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		summary *model.ScanSummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull})
		done <- result{summary, err}
	}()

	var root model.Root
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := f.store.GetRoot(ctx, f.root)
		if err == nil {
			root = *r
			if run, err := f.store.ActiveRun(ctx, root.ID); err == nil && run.Status == model.RunRunning {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := f.store.SetControl(ctx, root.ID, model.ControlPause); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		run, err := f.store.ActiveRun(ctx, root.ID)
		if err == nil && run.Status == model.RunPaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan did not pause: %+v", run)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := f.store.SetControl(ctx, root.ID, model.ControlRunning); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		run, err := f.store.ActiveRun(ctx, root.ID)
		if err == nil && run.Status == model.RunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan did not resume: %+v", run)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := f.store.SetControl(ctx, root.ID, model.ControlCancel); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if !errors.Is(res.err, model.ErrCanceled) {
			t.Fatalf("expected cancellation, got %v", res.err)
		}
		if res.summary == nil {
			t.Fatal("missing summary")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel timeout")
	}
	rows, err := f.store.ListDirs(ctx, root.ID, 0, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("canceled run published partial data: %+v", rows)
	}
}

func TestRootDirectFilesIncluded(t *testing.T) {
	f := newScannerFixture(t)
	writeFile(t, filepath.Join(f.root, "direct.bin"), 1<<20)
	writeFile(t, filepath.Join(f.root, "sub", "nested"), 10)
	ctx := context.Background()
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FileCount != 2 {
		t.Fatalf("expected 2 paths, got %d", summary.FileCount)
	}
	if summary.TotalAlloc < 1<<20 {
		t.Fatalf("root direct file missing: %d", summary.TotalAlloc)
	}
	root, err := f.store.GetRoot(ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if root.SelfFiles != 1 || root.SelfAlloc < 1<<20 {
		t.Fatalf("self stats=%+v", root)
	}
	if root.FileCount != 2 {
		t.Fatalf("root file count=%d", root.FileCount)
	}
	rows, err := f.store.ListDirs(ctx, root.ID, 0, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "sub" {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestRebuildPreservesRootSelf(t *testing.T) {
	f := newScannerFixture(t)
	writeFile(t, filepath.Join(f.root, "direct.bin"), 1<<20)
	writeFile(t, filepath.Join(f.root, "alpha", "a"), 1000)
	ctx := context.Background()
	if _, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull}); err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.GetRoot(ctx, f.root)
	writeFile(t, filepath.Join(f.root, "alpha", "new"), 2000)
	if _, err := f.scanner.Scan(ctx, Request{RootPath: f.root, TargetDir: "alpha", Mode: model.ModeRebuild}); err != nil {
		t.Fatal(err)
	}
	after, _ := f.store.GetRoot(ctx, f.root)
	if after.SelfAlloc != before.SelfAlloc || after.SelfFiles != before.SelfFiles {
		t.Fatalf("root self changed on rebuild: before=%+v after=%+v", before, after)
	}
	if after.TotalAlloc < (1<<20)+3000 {
		t.Fatalf("root total lost self or target: %d", after.TotalAlloc)
	}
}

func TestEmptyRootWithDirectFile(t *testing.T) {
	f := newScannerFixture(t)
	writeFile(t, filepath.Join(f.root, "only.bin"), 4096)
	ctx := context.Background()
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalDirs != 0 || summary.FileCount != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.TotalAlloc < 4096 || summary.DataStatus != model.DataFresh {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestResumeDropsVanishedStagedDir(t *testing.T) {
	f := newScannerFixture(t)
	writeFile(t, filepath.Join(f.root, "alpha", "a"), 1000)
	writeFile(t, filepath.Join(f.root, "beta", "b"), 2000)
	ctx := context.Background()
	if _, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull}); err != nil {
		t.Fatal(err)
	}
	root, _ := f.store.GetRoot(ctx, f.root)
	oldAlpha, _ := f.store.GetDir(ctx, root.ID, "alpha")

	run, err := f.store.StartRun(ctx, f.root, storage.StartOptions{
		Mode: model.ModeFull, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetTotalDirs(ctx, run.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveDirResult(ctx, run.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: oldAlpha.AllocBytes, LogicalBytes: oldAlpha.LogicalBytes,
		FileCount: oldAlpha.FileCount, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbortRun(ctx, run.ID, model.RunInterrupted, "test interrupt"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.root, "alpha")); err != nil {
		t.Fatal(err)
	}
	summary, err := f.scanner.Scan(ctx, Request{RootPath: f.root, Mode: model.ModeFull, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.DataStatus != model.DataPartial {
		t.Fatalf("status=%s", summary.DataStatus)
	}
	after, _ := f.store.GetDir(ctx, root.ID, "alpha")
	if after.DataStatus != model.DataStale {
		t.Fatalf("alpha status=%s", after.DataStatus)
	}
	if after.AllocBytes != oldAlpha.AllocBytes {
		t.Fatalf("alpha bytes changed: %d -> %d", oldAlpha.AllocBytes, after.AllocBytes)
	}
	beta, _ := f.store.GetDir(ctx, root.ID, "beta")
	if beta.DataStatus != model.DataFresh {
		t.Fatalf("beta status=%s", beta.DataStatus)
	}
}

func TestQuickScanMarksSampledAndCountsFiles(t *testing.T) {
	f := newScannerFixture(t)
	for i := 0; i < 30; i++ {
		writeFile(t, filepath.Join(f.root, "alpha", "sub", fmt.Sprintf("f%02d.bin", i)), 4096)
	}
	f.cfg.Scan.QuickSamplePercent = 100
	var lastProgress model.Progress
	summary, err := f.scanner.Scan(context.Background(), Request{
		RootPath: f.root,
		Mode:     model.ModeQuick,
		Progress: func(p model.Progress) { lastProgress = p },
	})
	if err != nil {
		t.Fatalf("quick scan: %v", err)
	}
	if summary.Accuracy != model.AccuracySampled {
		t.Fatalf("accuracy=%s, want sampled", summary.Accuracy)
	}
	if summary.TotalDirs != 1 || summary.DoneDirs != 1 || summary.FailedDirs != 0 {
		t.Fatalf("unexpected dirs: %+v", summary)
	}
	root, err := f.store.GetRoot(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.ListDirs(context.Background(), root.ID, 0, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "alpha" {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0].FileCount != 30 {
		t.Fatalf("file count=%d", rows[0].FileCount)
	}
	if rows[0].Accuracy != model.AccuracySampled {
		t.Fatalf("row accuracy=%s", rows[0].Accuracy)
	}
	if rows[0].AllocBytes < 30*4096 {
		t.Fatalf("sampled bytes too small: %d", rows[0].AllocBytes)
	}
	if lastProgress.Bytes < 30*4096 {
		t.Fatalf("quick progress undercounts bytes: %d", lastProgress.Bytes)
	}
	if lastProgress.Bytes != summary.TotalAlloc {
		t.Fatalf("progress bytes=%d, summary total=%d", lastProgress.Bytes, summary.TotalAlloc)
	}
}

func TestProgressActiveKeepsSlowWorkerVisible(t *testing.T) {
	var counters progressCounters
	counters.startCurrent("cae365-cae365-pvc")
	counters.startCurrent("turing-data-etcd-2")
	if got := counters.getCurrent(); !strings.Contains(got, "cae365") || !strings.Contains(got, "turing") {
		t.Fatalf("both active dirs should be visible: %q", got)
	}
	counters.finishCurrent("turing-data-etcd-2")
	if got := counters.getCurrent(); got != "cae365-cae365-pvc" {
		t.Fatalf("finished worker still visible: %q", got)
	}
	counters.finishCurrent("cae365-cae365-pvc")
	if got := counters.getCurrent(); got != "" {
		t.Fatalf("active set should be empty: %q", got)
	}
}
