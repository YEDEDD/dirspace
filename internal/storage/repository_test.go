package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "usage.db")
	store, err := Open(context.Background(), db, Options{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func runWithDir(t *testing.T, store *Store, rootPath, name string, alloc int64, dirErr error) *model.ScanSummary {
	t.Helper()
	ctx := context.Background()
	run, err := store.StartRun(ctx, rootPath, StartOptions{Mode: model.ModeFull, SizeBasis: model.BasisAllocated})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	out := model.DirOutcome{
		Name:        name,
		AllocBytes:  alloc,
		FileCount:   2,
		DirCount:    1,
		Duration:    10 * time.Millisecond,
		Err:         dirErr,
		CompletedAt: time.Now(),
	}
	if err := store.SaveDirResult(ctx, run.ID, out); err != nil {
		t.Fatalf("save dir: %v", err)
	}
	summary, err := store.CompleteRun(ctx, run.ID, false, false, nil)
	if err != nil {
		t.Fatalf("complete run: %v", err)
	}
	return summary
}

func TestCompleteRunAndIsolation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	runWithDir(t, store, rootA, "pvc-one", 1000, nil)
	runWithDir(t, store, rootB, "pvc-one", 2000, nil)

	a, err := store.GetRoot(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.GetRoot(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("roots share an id")
	}
	da, err := store.GetDir(ctx, a.ID, "pvc-one")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.GetDir(ctx, b.ID, "pvc-one")
	if err != nil {
		t.Fatal(err)
	}
	if da.AllocBytes != 1000 || db.AllocBytes != 2000 {
		t.Fatalf("isolated values wrong: %d %d", da.AllocBytes, db.AllocBytes)
	}
}

func TestFailurePreservesPreviousBytes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	runWithDir(t, store, root, "alpha", 4096, nil)
	runWithDir(t, store, root, "alpha", 0, errors.New("permission denied"))
	r, err := store.GetRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.GetDir(ctx, r.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if d.AllocBytes != 4096 {
		t.Fatalf("failed scan zeroed old data: %d", d.AllocBytes)
	}
	if d.DataStatus != model.DataStale && d.DataStatus != model.DataFailed {
		t.Fatalf("unexpected status: %s", d.DataStatus)
	}
	if d.LastError == "" {
		t.Fatal("missing error")
	}
	if r.TotalAlloc != 0 {
		t.Fatalf("failed-only root total should exclude hidden rows: %d", r.TotalAlloc)
	}
}

func TestMissingDirMarksStaleAndPreservesBytes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	runWithDir(t, store, root, "alpha", 1234, nil)
	// A later complete run that no longer sees alpha.
	run, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull, SizeBasis: model.BasisAllocated})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDirResult(ctx, run.ID, model.DirOutcome{
		Name: "beta", AllocBytes: 10, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteRun(ctx, run.ID, false, false, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := store.GetRoot(ctx, root)
	d, err := store.GetDir(ctx, r.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if d.AllocBytes != 1234 || d.DataStatus != model.DataStale {
		t.Fatalf("missing dir not preserved: %+v", d)
	}
	if r.Status != model.DataPartial || r.TotalAlloc != 10 {
		t.Fatalf("root partial state wrong: %+v", r)
	}
}

func TestStaleDirsAreHiddenAndPrunable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	runWithDir(t, store, root, "alpha", 1234, nil)
	run, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull, SizeBasis: model.BasisAllocated})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDirResult(ctx, run.ID, model.DirOutcome{
		Name: "beta", AllocBytes: 10, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteRun(ctx, run.ID, false, false, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := store.GetRoot(ctx, root)
	dirs, err := store.ListDirs(ctx, r.ID, 0, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if d.Name == "alpha" {
			t.Fatal("stale alpha should be hidden from list")
		}
	}
	if n, _ := store.CountStaleDirs(ctx, r.ID); n != 1 {
		t.Fatalf("stale count=%d", n)
	}
	if _, err := store.PruneStaleDirs(ctx, r.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDir(ctx, r.ID, "alpha"); err == nil {
		t.Fatal("stale row was not pruned")
	}
}

func TestHardlinkClaimOnlyOnce(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, filepath.Join(t.TempDir(), "root"), StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimHardlink(ctx, run.ID, 1, 2, "alpha")
	if err != nil || !first {
		t.Fatalf("first claim: %v %v", first, err)
	}
	second, err := store.ClaimHardlink(ctx, run.ID, 1, 2, "beta")
	if err != nil || second {
		t.Fatalf("second claim should be false: %v %v", second, err)
	}
}

func TestBackupAndCleanup(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	runWithDir(t, store, root, "alpha", 10, nil)
	runWithDir(t, store, root, "alpha", 20, nil)
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := store.Backup(ctx, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	backupStore, err := Open(ctx, backup, Options{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer backupStore.Close()
	if _, err := backupStore.GetRoot(ctx, root); err != nil {
		t.Fatalf("backup missing root: %v", err)
	}
	history, runs, err := store.CleanupHistory(ctx, time.Now().Add(time.Hour), false)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if history == 0 || runs == 0 {
		t.Fatalf("cleanup did nothing: history=%d runs=%d", history, runs)
	}
	if _, err := store.GetRoot(ctx, root); err != nil {
		t.Fatalf("cleanup removed current cache: %v", err)
	}
}

func TestResumeStaging(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	run, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDirResult(ctx, run.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 100, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortRun(ctx, run.ID, model.RunInterrupted, "test interrupt"); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || resumed.ID != run.ID {
		t.Fatalf("run not resumed: %+v", resumed)
	}
	completed, err := store.CompletedDirs(ctx, resumed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed["alpha"].AllocBytes != 100 {
		t.Fatalf("staged result lost: %+v", completed)
	}
}

func TestResumeRequiresSameScanMode(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	quick, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeQuick})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDirResult(ctx, quick.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 100, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortRun(ctx, quick.ID, model.RunInterrupted, "test interrupt"); err != nil {
		t.Fatal(err)
	}
	full, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if full.Resumed || full.ID == quick.ID || full.Mode != model.ModeFull {
		t.Fatalf("quick staging must not be resumed as a full scan: %+v", full)
	}
	completed, err := store.CompletedDirs(ctx, full.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 0 {
		t.Fatalf("new mode reused staging from another mode: %+v", completed)
	}
}

func TestCompleteRunRejectsCanceledControl(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	run, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetControl(ctx, run.RootID, model.ControlCancel); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteRun(ctx, run.ID, false, false, nil); !errors.Is(err, model.ErrCanceled) {
		t.Fatalf("expected canceled publish, got %v", err)
	}
	if n, err := store.CountDirs(ctx, run.RootID); err != nil || n != 0 {
		t.Fatalf("canceled publish must not commit rows: n=%d err=%v", n, err)
	}
	if got, err := store.GetRun(ctx, run.ID); err != nil || got.Status != model.RunRunning {
		t.Fatalf("publish guard should roll back the run write: %+v %v", got, err)
	}
}

func TestCompleteRunRejectsInactiveRun(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	run, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AbortRun(ctx, run.ID, model.RunInterrupted, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteRun(ctx, run.ID, false, false, nil); err == nil {
		t.Fatal("expected publish to reject an interrupted run")
	}
	if n, err := store.CountDirs(ctx, run.RootID); err != nil || n != 0 {
		t.Fatalf("inactive publish must not commit rows: n=%d err=%v", n, err)
	}
}

func TestCanceledRunResumeRequiresOptIn(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	canceled, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AbortRun(ctx, canceled.ID, model.RunCanceled, "user cancel"); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Resumed || fresh.ID == canceled.ID {
		t.Fatalf("daemon-style resume must not auto-resume a canceled run: %+v", fresh)
	}
	if err := store.AbortRun(ctx, fresh.ID, model.RunCanceled, "user cancel"); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.StartRun(ctx, root, StartOptions{
		Mode: model.ModeFull, Resume: true, ResumeCanceled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || resumed.ID != fresh.ID {
		t.Fatalf("explicit resume should reuse the canceled run: %+v", resumed)
	}
}

func TestResumeDoesNotSkipNewerCompletedRun(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	first, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDirResult(ctx, first.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 100, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortRun(ctx, first.ID, model.RunInterrupted, "interrupted"); err != nil {
		t.Fatal(err)
	}
	second, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDirResult(ctx, second.ID, model.DirOutcome{
		Name: "beta", AllocBytes: 200, FileCount: 1, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteRun(ctx, second.ID, false, false, nil); err != nil {
		t.Fatal(err)
	}
	next, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if next.Resumed || next.ID == first.ID || next.ID == second.ID {
		t.Fatalf("resumed an older run instead of starting a new one: %+v", next)
	}
	completed, err := store.CompletedDirs(ctx, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 0 {
		t.Fatalf("new run reused stale staging: %+v", completed)
	}
}

func TestMigrateV1AddsSelfColumns(t *testing.T) {
	db := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE roots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL DEFAULT 'never',
		accuracy TEXT NOT NULL DEFAULT 'unknown',
		total_alloc_bytes INTEGER NOT NULL DEFAULT 0,
		total_logical_bytes INTEGER NOT NULL DEFAULT 0,
		file_count INTEGER NOT NULL DEFAULT 0,
		dir_count INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		last_success_at INTEGER,
		last_attempt_at INTEGER,
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO roots(path, created_at, updated_at) VALUES(?,?,?)`,
		"/old", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), db, Options{BusyTimeout: time.Second})
	if err != nil {
		t.Fatalf("open migration: %v", err)
	}
	defer store.Close()
	root, err := store.GetRoot(context.Background(), "/old")
	if err != nil {
		t.Fatalf("get old root: %v", err)
	}
	if root.SelfAlloc != 0 || root.SelfFiles != 0 {
		t.Fatalf("unexpected self: %+v", root)
	}
	var version int
	if err := store.wdb.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("version=%d", version)
	}
}

func TestMigrateV2AddsRunSelfTransient(t *testing.T) {
	db := filepath.Join(t.TempDir(), "old2.db")
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE roots (
		id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL DEFAULT 'never', accuracy TEXT NOT NULL DEFAULT 'unknown',
		total_alloc_bytes INTEGER NOT NULL DEFAULT 0, total_logical_bytes INTEGER NOT NULL DEFAULT 0,
		file_count INTEGER NOT NULL DEFAULT 0, dir_count INTEGER NOT NULL DEFAULT 0,
		self_alloc_bytes INTEGER NOT NULL DEFAULT 0, self_logical_bytes INTEGER NOT NULL DEFAULT 0,
		self_file_count INTEGER NOT NULL DEFAULT 0, self_symlink_count INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0, last_success_at INTEGER, last_attempt_at INTEGER,
		last_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	);
	CREATE TABLE scan_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		root_id INTEGER NOT NULL,
		mode TEXT NOT NULL, status TEXT NOT NULL,
		size_basis TEXT NOT NULL DEFAULT 'allocated',
		resumed INTEGER NOT NULL DEFAULT 0,
		started_at INTEGER NOT NULL, finished_at INTEGER, heartbeat_at INTEGER NOT NULL,
		total_dirs INTEGER NOT NULL DEFAULT 0, processed_dirs INTEGER NOT NULL DEFAULT 0,
		total_files INTEGER NOT NULL DEFAULT 0, total_bytes INTEGER NOT NULL DEFAULT 0,
		self_alloc_bytes INTEGER NOT NULL DEFAULT 0, self_logical_bytes INTEGER NOT NULL DEFAULT 0,
		self_file_count INTEGER NOT NULL DEFAULT 0, self_symlink_count INTEGER NOT NULL DEFAULT 0,
		current_dir TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0
	);
	PRAGMA user_version=2;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), db, Options{BusyTimeout: time.Second})
	if err != nil {
		t.Fatalf("open v2 migration: %v", err)
	}
	defer store.Close()
	var transient, completed int
	if err := store.wdb.QueryRow(`SELECT self_transient_errors, self_completed FROM scan_runs LIMIT 1`).
		Scan(&transient, &completed); err != nil && err != sql.ErrNoRows {
		t.Fatalf("new columns missing: %v", err)
	}
	var version int
	if err := store.wdb.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("version=%d", version)
	}
}

func TestListDirsSortsBySelectedBasis(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root, err := store.StartRun(ctx, "/sort", StartOptions{Mode: model.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	now := nowMillis()
	for _, row := range []struct {
		name           string
		alloc, logical int64
	}{
		{"alpha", 100, 900},
		{"beta", 900, 100},
	} {
		if _, err := store.wdb.ExecContext(ctx, `INSERT INTO top_dirs
			(root_id, name, alloc_bytes, logical_bytes, file_count, dir_count, data_status,
			 accuracy, last_success_at, last_attempt_at, updated_at)
			VALUES(?,?,?,?,1,0,'fresh','exact',?,?,?)`,
			root.RootID, row.name, row.alloc, row.logical, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	allocated, err := store.ListDirs(ctx, root.RootID, 10, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if allocated[0].Name != "beta" {
		t.Fatalf("allocated order=%v", allocated)
	}
	logical, err := store.ListDirs(ctx, root.RootID, 10, model.BasisLogical)
	if err != nil {
		t.Fatal(err)
	}
	if logical[0].Name != "alpha" {
		t.Fatalf("logical order=%v", logical)
	}
}

func TestAddInventoryBatchChunks(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	records := make([]InventoryRecord, 0, 2500)
	for i := 0; i < 2500; i++ {
		records = append(records, InventoryRecord{
			RootID: 1, RunID: 1, TopDir: "d", RelPath: fmt.Sprintf("f%04d", i),
			SizeBytes: 1, AllocBytes: 4096, Inode: uint64(i + 1), Dev: 1, Nlink: 1,
		})
	}
	if err := store.AddInventoryBatch(ctx, records); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := store.wdb.QueryRow(`SELECT COUNT(*) FROM inventory`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(records) {
		t.Fatalf("rows=%d", n)
	}
}

func TestActiveRunIgnoresStaleHeartbeat(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	run, err := store.StartRun(ctx, root, StartOptions{Mode: model.ModeFull, SizeBasis: model.BasisAllocated})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Minute).UnixMilli()
	if _, err := store.wdb.ExecContext(ctx, `UPDATE scan_runs SET heartbeat_at=? WHERE id=?`, old, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActiveRun(ctx, run.RootID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale active run should not be live: %v", err)
	}
}

func TestStagedDirsReturnsPartialRows(t *testing.T) {
	store := testStore(t)
	root := filepath.Join(t.TempDir(), "partial-root")
	run, err := store.StartRun(context.Background(), root, StartOptions{
		Mode: model.ModeQuick, SizeBasis: model.BasisAllocated,
	})
	if err != nil {
		t.Fatal(err)
	}
	longName := "turing-data-etcd-2-pvc-4c25346d-d5c0-4876-b39e-02174655b88e"
	if err := store.SaveDirResult(context.Background(), run.ID, model.DirOutcome{
		Name: longName, AllocBytes: 7 << 40, FileCount: 12, DirCount: 3,
		CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.StagedDirs(context.Background(), run.ID, 10, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != longName {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0].AllocBytes != 7<<40 || rows[0].FileCount != 12 {
		t.Fatalf("unexpected staged row: %+v", rows[0])
	}
}
