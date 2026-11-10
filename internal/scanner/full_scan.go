package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/platform"
	"github.com/YEDEDD/dirspace/internal/storage"
)

const (
	readBatchSize = 256
	maxDepth      = 512
)

// scanTop recursively scans one first-level directory and returns its complete
// aggregate. Only metadata is read; file contents are never opened.
func (s *Scanner) scanTop(ctx context.Context, sess *scanSession, name string) (model.DirOutcome, error) {
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
	if sess.quick {
		return s.scanQuickTop(ctx, sess, name)
	}
	if err := s.scanTree(ctx, sess, name, full, &out); err != nil {
		out.Duration = time.Since(start)
		out.CompletedAt = time.Now()
		return out, err
	}
	out.Duration = time.Since(start)
	out.CompletedAt = time.Now()
	return out, nil
}

// scanRootSelf captures regular files and the directory block directly under
// the scan root. Child directories are handled by scanTop and are skipped here.
func (s *Scanner) scanRootSelf(ctx context.Context, sess *scanSession) (out model.DirOutcome, retErr error) {
	out = model.DirOutcome{Name: ".", CompletedAt: time.Now()}
	start := time.Now()
	defer func() { out.Duration = time.Since(start); out.CompletedAt = time.Now() }()

	if err := sess.wait(ctx); err != nil {
		return out, err
	}
	openStart := time.Now()
	f, err := platform.OpenDir(sess.rootPath)
	sess.controller.ObserveMetadata(time.Since(openStart), err)
	if err != nil {
		return out, fmt.Errorf("打开扫描根目录失败 %s: %w", sess.rootPath, err)
	}
	defer f.Close()
	if err := sess.wait(ctx); err != nil {
		return out, err
	}
	statStart := time.Now()
	info, statErr := f.Stat()
	sess.controller.ObserveMetadata(time.Since(statStart), statErr)
	if statErr != nil {
		out.TransientErrs++
	} else if meta, ok := platform.Stat(info); ok {
		out.AllocBytes += meta.Allocated
		out.LogicalBytes += meta.Logical
		sess.counters.alloc.Add(meta.Allocated)
		sess.counters.logical.Add(meta.Logical)
	} else {
		out.TransientErrs++
	}
	for {
		if err := sess.wait(ctx); err != nil {
			return out, err
		}
		readStart := time.Now()
		entries, readErr := f.ReadDir(readBatchSize)
		sess.controller.ObserveMetadata(time.Since(readStart), readErr)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				out.SymlinkCount++
				continue
			}
			if err := sess.wait(ctx); err != nil {
				return out, err
			}
			infoStart := time.Now()
			info, infoErr := entry.Info()
			sess.controller.ObserveMetadata(time.Since(infoStart), infoErr)
			if infoErr != nil {
				if errors.Is(infoErr, os.ErrNotExist) {
					out.TransientErrs++
					continue
				}
				return out, fmt.Errorf("读取根目录直属文件元数据失败 %s: %w", entry.Name(), infoErr)
			}
			if info.IsDir() {
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				out.SymlinkCount++
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			meta, exact := platform.Stat(info)
			if !exact {
				meta.Nlink = 1
				out.TransientErrs++
			}
			if meta.Nlink == 0 {
				out.TransientErrs++
				continue
			}
			counted := true
			if meta.Nlink > 1 {
				first, claimErr := sess.hardlinks.Claim(ctx, meta.Dev, meta.Inode, ".")
				if claimErr != nil {
					return out, fatal(fmt.Errorf("记录根目录硬链接失败 %s: %w", entry.Name(), claimErr))
				}
				counted = first
			}
			out.FileCount++
			sess.counters.files.Add(1)
			if counted {
				out.AllocBytes += meta.Allocated
				out.LogicalBytes += meta.Logical
				sess.counters.alloc.Add(meta.Allocated)
				sess.counters.logical.Add(meta.Logical)
			}
			if sess.inventory != nil {
				if err := sess.inventory.Add(ctx, storage.InventoryRecord{
					TopDir:     ".",
					RelPath:    entry.Name(),
					SizeBytes:  meta.Logical,
					AllocBytes: meta.Allocated,
					Inode:      meta.Inode,
					Dev:        meta.Dev,
					Nlink:      meta.Nlink,
					MtimeNS:    meta.MtimeNS,
					Mode:       meta.Mode,
				}); err != nil {
					return out, err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return out, nil
		}
		if readErr != nil {
			return out, fmt.Errorf("读取扫描根目录失败 %s: %w", sess.rootPath, readErr)
		}
	}
}
