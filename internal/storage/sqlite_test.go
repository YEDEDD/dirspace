package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
	summary, err := store.CompleteRun(context.Background(), run.ID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Root.Status != "fresh" {
		t.Fatalf("status=%s", summary.Root.Status)
	}
}
