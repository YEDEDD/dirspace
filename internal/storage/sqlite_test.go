package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
)

func TestOpenAndEmptyRun(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "test.db")
	store, err := Open(context.Background(), db, Options{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.StartRun(context.Background(), filepath.Join(dir, "root"), StartOptions{Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID == 0 {
		t.Fatal("no run id")
	}
	summary, err := store.CompleteRun(context.Background(), run.ID, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Root.Status != "fresh" {
		t.Fatalf("status=%s", summary.Root.Status)
	}
}

func TestLiveDirectoryProgressIsNotCompleted(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(context.Background(), filepath.Join(dir, "test.db"), Options{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run, err := store.StartRun(ctx, filepath.Join(dir, "root"), StartOptions{Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDirProgress(ctx, run.ID, []model.DirOutcome{
		{Name: "alpha", AllocBytes: 100, LogicalBytes: 200, FileCount: 10, DirCount: 2},
		{Name: "beta", AllocBytes: 50, LogicalBytes: 70, FileCount: 5, DirCount: 1},
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := store.StagedDirs(ctx, run.ID, 0, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 0 {
		t.Fatalf("live rows leaked into completed staged dirs: %+v", completed)
	}
	live, err := store.StagedProgressDirs(ctx, run.ID, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || !live[0].InProgress || live[0].FileCount != 10 {
		t.Fatalf("live rows = %+v", live)
	}
	if err := store.SaveDirResult(ctx, run.ID, model.DirOutcome{
		Name: "alpha", AllocBytes: 120, LogicalBytes: 220, FileCount: 11, DirCount: 2,
		CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	completed, err = store.StagedDirs(ctx, run.ID, 0, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].Name != "alpha" || completed[0].InProgress {
		t.Fatalf("completed rows = %+v", completed)
	}
	live, err = store.StagedProgressDirs(ctx, run.ID, model.BasisAllocated)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Name != "beta" {
		t.Fatalf("live rows after completion = %+v", live)
	}
}
