package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
)

// scanQuickTop estimates one first-level directory from directory entries and a
// salted, path-hashed sample of file metadata. Directory blocks and file counts
// are exact; byte counts are an estimate and are explicitly marked as sampled.
func (s *Scanner) scanQuickTop(ctx context.Context, sess *scanSession, name string) (model.DirOutcome, error) {
	start := time.Now()
	out := model.DirOutcome{Name: name}
	full := filepath.Join(sess.rootPath, name)
	if err := sess.wait(ctx); err != nil {
		return out, err
	}
	statStart := time.Now()
	info, err := os.Lstat(full)
	sess.controller.ObserveMetadata(time.Since(statStart), err)
	if err != nil {
		out.Duration = time.Since(start)
		out.CompletedAt = time.Now()
		return out, fmt.Errorf("无法读取目录 %s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return out, fmt.Errorf("不扫描符号链接: %s", full)
	}
	if !info.IsDir() {
		return out, fmt.Errorf("不是目录: %s", full)
	}
	sess.startLive(name)
	agg, truncated, err := s.scanQuickTree(ctx, sess, full, name)
	if err != nil {
		sess.finishLive(name)
		out.Duration = time.Since(start)
		out.CompletedAt = time.Now()
		return out, err
	}
	out.AllocBytes = agg.allocBytes
	out.LogicalBytes = agg.logicalBytes
	out.FileCount = agg.fileCount
	out.DirCount = agg.dirCount
	out.SymlinkCount = agg.symlinkCount
	out.TransientErrs = agg.transientErrs
	out.Truncated = truncated
	sess.finishLive(name)
	out.Duration = time.Since(start)
	out.CompletedAt = time.Now()
	return out, nil
}
