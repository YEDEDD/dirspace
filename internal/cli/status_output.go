package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/service"
)

func runStatusText(status model.RunStatus) string {
	switch status {
	case model.RunRunning:
		return "运行中"
	case model.RunPaused:
		return "已暂停"
	case model.RunCompleted:
		return "已完成"
	case model.RunPartial:
		return "部分完成"
	case model.RunFailed:
		return "失败"
	case model.RunCanceled:
		return "已取消"
	case model.RunInterrupted:
		return "已中断"
	default:
		return string(status)
	}
}

func controlText(state model.ControlState) string {
	switch state {
	case model.ControlPause:
		return "暂停请求"
	case model.ControlCancel:
		return "取消请求"
	case model.ControlRunning:
		return "运行中"
	default:
		return "空闲"
	}
}

type statusJSON struct {
	Path              string     `json:"path"`
	HasBaseline       bool       `json:"has_baseline"`
	ScanRoot          string     `json:"scan_root"`
	DataStatus        string     `json:"data_status"`
	DataStatusText    string     `json:"data_status_text"`
	Accuracy          string     `json:"accuracy"`
	AccuracyText      string     `json:"accuracy_text"`
	ScannedAt         *time.Time `json:"last_success_at"`
	LastAttemptAt     *time.Time `json:"last_attempt_at"`
	DurationSeconds   float64    `json:"duration_seconds"`
	SizeBasis         string     `json:"size_basis"`
	TotalBytes        int64      `json:"total_bytes"`
	TotalText         string     `json:"total_text"`
	TotalAllocBytes   int64      `json:"total_alloc_bytes"`
	TotalLogicalBytes int64      `json:"total_logical_bytes"`
	FileCount         int64      `json:"file_count"`
	DirCount          int64      `json:"dir_count"`
	Server            string     `json:"server,omitempty"`
	FSType            string     `json:"fstype,omitempty"`
	DirRows           int        `json:"cached_directories"`
	DBPath            string     `json:"database_path"`
	DBSize            int64      `json:"database_size_bytes"`
	ActiveRun         *runJSON   `json:"active_run,omitempty"`
	LastRun           *runJSON   `json:"last_run,omitempty"`
	Control           string     `json:"control_state"`
	ControlText       string     `json:"control_state_text"`
	Target            string     `json:"target_directory,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

type runJSON struct {
	ID              int64      `json:"id"`
	Mode            string     `json:"mode"`
	Status          string     `json:"status"`
	StatusText      string     `json:"status_text"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	HeartbeatAt     time.Time  `json:"heartbeat_at"`
	TotalDirs       int        `json:"total_directories"`
	ProcessedDirs   int        `json:"processed_directories"`
	Files           int64      `json:"files_seen"`
	Bytes           int64      `json:"bytes_seen"`
	CurrentDir      string     `json:"current_directory,omitempty"`
	DurationSeconds float64    `json:"duration_seconds"`
	Error           string     `json:"error,omitempty"`
	Resumed         bool       `json:"resumed"`
	SizeBasis       string     `json:"size_basis"`
}

func toRunJSON(run *model.Run) *runJSON {
	if run == nil {
		return nil
	}
	duration := run.Duration
	if duration == 0 && run.FinishedAt == nil {
		duration = time.Since(run.StartedAt)
	}
	return &runJSON{
		ID:              run.ID,
		Mode:            string(run.Mode),
		Status:          string(run.Status),
		StatusText:      runStatusText(run.Status),
		StartedAt:       run.StartedAt,
		FinishedAt:      run.FinishedAt,
		HeartbeatAt:     run.HeartbeatAt,
		TotalDirs:       run.TotalDirs,
		ProcessedDirs:   run.ProcessedDirs,
		Files:           run.TotalFiles,
		Bytes:           run.TotalBytes,
		CurrentDir:      validText(run.CurrentDir),
		DurationSeconds: duration.Seconds(),
		Error:           run.Error,
		Resumed:         run.Resumed,
		SizeBasis:       string(run.SizeBasis),
	}
}

func buildStatusJSON(view *service.StatusView) statusJSON {
	last := view.Root.LastSuccessAt
	accuracy := view.Root.Accuracy
	alloc := view.Root.TotalAlloc
	logical := view.Root.TotalLogical
	files := view.Root.FileCount
	dirs := view.Root.DirCount
	duration := view.Root.Duration
	attempt := view.Root.LastAttemptAt
	targetName := ""
	lastError := view.Root.LastError
	if view.Target != nil {
		last = view.Target.LastSuccessAt
		accuracy = view.Target.Accuracy
		alloc = view.Target.AllocBytes
		logical = view.Target.LogicalBytes
		files = view.Target.FileCount
		dirs = view.Target.DirCount
		duration = view.Target.Duration
		attempt = view.Target.LastAttemptAt
		lastError = view.Target.LastError
		targetName = view.Target.Name
	}
	selected := alloc
	if view.SizeBasis == model.BasisLogical {
		selected = logical
	}
	out := statusJSON{
		Path:              view.Path,
		HasBaseline:       last != nil,
		ScanRoot:          view.Root.Path,
		DataStatus:        string(view.DataStatus),
		DataStatusText:    statusWithContext(view.DataStatus, last),
		Accuracy:          string(accuracy),
		AccuracyText:      accuracy.Label(),
		ScannedAt:         last,
		LastAttemptAt:     attempt,
		DurationSeconds:   duration.Seconds(),
		SizeBasis:         basisValue(view.SizeBasis),
		TotalBytes:        selected,
		TotalText:         formatSize(selected),
		TotalAllocBytes:   alloc,
		TotalLogicalBytes: logical,
		FileCount:         files,
		DirCount:          dirs,
		Server:            view.Server,
		FSType:            view.FSType,
		DirRows:           view.DirCount,
		DBPath:            view.DBPath,
		DBSize:            view.DBSize,
		ActiveRun:         toRunJSON(view.Active),
		LastRun:           toRunJSON(view.Last),
		Control:           string(view.Control.State),
		ControlText:       controlText(view.Control.State),
		Target:            targetName,
		LastError:         lastError,
	}
	if last == nil {
		out.TotalText = "n/a"
	}
	return out
}

func printStatus(w io.Writer, view *service.StatusView) {
	last := view.Root.LastSuccessAt
	accuracy := view.Root.Accuracy
	alloc := view.Root.TotalAlloc
	logical := view.Root.TotalLogical
	files := view.Root.FileCount
	dirs := view.Root.DirCount
	attempt := view.Root.LastAttemptAt
	duration := view.Root.Duration
	if view.Target != nil {
		last = view.Target.LastSuccessAt
		accuracy = view.Target.Accuracy
		alloc = view.Target.AllocBytes
		logical = view.Target.LogicalBytes
		files = view.Target.FileCount
		dirs = view.Target.DirCount
		attempt = view.Target.LastAttemptAt
		duration = view.Target.Duration
	}
	fmt.Fprintln(w, "dirspace 状态")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "查询路径：%s\n", view.Path)
	fmt.Fprintf(w, "扫描根目录：%s\n", view.Root.Path)
	if view.Target != nil {
		fmt.Fprintf(w, "一级目录：%s\n", view.Target.Name)
	}
	fmt.Fprintf(w, "挂载来源：%s\n", serverText(view.Server, view.FSType))
	fmt.Fprintf(w, "数据状态：%s\n", statusWithContext(view.DataStatus, last))
	fmt.Fprintf(w, "准确性：%s\n", accuracy.Label())
	fmt.Fprintf(w, "上次成功统计：%s（%s）\n", formatTimestamp(last), formatAgeWithStatus(last, view.DataStatus))
	fmt.Fprintf(w, "最近尝试：%s\n", formatTimestamp(attempt))
	fmt.Fprintf(w, "上次扫描耗时：%s\n", formatDuration(duration))
	selectedSize := alloc
	if view.SizeBasis == model.BasisLogical {
		selectedSize = logical
	}
	if last == nil {
		fmt.Fprintln(w, "空间：n/a（尚未成功统计）")
	} else {
		fmt.Fprintf(w, "空间：%s（实际分配 %s，逻辑 %s）\n",
			formatSize(selectedSize), formatSize(alloc), formatSize(logical))
	}
	if last == nil {
		fmt.Fprintln(w, "文件数：n/a")
		fmt.Fprintln(w, "子目录数：n/a")
	} else {
		fmt.Fprintf(w, "文件数：%s\n", formatCount(files))
		fmt.Fprintf(w, "子目录数：%s\n", formatCount(dirs))
	}
	fmt.Fprintf(w, "缓存一级目录行数：%d\n", view.DirCount)
	if view.StaleDirs > 0 {
		fmt.Fprintf(w, "已删除/过期目录行：%d（已隐藏，按 stale_retention_days 清理）\n", view.StaleDirs)
	}
	fmt.Fprintf(w, "控制状态：%s\n", controlText(view.Control.State))
	lastError := view.Root.LastError
	if view.Target != nil {
		lastError = view.Target.LastError
	}
	if lastError != "" {
		fmt.Fprintf(w, "最近错误：%s\n", lastError)
	}
	if view.Active != nil {
		run := view.Active
		fmt.Fprintf(w, "当前任务：#%d %s（%s） %d/%d 目录，%s 文件，%s\n",
			run.ID, run.Mode.Label(), runStatusText(run.Status), run.ProcessedDirs,
			run.TotalDirs, formatCount(run.TotalFiles), formatSize(run.TotalBytes))
		if run.CurrentDir != "" {
			fmt.Fprintf(w, "当前目录：%s\n", run.CurrentDir)
		}
		if run.ProcessedDirs > 0 && run.TotalDirs > run.ProcessedDirs {
			elapsed := time.Since(run.StartedAt)
			eta := time.Duration(float64(elapsed) / float64(run.ProcessedDirs) * float64(run.TotalDirs-run.ProcessedDirs))
			fmt.Fprintf(w, "预计剩余：约 %s（按已完成目录平均）\n", formatDuration(eta))
		}
	}
	if view.Last != nil && (view.Active == nil || view.Last.ID != view.Active.ID) {
		fmt.Fprintf(w, "最近任务：#%d %s（%s），开始 %s，耗时 %s\n",
			view.Last.ID, view.Last.Mode.Label(), runStatusText(view.Last.Status),
			formatTimestamp(&view.Last.StartedAt), formatDuration(view.Last.Duration))
	}
	fmt.Fprintf(w, "数据库：%s（%s）\n", view.DBPath, formatSize(view.DBSize))
}

type scanJSON struct {
	RunID             int64     `json:"run_id"`
	Root              string    `json:"root"`
	Target            string    `json:"target,omitempty"`
	Mode              string    `json:"mode"`
	Scope             string    `json:"scope"`
	Status            string    `json:"status"`
	StatusText        string    `json:"status_text"`
	Accuracy          string    `json:"accuracy"`
	AccuracyText      string    `json:"accuracy_text"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	DurationSeconds   float64   `json:"duration_seconds"`
	TotalDirs         int       `json:"total_directories"`
	DoneDirs          int       `json:"processed_directories"`
	FailedDirs        int       `json:"failed_directories"`
	TotalBytes        int64     `json:"total_bytes"`
	TotalText         string    `json:"total_text"`
	TotalLogicalBytes int64     `json:"total_logical_bytes"`
	FileCount         int64     `json:"file_count"`
	FilesPerSecond    float64   `json:"files_per_second"`
	DirCount          int64     `json:"dir_count"`
	Resumed           bool      `json:"resumed"`
	SizeBasis         string    `json:"size_basis"`
	Errors            []string  `json:"errors,omitempty"`
}

func buildScanJSON(summary *model.ScanSummary) scanJSON {
	if summary == nil {
		return scanJSON{}
	}
	total := summary.TotalAlloc
	if summary.SizeBasis == model.BasisLogical {
		total = summary.TotalLogical
	}
	scope := "root"
	if summary.Mode == model.ModeRebuild {
		scope = "target"
	}
	return scanJSON{
		RunID:             summary.RunID,
		Root:              summary.Root.Path,
		Target:            summary.TargetDir,
		Mode:              string(summary.Mode),
		Scope:             scope,
		Status:            string(summary.DataStatus),
		StatusText:        statusText(summary.DataStatus),
		Accuracy:          string(summary.Accuracy),
		AccuracyText:      summary.Accuracy.Label(),
		StartedAt:         summary.StartedAt,
		FinishedAt:        summary.FinishedAt,
		DurationSeconds:   summary.Duration.Seconds(),
		TotalDirs:         summary.TotalDirs,
		DoneDirs:          summary.DoneDirs,
		FailedDirs:        summary.FailedDirs,
		TotalBytes:        total,
		TotalText:         formatSize(total),
		TotalLogicalBytes: summary.TotalLogical,
		FileCount:         summary.FileCount,
		FilesPerSecond:    scanFileRate(summary),
		DirCount:          summary.DirCount,
		Resumed:           summary.Resumed,
		SizeBasis:         basisValue(summary.SizeBasis),
		Errors:            summary.Errors,
	}
}

func scanFileRate(summary *model.ScanSummary) float64 {
	if summary == nil || summary.Duration <= 0 || summary.FileCount <= 0 {
		return 0
	}
	return float64(summary.FileCount) / summary.Duration.Seconds()
}

func printScanSummary(w io.Writer, summary *model.ScanSummary) {
	if summary == nil {
		return
	}
	fmt.Fprintln(w, "扫描结果")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "扫描根目录：%s\n", summary.Root.Path)
	fmt.Fprintf(w, "模式：%s\n", summary.Mode.Label())
	fmt.Fprintf(w, "任务： #%d\n", summary.RunID)
	fmt.Fprintf(w, "开始时间：%s\n", formatTimestamp(&summary.StartedAt))
	fmt.Fprintf(w, "结束时间：%s\n", formatTimestamp(&summary.FinishedAt))
	fmt.Fprintf(w, "扫描耗时：%s\n", formatDuration(summary.Duration))
	fmt.Fprintf(w, "一级目录：处理 %d / 共 %d，失败 %d\n", summary.DoneDirs, summary.TotalDirs, summary.FailedDirs)
	spaceLabel := "合计空间"
	fileLabel := "文件数"
	if summary.Mode == model.ModeRebuild {
		fmt.Fprintf(w, "目标目录：%s\n", filepath.Join(summary.Root.Path, summary.TargetDir))
		spaceLabel = "目标目录空间"
		fileLabel = "目标目录文件数"
		fmt.Fprintln(w, "说明：单目录重算只刷新目标一级目录；根合计仍包含其他目录与根直属文件。")
	}
	selectedTotal := summary.TotalAlloc
	if summary.SizeBasis == model.BasisLogical {
		selectedTotal = summary.TotalLogical
	}
	fmt.Fprintf(w, "%s：%s（实际分配 %s，逻辑 %s）\n",
		spaceLabel, formatSize(selectedTotal), formatSize(summary.TotalAlloc), formatSize(summary.TotalLogical))
	fmt.Fprintf(w, "%s：%s\n", fileLabel, formatCount(summary.FileCount))
	if rate := scanFileRate(summary); rate > 0 {
		fmt.Fprintf(w, "文件吞吐：%.0f 文件/秒\n", rate)
	}
	fmt.Fprintf(w, "数据状态：%s\n", statusText(summary.DataStatus))
	fmt.Fprintf(w, "准确性：%s\n", summary.Accuracy.Label())
	if summary.Resumed {
		fmt.Fprintln(w, "说明：本次任务包含断点续扫数据，因此整体标记为近似。")
	}
	if len(summary.Errors) > 0 {
		fmt.Fprintln(w, "错误：")
		for i, message := range summary.Errors {
			if i >= 5 {
				fmt.Fprintln(w, "  ...")
				break
			}
			fmt.Fprintf(w, "  - %s\n", message)
		}
	}
}
