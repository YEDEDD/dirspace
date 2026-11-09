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
	if err := s.walkDir(ctx, sess, name, full, 0, &out); err != nil {
		out.Duration = time.Since(start)
		out.CompletedAt = time.Now()
		return out, err
	}
	out.Duration = time.Since(start)
	out.CompletedAt = time.Now()
	return out, nil
}

func (s *Scanner) walkDir(ctx context.Context, sess *scanSession, topName, dir string, depth int, out *model.DirOutcome) error {
	if depth > maxDepth {
		return fmt.Errorf("目录层级超过 %d: %s", maxDepth, dir)
	}
	if err := sess.wait(ctx); err != nil {
		return err
	}
	openStart := time.Now()
	f, err := platform.OpenDir(dir)
	sess.controller.ObserveMetadata(time.Since(openStart), err)
	if err != nil {
		if depth > 0 && errors.Is(err, os.ErrNotExist) {
			out.TransientErrs++
			return nil
		}
		return fmt.Errorf("打开目录失败 %s: %w", dir, err)
	}
	defer f.Close()
	statStart := time.Now()
	dirInfo, statErr := f.Stat()
	sess.controller.ObserveMetadata(time.Since(statStart), statErr)
	if statErr != nil {
		out.TransientErrs++
	} else if meta, ok := platform.Stat(dirInfo); ok {
		out.AllocBytes += meta.Allocated
		out.LogicalBytes += meta.Logical
		sess.counters.alloc.Add(meta.Allocated)
		sess.counters.logical.Add(meta.Logical)
	} else {
		out.TransientErrs++
	}

	for {
		if err := sess.wait(ctx); err != nil {
			return err
		}
		readStart := time.Now()
		entries, readErr := f.ReadDir(readBatchSize)
		sess.controller.ObserveMetadata(time.Since(readStart), readErr)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if depth > 0 && errors.Is(readErr, os.ErrNotExist) {
				out.TransientErrs++
				return nil
			}
			return fmt.Errorf("读取目录失败 %s: %w", dir, readErr)
		}
		for _, entry := range entries {
			if err := sess.wait(ctx); err != nil {
				return err
			}
			name := entry.Name()
			path := filepath.Join(dir, name)
			typ := entry.Type()
			if typ&os.ModeSymlink != 0 {
				out.SymlinkCount++
				continue
			}
			if typ.IsDir() {
				out.DirCount++
				if err := s.walkDir(ctx, sess, topName, path, depth+1, out); err != nil {
					return err
				}
				continue
			}
			if err := sess.wait(ctx); err != nil {
				return err
			}
			statStart := time.Now()
			info, statErr := os.Lstat(path)
			sess.controller.ObserveMetadata(time.Since(statStart), statErr)
			if statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					out.TransientErrs++
					continue
				}
				return fmt.Errorf("读取文件元数据失败 %s: %w", path, statErr)
			}
			if info.IsDir() {
				out.DirCount++
				if err := s.walkDir(ctx, sess, topName, path, depth+1, out); err != nil {
					return err
				}
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
				first, claimErr := sess.hardlinks.Claim(ctx, meta.Dev, meta.Inode, topName)
				if claimErr != nil {
					return fatal(fmt.Errorf("记录硬链接失败 %s: %w", path, claimErr))
				}
				counted = first
			}
			// File count is the number of paths (matching find); hard links are
			// deduplicated only for bytes so one inode is not counted repeatedly.
			out.FileCount++
			sess.counters.files.Add(1)
			if counted {
				out.AllocBytes += meta.Allocated
				out.LogicalBytes += meta.Logical
				sess.counters.alloc.Add(meta.Allocated)
				sess.counters.logical.Add(meta.Logical)
			}
			if sess.inventory != nil {
				rel, relErr := filepath.Rel(sess.rootPath, path)
				if relErr != nil {
					return fatal(fmt.Errorf("计算相对路径失败 %s: %w", path, relErr))
				}
				if err := sess.inventory.Add(ctx, storage.InventoryRecord{
					TopDir:     topName,
					RelPath:    rel,
					SizeBytes:  meta.Logical,
					AllocBytes: meta.Allocated,
					Inode:      meta.Inode,
					Dev:        meta.Dev,
					Nlink:      meta.Nlink,
					MtimeNS:    meta.MtimeNS,
					Mode:       meta.Mode,
				}); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
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
