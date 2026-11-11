package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
)

// MergeShards combines disjoint scan shard databases into targetDB. Each shard
// must have the same root path and must contain only complete top-level
// directories. The merged root is marked exact; callers are responsible for
// ensuring hard links do not cross shard boundaries, because hardlink
// deduplication across arbitrary shards is not representable in the current
// schema.
func MergeShards(ctx context.Context, targetDB, rootPath string, inputs []string) (*model.Root, error) {
	if targetDB == "" {
		return nil, errors.New("合并目标数据库为空")
	}
	if rootPath == "" {
		return nil, errors.New("合并根路径为空")
	}
	if len(inputs) == 0 {
		return nil, errors.New("至少需要一个分片数据库")
	}
	absTarget, err := filepath.Abs(targetDB)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{absTarget: true}
	absInputs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		if input == "" {
			return nil, errors.New("分片数据库路径为空")
		}
		abs, err := filepath.Abs(input)
		if err != nil {
			return nil, err
		}
		if seen[abs] {
			return nil, fmt.Errorf("分片数据库重复: %s", input)
		}
		seen[abs] = true
		absInputs = append(absInputs, abs)
	}
	targetDB = absTarget
	inputs = absInputs

	out, err := Open(ctx, targetDB, Options{})
	if err != nil {
		return nil, err
	}
	defer out.Close()

	type shard struct {
		store *Store
		root  *model.Root
	}
	shards := make([]shard, 0, len(inputs))
	for _, input := range inputs {
		st, err := Open(ctx, input, Options{})
		if err != nil {
			return nil, fmt.Errorf("打开分片 %s: %w", input, err)
		}
		root, err := st.GetRoot(ctx, rootPath)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("分片 %s 缺少根 %s: %w", input, rootPath, err)
		}
		shards = append(shards, shard{store: st, root: root})
	}
	defer func() {
		for _, sh := range shards {
			sh.store.Close()
		}
	}()

	var totalAlloc, totalLogical, files, dirs int64
	var selfAlloc, selfLogical, selfFiles, selfSymlinks int64
	var duration time.Duration
	var lastSuccess, lastAttempt *time.Time
	for i, sh := range shards {
		if sh.root.Path != rootPath {
			return nil, fmt.Errorf("分片 %d 根路径 %s 与目标 %s 不一致", i, sh.root.Path, rootPath)
		}
		switch sh.root.Status {
		case model.DataFailed, model.DataStale, model.DataNever:
			return nil, fmt.Errorf("分片 %d 状态为 %s，不能合并", i, sh.root.Status)
		}
		totalAlloc += sh.root.TotalAlloc
		totalLogical += sh.root.TotalLogical
		files += sh.root.FileCount
		dirs += sh.root.DirCount
		selfAlloc += sh.root.SelfAlloc
		selfLogical += sh.root.SelfLogical
		selfFiles += sh.root.SelfFiles
		selfSymlinks += sh.root.SelfSymlinks
		duration += sh.root.Duration
		lastSuccess = later(lastSuccess, sh.root.LastSuccessAt)
		lastAttempt = later(lastAttempt, sh.root.LastAttemptAt)
	}

	now := time.Now()
	err = out.writeTx(ctx, func(tx *sql.Tx) error {
		nowMS := toMillis(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO roots
			(path, status, accuracy, total_alloc_bytes, total_logical_bytes, file_count, dir_count,
			 self_alloc_bytes, self_logical_bytes, self_file_count, self_symlink_count,
			 duration_ms, last_success_at, last_attempt_at, last_error, created_at, updated_at)
			VALUES (?, 'fresh', 'exact', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
			ON CONFLICT(path) DO UPDATE SET
			 status='fresh', accuracy='exact',
			 total_alloc_bytes=excluded.total_alloc_bytes, total_logical_bytes=excluded.total_logical_bytes,
			 file_count=excluded.file_count, dir_count=excluded.dir_count,
			 self_alloc_bytes=excluded.self_alloc_bytes, self_logical_bytes=excluded.self_logical_bytes,
			 self_file_count=excluded.self_file_count, self_symlink_count=excluded.self_symlink_count,
			 duration_ms=excluded.duration_ms, last_success_at=excluded.last_success_at,
			 last_attempt_at=excluded.last_attempt_at, last_error='', updated_at=excluded.updated_at`,
			rootPath, totalAlloc, totalLogical, files, dirs,
			selfAlloc, selfLogical, selfFiles, selfSymlinks, duration.Milliseconds(),
			nullMillis(lastSuccess), nullMillis(lastAttempt), nowMS, nowMS); err != nil {
			return err
		}
		var rootID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM roots WHERE path=?`, rootPath).Scan(&rootID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM top_dirs WHERE root_id=?`, rootID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dir_index WHERE root_id=?`, rootID); err != nil {
			return err
		}

		topStmt, err := tx.PrepareContext(ctx, `INSERT INTO top_dirs
			(root_id, name, alloc_bytes, logical_bytes, file_count, dir_count, symlink_count,
			 duration_ms, truncated, data_status, accuracy, last_success_at, last_attempt_at,
			 last_error, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer topStmt.Close()
		for _, sh := range shards {
			rows, err := sh.store.ListDirs(ctx, sh.root.ID, 0, model.BasisAllocated)
			if err != nil {
				return err
			}
			for _, d := range rows {
				updated := now
				if d.LastSuccessAt != nil {
					updated = *d.LastSuccessAt
				}
				if _, err := topStmt.ExecContext(ctx, rootID, d.Name, d.AllocBytes, d.LogicalBytes,
					d.FileCount, d.DirCount, d.SymlinkCount, d.Duration.Milliseconds(),
					boolInt(d.Truncated), string(d.DataStatus), string(d.Accuracy),
					nullMillis(d.LastSuccessAt), nullMillis(d.LastAttemptAt), d.LastError,
					toMillis(updated)); err != nil {
					return err
				}
			}
		}

		for _, sh := range shards {
			rows, err := sh.store.rdb.QueryContext(ctx, `SELECT `+dirIndexColumns+` FROM dir_index WHERE root_id=?`, sh.root.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				e, err := scanDirIndex(rows)
				if err != nil {
					rows.Close()
					return err
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO dir_index
					(root_id, path, parent_path, name, depth, alloc_bytes, logical_bytes,
					 file_count, dir_count, symlink_count, data_status, accuracy, truncated,
					 run_id, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					rootID, e.Path, e.ParentPath, e.Name, e.Depth, e.AllocBytes, e.LogicalBytes,
					e.FileCount, e.DirCount, e.SymlinkCount, string(e.DataStatus), string(e.Accuracy),
					boolInt(e.Truncated), 0, toMillis(e.UpdatedAt))
				if err != nil {
					rows.Close()
					return err
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out.GetRoot(ctx, rootPath)
}

func later(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil || !b.After(*a) {
		return a
	}
	return b
}

func nullMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return toMillis(*t)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
