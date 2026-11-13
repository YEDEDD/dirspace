package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/YEDEDD/dirspace/internal/model"
)

func formatSize(n int64) string {
	if n < 0 {
		return "n/a"
	}
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
		tib = 1 << 40
		pib = 1 << 50
	)
	switch {
	case n >= pib:
		return fmt.Sprintf("%.2f PB", float64(n)/pib)
	case n >= tib:
		return fmt.Sprintf("%.2f TB", float64(n)/tib)
	case n >= gib:
		return fmt.Sprintf("%.2f GB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.2f MB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.2f KB", float64(n)/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func formatCount(n int64) string {
	negative := n < 0
	if negative {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	start := len(s) % 3
	if start == 0 {
		start = 3
	}
	b.WriteString(s[:start])
	for i := start; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	if negative {
		return "-" + b.String()
	}
	return b.String()
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Millisecond)
	if d < time.Second {
		return d.String()
	}
	seconds := int64(math.Round(d.Seconds()))
	days := seconds / 86400
	seconds %= 86400
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60
	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%dd", days)
	}
	if hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
	}
	if minutes > 0 {
		fmt.Fprintf(&b, "%dm", minutes)
	}
	if seconds > 0 && days == 0 {
		fmt.Fprintf(&b, "%ds", seconds)
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}

func formatAge(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "从未成功统计"
	}
	age := time.Since(*t)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return "刚刚"
	case age < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(age.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(age.Hours()/24))
	}
}

func formatUntil(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "未安排"
	}
	until := time.Until(*t)
	if until <= 0 {
		return "已到期"
	}
	switch {
	case until < time.Minute:
		return "不到 1 分钟后"
	case until < time.Hour:
		return fmt.Sprintf("%d 分钟后", int(until.Minutes()))
	case until < 24*time.Hour:
		return fmt.Sprintf("%d 小时后", int(until.Hours()))
	default:
		return fmt.Sprintf("%d 天后", int(until.Hours()/24))
	}
}

func formatTimestamp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32:
		return 0
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1FAFF:
		return 2
	default:
		return 1
	}
}

func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		width += runeWidth(r)
	}
	return width
}

func truncateDisplay(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	var b strings.Builder
	width := 0
	for _, r := range s {
		w := runeWidth(r)
		if width+w > max-1 {
			break
		}
		b.WriteRune(r)
		width += w
	}
	b.WriteString("…")
	return b.String()
}

func padRight(s string, width int) string {
	w := displayWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func padLeft(s string, width int) string {
	w := displayWidth(s)
	if w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}

func validText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return '?'
		}
		return r
	}, s)
}

// formatAgeWithStatus distinguishes a root that has partial per-directory data
// but no complete baseline from one that has never produced any usable data.
func formatAgeWithStatus(t *time.Time, status model.DataStatus) string {
	if (t == nil || t.IsZero()) && status == model.DataPartial {
		return "尚无完整统计"
	}
	return formatAge(t)
}
