package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/YEDEDD/dirspace/internal/platform"
	"github.com/YEDEDD/dirspace/internal/storage"
)

// fileScanResult is the per-file result of one metadata lookup. Results are
// collected by index, so a batch can be processed by several workers without
// sharing the parent DirOutcome.
type fileScanResult struct {
	path          string
	isDir         bool
	symlink       bool
	fileCount     int64
	allocBytes    int64
	logicalBytes  int64
	transientErrs int64
	err           error
}

// scanFiles processes regular-file candidates with a bounded worker pool.
// Directory entries whose type is unknown are included as candidates too; if
// Lstat says they are a directory, the caller recurses.
func (s *Scanner) scanFiles(ctx context.Context, sess *scanSession, topName string, files []string) ([]fileScanResult, error) {
	results := make([]fileScanResult, len(files))
	if len(files) == 0 {
		return results, nil
	}
	workers := sess.intraWorkers
	if workers < 1 {
		workers = 1
	}
	if workers > len(files) {
		workers = len(files)
	}
	if workers == 1 {
		for i, path := range files {
			results[i] = s.scanFile(ctx, sess, topName, path)
		}
		return results, firstFileError(results)
	}

	jobs := make(chan int, len(files))
	for i := range files {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for index := range jobs {
				results[index] = s.scanFile(ctx, sess, topName, files[index])
			}
		}()
	}
	wg.Wait()
	return results, firstFileError(results)
}

func firstFileError(results []fileScanResult) error {
	for i := range results {
		if results[i].err != nil {
			return results[i].err
		}
	}
	return nil
}

// scanFile performs one Lstat and, for a regular file, hardlink dedupe and
// optional inventory write. It never mutates the parent outcome.
func (s *Scanner) scanFile(ctx context.Context, sess *scanSession, topName, path string) fileScanResult {
	res := fileScanResult{path: path}
	if err := sess.wait(ctx); err != nil {
		res.err = err
		return res
	}
	statStart := time.Now()
	info, statErr := os.Lstat(path)
	sess.controller.ObserveMetadata(time.Since(statStart), statErr)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			res.transientErrs++
			return res
		}
		res.err = fmt.Errorf("读取文件元数据失败 %s: %w", path, statErr)
		return res
	}
	if info.IsDir() {
		res.isDir = true
		return res
	}
	if info.Mode()&os.ModeSymlink != 0 {
		res.symlink = true
		return res
	}
	if !info.Mode().IsRegular() {
		return res
	}
	meta, exact := platform.Stat(info)
	if !exact {
		meta.Nlink = 1
		res.transientErrs++
	}
	if meta.Nlink == 0 {
		res.transientErrs++
		return res
	}
	counted := true
	if meta.Nlink > 1 {
		first, claimErr := sess.hardlinks.Claim(ctx, meta.Dev, meta.Inode, topName)
		if claimErr != nil {
			res.err = fatal(fmt.Errorf("记录硬链接失败 %s: %w", path, claimErr))
			return res
		}
		counted = first
	}
	// File count is the number of paths; hard links are deduplicated only for
	// bytes so one inode is not counted repeatedly.
	res.fileCount = 1
	if counted {
		res.allocBytes = meta.Allocated
		res.logicalBytes = meta.Logical
	}
	if sess.inventory != nil {
		rel, relErr := filepath.Rel(sess.rootPath, path)
		if relErr != nil {
			res.err = fatal(fmt.Errorf("计算相对路径失败 %s: %w", path, relErr))
			return res
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
			res.err = err
			return res
		}
	}
	return res
}
