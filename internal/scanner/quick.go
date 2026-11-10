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
)

// scanQuickTop estimates one first-level directory from directory entries and a
// deterministic sample of file metadata. Directory blocks and file counts are
// exact; byte counts are an estimate and are explicitly marked as sampled.
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
	if err := s.walkQuickDir(ctx, sess, full, 0, &out); err != nil {
		out.Duration = time.Since(start)
		out.CompletedAt = time.Now()
		return out, err
	}
	out.Duration = time.Since(start)
	out.CompletedAt = time.Now()
	return out, nil
}

func (s *Scanner) walkQuickDir(ctx context.Context, sess *scanSession, dir string, depth int, out *model.DirOutcome) error {
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

	percent := sess.quickSamplePercent
	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}
	step := 100 / percent
	if step < 1 {
		step = 1
	}
	fileSeen := 0
	sampledFiles := 0
	var sampledAlloc, sampledLogical int64
	var firstFile string

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
			name := entry.Name()
			path := filepath.Join(dir, name)
			typ := entry.Type()
			if typ&os.ModeSymlink != 0 {
				out.SymlinkCount++
				continue
			}
			if typ.IsDir() {
				out.DirCount++
				if err := s.walkQuickDir(ctx, sess, path, depth+1, out); err != nil {
					return err
				}
				continue
			}
			if typ != 0 && !typ.IsRegular() {
				continue
			}

			fileSeen++
			if fileSeen == 1 {
				firstFile = path
			}
			out.FileCount++
			sess.counters.files.Add(1)

			sample := fileSeen%step == 0
			var fileInfo os.FileInfo
			var infoErr error
			if typ == 0 {
				// Type 0 is both "regular" and "DT_UNKNOWN". On a large
				// directory, inspect only the sample (and assume the rest are
				// regular); on a small batch inspect everything so a directory
				// hiding in DT_UNKNOWN is still discovered. This is what makes
				// quick mode faster than a full lstat walk.
				classify := sample || len(entries) <= 32
				if !classify {
					continue
				}
				infoStart := time.Now()
				fileInfo, infoErr = entry.Info()
				sess.controller.ObserveMetadata(time.Since(infoStart), infoErr)
				if infoErr == nil && fileInfo.IsDir() {
					out.DirCount++
					if err := s.walkQuickDir(ctx, sess, path, depth+1, out); err != nil {
						return err
					}
					out.FileCount--
					sess.counters.files.Add(-1)
					fileSeen--
					continue
				}
				if infoErr == nil && fileInfo.Mode()&os.ModeSymlink != 0 {
					out.SymlinkCount++
					out.FileCount--
					sess.counters.files.Add(-1)
					fileSeen--
					continue
				}
				if infoErr == nil && !fileInfo.Mode().IsRegular() {
					out.FileCount--
					sess.counters.files.Add(-1)
					fileSeen--
					continue
				}
				if infoErr != nil {
					if errors.Is(infoErr, os.ErrNotExist) {
						out.TransientErrs++
						continue
					}
					return fmt.Errorf("读取文件元数据失败 %s: %w", path, infoErr)
				}
				// Small batches are exact: every regular file was inspected.
				sample = true
			}
			if !sample {
				continue
			}
			if fileInfo == nil {
				infoStart := time.Now()
				fileInfo, infoErr = os.Lstat(path)
				sess.controller.ObserveMetadata(time.Since(infoStart), infoErr)
				if infoErr != nil {
					if errors.Is(infoErr, os.ErrNotExist) {
						out.TransientErrs++
						continue
					}
					return fmt.Errorf("读取文件元数据失败 %s: %w", path, infoErr)
				}
			}
			if !fileInfo.Mode().IsRegular() {
				continue
			}
			meta, exact := platform.Stat(fileInfo)
			if !exact {
				meta.Nlink = 1
				out.TransientErrs++
			}
			sampledFiles++
			sampledAlloc += meta.Allocated
			sampledLogical += meta.Logical
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	if sampledFiles == 0 && fileSeen > 0 && firstFile != "" {
		// A small directory may be shorter than the sampling step. One
		// metadata lookup gives it a non-zero estimate.
		if err := sess.wait(ctx); err != nil {
			return err
		}
		infoStart := time.Now()
		fi, infoErr := os.Lstat(firstFile)
		sess.controller.ObserveMetadata(time.Since(infoStart), infoErr)
		if infoErr == nil && fi.Mode().IsRegular() {
			meta, exact := platform.Stat(fi)
			if !exact {
				meta.Nlink = 1
				out.TransientErrs++
			}
			sampledFiles = 1
			sampledAlloc = meta.Allocated
			sampledLogical = meta.Logical
		}
	}
	if sampledFiles > 0 && fileSeen > 0 {
		// Scale only this directory's files. out.FileCount also contains
		// recursively counted children, which have already been estimated by
		// their own walkQuickDir call.
		out.AllocBytes += scaleSample(sampledAlloc, int64(sampledFiles), int64(fileSeen))
		out.LogicalBytes += scaleSample(sampledLogical, int64(sampledFiles), int64(fileSeen))
	}
	return nil
}

func scaleSample(sample int64, sampled, total int64) int64 {
	if sample <= 0 || sampled <= 0 || total <= 0 {
		return 0
	}
	return int64(float64(sample) * float64(total) / float64(sampled))
}
