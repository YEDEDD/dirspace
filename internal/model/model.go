// Package model contains the shared domain types used by storage, scanner,
// service, and CLI layers.
package model

import (
	"fmt"
	"time"
)

// DataStatus describes the freshness/quality of a cached result.
type DataStatus string

const (
	DataNever    DataStatus = "never"
	DataScanning DataStatus = "scanning"
	DataFresh    DataStatus = "fresh"
	DataStale    DataStatus = "stale"
	DataPartial  DataStatus = "partial"
	DataFailed   DataStatus = "failed"
)

func (s DataStatus) Label() string {
	switch s {
	case DataFresh:
		return "正常"
	case DataStale:
		return "已过期"
	case DataPartial:
		return "部分更新"
	case DataScanning:
		return "扫描中"
	case DataFailed:
		return "失败"
	case DataNever:
		return "未扫描"
	default:
		return string(s)
	}
}

// Accuracy distinguishes a complete metadata walk from a degraded one.
type Accuracy string

const (
	AccuracyExact       Accuracy = "exact"
	AccuracySampled     Accuracy = "sampled"
	AccuracyIncremental Accuracy = "incremental"
	AccuracyApproximate Accuracy = "approximate"
	AccuracyUnknown     Accuracy = "unknown"
)

func (a Accuracy) Label() string {
	switch a {
	case AccuracyExact:
		return "精确（客户端元数据）"
	case AccuracySampled:
		return "抽样估算（非精确）"
	case AccuracyIncremental:
		return "近似（新增目录补扫）"
	case AccuracyApproximate:
		return "近似（存在错误/断点续扫）"
	default:
		return "未知"
	}
}

// ScanMode selects the scanning strategy.
type ScanMode string

const (
	ModeFull        ScanMode = "full"
	ModeQuick       ScanMode = "quick"
	ModeIncremental ScanMode = "incremental"
	ModeRebuild     ScanMode = "rebuild"
)

func (m ScanMode) Label() string {
	switch m {
	case ModeFull:
		return "全量"
	case ModeQuick:
		return "快速估算"
	case ModeIncremental:
		return "增量"
	case ModeRebuild:
		return "单目录重算"
	default:
		return string(m)
	}
}

// RunStatus is persisted in scan_runs.
type RunStatus string

const (
	RunRunning     RunStatus = "running"
	RunPaused      RunStatus = "paused"
	RunCompleted   RunStatus = "completed"
	RunPartial     RunStatus = "partial"
	RunFailed      RunStatus = "failed"
	RunCanceled    RunStatus = "canceled"
	RunInterrupted RunStatus = "interrupted"
)

func (s RunStatus) Active() bool { return s == RunRunning || s == RunPaused }

// ControlState is the cross-process pause/cancel channel.
type ControlState string

const (
	ControlIdle    ControlState = "idle"
	ControlRunning ControlState = "running"
	ControlPause   ControlState = "pause"
	ControlCancel  ControlState = "cancel"
)

// SizeBasis selects the accounting metric.
type SizeBasis string

const (
	BasisAllocated SizeBasis = "allocated"
	BasisLogical   SizeBasis = "logical"
)

func (b SizeBasis) Label() string {
	if b == BasisLogical {
		return "逻辑大小（st_size）"
	}
	return "实际分配空间（st_blocks × 512）"
}

// Root is a scanned or known scan root.
type Root struct {
	ID            int64
	Path          string
	Status        DataStatus
	Accuracy      Accuracy
	TotalAlloc    int64
	TotalLogical  int64
	FileCount     int64
	DirCount      int64
	SelfAlloc     int64
	SelfLogical   int64
	SelfFiles     int64
	SelfSymlinks  int64
	Duration      time.Duration
	LastSuccessAt *time.Time
	LastAttemptAt *time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DirStat is the cached aggregate for one first-level directory.
type DirStat struct {
	ID            int64
	RootID        int64
	Name          string
	AllocBytes    int64
	LogicalBytes  int64
	FileCount     int64
	DirCount      int64
	SymlinkCount  int64
	Duration      time.Duration
	DataStatus    DataStatus
	Accuracy      Accuracy
	LastSuccessAt *time.Time
	LastAttemptAt *time.Time
	LastError     string
	Truncated     bool
}

// HasData reports whether a row has a successful baseline to display.
func (d DirStat) HasData() bool { return d.LastSuccessAt != nil }

// DirIndexEntry is one cached directory in the depth-limited directory index.
// Path is relative to the scan root and uses forward slashes. Entries are
// selected per parent by top-K allocated bytes, so an entry is guaranteed to
// have a parent entry but not every child of a directory is present.
type DirIndexEntry struct {
	RootID       int64
	Path         string
	ParentPath   string
	Name         string
	Depth        int
	AllocBytes   int64
	LogicalBytes int64
	FileCount    int64
	DirCount     int64
	SymlinkCount int64
	DataStatus   DataStatus
	Accuracy     Accuracy
	Truncated    bool
	UpdatedAt    time.Time
}

// Run describes a scan run and its progress.
type Run struct {
	ID            int64
	RootID        int64
	Mode          ScanMode
	Status        RunStatus
	StartedAt     time.Time
	FinishedAt    *time.Time
	HeartbeatAt   time.Time
	TotalDirs     int
	ProcessedDirs int
	TotalFiles    int64
	TotalBytes    int64
	SelfAlloc     int64
	SelfLogical   int64
	SelfFiles     int64
	SelfSymlinks  int64
	SelfTransient int64
	SelfCompleted bool
	CurrentDir    string
	Error         string
	Resumed       bool
	SizeBasis     SizeBasis
	RootPath      string
	Duration      time.Duration
	MetadataOps   int64
	MetadataQPS   float64
	MetadataLimit float64
}

// Control is the current control request for a root.
type Control struct {
	RootID    int64
	State     ControlState
	UpdatedAt time.Time
}

// DirOutcome is the result of scanning one first-level directory.
type DirOutcome struct {
	Name          string
	AllocBytes    int64
	LogicalBytes  int64
	FileCount     int64
	DirCount      int64
	SymlinkCount  int64
	Duration      time.Duration
	Err           error
	TransientErrs int64
	Truncated     bool
	CompletedAt   time.Time
}

// Progress is emitted while a scan is running.
type Progress struct {
	RootPath   string
	CurrentDir string
	TotalDirs  int
	DoneDirs   int
	Files      int64
	Bytes      int64
	Paused     bool
	Elapsed    time.Duration
	QPS        float64
}

// ScanSummary is returned after a scan finishes or aborts.
type ScanSummary struct {
	RunID        int64
	Root         Root
	TargetDir    string
	Mode         ScanMode
	SizeBasis    SizeBasis
	StartedAt    time.Time
	FinishedAt   time.Time
	Duration     time.Duration
	DataStatus   DataStatus
	Accuracy     Accuracy
	TotalAlloc   int64
	TotalLogical int64
	FileCount    int64
	DirCount     int64
	TotalDirs    int
	DoneDirs     int
	FailedDirs   int
	Resumed      bool
	Errors       []string
}

// ErrCanceled is returned when the user cancels a scan.
var ErrCanceled = fmt.Errorf("扫描已取消")
