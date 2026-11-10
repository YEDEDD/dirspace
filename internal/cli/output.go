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
			LastError:       d.LastError,
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

	header := fmt.Sprintf("%s  %s  %s  %s  %s  %s  %s  %s",
		padRight("排名", 4), padRight("目录名称", 40), padLeft("空间", 12),
		padLeft("文件数", 14), padLeft("子目录", 8), padLeft("耗时", 9),
		padRight("统计时间", 19), "状态")
	fmt.Fprintln(w, header)
	for i, d := range view.Dirs {
		name := truncateDisplay(validText(d.Name), 40)
		size := dirBytes(d, view.SizeBasis)
		last := d.LastSuccessAt
		effective := svc.EffectiveStatus(d.DataStatus, last)
		sizeText := formatSize(size)
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
			padRight(name, 40),
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
