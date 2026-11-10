package service

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/YEDEDD/dirspace/internal/storage"
)

// CleanupHistory removes old history/run records but keeps current cache rows.
func (s *Service) CleanupHistory(ctx context.Context, days int, vacuum bool) (int64, int64, error) {
	if days <= 0 {
		days = s.Cfg.Database.RetentionDays
	}
	if days < 1 {
		days = 90
	}
	if _, err := s.PruneStale(ctx, 0); err != nil {
		return 0, 0, err
	}
	before := time.Now().AddDate(0, 0, -days)
	return s.Store.CleanupHistory(ctx, before, vacuum)
}

// PruneStale removes old deleted/failed directory rows. It never removes a row
// that currently has a successful baseline.
func (s *Service) PruneStale(ctx context.Context, rootID int64) (int64, error) {
	days := s.Cfg.Database.StaleRetentionDays
	if days < 1 {
		days = 7
	}
	before := time.Now().AddDate(0, 0, -days)
	return s.Store.PruneStaleDirs(ctx, rootID, before)
}

// Backup writes a consistent SQLite copy to a local path.
func (s *Service) Backup(ctx context.Context, destination string) error {
	return s.Store.Backup(ctx, destination)
}

// QuickCheck validates SQLite page structure.
func (s *Service) QuickCheck(ctx context.Context) ([]string, error) {
	return s.Store.QuickCheck(ctx)
}

// RepairDatabase moves a damaged database (and its WAL sidecars) aside and
// creates a fresh cache. The caller must close the current Store first.
func RepairDatabase(ctx context.Context, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("数据库不存在，无法修复: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("数据库路径是目录: %s", path)
	}
	suffix := time.Now().Format("20060102-150405")
	target := path + ".corrupt-" + suffix
	if err := os.Rename(path, target); err != nil {
		return "", fmt.Errorf("移走损坏数据库失败: %w", err)
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			if err := os.Rename(sidecar, target+"-"+sidecar[len(path)+1:]); err != nil {
				return target, fmt.Errorf("移走数据库边车文件 %s 失败: %w", sidecar, err)
			}
		}
	}
	store, err := storage.Open(ctx, path, storage.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		return target, fmt.Errorf("重建数据库失败: %w", err)
	}
	if err := store.Close(); err != nil {
		return target, err
	}
	return target, nil
}
