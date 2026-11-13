package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YEDEDD/dirspace/internal/model"
)

// ErrNotFound is returned when a requested root or directory has no cache row.
var ErrNotFound = errors.New("记录不存在")

// Options configures the SQLite store.
type Options struct {
	BusyTimeout time.Duration
}

// Store owns two SQLite handles: one serialized writer and one small reader
// pool. WAL lets CLI queries read the previous committed snapshot while a scan
// writes staging rows.
type Store struct {
	path    string
	rdb     *sql.DB
	wdb     *sql.DB
	writeMu sync.Mutex
}

// Open opens (and initializes) the cache database.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("数据库路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("数据库路径不能是符号链接: %s", abs)
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 5 * time.Second
	}
	dsn := buildDSN(abs, opts.BusyTimeout)
	wdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	if err := wdb.PingContext(ctx); err != nil {
		wdb.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	rdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		wdb.Close()
		return nil, fmt.Errorf("打开只读连接失败: %w", err)
	}
	rdb.SetMaxOpenConns(4)
	rdb.SetMaxIdleConns(2)

	store := &Store{path: abs, rdb: rdb, wdb: wdb}
	if err := store.initialize(ctx); err != nil {
		store.Close()
		return nil, err
	}
	_ = os.Chmod(abs, 0o600)
	return store, nil
}

func buildDSN(path string, busy time.Duration) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "temp_store(MEMORY)")
	q.Add("_pragma", "cache_size(-8192)")
	u.RawQuery = q.Encode()
	return u.String()
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, ddl string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+ddl)
	return err
}

func (s *Store) initialize(ctx context.Context) error {
	var version int
	if err := s.wdb.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本失败: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("数据库版本 %d 高于程序支持的版本 %d，请升级 dirspace", version, schemaVersion)
	}
	// Fast path for the common read-only query: an already-current schema needs
	// no write transaction. This keeps `dirspace top` off the scanner's write
	// lock and avoids paying schema DDL on every CLI invocation.
	if version == schemaVersion {
		var tables int
		if err := s.wdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name IN
			('roots','scan_runs','run_dirs','top_dirs','dir_index','dir_history','hardlink_seen','control','inventory')`).
			Scan(&tables); err != nil {
			return fmt.Errorf("检查数据库结构失败: %w", err)
		}
		if tables == 9 {
			return nil
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range schemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("初始化数据库失败: %w", err)
		}
	}
	for _, col := range schemaColumns {
		if err := ensureColumn(ctx, tx, col.table, col.name, col.ddl); err != nil {
			return fmt.Errorf("升级数据库列 %s.%s 失败: %w", col.table, col.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		return fmt.Errorf("写入数据库版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交数据库初始化失败: %w", err)
	}
	return nil
}

// Path returns the absolute database path.
func (s *Store) Path() string { return s.path }

// Dir returns the directory containing the database.
func (s *Store) Dir() string { return filepath.Dir(s.path) }

// Close closes both handles. It deliberately does not checkpoint: a read-only
// CLI query must not take a write lock or pay WAL checkpoint latency on exit.
// Scans checkpoint explicitly after committing their results.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var first error
	if s.wdb != nil {
		if err := s.wdb.Close(); err != nil {
			first = err
		}
	}
	if s.rdb != nil {
		if err := s.rdb.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Checkpoint runs a passive WAL checkpoint; it never blocks readers for long.
func (s *Store) Checkpoint(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.wdb.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
	return err
}

func (s *Store) writeTx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nowMillis() int64 { return time.Now().UTC().UnixMilli() }

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

func fromMillis(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func nullableTime(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 == 0 {
		return nil
	}
	t := fromMillis(v.Int64)
	return &t
}

func nullInt(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

// ---------------------------------------------------------------------------
// Roots

const rootColumns = `id, path, status, accuracy, total_alloc_bytes, total_logical_bytes,
	file_count, dir_count, self_alloc_bytes, self_logical_bytes, self_file_count,
	self_symlink_count, duration_ms, last_success_at, last_attempt_at,
	last_error, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRoot(row rowScanner) (*model.Root, error) {
	var root model.Root
	var status, accuracy string
	var lastSuccess, lastAttempt sql.NullInt64
	var created, updated int64
	var duration int64
	err := row.Scan(
		&root.ID, &root.Path, &status, &accuracy, &root.TotalAlloc, &root.TotalLogical,
		&root.FileCount, &root.DirCount,
		&root.SelfAlloc, &root.SelfLogical, &root.SelfFiles, &root.SelfSymlinks,
		&duration, &lastSuccess, &lastAttempt,
		&root.LastError, &created, &updated,
	)
	if err != nil {
		return nil, err
	}
	root.Status = model.DataStatus(status)
	root.Accuracy = model.Accuracy(accuracy)
	root.Duration = time.Duration(duration) * time.Millisecond
	root.LastSuccessAt = nullableTime(lastSuccess)
	root.LastAttemptAt = nullableTime(lastAttempt)
	root.CreatedAt = fromMillis(created)
	root.UpdatedAt = fromMillis(updated)
	return &root, nil
}

// GetRoot looks up a root by its exact normalized path.
func (s *Store) GetRoot(ctx context.Context, path string) (*model.Root, error) {
	root, err := scanRoot(s.rdb.QueryRowContext(ctx, `SELECT `+rootColumns+` FROM roots WHERE path = ?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return root, nil
}

// GetRootByID looks up a root by id.
func (s *Store) GetRootByID(ctx context.Context, id int64) (*model.Root, error) {
	root, err := scanRoot(s.rdb.QueryRowContext(ctx, `SELECT `+rootColumns+` FROM roots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return root, err
}

// ListRoots returns every known root ordered by path.
func (s *Store) ListRoots(ctx context.Context) ([]model.Root, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+rootColumns+` FROM roots ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Root
	for rows.Next() {
		root, err := scanRoot(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *root)
	}
	return result, rows.Err()
}

// FindRootAncestor returns the closest known root equal to or above path.
func (s *Store) FindRootAncestor(ctx context.Context, path string) (*model.Root, error) {
	roots, err := s.ListRoots(ctx)
	if err != nil {
		return nil, err
	}
	var best *model.Root
	for i := range roots {
		if path == roots[i].Path || strings.HasPrefix(path, roots[i].Path+string(filepath.Separator)) {
			if best == nil || len(roots[i].Path) > len(best.Path) {
				copy := roots[i]
				best = &copy
			}
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// ---------------------------------------------------------------------------
// Scan runs

const runColumns = `r.id, r.root_id, r.mode, r.status, r.started_at, r.finished_at,
	r.heartbeat_at, r.total_dirs, r.processed_dirs, r.total_files, r.total_bytes,
	r.self_alloc_bytes, r.self_logical_bytes, r.self_file_count, r.self_symlink_count,
	r.self_transient_errors, r.self_completed, r.current_dir, r.error, r.resumed, r.size_basis, r.duration_ms,
	r.metadata_ops, r.metadata_qps, r.metadata_limit, r.initial_metadata_limit, r.active_workers, roots.path`

func scanRun(row rowScanner) (*model.Run, error) {
	var run model.Run
	var mode, status, basis, rootPath string
	var started, heartbeat int64
	var finished sql.NullInt64
	var resumed int
	var selfCompleted int
	var duration int64
	err := row.Scan(
		&run.ID, &run.RootID, &mode, &status, &started, &finished,
		&heartbeat, &run.TotalDirs, &run.ProcessedDirs, &run.TotalFiles, &run.TotalBytes,
		&run.SelfAlloc, &run.SelfLogical, &run.SelfFiles, &run.SelfSymlinks,
		&run.SelfTransient, &selfCompleted,
		&run.CurrentDir, &run.Error, &resumed, &basis, &duration,
		&run.MetadataOps, &run.MetadataQPS, &run.MetadataLimit, &run.InitialMetadataLimit, &run.ActiveWorkers, &rootPath,
	)
	if err != nil {
		return nil, err
	}
	run.Mode = model.ScanMode(mode)
	run.Status = model.RunStatus(status)
	run.StartedAt = fromMillis(started)
	run.HeartbeatAt = fromMillis(heartbeat)
	run.FinishedAt = nullableTime(finished)
	run.Resumed = resumed != 0
	run.SelfCompleted = selfCompleted != 0
	run.SizeBasis = model.SizeBasis(basis)
	run.Duration = time.Duration(duration) * time.Millisecond
	run.RootPath = rootPath
	if run.Duration == 0 && run.FinishedAt != nil {
		run.Duration = run.FinishedAt.Sub(run.StartedAt)
	}
	return &run, nil
}

// StartOptions configures a new or resumed run.
type StartOptions struct {
	Mode           model.ScanMode
	SizeBasis      model.SizeBasis
	Resume         bool
	ResumeCanceled bool
	MetadataLimit  float64
}

// StartRun creates a run, or resumes the newest interrupted compatible run.
func (s *Store) StartRun(ctx context.Context, rootPath string, opts StartOptions) (*model.Run, error) {
	if opts.SizeBasis != model.BasisLogical {
		opts.SizeBasis = model.BasisAllocated
	}
	if opts.Mode == "" {
		opts.Mode = model.ModeFull
	}
	var runID int64
	now := nowMillis()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO roots(path, status, accuracy, created_at, updated_at)
			VALUES(?, 'never', 'unknown', ?, ?)
			ON CONFLICT(path) DO UPDATE SET updated_at=excluded.updated_at`, rootPath, now, now); err != nil {
			return err
		}
		var rootID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM roots WHERE path=?`, rootPath).Scan(&rootID); err != nil {
			return err
		}
		if opts.Resume && opts.Mode != model.ModeRebuild {
			// Only the newest run may be resumed. Skipping over a newer
			// completed/failed run and resuming an older one could publish stale
			// staging after a newer successful scan.
			var existing int64
			var existingStatus, existingMode string
			err := tx.QueryRowContext(ctx, `SELECT id, status, mode FROM scan_runs
				WHERE root_id=? AND size_basis=? ORDER BY id DESC LIMIT 1`, rootID, string(opts.SizeBasis)).
				Scan(&existing, &existingStatus, &existingMode)
			// A run may only be resumed by the same scan mode. Resuming a quick
			// staging row as a full scan (or vice versa) would publish sampled
			// numbers as exact, and the newest run is the only safe resume point.
			if err == nil && existingMode == string(opts.Mode) &&
				resumableRunStatus(model.RunStatus(existingStatus), opts.ResumeCanceled) {
				runID = existing
				if _, err := tx.ExecContext(ctx, `UPDATE scan_runs SET status='running', resumed=1,
					heartbeat_at=?, finished_at=NULL, error='', metadata_limit=? WHERE id=?`,
					now, opts.MetadataLimit, runID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE scan_runs SET status='interrupted', finished_at=?,
					error=CASE WHEN error='' THEN '扫描进程已退出' ELSE error END
					WHERE root_id=? AND id<>? AND status IN ('running','paused')`, now, rootID, runID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE roots SET status='scanning', last_attempt_at=?, updated_at=? WHERE id=?`, now, now, rootID); err != nil {
					return err
				}
				return s.setControlTx(ctx, tx, rootID, model.ControlRunning, now)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		// A non-resumed run invalidates old staging data. Cache rows in
		// top_dirs are never touched here.
		if _, err := tx.ExecContext(ctx, `UPDATE scan_runs SET status='interrupted', finished_at=?,
			error=CASE WHEN error='' THEN '扫描进程已退出' ELSE error END
			WHERE root_id=? AND status IN ('running','paused')`, now, rootID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM run_dirs WHERE run_id IN
			(SELECT id FROM scan_runs WHERE root_id=?)`, rootID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM hardlink_seen WHERE run_id IN
			(SELECT id FROM scan_runs WHERE root_id=?)`, rootID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO scan_runs
			(root_id, mode, status, size_basis, started_at, heartbeat_at, metadata_limit, initial_metadata_limit)
			VALUES(?, ?, 'running', ?, ?, ?, ?, ?)`,
			rootID, string(opts.Mode), string(opts.SizeBasis), now, now, opts.MetadataLimit, opts.MetadataLimit)
		if err != nil {
			return err
		}
		runID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE roots SET status='scanning', last_attempt_at=?, updated_at=? WHERE id=?`, now, now, rootID); err != nil {
			return err
		}
		return s.setControlTx(ctx, tx, rootID, model.ControlRunning, now)
	})
	if err != nil {
		return nil, err
	}
	return s.GetRun(ctx, runID)
}

// GetRun returns one scan run.
func (s *Store) GetRun(ctx context.Context, id int64) (*model.Run, error) {
	run, err := scanRun(s.rdb.QueryRowContext(ctx, `SELECT `+runColumns+`
		FROM scan_runs r JOIN roots ON roots.id=r.root_id WHERE r.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return run, err
}

// ActiveRun returns the current running/paused run for a root.
func (s *Store) ActiveRun(ctx context.Context, rootID int64) (*model.Run, error) {
	// A killed scanner cannot clear its own row. Do not present a heartbeat
	// older than 90 seconds as a live task; the daemon/status recovery path
	// will mark it interrupted.
	cutoff := time.Now().UTC().Add(-90 * time.Second).UnixMilli()
	run, err := scanRun(s.rdb.QueryRowContext(ctx, `SELECT `+runColumns+`
		FROM scan_runs r JOIN roots ON roots.id=r.root_id
		WHERE r.root_id=? AND r.status IN ('running','paused') AND r.heartbeat_at >= ?
		ORDER BY r.id DESC LIMIT 1`, rootID, cutoff))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return run, err
}

// ActiveRuns returns all live runs, including runs for child roots that are
// not the configured parent root. Heartbeats older than 90 seconds are ignored,
// matching ActiveRun.
func (s *Store) ActiveRuns(ctx context.Context) ([]*model.Run, error) {
	cutoff := time.Now().UTC().Add(-90 * time.Second).UnixMilli()
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+runColumns+`
		FROM scan_runs r JOIN roots ON roots.id=r.root_id
		WHERE r.status IN ('running','paused') AND r.heartbeat_at >= ?
		ORDER BY r.id DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// LastRunByMode returns the newest run of a specific mode for a root.
func (s *Store) LastRunByMode(ctx context.Context, rootID int64, mode model.ScanMode) (*model.Run, error) {
	run, err := scanRun(s.rdb.QueryRowContext(ctx, `SELECT `+runColumns+`
		FROM scan_runs r JOIN roots ON roots.id=r.root_id
		WHERE r.root_id=? AND r.mode=? ORDER BY r.id DESC LIMIT 1`, rootID, string(mode)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return run, err
}

// LastRun returns the newest run for a root, including completed ones.
func (s *Store) LastRun(ctx context.Context, rootID int64) (*model.Run, error) {
	run, err := scanRun(s.rdb.QueryRowContext(ctx, `SELECT `+runColumns+`
		FROM scan_runs r JOIN roots ON roots.id=r.root_id
		WHERE r.root_id=? ORDER BY r.id DESC LIMIT 1`, rootID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return run, err
}

// HistoryPoint is one top-level directory value in a completed run.
type HistoryPoint struct {
	AllocBytes   int64
	LogicalBytes int64
	FileCount    int64
	Accuracy     string
}

// HistoryDiffRow compares one top-level directory between two completed runs.
type HistoryDiffRow struct {
	Name    string
	Base    HistoryPoint
	Head    HistoryPoint
	HasBase bool
	HasHead bool
}

// HistoryDiff returns the newest completed full/quick run and the newest
// completed run of the same mode at or before cutoff. It only uses cached
// history; it never touches the scanned filesystem.
func (s *Store) HistoryDiff(ctx context.Context, rootID int64, cutoff time.Time) (base, head *model.Run, rows []HistoryDiffRow, err error) {
	queryRun := func(where string, args ...any) (*model.Run, error) {
		q := `SELECT ` + runColumns + `
			FROM scan_runs r JOIN roots ON roots.id=r.root_id
			WHERE r.root_id=? AND r.status=? AND r.mode IN (?,?)
			  AND r.finished_at IS NOT NULL ` + where + `
			ORDER BY r.finished_at DESC, r.id DESC LIMIT 1`
		all := append([]any{rootID, string(model.RunCompleted), string(model.ModeFull), string(model.ModeQuick)}, args...)
		run, scanErr := scanRun(s.rdb.QueryRowContext(ctx, q, all...))
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return run, scanErr
	}
	head, err = queryRun("")
	if err != nil {
		return nil, nil, nil, err
	}
	if head.FinishedAt == nil {
		return nil, head, nil, ErrNotFound
	}
	if !head.FinishedAt.After(cutoff) {
		return nil, head, nil, ErrNotFound
	}
	base, err = queryRun("AND r.finished_at <= ? AND r.mode = ?", toMillis(cutoff), string(head.Mode))
	if err != nil {
		return nil, head, nil, err
	}
	load := func(runID int64) (map[string]HistoryPoint, error) {
		rows, queryErr := s.rdb.QueryContext(ctx, `SELECT name, alloc_bytes, logical_bytes, file_count, accuracy
			FROM dir_history WHERE run_id=?`, runID)
		if queryErr != nil {
			return nil, queryErr
		}
		defer rows.Close()
		out := make(map[string]HistoryPoint)
		for rows.Next() {
			var name string
			var p HistoryPoint
			if scanErr := rows.Scan(&name, &p.AllocBytes, &p.LogicalBytes, &p.FileCount, &p.Accuracy); scanErr != nil {
				return nil, scanErr
			}
			out[name] = p
		}
		return out, rows.Err()
	}
	baseRows, err := load(base.ID)
	if err != nil {
		return nil, head, nil, err
	}
	headRows, err := load(head.ID)
	if err != nil {
		return nil, head, nil, err
	}
	seen := make(map[string]struct{}, len(baseRows)+len(headRows))
	for name := range baseRows {
		seen[name] = struct{}{}
	}
	for name := range headRows {
		seen[name] = struct{}{}
	}
	for name := range seen {
		b, hasBase := baseRows[name]
		h, hasHead := headRows[name]
		rows = append(rows, HistoryDiffRow{Name: name, Base: b, Head: h, HasBase: hasBase, HasHead: hasHead})
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i].Head.AllocBytes, rows[j].Head.AllocBytes
		if !rows[i].HasHead {
			left = rows[i].Base.AllocBytes
		}
		if !rows[j].HasHead {
			right = rows[j].Base.AllocBytes
		}
		if left != right {
			return left > right
		}
		return rows[i].Name < rows[j].Name
	})
	return base, head, rows, nil
}

// SetRunMetadataLimit records a live QPS limit change for status/monitor.
func (s *Store) SetRunMetadataLimit(ctx context.Context, runID int64, limit float64) error {
	_, err := s.wdb.ExecContext(ctx, `UPDATE scan_runs SET metadata_limit=? WHERE id=?`, limit, runID)
	return err
}

// SetTotalDirs records how many first-level entries this run will inspect.
func (s *Store) SetTotalDirs(ctx context.Context, runID int64, total int) error {
	_, err := s.wdb.ExecContext(ctx, `UPDATE scan_runs SET total_dirs=?, heartbeat_at=? WHERE id=?`,
		total, nowMillis(), runID)
	return err
}

// CompletedDirs returns successfully staged directories, used for resume.
func (s *Store) CompletedDirs(ctx context.Context, runID int64) (map[string]model.DirOutcome, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT name, alloc_bytes, logical_bytes, file_count,
		dir_count, symlink_count, duration_ms, transient_errors, truncated, completed_at
		FROM run_dirs WHERE run_id=? AND error='' AND completed_at>0`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]model.DirOutcome)
	for rows.Next() {
		var out model.DirOutcome
		var duration, completed int64
		var truncated int
		if err := rows.Scan(&out.Name, &out.AllocBytes, &out.LogicalBytes, &out.FileCount,
			&out.DirCount, &out.SymlinkCount, &duration, &out.TransientErrs, &truncated, &completed); err != nil {
			return nil, err
		}
		out.Duration = time.Duration(duration) * time.Millisecond
		out.Truncated = truncated != 0
		out.CompletedAt = fromMillis(completed)
		result[out.Name] = out
	}
	return result, rows.Err()
}

// DeleteDirResult removes a staged directory when a resumed run discovers that
// it no longer exists. The previous published row is left untouched and will be
// marked stale by CompleteRun.
func (s *Store) DeleteDirResult(ctx context.Context, runID int64, name string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.wdb.ExecContext(ctx, `DELETE FROM run_dirs WHERE run_id=? AND name=?`, runID, name)
	return err
}

// StagedProgressDirs returns first-level directories that are still being
// scanned. They live in run_dirs with completed_at=0 and are never promoted to
// top_dirs until SaveDirResult marks them complete.
func (s *Store) StagedProgressDirs(ctx context.Context, runID int64, basis model.SizeBasis) ([]model.DirStat, error) {
	orderBy := "alloc_bytes"
	if basis == model.BasisLogical {
		orderBy = "logical_bytes"
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT name, alloc_bytes, logical_bytes, file_count,
		dir_count, symlink_count, duration_ms, truncated
		FROM run_dirs WHERE run_id=? AND error='' AND completed_at=0
		ORDER BY `+orderBy+` DESC, name ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.DirStat
	for rows.Next() {
		var d model.DirStat
		var durationMS int64
		var truncated int
		if err := rows.Scan(&d.Name, &d.AllocBytes, &d.LogicalBytes, &d.FileCount,
			&d.DirCount, &d.SymlinkCount, &durationMS, &truncated); err != nil {
			return nil, err
		}
		d.ID = runID
		d.Truncated = truncated != 0
		d.InProgress = true
		d.Duration = time.Duration(durationMS) * time.Millisecond
		d.DataStatus = model.DataPartial
		result = append(result, d)
	}
	return result, rows.Err()
}

// SaveDirProgress persists a live snapshot of unfinished first-level directory
// aggregates. A completed row is never overwritten by a later partial snapshot.
func (s *Store) SaveDirProgress(ctx context.Context, runID int64, outs []model.DirOutcome) error {
	if len(outs) == 0 {
		return nil
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, out := range outs {
			truncated := 0
			if out.Truncated {
				truncated = 1
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO run_dirs
				(run_id, name, alloc_bytes, logical_bytes, file_count, dir_count, symlink_count,
				 duration_ms, transient_errors, truncated, error, completed_at)
				VALUES(?,?,?,?,?,?,?,?,?,?, '', 0)
				ON CONFLICT(run_id, name) DO UPDATE SET
				 alloc_bytes=excluded.alloc_bytes, logical_bytes=excluded.logical_bytes,
				 file_count=excluded.file_count, dir_count=excluded.dir_count,
				 symlink_count=excluded.symlink_count, duration_ms=excluded.duration_ms,
				 transient_errors=excluded.transient_errors, truncated=excluded.truncated
				WHERE run_dirs.completed_at=0`,
				runID, out.Name, out.AllocBytes, out.LogicalBytes, out.FileCount, out.DirCount,
				out.SymlinkCount, out.Duration.Milliseconds(), out.TransientErrs, truncated); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE scan_runs SET
			processed_dirs=(SELECT COUNT(*) FROM run_dirs WHERE run_id=? AND completed_at>0),
			total_files=COALESCE((SELECT SUM(file_count) FROM run_dirs WHERE run_id=? AND error=''),0),
			total_bytes=COALESCE((SELECT SUM(alloc_bytes) FROM run_dirs WHERE run_id=? AND error=''),0),
			heartbeat_at=? WHERE id=?`,
			runID, runID, runID, nowMillis(), runID)
		return err
	})
}

// SaveDirResult atomically stages one completed directory and updates progress.
func (s *Store) SaveDirResult(ctx context.Context, runID int64, out model.DirOutcome) error {
	now := nowMillis()
	completed := toMillis(out.CompletedAt)
	if completed == 0 {
		completed = now
	}
	errText := ""
	if out.Err != nil {
		errText = out.Err.Error()
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		truncated := 0
		if out.Truncated {
			truncated = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_dirs
			(run_id, name, alloc_bytes, logical_bytes, file_count, dir_count, symlink_count,
			 duration_ms, transient_errors, truncated, error, completed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(run_id, name) DO UPDATE SET
			 alloc_bytes=excluded.alloc_bytes, logical_bytes=excluded.logical_bytes,
			 file_count=excluded.file_count, dir_count=excluded.dir_count,
			 symlink_count=excluded.symlink_count, duration_ms=excluded.duration_ms,
			 transient_errors=excluded.transient_errors, truncated=excluded.truncated,
			 error=excluded.error, completed_at=excluded.completed_at`,
			runID, out.Name, out.AllocBytes, out.LogicalBytes, out.FileCount, out.DirCount,
			out.SymlinkCount, out.Duration.Milliseconds(), out.TransientErrs, truncated, errText, completed); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE scan_runs SET
			processed_dirs=(SELECT COUNT(*) FROM run_dirs WHERE run_id=? AND completed_at>0),
			total_files=COALESCE((SELECT SUM(file_count) FROM run_dirs WHERE run_id=? AND error=''),0),
			total_bytes=COALESCE((SELECT SUM(alloc_bytes) FROM run_dirs WHERE run_id=? AND error=''),0),
			heartbeat_at=?, current_dir=? WHERE id=?`,
			runID, runID, runID, now, out.Name, runID)
		return err
	})
}

// SaveRootSelf records the direct files and directory block of the scan root.
// This is separate from first-level rows and only promoted by CompleteRun.
func (s *Store) SaveRootSelf(ctx context.Context, runID int64, out model.DirOutcome) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE scan_runs SET self_alloc_bytes=?,
			self_logical_bytes=?, self_file_count=?, self_symlink_count=?,
			self_transient_errors=?, self_completed=1,
			heartbeat_at=? WHERE id=? AND status IN ('running','paused')`,
			out.AllocBytes, out.LogicalBytes, out.FileCount, out.SymlinkCount,
			out.TransientErrs, nowMillis(), runID)
		return err
	})
}

// Heartbeat updates liveness and in-progress counters. It returns false when
// the run is no longer active (for example it was marked interrupted).
func (s *Store) Heartbeat(ctx context.Context, runID int64, currentDir string, processed, files, bytes, metadataOps int64, metadataQPS, metadataLimit float64, activeWorkers int) (bool, error) {
	res, err := s.wdb.ExecContext(ctx, `UPDATE scan_runs SET heartbeat_at=?, current_dir=?,
		processed_dirs=?, total_files=?, total_bytes=?, metadata_ops=?, metadata_qps=?, metadata_limit=?, active_workers=?
		WHERE id=? AND status IN ('running','paused')`,
		nowMillis(), currentDir, processed, files, bytes, metadataOps, metadataQPS, metadataLimit, activeWorkers, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetRunMetadata stores the final observed metadata counter and rate. It is
// called before completion so even scans shorter than one heartbeat window
// report a useful QPS in status.
func (s *Store) SetRunMetadata(ctx context.Context, runID, metadataOps int64, metadataQPS, metadataLimit float64) error {
	_, err := s.wdb.ExecContext(ctx, `UPDATE scan_runs SET metadata_ops=?, metadata_qps=?, metadata_limit=? WHERE id=?`,
		metadataOps, metadataQPS, metadataLimit, runID)
	return err
}

// ClaimHardlink atomically records an inode in the run-wide hardlink set.
// It returns true exactly once per (device, inode) for this run.
func (s *Store) ClaimHardlink(ctx context.Context, runID int64, dev, ino uint64, topDir string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.wdb.ExecContext(ctx, `INSERT INTO hardlink_seen(run_id, dev, ino, top_dir)
		VALUES(?,?,?,?) ON CONFLICT(run_id, dev, ino) DO NOTHING`,
		runID, int64(dev), int64(ino), topDir)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeleteIncompleteHardlinks removes claims made by directories that were not
// successfully staged (for example an interrupted directory).
func (s *Store) DeleteIncompleteHardlinks(ctx context.Context, runID int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.wdb.ExecContext(ctx, `DELETE FROM hardlink_seen WHERE run_id=? AND top_dir<>'.' AND top_dir NOT IN
		(SELECT name FROM run_dirs WHERE run_id=? AND error='' AND completed_at>0)`, runID, runID)
	return err
}

// DeleteHardlinkTopDir removes claims owned by a partial scan of one top-level
// scope. It is used when the root-direct-file scan did not finish before an
// interruption.
func (s *Store) DeleteHardlinkTopDir(ctx context.Context, runID int64, topDir string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.wdb.ExecContext(ctx, `DELETE FROM hardlink_seen WHERE run_id=? AND top_dir=?`, runID, topDir)
	return err
}

// SetRunStatus changes running/paused state and records a message.
func (s *Store) SetRunStatus(ctx context.Context, runID int64, status model.RunStatus, message string) error {
	_, err := s.wdb.ExecContext(ctx, `UPDATE scan_runs SET status=?, error=?, heartbeat_at=?
		WHERE id=? AND status IN ('running','paused')`, string(status), message, nowMillis(), runID)
	return err
}

// AbortRun marks a run canceled or interrupted. Cached top_dirs are untouched.
func (s *Store) AbortRun(ctx context.Context, runID int64, status model.RunStatus, reason string) error {
	if status != model.RunInterrupted && status != model.RunCanceled && status != model.RunFailed {
		return fmt.Errorf("不支持的终止状态: %s", status)
	}
	now := nowMillis()
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var rootID, started int64
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT root_id, started_at, status FROM scan_runs WHERE id=?`, runID).
			Scan(&rootID, &started, &current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if current == string(model.RunCompleted) || current == string(model.RunPartial) || current == string(model.RunFailed) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE scan_runs SET status=?, finished_at=?, heartbeat_at=?,
			error=?, duration_ms=? WHERE id=?`,
			string(status), now, now, reason, now-started, runID); err != nil {
			return err
		}
		var lastSuccess sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT last_success_at FROM roots WHERE id=?`, rootID).Scan(&lastSuccess); err != nil {
			return err
		}
		rootStatus := string(model.DataNever)
		var existingDirs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM top_dirs WHERE root_id=?`, rootID).Scan(&existingDirs); err != nil {
			return err
		}
		if (lastSuccess.Valid && lastSuccess.Int64 > 0) || existingDirs > 0 {
			rootStatus = string(model.DataStale)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE roots SET status=?, last_attempt_at=?, last_error=?, updated_at=?
			WHERE id=?`, rootStatus, now, reason, now, rootID); err != nil {
			return err
		}
		return s.setControlTx(ctx, tx, rootID, model.ControlIdle, now)
	})
}

// RecoverStaleRuns marks abandoned runs as interrupted. A live scan refreshes
// its heartbeat, so a recent heartbeat is never disturbed.
func (s *Store) RecoverStaleRuns(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		olderThan = 90 * time.Second
	}
	cutoff := time.Now().UTC().Add(-olderThan).UnixMilli()
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, root_id FROM scan_runs
		WHERE status IN ('running','paused') AND heartbeat_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	type pair struct{ runID, rootID int64 }
	var stale []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.runID, &p.rootID); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := nowMillis()
	for _, p := range stale {
		err := s.writeTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE scan_runs SET status='interrupted', finished_at=?,
				heartbeat_at=?, error=CASE WHEN error='' THEN '进程心跳超时，任务已中断，可执行 scan --resume'
				ELSE error END WHERE id=? AND status IN ('running','paused')`, now, now, p.runID); err != nil {
				return err
			}
			var lastSuccess sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT last_success_at FROM roots WHERE id=?`, p.rootID).Scan(&lastSuccess); err != nil {
				return err
			}
			status := string(model.DataNever)
			var existingDirs int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM top_dirs WHERE root_id=?`, p.rootID).Scan(&existingDirs); err != nil {
				return err
			}
			if (lastSuccess.Valid && lastSuccess.Int64 > 0) || existingDirs > 0 {
				status = string(model.DataStale)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE roots SET status=?, last_attempt_at=?, updated_at=?
				WHERE id=? AND status='scanning'`, status, now, now, p.rootID); err != nil {
				return err
			}
			return s.setControlTx(ctx, tx, p.rootID, model.ControlIdle, now)
		})
		if err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// CompleteRun atomically promotes staged results into the cache. Failed
// directories keep their previous bytes and are marked stale, never zero.
func (s *Store) CompleteRun(ctx context.Context, runID int64, single, preserveMissing bool, canceled func() bool) (*model.ScanSummary, error) {
	now := time.Now().UTC()
	nowMS := toMillis(now)
	var rootID int64
	failedDirs := 0
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		if canceled != nil && canceled() {
			return model.ErrCanceled
		}
		var mode, basis, storedStatus string
		var started, resumed int64
		var selfAlloc, selfLogical, selfFiles, selfSymlinks, selfTransient int64
		if err := tx.QueryRowContext(ctx, `SELECT root_id, mode, size_basis, status, started_at, resumed,
			self_alloc_bytes, self_logical_bytes, self_file_count, self_symlink_count,
			self_transient_errors
			FROM scan_runs WHERE id=?`, runID).Scan(&rootID, &mode, &basis, &storedStatus, &started, &resumed,
			&selfAlloc, &selfLogical, &selfFiles, &selfSymlinks, &selfTransient); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if storedStatus != string(model.RunRunning) && storedStatus != string(model.RunPaused) {
			return fmt.Errorf("扫描任务已不处于运行状态: %s", storedStatus)
		}
		var control string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT state FROM control WHERE root_id=?), 'idle')`, rootID).Scan(&control); err != nil {
			return err
		}
		if control == string(model.ControlCancel) {
			return model.ErrCanceled
		}
		type dirRow struct {
			name      string
			alloc     int64
			logical   int64
			files     int64
			dirs      int64
			symlinks  int64
			duration  int64
			transient int64
			truncated int
			errText   string
			completed int64
		}
		var staged []dirRow
		rows, err := tx.QueryContext(ctx, `SELECT name, alloc_bytes, logical_bytes, file_count,
			dir_count, symlink_count, duration_ms, transient_errors, truncated, error, completed_at
			FROM run_dirs WHERE run_id=? AND completed_at>0`, runID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d dirRow
			if err := rows.Scan(&d.name, &d.alloc, &d.logical, &d.files, &d.dirs, &d.symlinks,
				&d.duration, &d.transient, &d.truncated, &d.errText, &d.completed); err != nil {
				rows.Close()
				return err
			}
			staged = append(staged, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		existing := map[string]bool{}
		existingRows, err := tx.QueryContext(ctx, `SELECT name FROM top_dirs WHERE root_id=?`, rootID)
		if err != nil {
			return err
		}
		for existingRows.Next() {
			var name string
			if err := existingRows.Scan(&name); err != nil {
				existingRows.Close()
				return err
			}
			existing[name] = true
		}
		existingRows.Close()
		if err := existingRows.Err(); err != nil {
			return err
		}

		successes, failures := 0, 0
		globalSampled := mode == string(model.ModeQuick)
		incremental := mode == string(model.ModeIncremental)
		globalApprox := globalSampled || resumed != 0 || selfTransient > 0
		hasTransient := false
		hasTruncated := false
		var errorMessages []string
		stagedNames := map[string]bool{}
		successNames := make([]string, 0, len(staged))
		for _, d := range staged {
			stagedNames[d.name] = true
			if d.errText != "" {
				failures++
				errorMessages = append(errorMessages, fmt.Sprintf("%s: %s", d.name, d.errText))
				status := "stale"
				if !existing[d.name] {
					status = "failed"
				}
				res, err := tx.ExecContext(ctx, `UPDATE top_dirs SET last_attempt_at=?, data_status=?,
					accuracy='approximate', last_error=?, updated_at=? WHERE root_id=? AND name=?`,
					nowMS, status, d.errText, nowMS, rootID, d.name)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					if _, err := tx.ExecContext(ctx, `INSERT INTO top_dirs
						(root_id, name, data_status, accuracy, last_attempt_at, last_error, updated_at)
						VALUES(?,?, 'failed', 'unknown', ?, ?, ?)`,
						rootID, d.name, nowMS, d.errText, nowMS); err != nil {
						return err
					}
				}
				continue
			}
			successes++
			successNames = append(successNames, d.name)
			if d.transient > 0 {
				hasTransient = true
			}
			if d.truncated != 0 {
				hasTruncated = true
			}
			accuracy := "exact"
			if d.truncated != 0 {
				accuracy = "approximate"
			} else if globalSampled {
				accuracy = "sampled"
			} else if globalApprox || d.transient > 0 {
				accuracy = "approximate"
			}
			completed := d.completed
			if completed == 0 {
				completed = nowMS
			}
			dataStatus := "fresh"
			if d.truncated != 0 {
				dataStatus = "partial"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO top_dirs
				(root_id, name, alloc_bytes, logical_bytes, file_count, dir_count, symlink_count,
				 duration_ms, truncated, data_status, accuracy, last_success_at, last_attempt_at, last_error, updated_at)
				VALUES(?,?,?,?,?,?,?,?,?, ?, ?, ?, ?, '', ?)
				ON CONFLICT(root_id, name) DO UPDATE SET
				 alloc_bytes=excluded.alloc_bytes, logical_bytes=excluded.logical_bytes,
				 file_count=excluded.file_count, dir_count=excluded.dir_count,
				 symlink_count=excluded.symlink_count, duration_ms=excluded.duration_ms,
				 truncated=excluded.truncated, data_status=excluded.data_status,
				 accuracy=excluded.accuracy,
				 last_success_at=excluded.last_success_at, last_attempt_at=excluded.last_attempt_at,
				 last_error='', updated_at=excluded.updated_at`,
				rootID, d.name, d.alloc, d.logical, d.files, d.dirs, d.symlinks,
				d.duration, d.truncated, dataStatus, accuracy, completed, nowMS, nowMS); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO dir_history
				(root_id, run_id, name, alloc_bytes, logical_bytes, file_count, dir_count,
				 symlink_count, accuracy, recorded_at)
				VALUES(?,?,?,?,?,?,?,?,?,?)`,
				rootID, runID, d.name, d.alloc, d.logical, d.files, d.dirs, d.symlinks,
				accuracy, completed); err != nil {
				return err
			}
		}

		if !single && !preserveMissing {
			for name := range existing {
				if stagedNames[name] {
					continue
				}
				failures++
				errorMessages = append(errorMessages, fmt.Sprintf("%s: 目录在本次扫描时不存在", name))
				if _, err := tx.ExecContext(ctx, `UPDATE top_dirs SET data_status='stale',
					last_attempt_at=?, last_error=?, updated_at=? WHERE root_id=? AND name=?`,
					nowMS, "目录在本次扫描时不存在", nowMS, rootID, name); err != nil {
					return err
				}
			}
		}

		for _, name := range successNames {
			if _, err := tx.ExecContext(ctx, `DELETE FROM inventory
				WHERE root_id=? AND top_dir=? AND generation<>?`, rootID, name, runID); err != nil {
				return err
			}
		}
		includeSelf := !single && mode != string(model.ModeRebuild) && mode != string(model.ModeIncremental)
		if includeSelf {
			if _, err := tx.ExecContext(ctx, `DELETE FROM inventory
				WHERE root_id=? AND top_dir='.' AND generation<>?`, rootID, runID); err != nil {
				return err
			}
		} else {
			// A single-directory rebuild does not touch files directly under the
			// scan root; preserve the previous root-self contribution.
			if err := tx.QueryRowContext(ctx, `SELECT self_alloc_bytes, self_logical_bytes,
				self_file_count, self_symlink_count FROM roots WHERE id=?`, rootID).
				Scan(&selfAlloc, &selfLogical, &selfFiles, &selfSymlinks); err != nil {
				return err
			}
		}

		var totalAlloc, totalLogical, totalFiles, totalDirs int64
		var dirCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(alloc_bytes),0),
			COALESCE(SUM(logical_bytes),0), COALESCE(SUM(file_count),0), COALESCE(SUM(dir_count),0)
			FROM top_dirs WHERE root_id=? AND data_status IN ('fresh','partial')`, rootID).
			Scan(&dirCount, &totalAlloc, &totalLogical, &totalFiles, &totalDirs); err != nil {
			return err
		}
		totalAlloc += selfAlloc
		totalLogical += selfLogical
		totalFiles += selfFiles
		// The root's subdirectory count includes first-level rows themselves.
		totalDirs += int64(dirCount)

		approximate := globalApprox || incremental || hasTransient || hasTruncated || failures > 0
		runStatus := string(model.RunCompleted)
		rootStatus := string(model.DataFresh)
		rootAccuracy := model.AccuracyExact
		if hasTruncated {
			rootAccuracy = model.AccuracyApproximate
		} else if globalSampled {
			rootAccuracy = model.AccuracySampled
		} else if incremental && failures == 0 {
			rootAccuracy = model.AccuracyIncremental
		} else if approximate {
			rootAccuracy = model.AccuracyApproximate
		}
		switch {
		case failures > 0 && successes > 0:
			runStatus = string(model.RunPartial)
			rootStatus = string(model.DataPartial)
			rootAccuracy = model.AccuracyApproximate
		case failures > 0 && successes == 0:
			runStatus = string(model.RunFailed)
			rootStatus = string(model.DataFailed)
			rootAccuracy = model.AccuracyApproximate
		case hasTruncated:
			rootStatus = string(model.DataPartial)
		}
		if single {
			runStatus = string(model.RunCompleted)
			if failures > 0 {
				runStatus = string(model.RunPartial)
				rootStatus = string(model.DataPartial)
				rootAccuracy = model.AccuracyApproximate
			} else if dirCount > 1 {
				rootStatus = string(model.DataPartial)
				rootAccuracy = model.AccuracyApproximate
			} else {
				rootStatus = string(model.DataFresh)
			}
		}
		lastSuccess := any(nil)
		if failures == 0 && (!single || dirCount <= 1) {
			lastSuccess = nowMS
		}
		lastError := strings.Join(errorMessages, "; ")
		if len(lastError) > 2000 {
			lastError = lastError[:2000]
		}
		if _, err := tx.ExecContext(ctx, `UPDATE roots SET status=?, accuracy=?, total_alloc_bytes=?,
			total_logical_bytes=?, file_count=?, dir_count=?, self_alloc_bytes=?,
			self_logical_bytes=?, self_file_count=?, self_symlink_count=?, duration_ms=?,
			last_success_at=COALESCE(?, last_success_at), last_attempt_at=?, last_error=?,
			updated_at=? WHERE id=?`,
			rootStatus, string(rootAccuracy), totalAlloc, totalLogical, totalFiles, totalDirs,
			selfAlloc, selfLogical, selfFiles, selfSymlinks,
			now.Sub(fromMillis(started)).Milliseconds(), lastSuccess, nowMS, lastError, nowMS, rootID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE scan_runs SET status=?, finished_at=?, heartbeat_at=?,
			processed_dirs=?, total_files=?, total_bytes=?, duration_ms=?, error=?
			WHERE id=?`,
			runStatus, nowMS, nowMS, len(staged),
			func() int64 {
				var sum int64
				for _, d := range staged {
					if d.errText == "" {
						sum += d.files
					}
				}
				if includeSelf {
					sum += selfFiles
				}
				return sum
			}(),
			func() int64 {
				var sum int64
				for _, d := range staged {
					if d.errText == "" {
						sum += d.alloc
					}
				}
				if includeSelf {
					sum += selfAlloc
				}
				return sum
			}(),
			now.Sub(fromMillis(started)).Milliseconds(), lastError, runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM hardlink_seen WHERE run_id=?`, runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM run_dirs WHERE run_id=?`, runID); err != nil {
			return err
		}
		if canceled != nil && canceled() {
			return model.ErrCanceled
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT state FROM control WHERE root_id=?), 'idle')`, rootID).Scan(&control); err != nil {
			return err
		}
		if control == string(model.ControlCancel) {
			return model.ErrCanceled
		}
		if err := s.setControlTx(ctx, tx, rootID, model.ControlIdle, nowMS); err != nil {
			return err
		}
		failedDirs = failures
		return nil
	})
	if err != nil {
		return nil, err
	}
	root, err := s.GetRootByID(ctx, rootID)
	if err != nil {
		return nil, err
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &model.ScanSummary{
		RunID:        runID,
		Root:         *root,
		Mode:         run.Mode,
		SizeBasis:    run.SizeBasis,
		StartedAt:    run.StartedAt,
		FinishedAt:   now,
		Duration:     run.Duration,
		DataStatus:   root.Status,
		Accuracy:     root.Accuracy,
		TotalAlloc:   root.TotalAlloc,
		TotalLogical: root.TotalLogical,
		FileCount:    root.FileCount,
		DirCount:     root.DirCount,
		TotalDirs:    run.TotalDirs,
		DoneDirs:     run.ProcessedDirs,
		FailedDirs:   failedDirs,
		Resumed:      run.Resumed,
	}, nil
}

// InventoryRecord is one optional file-inventory row. It never participates in
// aggregate statistics; it is an opt-in local manifest for change accounting.
type InventoryRecord struct {
	RootID     int64
	RunID      int64
	TopDir     string
	RelPath    string
	SizeBytes  int64
	AllocBytes int64
	Inode      uint64
	Dev        uint64
	Nlink      uint64
	MtimeNS    int64
	Mode       uint32
}

// AddInventoryBatch writes a bounded batch of inventory rows in one transaction.
// It chunks the input because SQLite has a finite host-parameter limit; 11
// columns times a large batch would otherwise exceed it.
func (s *Store) AddInventoryBatch(ctx context.Context, records []InventoryRecord) error {
	const maxRows = 2000
	if len(records) == 0 {
		return nil
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for start := 0; start < len(records); start += maxRows {
			end := start + maxRows
			if end > len(records) {
				end = len(records)
			}
			if err := insertInventoryChunk(ctx, tx, records[start:end]); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertInventoryChunk(ctx context.Context, tx *sql.Tx, records []InventoryRecord) error {
	const columns = 11
	var builder strings.Builder
	builder.WriteString(`INSERT INTO inventory
		(root_id, top_dir, rel_path, size_bytes, alloc_bytes, inode, dev, nlink,
		 mtime_ns, mode, generation) VALUES `)
	args := make([]any, 0, len(records)*columns)
	for i, record := range records {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString("(?,?,?,?,?,?,?,?,?,?,?)")
		args = append(args,
			record.RootID, record.TopDir, record.RelPath, record.SizeBytes,
			record.AllocBytes, int64(record.Inode), int64(record.Dev),
			int64(record.Nlink), record.MtimeNS, int64(record.Mode), record.RunID)
	}
	builder.WriteString(` ON CONFLICT(root_id, top_dir, rel_path) DO UPDATE SET
		size_bytes=excluded.size_bytes, alloc_bytes=excluded.alloc_bytes,
		inode=excluded.inode, dev=excluded.dev, nlink=excluded.nlink,
		mtime_ns=excluded.mtime_ns, mode=excluded.mode, generation=excluded.generation`)
	_, err := tx.ExecContext(ctx, builder.String(), args...)
	return err
}

// ---------------------------------------------------------------------------
// Control

func (s *Store) setControlTx(ctx context.Context, tx *sql.Tx, rootID int64, state model.ControlState, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO control(root_id, state, updated_at) VALUES(?,?,?)
		ON CONFLICT(root_id) DO UPDATE SET state=excluded.state, updated_at=excluded.updated_at`,
		rootID, string(state), now)
	return err
}

// SetControl writes a cross-process control request.
func (s *Store) SetControl(ctx context.Context, rootID int64, state model.ControlState) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		return s.setControlTx(ctx, tx, rootID, state, nowMillis())
	})
}

// GetControl reads the current control state (idle when absent).
func (s *Store) GetControl(ctx context.Context, rootID int64) (model.Control, error) {
	var control model.Control
	var state string
	var updated int64
	err := s.rdb.QueryRowContext(ctx, `SELECT root_id, state, updated_at FROM control WHERE root_id=?`, rootID).
		Scan(&control.RootID, &state, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Control{RootID: rootID, State: model.ControlIdle}, nil
	}
	if err != nil {
		return control, err
	}
	control.State = model.ControlState(state)
	control.UpdatedAt = fromMillis(updated)
	return control, nil
}

// CheckControl is called by the scanner's monitor goroutine.
func (s *Store) CheckControl(ctx context.Context, rootID int64) (model.ControlState, error) {
	var state string
	err := s.rdb.QueryRowContext(ctx, `SELECT state FROM control WHERE root_id=?`, rootID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ControlIdle, nil
	}
	if err != nil {
		return model.ControlIdle, err
	}
	return model.ControlState(state), nil
}

// ---------------------------------------------------------------------------
// Cached directories

const dirColumns = `id, root_id, name, alloc_bytes, logical_bytes, file_count, dir_count,
	symlink_count, duration_ms, truncated, data_status, accuracy, last_success_at, last_attempt_at, last_error`

func scanDir(row rowScanner) (*model.DirStat, error) {
	var d model.DirStat
	var status, accuracy string
	var duration int64
	var truncated int
	var lastSuccess, lastAttempt sql.NullInt64
	err := row.Scan(&d.ID, &d.RootID, &d.Name, &d.AllocBytes, &d.LogicalBytes, &d.FileCount,
		&d.DirCount, &d.SymlinkCount, &duration, &truncated, &status, &accuracy, &lastSuccess,
		&lastAttempt, &d.LastError)
	if err != nil {
		return nil, err
	}
	d.Duration = time.Duration(duration) * time.Millisecond
	d.Truncated = truncated != 0
	d.DataStatus = model.DataStatus(status)
	d.Accuracy = model.Accuracy(accuracy)
	d.LastSuccessAt = nullableTime(lastSuccess)
	d.LastAttemptAt = nullableTime(lastAttempt)
	return &d, nil
}

// ListDirs returns cached top-level directories ordered by allocated bytes.
func (s *Store) ListDirs(ctx context.Context, rootID int64, limit int, basis model.SizeBasis) ([]model.DirStat, error) {
	orderBy := "alloc_bytes"
	if basis == model.BasisLogical {
		orderBy = "logical_bytes"
	}
	query := `SELECT ` + dirColumns + ` FROM top_dirs
		WHERE root_id=? AND data_status IN ('fresh','partial')
		ORDER BY CASE WHEN last_success_at IS NULL THEN 1 ELSE 0 END,
		` + orderBy + ` DESC, name ASC`
	args := []any{rootID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.DirStat
	for rows.Next() {
		d, err := scanDir(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *d)
	}
	return result, rows.Err()
}

// GetDir returns one cached first-level directory.
func (s *Store) GetDir(ctx context.Context, rootID int64, name string) (*model.DirStat, error) {
	d, err := scanDir(s.rdb.QueryRowContext(ctx, `SELECT `+dirColumns+`
		FROM top_dirs WHERE root_id=? AND name=?`, rootID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// CountDirs counts visible cached rows.
func (s *Store) CountDirs(ctx context.Context, rootID int64) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM top_dirs
		WHERE root_id=? AND data_status IN ('fresh','partial')`, rootID).Scan(&n)
	return n, err
}

// CountStaleDirs counts cached rows that are no longer current, such as
// directories removed from the filesystem.
func (s *Store) CountStaleDirs(ctx context.Context, rootID int64) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM top_dirs
		WHERE root_id=? AND data_status NOT IN ('fresh','partial')`, rootID).Scan(&n)
	return n, err
}

// DirStates returns every cached directory row with its data status. It
// includes stale rows so incremental scans can re-scan recreated directories.
func (s *Store) DirStates(ctx context.Context, rootID int64) (map[string]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT name, data_status FROM top_dirs WHERE root_id=?`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var name, status string
		if err := rows.Scan(&name, &status); err != nil {
			return nil, err
		}
		result[name] = status
	}
	return result, rows.Err()
}

// PruneStaleDirs removes old stale/failed cache rows. A zero rootID prunes all
// roots. It never removes rows that still have fresh data.
func (s *Store) PruneStaleDirs(ctx context.Context, rootID int64, before time.Time) (int64, error) {
	cutoff := toMillis(before)
	var res sql.Result
	var err error
	if rootID == 0 {
		res, err = s.wdb.ExecContext(ctx, `DELETE FROM top_dirs
			WHERE data_status IN ('stale','failed') AND COALESCE(last_attempt_at,0) < ?`, cutoff)
	} else {
		res, err = s.wdb.ExecContext(ctx, `DELETE FROM top_dirs
			WHERE root_id=? AND data_status IN ('stale','failed') AND COALESCE(last_attempt_at,0) < ?`,
			rootID, cutoff)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Maintenance

// CleanupHistory removes history and old finished scan runs. Current cache rows
// and roots are never removed by this operation.
func (s *Store) CleanupHistory(ctx context.Context, before time.Time, vacuum bool) (history int64, runs int64, err error) {
	cutoff := toMillis(before)
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM dir_history WHERE recorded_at < ?`, cutoff)
		if err != nil {
			return err
		}
		history, _ = res.RowsAffected()
		res, err = tx.ExecContext(ctx, `DELETE FROM scan_runs WHERE finished_at IS NOT NULL
			AND finished_at < ? AND status NOT IN ('running','paused')`, cutoff)
		if err != nil {
			return err
		}
		runs, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM run_dirs WHERE run_id NOT IN (SELECT id FROM scan_runs)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inventory WHERE generation NOT IN (SELECT id FROM scan_runs)`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM hardlink_seen WHERE run_id NOT IN (SELECT id FROM scan_runs)`)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	if vacuum {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		if _, err := s.wdb.ExecContext(ctx, `VACUUM`); err != nil {
			return history, runs, fmt.Errorf("VACUUM 失败: %w", err)
		}
	}
	return history, runs, nil
}

// Backup creates a consistent copy with VACUUM INTO. Destination must not exist.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if destination == "" {
		return errors.New("备份路径为空")
	}
	abs, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("备份目标已存在，拒绝覆盖: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	quoted := strings.ReplaceAll(abs, "'", "''")
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.wdb.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil {
		return fmt.Errorf("数据库备份失败: %w", err)
	}
	_ = os.Chmod(abs, 0o600)
	return nil
}

// QuickCheck runs SQLite's quick integrity check.
func (s *Store) QuickCheck(ctx context.Context) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		result = append(result, line)
	}
	return result, rows.Err()
}

func resumableRunStatus(status model.RunStatus, includeCanceled bool) bool {
	switch status {
	case model.RunInterrupted, model.RunRunning, model.RunPaused:
		return true
	case model.RunCanceled:
		return includeCanceled
	default:
		return false
	}
}

// StagedDirs returns the completed first-level directories of one run. These
// rows are still staging data: they are visible through "partial" but not yet
// published to top_dirs.
func (s *Store) StagedDirs(ctx context.Context, runID int64, limit int, basis model.SizeBasis) ([]model.DirStat, error) {
	orderBy := "alloc_bytes"
	if basis == model.BasisLogical {
		orderBy = "logical_bytes"
	}
	query := `SELECT name, alloc_bytes, logical_bytes, file_count, dir_count,
		symlink_count, duration_ms, truncated, completed_at
		FROM run_dirs WHERE run_id=? AND error='' AND completed_at>0 ORDER BY ` + orderBy + ` DESC, name ASC`
	args := []any{runID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.DirStat
	for rows.Next() {
		var d model.DirStat
		var durationMS, completedMS int64
		var truncated int
		if err := rows.Scan(&d.Name, &d.AllocBytes, &d.LogicalBytes, &d.FileCount,
			&d.DirCount, &d.SymlinkCount, &durationMS, &truncated, &completedMS); err != nil {
			return nil, err
		}
		d.ID = runID
		d.Truncated = truncated != 0
		d.Duration = time.Duration(durationMS) * time.Millisecond
		t := time.UnixMilli(completedMS)
		d.LastSuccessAt = &t
		d.LastAttemptAt = &t
		d.DataStatus = model.DataFresh
		if d.Truncated {
			d.DataStatus = model.DataPartial
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Depth-limited directory index

const dirIndexColumns = `root_id, path, parent_path, name, depth, alloc_bytes,
	logical_bytes, file_count, dir_count, symlink_count, data_status, accuracy,
	truncated, updated_at`

func scanDirIndex(row rowScanner) (*model.DirIndexEntry, error) {
	var e model.DirIndexEntry
	var status, accuracy string
	var truncated int
	var updated int64
	err := row.Scan(&e.RootID, &e.Path, &e.ParentPath, &e.Name, &e.Depth,
		&e.AllocBytes, &e.LogicalBytes, &e.FileCount, &e.DirCount, &e.SymlinkCount,
		&status, &accuracy, &truncated, &updated)
	if err != nil {
		return nil, err
	}
	e.DataStatus = model.DataStatus(status)
	e.Accuracy = model.Accuracy(accuracy)
	e.Truncated = truncated != 0
	e.UpdatedAt = fromMillis(updated)
	return &e, nil
}

// MarkRootPartial records that a root aggregate contains only one shard of a
// larger scan. The root can still be queried, but its total is incomplete.
func (s *Store) MarkRootPartial(ctx context.Context, rootID int64) error {
	_, err := s.wdb.ExecContext(ctx, `UPDATE roots SET status='partial', updated_at=? WHERE id=?`,
		nowMillis(), rootID)
	return err
}

// ReplaceDirIndex atomically replaces the cached directory index for a root.
func (s *Store) ReplaceDirIndex(ctx context.Context, rootID, runID int64, entries []model.DirIndexEntry) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM dir_index WHERE root_id=?`, rootID); err != nil {
			return err
		}
		const batchSize = 200
		for start := 0; start < len(entries); start += batchSize {
			end := start + batchSize
			if end > len(entries) {
				end = len(entries)
			}
			var query strings.Builder
			query.WriteString(`INSERT INTO dir_index
				(root_id, path, parent_path, name, depth, alloc_bytes, logical_bytes,
				 file_count, dir_count, symlink_count, data_status, accuracy, truncated,
				 run_id, updated_at) VALUES `)
			args := make([]any, 0, (end-start)*15)
			for i, e := range entries[start:end] {
				if i > 0 {
					query.WriteByte(',')
				}
				query.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
				truncated := 0
				if e.Truncated {
					truncated = 1
				}
				args = append(args, rootID, e.Path, e.ParentPath, e.Name,
					e.Depth, e.AllocBytes, e.LogicalBytes, e.FileCount, e.DirCount,
					e.SymlinkCount, string(e.DataStatus), string(e.Accuracy), truncated,
					runID, toMillis(e.UpdatedAt))
			}
			if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// Children returns indexed direct children of parentPath, largest first.
// parentPath is relative to the root and empty for top-level entries.
func (s *Store) Children(ctx context.Context, rootID int64, parentPath string, limit int, basis model.SizeBasis) ([]model.DirIndexEntry, error) {
	orderBy := "alloc_bytes"
	if basis == model.BasisLogical {
		orderBy = "logical_bytes"
	}
	query := `SELECT ` + dirIndexColumns + ` FROM dir_index
		WHERE root_id=? AND parent_path=? AND data_status IN ('fresh','partial')
		ORDER BY ` + orderBy + ` DESC, name ASC`
	args := []any{rootID, parentPath}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.DirIndexEntry
	for rows.Next() {
		e, err := scanDirIndex(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *e)
	}
	return result, rows.Err()
}

// GetIndexEntry returns one indexed path.
func (s *Store) GetIndexEntry(ctx context.Context, rootID int64, path string) (*model.DirIndexEntry, error) {
	e, err := scanDirIndex(s.rdb.QueryRowContext(ctx, `SELECT `+dirIndexColumns+`
		FROM dir_index WHERE root_id=? AND path=?`, rootID, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// CountIndexedDirs returns the number of cached index entries for a root.
func (s *Store) CountIndexedDirs(ctx context.Context, rootID int64) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM dir_index WHERE root_id=?`, rootID).Scan(&n)
	return n, err
}
