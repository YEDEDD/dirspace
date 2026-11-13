package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/platform"
)

// treeTask is either a directory to enumerate or a small batch of file paths
// to stat. Keeping both kinds in one queue lets a flat directory use many
// workers, while a deep tree still keeps all workers busy.
type treeTask struct {
	path  string
	depth int
	files []string
	quick *quickFileTask
}

// treeQueue is an unbounded work queue with dynamic pending accounting. It
// closes when every queued task, including tasks discovered by other tasks,
// has been processed.
type treeQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	tasks   []treeTask
	pending int
	closed  bool
}

func newTreeQueue() *treeQueue {
	q := &treeQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *treeQueue) push(task treeTask) {
	q.mu.Lock()
	q.tasks = append(q.tasks, task)
	q.pending++
	q.cond.Signal()
	q.mu.Unlock()
}

func (q *treeQueue) done() {
	q.mu.Lock()
	q.pending--
	if q.pending <= 0 {
		q.closed = true
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

func (q *treeQueue) pop(ctx context.Context) (treeTask, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.tasks) == 0 && !q.closed {
		if ctx.Err() != nil {
			return treeTask{}, false
		}
		q.cond.Wait()
	}
	if ctx.Err() != nil {
		return treeTask{}, false
	}
	if len(q.tasks) == 0 {
		return treeTask{}, false
	}
	task := q.tasks[0]
	q.tasks = q.tasks[1:]
	return task, true
}

type treeAggregate struct {
	allocBytes    int64
	logicalBytes  int64
	fileCount     int64
	dirCount      int64
	symlinkCount  int64
	transientErrs int64
}

func (a *treeAggregate) add(other treeAggregate) {
	a.allocBytes += other.allocBytes
	a.logicalBytes += other.logicalBytes
	a.fileCount += other.fileCount
	a.dirCount += other.dirCount
	a.symlinkCount += other.symlinkCount
	a.transientErrs += other.transientErrs
}

// scanTree walks one first-level directory with a shared worker pool. Directory
// enumeration and file lstat work are both queued, so a directory with many
// subdirectories and a flat directory with many files use the same concurrency.
func (s *Scanner) scanTree(ctx context.Context, sess *scanSession, topName, root string, out *model.DirOutcome) error {
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

	// Wake waiting workers when the scan is canceled. Cond.Wait cannot select
	// on a context directly.
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
				if err := sess.acquireSlot(ctx); err != nil {
					q.done()
					return
				}
				sess.counters.startWorker(topName)
				var taskAgg treeAggregate
				var err error
				switch {
				case task.quick != nil:
					taskAgg, err = s.processQuickFileTask(ctx, sess, q, task.quick)
				case task.files != nil:
					taskAgg, err = s.processFileTask(ctx, sess, topName, task, q)
				default:
					taskAgg, err = s.processDirTask(ctx, sess, q, task)
				}
				sess.counters.finishWorker(topName)
				sess.releaseSlot()
				if err != nil {
					fail(err)
				} else {
					mu.Lock()
					agg.add(taskAgg)
					mu.Unlock()
				}
				q.done()
				if err != nil && ctx.Err() != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		if firstErr != nil {
			return firstErr
		}
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	out.AllocBytes += agg.allocBytes
	out.LogicalBytes += agg.logicalBytes
	out.FileCount += agg.fileCount
	out.DirCount += agg.dirCount
	out.SymlinkCount += agg.symlinkCount
	out.TransientErrs += agg.transientErrs
	return nil
}

func (s *Scanner) processDirTask(ctx context.Context, sess *scanSession, q *treeQueue, task treeTask) (treeAggregate, error) {
	var agg treeAggregate
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
			return agg, nil
		}
		return agg, fmt.Errorf("打开目录失败 %s: %w", task.path, err)
	}
	defer f.Close()

	statStart := time.Now()
	dirInfo, statErr := f.Stat()
	sess.controller.ObserveMetadata(time.Since(statStart), statErr)
	if statErr != nil {
		agg.transientErrs++
	} else if meta, ok := platform.Stat(dirInfo); ok {
		agg.allocBytes += meta.Allocated
		agg.logicalBytes += meta.Logical
		sess.counters.alloc.Add(meta.Allocated)
		sess.counters.logical.Add(meta.Logical)
	} else {
		agg.transientErrs++
	}

	var fileBatch []string
	flushFiles := func() {
		if len(fileBatch) == 0 {
			return
		}
		q.push(treeTask{path: task.path, depth: task.depth, files: fileBatch})
		fileBatch = nil
	}

	for {
		if err := sess.wait(ctx); err != nil {
			return agg, err
		}
		readStart := time.Now()
		entries, readErr := f.ReadDir(sess.batchSize())
		sess.controller.ObserveMetadata(time.Since(readStart), readErr)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if task.depth > 0 && errors.Is(readErr, os.ErrNotExist) {
				agg.transientErrs++
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
			// DT_UNKNOWN is common on shared filesystems. The file worker will
			// lstat it and enqueue a directory task if needed.
			fileBatch = append(fileBatch, path)
			if len(fileBatch) >= 64 {
				flushFiles()
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	flushFiles()
	sess.rollups.addDirect(task.path, agg, false)
	return agg, nil
}

func (s *Scanner) processFileTask(ctx context.Context, sess *scanSession, topName string, task treeTask, q *treeQueue) (treeAggregate, error) {
	var agg treeAggregate
	for _, path := range task.files {
		res := s.scanFile(ctx, sess, topName, path)
		if res.err != nil {
			return agg, res.err
		}
		if res.isDir {
			agg.dirCount++
			if task.depth+1 > maxDepth {
				return agg, fmt.Errorf("目录层级超过 %d: %s", maxDepth, path)
			}
			q.push(treeTask{path: path, depth: task.depth + 1})
			continue
		}
		if res.symlink {
			agg.symlinkCount++
			continue
		}
		agg.fileCount += res.fileCount
		agg.allocBytes += res.allocBytes
		agg.logicalBytes += res.logicalBytes
		agg.transientErrs += res.transientErrs
		if res.fileCount > 0 {
			sess.counters.files.Add(res.fileCount)
		}
		if res.allocBytes != 0 {
			sess.counters.alloc.Add(res.allocBytes)
		}
		if res.logicalBytes != 0 {
			sess.counters.logical.Add(res.logicalBytes)
		}
	}
	sess.rollups.addDirect(task.path, agg, false)
	return agg, nil
}
