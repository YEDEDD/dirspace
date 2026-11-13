package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/service"
)

func printDiff(w io.Writer, view *service.HistoryDiffView) {
	fmt.Fprintln(w, "目录空间增长对比")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "查询路径：%s\n", view.Path)
	fmt.Fprintf(w, "扫描根目录：%s\n", view.Root)
	if view.Base != nil && view.Head != nil {
		baseTime := runFinishedText(view.Base)
		headTime := runFinishedText(view.Head)
		fmt.Fprintf(w, "基线：#%d %s（%s，%s）\n", view.Base.ID, view.Base.Mode.Label(), runStatusText(view.Base.Status), baseTime)
		fmt.Fprintf(w, "当前：#%d %s（%s，%s）\n", view.Head.ID, view.Head.Mode.Label(), runStatusText(view.Head.Status), headTime)
	}
	fmt.Fprintf(w, "对比范围：最近 %s（同模式快照）\n\n", formatDuration(view.Since))

	fmt.Fprintln(w, "目录                         基线          当前          变化")
	var baseTotal, headTotal int64
	for _, row := range view.Rows {
		baseSize, headSize := row.Base.AllocBytes, row.Head.AllocBytes
		if !row.HasBase {
			baseSize = 0
		}
		if !row.HasHead {
			headSize = 0
		}
		baseTotal += baseSize
		headTotal += headSize
		fmt.Fprintf(w, "%-28s %-12s %-12s %s\n",
			clipText(row.Name, 28), formatSize(baseSize), formatSize(headSize), formatDelta(headSize-baseSize))
	}
	fmt.Fprintf(w, "%-28s %-12s %-12s %s\n", "合计", formatSize(baseTotal), formatSize(headTotal), formatDelta(headTotal-baseTotal))
	if len(view.Rows) == 0 {
		fmt.Fprintln(w, "（没有可比较的一级目录）")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "说明：diff 只读本地历史，不重新扫描；基线取同一模式在范围前最近一次完成快照。")
}

func buildDiffJSON(view *service.HistoryDiffView) any {
	dirs := make([]map[string]any, 0, len(view.Rows))
	for _, row := range view.Rows {
		baseSize, headSize := row.Base.AllocBytes, row.Head.AllocBytes
		if !row.HasBase {
			baseSize = 0
		}
		if !row.HasHead {
			headSize = 0
		}
		dirs = append(dirs, map[string]any{
			"name":             row.Name,
			"base_bytes":       baseSize,
			"current_bytes":    headSize,
			"delta_bytes":      headSize - baseSize,
			"base_files":       row.Base.FileCount,
			"current_files":    row.Head.FileCount,
			"base_present":     row.HasBase,
			"current_present":  row.HasHead,
			"base_accuracy":    row.Base.Accuracy,
			"current_accuracy": row.Head.Accuracy,
		})
	}
	return map[string]any{
		"path":          view.Path,
		"root":          view.Root,
		"since_seconds": int64(view.Since / time.Second),
		"base":          runSummaryJSON(view.Base),
		"current":       runSummaryJSON(view.Head),
		"directories":   dirs,
	}
}

func runSummaryJSON(run *model.Run) any {
	if run == nil {
		return nil
	}
	out := map[string]any{
		"id":          run.ID,
		"mode":        string(run.Mode),
		"status":      string(run.Status),
		"started_at":  run.StartedAt,
		"finished_at": run.FinishedAt,
		"duration_ms": run.Duration.Milliseconds(),
	}
	return out
}

func runFinishedText(run *model.Run) string {
	if run == nil || run.FinishedAt == nil {
		return "未知"
	}
	return formatTimestamp(run.FinishedAt)
}

func formatDelta(n int64) string {
	if n == 0 {
		return "0 B"
	}
	sign := "+"
	if n < 0 {
		sign = "-"
		n = -n
	}
	return sign + formatSize(n)
}

func clipText(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit < 2 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}
