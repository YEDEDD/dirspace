package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YEDEDD/dirspace/internal/platform"
)

type quickBudget struct {
	max       int64
	files     atomic.Int64
	truncated atomic.Bool
}

func (b *quickBudget) add(n int64) bool {
	if b == nil || b.max <= 0 {
		return false
	}
	if b.files.Add(n) >= b.max {
		b.truncated.Store(true)
		return true
	}
	return false
}

func (b *quickBudget) exceeded() bool {
	return b != nil && b.max > 0 && b.files.Load() >= b.max
}

var errQuickBudget = errors.New("quick per-directory budget reached")

// scanQuickTree is the parallel quick-mode walker. Unlike the old single-thread
// recursion, every subdirectory is an independent queue task, so one huge
// first-level directory cannot pin the whole scan to one worker. The boolean
// result reports whether a per-top budget stopped the walk early.
func (s *Scanner) scanQuickTree(ctx context.Context, sess *scanSession, root string) (treeAggregate, bool, error) {
	budget := &quickBudget{max: sess.quickMaxFiles}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := sess.intraWorkers
	if workers < 1 {
		workers = 1
	}
	q := newTreeQueue()
	q.push(treeTask{path: root, depth: 0})

	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}

	wakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.cond.Broadcast()
			q.mu.Unlock()
		case <-wakeDone:
		}
	}()
	defer close(wakeDone)

	var agg treeAggregate
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for {
				task, ok := q.pop(ctx)
				if !ok {
					return
				}
				taskAgg, err := s.processQuickDirTask(ctx, sess, q, task, budget)
				mu.Lock()
				agg.add(taskAgg)
				mu.Unlock()
				if err != nil {
					if errors.Is(err, errQuickBudget) {
						budget.truncated.Store(true)
						cancel()
					} else {
						fail(err)
					}
				}
				q.done()
				if err != nil && ctx.Err() != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return agg, budget.truncated.Load(), firstErr
	}
	if budget.truncated.Load() {
		return agg, true, nil
	}
	if err := ctx.Err(); err != nil {
		return agg, false, err
	}
	return agg, false, nil
}

// processQuickDirTask counts every entry in one directory and samples file
// metadata. Child directories are pushed back to the shared queue.
func (s *Scanner) processQuickDirTask(ctx context.Context, sess *scanSession, q *treeQueue, task treeTask, budget *quickBudget) (treeAggregate, error) {
	var agg treeAggregate
	if budget.exceeded() {
		return agg, errQuickBudget
	}
	if sess.quickSlots != nil {
		select {
		case sess.quickSlots <- struct{}{}:
			defer func() { <-sess.quickSlots }()
		case <-ctx.Done():
			return agg, ctx.Err()
		}
	}
	if task.depth > maxDepth {
		return agg, fmt.Errorf("目录层级超过 %d: %s", maxDepth, task.path)
	}
	if err := sess.wait(ctx); err != nil {
		return agg, err
	}
	openStart := time.Now()
	f, err := platform.OpenDir(task.path)
	sess.controller.ObserveMetadata(time.Since(openStart), err)
	if err != nil {
		if task.depth > 0 && errors.Is(err, os.ErrNotExist) {
			agg.transientErrs++
			sess.rollups.addDirect(task.path, agg, budget.truncated.Load())
			return agg, nil
		}
		return agg, fmt.Errorf("打开目录失败 %s: %w", task.path, err)
	}
	defer f.Close()

	statStart := time.Now()
	info, statErr := f.Stat()
	sess.controller.ObserveMetadata(time.Since(statStart), statErr)
	if statErr != nil {
		agg.transientErrs++
	} else if meta, ok := platform.Stat(info); ok {
		agg.allocBytes += meta.Allocated
		agg.logicalBytes += meta.Logical
		sess.counters.alloc.Add(meta.Allocated)
		sess.counters.logical.Add(meta.Logical)
	} else {
		agg.transientErrs++
	}

	percent := sess.quickSamplePercent
	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}
	fileSeen := 0
	sampledFiles := 0
	var sampledAllocWeighted, sampledLogicalWeighted float64
	var fallbackAlloc, fallbackLogical int64
	var firstFile string
	budgetHit := false
	sampleProb := float64(percent) / 100

readLoop:
	for {
		if err := sess.wait(ctx); err != nil {
			return agg, err
		}
		readStart := time.Now()
		entries, readErr := f.ReadDir(readBatchSize)
		sess.controller.ObserveMetadata(time.Since(readStart), readErr)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if task.depth > 0 && errors.Is(readErr, os.ErrNotExist) {
				agg.transientErrs++
				sess.rollups.addDirect(task.path, agg, budget.truncated.Load())
				return agg, nil
			}
			return agg, fmt.Errorf("读取目录失败 %s: %w", task.path, readErr)
		}
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(task.path, name)
			typ := entry.Type()
			if typ&os.ModeSymlink != 0 {
				agg.symlinkCount++
				continue
			}
			if typ.IsDir() {
				agg.dirCount++
				if task.depth+1 > maxDepth {
					return agg, fmt.Errorf("目录层级超过 %d: %s", maxDepth, path)
				}
				q.push(treeTask{path: path, depth: task.depth + 1})
				continue
			}
			if typ != 0 && !typ.IsRegular() {
				continue
			}

			fileSeen++
			if fileSeen == 1 {
				firstFile = path
			}
			agg.fileCount++
			sess.counters.files.Add(1)
			if budget.add(1) {
				budgetHit = true
				break readLoop
			}

			census := fileSeen <= quickCensusFiles
			sample := census || quickSampled(path, sess.quickSalt, percent)
			forcedCensus := false
			var fileInfo os.FileInfo
			var infoErr error
			if typ == 0 {
				classify := sample || len(entries) <= 32
				if !classify {
					continue
				}
				infoStart := time.Now()
				fileInfo, infoErr = entry.Info()
				sess.controller.ObserveMetadata(time.Since(infoStart), infoErr)
				if infoErr == nil && fileInfo.IsDir() {
					agg.dirCount++
					if task.depth+1 > maxDepth {
						return agg, fmt.Errorf("目录层级超过 %d: %s", maxDepth, path)
					}
					q.push(treeTask{path: path, depth: task.depth + 1})
					agg.fileCount--
					sess.counters.files.Add(-1)
					fileSeen--
					continue
				}
				if infoErr == nil && fileInfo.Mode()&os.ModeSymlink != 0 {
					agg.symlinkCount++
					agg.fileCount--
					sess.counters.files.Add(-1)
					fileSeen--
					continue
				}
				if infoErr == nil && !fileInfo.Mode().IsRegular() {
					agg.fileCount--
					sess.counters.files.Add(-1)
					fileSeen--
					continue
				}
				if infoErr != nil {
					if errors.Is(infoErr, os.ErrNotExist) {
						agg.transientErrs++
						continue
					}
					return agg, fmt.Errorf("读取文件元数据失败 %s: %w", path, infoErr)
				}
				if !sample {
					forcedCensus = true
				}
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
						agg.transientErrs++
						continue
					}
					return agg, fmt.Errorf("读取文件元数据失败 %s: %w", path, infoErr)
				}
			}
			if !fileInfo.Mode().IsRegular() {
				continue
			}
			meta, exact := platform.Stat(fileInfo)
			if !exact {
				meta.Nlink = 1
				agg.transientErrs++
			}
			prob := sampleProb
			if census || forcedCensus {
				prob = 1
			}
			sampledFiles++
			if prob > 0 {
				sampledAllocWeighted += float64(meta.Allocated) / prob
				sampledLogicalWeighted += float64(meta.Logical) / prob
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	if sampledFiles == 0 && fileSeen > 0 && firstFile != "" {
		if err := sess.wait(ctx); err != nil {
			return agg, err
		}
		infoStart := time.Now()
		fi, infoErr := os.Lstat(firstFile)
		sess.controller.ObserveMetadata(time.Since(infoStart), infoErr)
		if infoErr == nil && fi.Mode().IsRegular() {
			meta, exact := platform.Stat(fi)
			if !exact {
				meta.Nlink = 1
				agg.transientErrs++
			}
			fallbackAlloc = meta.Allocated * int64(fileSeen)
			fallbackLogical = meta.Logical * int64(fileSeen)
		}
	}
	allocEstimate := int64(sampledAllocWeighted + 0.5)
	logicalEstimate := int64(sampledLogicalWeighted + 0.5)
	if sampledFiles == 0 {
		allocEstimate = fallbackAlloc
		logicalEstimate = fallbackLogical
	}
	agg.allocBytes += allocEstimate
	agg.logicalBytes += logicalEstimate
	sess.counters.alloc.Add(allocEstimate)
	sess.counters.logical.Add(logicalEstimate)
	truncated := budget.truncated.Load() || budgetHit
	sess.rollups.addDirect(task.path, agg, truncated)
	if truncated {
		return agg, errQuickBudget
	}
	return agg, nil
}
