package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/service"
)

func statusText(status model.DataStatus) string {
	switch status {
	case model.DataFresh:
		return "正常"
	case model.DataStale:
		return "过期"
	case model.DataPartial:
		return "部分更新"
	case model.DataFailed:
		return "失败"
	case model.DataScanning:
		return "扫描中"
	case model.DataNever:
		return "未扫描"
	default:
		return string(status)
	}
}

func statusWithContext(status model.DataStatus, last *time.Time) string {
	text := statusText(status)
	if status == model.DataScanning && last != nil {
		text += "（显示上次结果）"
	}
	if status == model.DataFailed && last != nil {
		text += "（保留上次有效数据）"
	}
	if status == model.DataStale && last != nil {
		text += "（保留上次有效数据）"
	}
	if status == model.DataPartial && last != nil {
		text += "（部分目录保留上次结果）"
	}
	return text
}

func basisValue(basis model.SizeBasis) string {
	if basis == model.BasisLogical {
		return "logical"
	}
	return "allocated"
}

func dirBytes(d model.DirStat, basis model.SizeBasis) int64 {
	if basis == model.BasisLogical {
		return d.LogicalBytes
	}
	return d.AllocBytes
}

type queryDirJSON struct {
	Rank            int        `json:"rank"`
	HasBaseline     bool       `json:"has_baseline"`
	Name            string     `json:"name"`
	SizeBytes       int64      `json:"size_bytes"`
	SizeText        string     `json:"size_text"`
	AllocBytes      int64      `json:"alloc_bytes"`
	LogicalBytes    int64      `json:"logical_bytes"`
	FileCount       int64      `json:"file_count"`
	DirCount        int64      `json:"dir_count"`
	SymlinkCount    int64      `json:"symlink_count"`
	DataStatus      string     `json:"data_status"`
	DataStatusText  string     `json:"data_status_text"`
	Accuracy        string     `json:"accuracy"`
	AccuracyText    string     `json:"accuracy_text"`
	ScannedAt       *time.Time `json:"scanned_at"`
	ScannedAtText   string     `json:"scanned_at_text"`
	AgeSeconds      int64      `json:"age_seconds"`
	DurationSeconds float64    `json:"duration_seconds"`
	Truncated       bool       `json:"truncated"`
	LastError       string     `json:"last_error,omitempty"`
}

type queryJSON struct {
	Path              string         `json:"path"`
	HasBaseline       bool           `json:"has_baseline"`
	ScanRoot          string         `json:"scan_root"`
	Single            bool           `json:"single_directory"`
	Server            string         `json:"server,omitempty"`
	FSType            string         `json:"fstype,omitempty"`
	Basis             string         `json:"size_basis"`
	ScannedAt         *time.Time     `json:"scanned_at"`
	ScannedAtText     string         `json:"scanned_at_text"`
	LastAttemptAt     *time.Time     `json:"last_attempt_at,omitempty"`
	LastAttemptText   string         `json:"last_attempt_at_text,omitempty"`
	AgeSeconds        int64          `json:"age_seconds"`
	DurationSeconds   float64        `json:"duration_seconds"`
	DataStatus        string         `json:"data_status"`
	DataStatusText    string         `json:"data_status_text"`
	Accuracy          string         `json:"accuracy"`
	AccuracyText      string         `json:"accuracy_text"`
	TotalBytes        int64          `json:"total_bytes"`
	TotalText         string         `json:"total_text"`
	TotalAllocBytes   int64          `json:"total_alloc_bytes"`
	TotalLogicalBytes int64          `json:"total_logical_bytes"`
	FileCount         int64          `json:"file_count"`
	DirCount          int64          `json:"dir_count"`
	Directories       []queryDirJSON `json:"directories"`
	Note              string         `json:"note"`
}

func buildQueryJSON(svc *service.Service, view *service.View) queryJSON {
	selectedLast := view.Root.LastSuccessAt
	selectedAttempt := view.Root.LastAttemptAt
	selectedStatus := view.DataStatus
	duration := view.Root.Duration
	totalAlloc := view.Root.TotalAlloc
	totalLogical := view.Root.TotalLogical
	totalFiles := view.Root.FileCount
	totalDirs := view.Root.DirCount
	accuracy := view.Root.Accuracy
	if view.Single && view.Target != nil {
		selectedLast = view.Target.LastSuccessAt
		selectedAttempt = view.Target.LastAttemptAt
		duration = view.Target.Duration
		totalAlloc = view.Target.AllocBytes
		totalLogical = view.Target.LogicalBytes
		totalFiles = view.Target.FileCount
		totalDirs = view.Target.DirCount
		accuracy = view.Target.Accuracy
	}
	selectedBytes := totalAlloc
	if view.SizeBasis == model.BasisLogical {
		selectedBytes = totalLogical
	}
	out := queryJSON{
		Path:              view.Path,
		ScanRoot:          view.ScanRoot,
		Single:            view.Single,
		Server:            view.Server,
		FSType:            view.FSType,
		Basis:             basisValue(view.SizeBasis),
		ScannedAt:         selectedLast,
		ScannedAtText:     formatTimestamp(selectedLast),
		LastAttemptAt:     selectedAttempt,
		LastAttemptText:   formatTimestamp(selectedAttempt),
		DurationSeconds:   duration.Seconds(),
		DataStatus:        string(selectedStatus),
		DataStatusText:    statusWithContext(selectedStatus, selectedLast),
		Accuracy:          string(accuracy),
		AccuracyText:      accuracy.Label(),
		TotalBytes:        selectedBytes,
		TotalText:         formatSize(selectedBytes),
		TotalAllocBytes:   totalAlloc,
		TotalLogicalBytes: totalLogical,
		FileCount:         totalFiles,
		DirCount:          totalDirs,
		Directories:       make([]queryDirJSON, 0, len(view.Dirs)),
		Note:              "客户端元数据快照，不代表共享文件系统服务端实时占用；扫描期间文件可能变化。",
	}
	out.HasBaseline = selectedLast != nil
	if selectedStatus == model.DataScanning {
		out.Note = "后台正在扫描，以下为上次已完成的缓存结果；扫描完成后自动更新。"
	}
	if selectedLast != nil {
		out.AgeSeconds = int64(time.Since(*selectedLast).Seconds())
	} else {
		out.TotalText = "n/a"
	}
	for i, d := range view.Dirs {
		last := d.LastSuccessAt
		effective := svc.EffectiveStatus(d.DataStatus, last)
		size := dirBytes(d, view.SizeBasis)
		entry := queryDirJSON{
			Rank:            i + 1,
			HasBaseline:     last != nil,
			Name:            validText(d.Name),
			SizeBytes:       size,
			SizeText:        formatSize(size),
			AllocBytes:      d.AllocBytes,
			LogicalBytes:    d.LogicalBytes,
			FileCount:       d.FileCount,
			DirCount:        d.DirCount,
			SymlinkCount:    d.SymlinkCount,
			DataStatus:      string(effective),
			DataStatusText:  statusWithContext(effective, last),
			Accuracy:        string(d.Accuracy),
			AccuracyText:    d.Accuracy.Label(),
			ScannedAt:       last,
			ScannedAtText:   formatTimestamp(last),
			DurationSeconds: d.Duration.Seconds(),
			Truncated:       d.Truncated,
			LastError:       d.LastError,
		}
		if d.Truncated {
			entry.SizeText = "≥" + entry.SizeText
		}
		if last == nil {
			entry.SizeText = "n/a"
		} else {
			entry.AgeSeconds = int64(time.Since(*last).Seconds())
		}
		out.Directories = append(out.Directories, entry)
	}
	return out
}

func printQuery(w io.Writer, svc *service.Service, view *service.View) {
	selectedLast := view.Root.LastSuccessAt
	selectedAttempt := view.Root.LastAttemptAt
	selectedStatus := view.DataStatus
	duration := view.Root.Duration
	accuracy := view.Root.Accuracy
	totalAlloc := view.Root.TotalAlloc
	totalLogical := view.Root.TotalLogical
	totalFiles := view.Root.FileCount
	totalDirs := view.Root.DirCount
	if view.Single && view.Target != nil {
		selectedLast = view.Target.LastSuccessAt
		selectedAttempt = view.Target.LastAttemptAt
		duration = view.Target.Duration
		accuracy = view.Target.Accuracy
		selectedStatus = view.DataStatus
		totalAlloc = view.Target.AllocBytes
		totalLogical = view.Target.LogicalBytes
		totalFiles = view.Target.FileCount
		totalDirs = view.Target.DirCount
	}
	totalSelected := totalAlloc
	if view.SizeBasis == model.BasisLogical {
		totalSelected = totalLogical
	}

	fmt.Fprintln(w, "目录空间统计")
	fmt.Fprintln(w)
	if view.Single {
		fmt.Fprintf(w, "查询目录：%s\n", view.Path)
		fmt.Fprintf(w, "所属扫描根目录：%s\n", view.ScanRoot)
	} else {
		fmt.Fprintf(w, "扫描根目录：%s\n", view.ScanRoot)
	}
	fmt.Fprintf(w, "挂载来源：%s\n", serverText(view.Server, view.FSType))
	fmt.Fprintf(w, "统计时间：%s（%s）\n", formatTimestamp(selectedLast), formatAgeWithStatus(selectedLast, selectedStatus))
	fmt.Fprintf(w, "扫描耗时：%s\n", formatDuration(duration))
	fmt.Fprintf(w, "数据状态：%s\n", statusWithContext(selectedStatus, selectedLast))
	fmt.Fprintf(w, "准确性：%s\n", accuracy.Label())
	fmt.Fprintf(w, "统计口径：%s\n", view.SizeBasis.Label())
	if selectedStatus == model.DataScanning {
		fmt.Fprintln(w, "提示：后台正在扫描，以下为上次已完成的缓存结果；扫描完成后自动更新。")
	}
	totalText := formatSize(totalSelected)
	fileText := formatCount(totalFiles)
	dirText := formatCount(totalDirs)
	if selectedLast == nil {
		totalText = "n/a"
		fileText = "n/a"
		dirText = "n/a"
	}
	fmt.Fprintf(w, "合计空间：%s（逻辑大小 %s，文件 %s，子目录 %s）\n",
		totalText, formatSize(totalLogical), fileText, dirText)
	if !view.Single && view.Root.SelfFiles > 0 {
		selfBytes := view.Root.SelfAlloc
		if view.SizeBasis == model.BasisLogical {
			selfBytes = view.Root.SelfLogical
		}
		fmt.Fprintf(w, "根目录直属文件：%s 个，%s（已计入合计）\n",
			formatCount(view.Root.SelfFiles), formatSize(selfBytes))
	}
	if selectedLast == nil && selectedAttempt != nil {
		fmt.Fprintf(w, "最近尝试：%s（尚未成功建立基线）\n", formatTimestamp(selectedAttempt))
	}
	fmt.Fprintln(w)

	if len(view.Dirs) == 0 {
		if totalSelected > 0 {
			fmt.Fprintln(w, "该根目录下没有一级子目录；合计空间仅包含根目录直属文件和目录块。")
		} else {
			fmt.Fprintln(w, "暂无一级目录统计。若尚未扫描，请先执行：dirspace scan")
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "说明：统计结果为客户端元数据快照，不代表 NFS 服务端实时占用。")
		return
	}

	nameWidth := 40
	for _, d := range view.Dirs {
		if n := displayWidth(validText(d.Name)); n > nameWidth {
			nameWidth = n
		}
	}
	header := fmt.Sprintf("%s  %s  %s  %s  %s  %s  %s  %s",
		padRight("排名", 4), padRight("目录名称", nameWidth), padLeft("空间", 12),
		padLeft("文件数", 14), padLeft("子目录", 8), padLeft("耗时", 9),
		padRight("统计时间", 19), "状态")
	fmt.Fprintln(w, header)
	for i, d := range view.Dirs {
		name := validText(d.Name)
		size := dirBytes(d, view.SizeBasis)
		last := d.LastSuccessAt
		effective := svc.EffectiveStatus(d.DataStatus, last)
		sizeText := formatSize(size)
		if d.Truncated {
			sizeText = "≥" + sizeText
		}
		fileText := formatCount(d.FileCount)
		dirText := formatCount(d.DirCount)
		status := statusWithContext(effective, last)
		if last == nil {
			sizeText = "n/a"
			fileText = "n/a"
			dirText = "n/a"
		}
		fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s  %s  %s\n",
			padRight(fmt.Sprintf("%d", i+1), 4),
			padRight(name, nameWidth),
			padLeft(sizeText, 12),
			padLeft(fileText, 14),
			padLeft(dirText, 8),
			padLeft(formatDuration(d.Duration), 9),
			padRight(formatTimestamp(last), 19),
			status)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "说明：空间为客户端按元数据汇总的估算值，不代表 NFS 服务端实时占用；")
	fmt.Fprintln(w, "      扫描期间文件可能新增、删除或改变大小，准确性以“数据状态”与“准确性”为准。")
}

func serverText(server, fsType string) string {
	if server != "" {
		if fsType == "" {
			return server
		}
		return fmt.Sprintf("%s（%s）", server, fsType)
	}
	if fsType != "" {
		return "本地或非 NFS 文件系统（" + fsType + "）"
	}
	return "未检测到挂载信息"
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

type partialDirJSON struct {
	Rank            int       `json:"rank"`
	Name            string    `json:"name"`
	SizeBytes       int64     `json:"size_bytes"`
	SizeText        string    `json:"size_text"`
	AllocBytes      int64     `json:"alloc_bytes"`
	LogicalBytes    int64     `json:"logical_bytes"`
	FileCount       int64     `json:"file_count"`
	DirCount        int64     `json:"dir_count"`
	SymlinkCount    int64     `json:"symlink_count"`
	DurationSeconds float64   `json:"duration_seconds"`
	Truncated       bool      `json:"truncated"`
	Source          string    `json:"source"`
	DataStatus      string    `json:"data_status"`
	Accuracy        string    `json:"accuracy"`
	CompletedAt     time.Time `json:"completed_at"`
}

type partialJSON struct {
	Path          string           `json:"path"`
	ScanRoot      string           `json:"scan_root"`
	RunID         int64            `json:"run_id"`
	Mode          string           `json:"mode"`
	ModeText      string           `json:"mode_text"`
	Status        string           `json:"status"`
	DoneDirs      int              `json:"done_dirs"`
	TotalDirs     int              `json:"total_dirs"`
	FileCount     int64            `json:"file_count"`
	TotalBytes    int64            `json:"total_bytes"`
	MergedBytes   int64            `json:"merged_total_bytes"`
	FreshDirs     int              `json:"fresh_dirs"`
	CarriedDirs   int              `json:"carried_dirs"`
	FreshBytes    int64            `json:"fresh_total_bytes"`
	CarriedBytes  int64            `json:"carried_total_bytes"`
	Mixed         bool             `json:"mixed"`
	CurrentDir    string           `json:"current_dir"`
	CompletedDirs []partialDirJSON `json:"directories"`
	Note          string           `json:"note"`
}

func partialSourceText(source service.PartialSource) string {
	if source == service.PartialSourceLast {
		return "上次"
	}
	return "本轮"
}

func buildPartialJSON(view *service.PartialView) partialJSON {
	note := "staging 数据：仅包含当前任务已完成的一级目录，尚未发布到正式缓存。"
	if view.CarriedDirs > 0 && view.FreshDirs == 0 {
		note = "合并视图：本轮尚无已完成目录，当前显示上次成功结果；不是同一时点快照。"
	} else if view.Mixed {
		note = "合并视图：本轮已完成目录覆盖上次成功结果，未完成目录沿用上次；混合合计不是同一时点快照。"
	}
	out := partialJSON{
		Path: view.Path, ScanRoot: view.Root.Path,
		CompletedDirs: make([]partialDirJSON, 0, len(view.Dirs)),
		FreshDirs:     view.FreshDirs, CarriedDirs: view.CarriedDirs,
		FreshBytes: view.FreshBytes, CarriedBytes: view.CarriedBytes,
		Mixed: view.Mixed, Note: note,
	}
	if view.Run != nil {
		out.RunID = view.Run.ID
		out.Mode = string(view.Run.Mode)
		out.ModeText = view.Run.Mode.Label()
		out.Status = string(view.Run.Status)
		out.DoneDirs = view.Run.ProcessedDirs
		out.TotalDirs = view.Run.TotalDirs
		out.FileCount = view.Run.TotalFiles
		out.TotalBytes = view.Run.TotalBytes
		out.CurrentDir = view.Run.CurrentDir
	}
	for i, item := range view.Dirs {
		d := item.Dir
		size := dirBytes(d, view.SizeBasis)
		entry := partialDirJSON{
			Rank: i + 1, Name: validText(d.Name), SizeBytes: size,
			SizeText: formatSize(size), AllocBytes: d.AllocBytes,
			LogicalBytes: d.LogicalBytes, FileCount: d.FileCount,
			DirCount: d.DirCount, SymlinkCount: d.SymlinkCount,
			DurationSeconds: d.Duration.Seconds(), Truncated: d.Truncated,
			Source: string(item.Source), DataStatus: string(d.DataStatus),
			Accuracy: string(d.Accuracy),
		}
		if d.LastSuccessAt != nil {
			entry.CompletedAt = *d.LastSuccessAt
		}
		out.MergedBytes += size
		out.CompletedDirs = append(out.CompletedDirs, entry)
	}
	return out
}

func printPartial(w io.Writer, view *service.PartialView) {
	fmt.Fprintln(w, "dirspace 部分扫描结果")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "扫描根：%s\n", view.Root.Path)
	if view.Run != nil {
		elapsed := view.Run.Duration
		if view.Run.FinishedAt == nil {
			elapsed = time.Since(view.Run.StartedAt)
		}
		fmt.Fprintf(w, "当前任务：#%d %s（%s） %d/%d 目录，%s 文件，已运行 %s\n",
			view.Run.ID, view.Run.Mode.Label(), view.Run.Status,
			view.Run.ProcessedDirs, view.Run.TotalDirs,
			formatCount(view.Run.TotalFiles), formatDuration(elapsed))
		if view.Run.CurrentDir != "" {
			fmt.Fprintf(w, "当前目录：%s\n", validText(view.Run.CurrentDir))
		}
	}
	if len(view.Dirs) == 0 {
		fmt.Fprintln(w, "暂无已完成的目录；当前目录仍在扫描，完成后会出现在这里。")
		return
	}
	var freshAlloc, freshLogical, freshFiles int64
	var carriedAlloc, carriedLogical, carriedFiles int64
	for _, item := range view.Dirs {
		if item.Source == service.PartialSourceLast {
			carriedAlloc += item.Dir.AllocBytes
			carriedLogical += item.Dir.LogicalBytes
			carriedFiles += item.Dir.FileCount
		} else {
			freshAlloc += item.Dir.AllocBytes
			freshLogical += item.Dir.LogicalBytes
			freshFiles += item.Dir.FileCount
		}
	}
	if view.CarriedDirs > 0 {
		fmt.Fprintf(w, "本轮已完成：%d 个，合计 %s（逻辑 %s，文件 %s）\n",
			view.FreshDirs, formatSize(freshAlloc), formatSize(freshLogical), formatCount(freshFiles))
		fmt.Fprintf(w, "沿用上次：%d 个，合计 %s（逻辑 %s，文件 %s）\n",
			view.CarriedDirs, formatSize(carriedAlloc), formatSize(carriedLogical), formatCount(carriedFiles))
		fmt.Fprintf(w, "混合合计：%s（非同一时点快照）\n",
			formatSize(freshAlloc+carriedAlloc))
	} else {
		fmt.Fprintf(w, "已完成目录：%d 个，合计 %s（逻辑 %s，文件 %s）\n",
			view.FreshDirs, formatSize(freshAlloc), formatSize(freshLogical), formatCount(freshFiles))
	}
	if view.CarriedDirs > 0 {
		fmt.Fprintln(w, "说明：本轮已完成的目录用本轮结果，未完成的沿用上次成功结果；不会提前发布 top_dirs。")
	} else {
		fmt.Fprintln(w, "说明：以下为当前任务 staging 结果，尚未发布为正式缓存；快速估算的空间为抽样值。")
	}
	fmt.Fprintln(w)

	nameWidth := 40
	for _, item := range view.Dirs {
		if n := displayWidth(validText(item.Dir.Name)); n > nameWidth {
			nameWidth = n
		}
	}
	fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s  %s  %s\n",
		padRight("排名", 4), padRight("目录名称", nameWidth), padLeft("空间", 12),
		padLeft("文件数", 14), padLeft("子目录", 8), padLeft("来源", 6),
		padLeft("耗时", 9), padRight("完成时间", 19))
	for i, item := range view.Dirs {
		d := item.Dir
		last := d.LastSuccessAt
		sizeText := formatSize(dirBytes(d, view.SizeBasis))
		if d.Truncated {
			sizeText = "≥" + sizeText
		}
		fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s  %s  %s\n",
			padRight(fmt.Sprintf("%d", i+1), 4),
			padRight(validText(d.Name), nameWidth),
			padLeft(sizeText, 12),
			padLeft(formatCount(d.FileCount), 14),
			padLeft(formatCount(d.DirCount), 8),
			padLeft(partialSourceText(item.Source), 6),
			padLeft(formatDuration(d.Duration), 9),
			padRight(formatTimestamp(last), 19))
	}
}

type sizeJSON struct {
	Path          string     `json:"path"`
	ScanRoot      string     `json:"scan_root"`
	SizeBytes     int64      `json:"size_bytes"`
	SizeText      string     `json:"size_text"`
	AllocBytes    int64      `json:"alloc_bytes"`
	LogicalBytes  int64      `json:"logical_bytes"`
	FileCount     int64      `json:"file_count"`
	DirCount      int64      `json:"dir_count"`
	Basis         string     `json:"size_basis"`
	Source        string     `json:"source"`
	DataStatus    string     `json:"data_status"`
	Accuracy      string     `json:"accuracy"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at,omitempty"`
	ExactScan     bool       `json:"exact_scan"`
}

func buildSizeJSON(view *service.SizeView) sizeJSON {
	out := sizeJSON{
		Path: view.Path, ScanRoot: view.Root.Path, Basis: basisValue(view.SizeBasis),
		Source: "root", ExactScan: view.Scanned,
	}
	if view.Entry != nil {
		e := view.Entry
		out.Source = "index"
		out.AllocBytes, out.LogicalBytes = e.AllocBytes, e.LogicalBytes
		out.FileCount, out.DirCount = e.FileCount, e.DirCount
		out.DataStatus, out.Accuracy = string(e.DataStatus), string(e.Accuracy)
		out.UpdatedAt = e.UpdatedAt
		last := e.UpdatedAt
		out.LastSuccessAt = &last
	} else {
		r := view.Root
		out.AllocBytes, out.LogicalBytes = r.TotalAlloc, r.TotalLogical
		out.FileCount, out.DirCount = r.FileCount, r.DirCount
		out.DataStatus, out.Accuracy = string(r.Status), string(r.Accuracy)
		out.LastSuccessAt, out.UpdatedAt = r.LastSuccessAt, r.UpdatedAt
	}
	if view.SizeBasis == model.BasisLogical {
		out.SizeBytes = out.LogicalBytes
	} else {
		out.SizeBytes = out.AllocBytes
	}
	out.SizeText = formatSize(out.SizeBytes)
	return out
}

func printSize(w io.Writer, view *service.SizeView) {
	out := buildSizeJSON(view)
	fmt.Fprintln(w, "目录大小")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "目录：%s\n", validText(out.Path))
	fmt.Fprintf(w, "扫描根：%s\n", validText(out.ScanRoot))
	fmt.Fprintf(w, "空间：%s\n", out.SizeText)
	fmt.Fprintf(w, "文件数：%s\n", formatCount(out.FileCount))
	fmt.Fprintf(w, "子目录数：%s\n", formatCount(out.DirCount))
	fmt.Fprintf(w, "准确性：%s\n", out.Accuracy)
	fmt.Fprintf(w, "数据状态：%s\n", out.DataStatus)
	if out.LastSuccessAt != nil {
		fmt.Fprintf(w, "数据时间：%s\n", formatTimestamp(out.LastSuccessAt))
	} else {
		fmt.Fprintln(w, "数据时间：—")
	}
	source := "扫描根汇总"
	if out.Source == "index" {
		source = "目录索引"
	}
	if out.ExactScan {
		source = "本次精确扫描"
	}
	fmt.Fprintf(w, "来源：%s\n", source)
}

type childJSON struct {
	Rank         int    `json:"rank"`
	Path         string `json:"path"`
	Name         string `json:"name"`
	Depth        int    `json:"depth"`
	SizeBytes    int64  `json:"size_bytes"`
	SizeText     string `json:"size_text"`
	AllocBytes   int64  `json:"alloc_bytes"`
	LogicalBytes int64  `json:"logical_bytes"`
	FileCount    int64  `json:"file_count"`
	DirCount     int64  `json:"dir_count"`
	Accuracy     string `json:"accuracy"`
	AccuracyText string `json:"accuracy_text"`
	DataStatus   string `json:"data_status"`
	Truncated    bool   `json:"truncated"`
}

type childrenJSON struct {
	Path      string      `json:"path"`
	ScanRoot  string      `json:"scan_root"`
	Depth     int         `json:"depth"`
	SizeBytes int64       `json:"size_bytes"`
	SizeText  string      `json:"size_text"`
	Basis     string      `json:"size_basis"`
	Children  []childJSON `json:"children"`
	Note      string      `json:"note"`
}

func indexBytes(e model.DirIndexEntry, basis model.SizeBasis) int64 {
	if basis == model.BasisLogical {
		return e.LogicalBytes
	}
	return e.AllocBytes
}

func buildChildrenJSON(view *service.IndexChildrenView) childrenJSON {
	out := childrenJSON{
		Path: view.Path, ScanRoot: view.ScanRoot, Basis: basisValue(view.SizeBasis),
		Children: make([]childJSON, 0, len(view.Dirs)),
		Note:     "目录索引只保留每层 top K 子目录；更深的路径可能未索引。",
	}
	if view.Parent != nil {
		out.Depth = view.Parent.Depth
		out.SizeBytes = indexBytes(*view.Parent, view.SizeBasis)
		out.SizeText = formatSize(out.SizeBytes)
	}
	for i, e := range view.Dirs {
		size := indexBytes(e, view.SizeBasis)
		out.Children = append(out.Children, childJSON{
			Rank: i + 1, Path: e.Path, Name: validText(e.Name), Depth: e.Depth,
			SizeBytes: size, SizeText: formatSize(size), AllocBytes: e.AllocBytes,
			LogicalBytes: e.LogicalBytes, FileCount: e.FileCount, DirCount: e.DirCount,
			Accuracy: string(e.Accuracy), AccuracyText: e.Accuracy.Label(),
			DataStatus: string(e.DataStatus), Truncated: e.Truncated,
		})
	}
	return out
}

func printChildren(w io.Writer, view *service.IndexChildrenView) {
	fmt.Fprintln(w, "目录索引下钻")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "查询目录：%s\n", view.Path)
	fmt.Fprintf(w, "扫描根：%s\n", view.ScanRoot)
	if view.Parent != nil {
		fmt.Fprintf(w, "本目录空间：%s（%s）\n", formatSize(indexBytes(*view.Parent, view.SizeBasis)),
			view.Parent.Accuracy.Label())
	}
	if len(view.Dirs) == 0 {
		fmt.Fprintln(w, "该目录没有索引子目录；可能尚未扫描、索引深度不足，或该目录为空。")
		return
	}
	nameWidth := 30
	for _, e := range view.Dirs {
		if n := displayWidth(e.Path); n > nameWidth {
			nameWidth = n
		}
	}
	fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s\n",
		padRight("排名", 4), padRight("路径", nameWidth), padLeft("空间", 12),
		padLeft("文件数", 14), padLeft("子目录", 8), "准确性")
	for i, e := range view.Dirs {
		fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s\n",
			padRight(fmt.Sprintf("%d", i+1), 4),
			padRight(e.Path, nameWidth),
			padLeft(formatSize(indexBytes(e, view.SizeBasis)), 12),
			padLeft(formatCount(e.FileCount), 14),
			padLeft(formatCount(e.DirCount), 8),
			e.Accuracy.Label())
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "说明：索引每层只保留 top K 子目录；未出现的深层路径需要提高 index_depth 或按需重算。")
}
