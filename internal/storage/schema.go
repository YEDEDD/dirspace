package storage

// schemaVersion is bumped whenever an incompatible or additive migration is
// required. The program refuses to open a database created by a newer version.
const schemaVersion = 3

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS roots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL DEFAULT 'never',
		accuracy TEXT NOT NULL DEFAULT 'unknown',
		total_alloc_bytes INTEGER NOT NULL DEFAULT 0,
		total_logical_bytes INTEGER NOT NULL DEFAULT 0,
		file_count INTEGER NOT NULL DEFAULT 0,
		dir_count INTEGER NOT NULL DEFAULT 0,
		self_alloc_bytes INTEGER NOT NULL DEFAULT 0,
		self_logical_bytes INTEGER NOT NULL DEFAULT 0,
		self_file_count INTEGER NOT NULL DEFAULT 0,
		self_symlink_count INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		last_success_at INTEGER,
		last_attempt_at INTEGER,
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS scan_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		root_id INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
		mode TEXT NOT NULL,
		status TEXT NOT NULL,
		size_basis TEXT NOT NULL DEFAULT 'allocated',
		resumed INTEGER NOT NULL DEFAULT 0,
		started_at INTEGER NOT NULL,
		finished_at INTEGER,
		heartbeat_at INTEGER NOT NULL,
		total_dirs INTEGER NOT NULL DEFAULT 0,
		processed_dirs INTEGER NOT NULL DEFAULT 0,
		total_files INTEGER NOT NULL DEFAULT 0,
		total_bytes INTEGER NOT NULL DEFAULT 0,
		self_alloc_bytes INTEGER NOT NULL DEFAULT 0,
		self_logical_bytes INTEGER NOT NULL DEFAULT 0,
		self_file_count INTEGER NOT NULL DEFAULT 0,
		self_symlink_count INTEGER NOT NULL DEFAULT 0,
		self_transient_errors INTEGER NOT NULL DEFAULT 0,
		self_completed INTEGER NOT NULL DEFAULT 0,
		current_dir TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_scan_runs_root_status ON scan_runs(root_id, status, id DESC)`,

	`CREATE TABLE IF NOT EXISTS run_dirs (
		run_id INTEGER NOT NULL REFERENCES scan_runs(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		alloc_bytes INTEGER NOT NULL DEFAULT 0,
		logical_bytes INTEGER NOT NULL DEFAULT 0,
		file_count INTEGER NOT NULL DEFAULT 0,
		dir_count INTEGER NOT NULL DEFAULT 0,
		symlink_count INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		transient_errors INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		completed_at INTEGER NOT NULL,
		PRIMARY KEY(run_id, name)
	) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS top_dirs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		root_id INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		alloc_bytes INTEGER NOT NULL DEFAULT 0,
		logical_bytes INTEGER NOT NULL DEFAULT 0,
		file_count INTEGER NOT NULL DEFAULT 0,
		dir_count INTEGER NOT NULL DEFAULT 0,
		symlink_count INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		data_status TEXT NOT NULL DEFAULT 'never',
		accuracy TEXT NOT NULL DEFAULT 'unknown',
		last_success_at INTEGER,
		last_attempt_at INTEGER,
		last_error TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL,
		UNIQUE(root_id, name)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_top_dirs_rank ON top_dirs(root_id, alloc_bytes DESC, name ASC)`,

	`CREATE TABLE IF NOT EXISTS dir_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		root_id INTEGER NOT NULL,
		run_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		alloc_bytes INTEGER NOT NULL,
		logical_bytes INTEGER NOT NULL,
		file_count INTEGER NOT NULL,
		dir_count INTEGER NOT NULL,
		symlink_count INTEGER NOT NULL,
		accuracy TEXT NOT NULL,
		recorded_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_history_time ON dir_history(recorded_at)`,
	`CREATE INDEX IF NOT EXISTS idx_dir_history_root_time ON dir_history(root_id, recorded_at DESC)`,

	`CREATE TABLE IF NOT EXISTS hardlink_seen (
		run_id INTEGER NOT NULL,
		dev INTEGER NOT NULL,
		ino INTEGER NOT NULL,
		top_dir TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(run_id, dev, ino)
	) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS control (
		root_id INTEGER PRIMARY KEY REFERENCES roots(id) ON DELETE CASCADE,
		state TEXT NOT NULL DEFAULT 'idle',
		updated_at INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS inventory (
		root_id INTEGER NOT NULL,
		top_dir TEXT NOT NULL,
		rel_path TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		alloc_bytes INTEGER NOT NULL,
		inode INTEGER NOT NULL,
		dev INTEGER NOT NULL,
		nlink INTEGER NOT NULL,
		mtime_ns INTEGER NOT NULL,
		mode INTEGER NOT NULL,
		generation INTEGER NOT NULL,
		PRIMARY KEY(root_id, top_dir, rel_path)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS idx_inventory_generation ON inventory(root_id, top_dir, generation)`,
}

// schemaColumns are additive migrations applied to databases created by older
// releases. CREATE TABLE IF NOT EXISTS cannot add columns to an existing table.
var schemaColumns = []struct {
	table string
	name  string
	ddl   string
}{
	{"roots", "self_alloc_bytes", "INTEGER NOT NULL DEFAULT 0"},
	{"roots", "self_logical_bytes", "INTEGER NOT NULL DEFAULT 0"},
	{"roots", "self_file_count", "INTEGER NOT NULL DEFAULT 0"},
	{"roots", "self_symlink_count", "INTEGER NOT NULL DEFAULT 0"},
	{"scan_runs", "self_alloc_bytes", "INTEGER NOT NULL DEFAULT 0"},
	{"scan_runs", "self_logical_bytes", "INTEGER NOT NULL DEFAULT 0"},
	{"scan_runs", "self_file_count", "INTEGER NOT NULL DEFAULT 0"},
	{"scan_runs", "self_symlink_count", "INTEGER NOT NULL DEFAULT 0"},
	{"scan_runs", "self_transient_errors", "INTEGER NOT NULL DEFAULT 0"},
	{"scan_runs", "self_completed", "INTEGER NOT NULL DEFAULT 0"},
}
