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
				if err := sess.acquireSlot(ctx); err != nil {
					q.done()
					return
				}
				sess.counters.startWorker()
				var taskAgg treeAggregate
				var err error
				if task.quick != nil {
					taskAgg, err = s.processQuickFileTask(ctx, sess, q, task.quick)
				} else {
					taskAgg, err = s.processQuickDirTask(ctx, sess, q, task, budget, func(a treeAggregate) {
						mu.Lock()
						agg.add(a)
						mu.Unlock()
					})
				}
				sess.counters.finishWorker()
				sess.releaseSlot()
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
type quickFileItem struct {
	path string
	prob float64
}

type quickFileTask struct {
	state *quickDirState
	items []quickFileItem
}

// quickDirState owns the per-directory counters and sampled byte totals. File
// batches run in parallel, but the directory is only finalized after every
// batch has returned. Until then the tree queue keeps the directory task open.
type quickDirState struct {
	path  string
	depth int

	mu        sync.Mutex
	alloc     int64
	logical   int64
	sealed    bool
	pending   int
	finalized bool
	firstFile string
	firstSet  atomic.Bool

	fileCount     atomic.Int64
	dirCount      atomic.Int64
	symlinkCount  atomic.Int64
	transientErrs atomic.Int64
	fileSeen      atomic.Int64

	sampledFiles    int
	allocWeighted   float64
	logicalWeighted float64
	truncated       bool

	add func(treeAggregate)
}

func (st *quickDirState) countFile() int64 {
	st.fileCount.Add(1)
	return st.fileSeen.Add(1)
}

func (st *quickDirState) setFirstFile(path string) {
	if !st.firstSet.CompareAndSwap(false, true) {
		return
	}
	st.mu.Lock()
	st.firstFile = path
	st.mu.Unlock()
}

func (st *quickDirState) addDir() {
	st.dirCount.Add(1)
}

func (st *quickDirState) addSymlink() {
	st.symlinkCount.Add(1)
}

func (st *quickDirState) addTransient() {
	st.transientErrs.Add(1)
}

func (st *quickDirState) addDirBlock(meta platform.FileMeta) {
	st.mu.Lock()
	st.alloc += meta.Allocated
	st.logical += meta.Logical
	st.mu.Unlock()
}

func (st *quickDirState) demoteToDir() {
	st.fileCount.Add(-1)
	st.dirCount.Add(1)
	st.fileSeen.Add(-1)
}

func (st *quickDirState) demoteToSymlink() {
	st.fileCount.Add(-1)
	st.symlinkCount.Add(1)
	st.fileSeen.Add(-1)
}

func (st *quickDirState) demoteToOther() {
	st.fileCount.Add(-1)
	st.fileSeen.Add(-1)
}

func (st *quickDirState) markTruncated() {
	st.mu.Lock()
	st.truncated = true
	st.mu.Unlock()
}

func (st *quickDirState) recordSample(meta platform.FileMeta, prob float64) {
	st.mu.Lock()
	st.sampledFiles++
	if prob > 0 {
		st.allocWeighted += float64(meta.Allocated) / prob
		st.logicalWeighted += float64(meta.Logical) / prob
	}
	st.mu.Unlock()
}

func (st *quickDirState) queueBatch(q *treeQueue, items []quickFileItem) {
	if len(items) == 0 {
		return
	}
	batch := append([]quickFileItem(nil), items...)
	st.mu.Lock()
	st.pending++
	st.mu.Unlock()
	q.push(treeTask{path: st.path, depth: st.depth, quick: &quickFileTask{state: st, items: batch}})
}

func (st *quickDirState) seal(ctx context.Context, sess *scanSession) {
	st.mu.Lock()
	st.sealed = true
	st.mu.Unlock()
	st.finalize(ctx, sess)
}

func (st *quickDirState) done(ctx context.Context, sess *scanSession) {
	st.mu.Lock()
	if st.pending > 0 {
		st.pending--
	}
	st.mu.Unlock()
	st.finalize(ctx, sess)
}

func (st *quickDirState) finalize(ctx context.Context, sess *scanSession) {
	st.mu.Lock()
	if !st.sealed || st.pending != 0 || st.finalized {
		st.mu.Unlock()
		return
	}
	st.finalized = true
	agg := treeAggregate{
		allocBytes:    st.alloc,
		logicalBytes:  st.logical,
		fileCount:     st.fileCount.Load(),
		dirCount:      st.dirCount.Load(),
		symlinkCount:  st.symlinkCount.Load(),
		transientErrs: st.transientErrs.Load(),
	}
	sampled := st.sampledFiles
	allocWeighted := st.allocWeighted
	logicalWeighted := st.logicalWeighted
	fileSeen := st.fileSeen.Load()
	firstFile := st.firstFile
	truncated := st.truncated
	add := st.add
	st.mu.Unlock()

	if sampled == 0 && fileSeen > 0 && firstFile != "" && ctx.Err() == nil {
		if err := sess.wait(ctx); err == nil {
			infoStart := time.Now()
			fi, infoErr := os.Lstat(firstFile)
			sess.controller.ObserveMetadata(time.Since(infoStart), infoErr)
			if infoErr == nil && fi.Mode().IsRegular() {
				meta, exact := platform.Stat(fi)
				if !exact {
					meta.Nlink = 1
					agg.transientErrs++
				}
				agg.allocBytes += meta.Allocated * fileSeen
				agg.logicalBytes += meta.Logical * fileSeen
			}
		}
	} else {
		agg.allocBytes += int64(allocWeighted + 0.5)
		agg.logicalBytes += int64(logicalWeighted + 0.5)
	}
	sess.counters.alloc.Add(agg.allocBytes)
	sess.counters.logical.Add(agg.logicalBytes)
	sess.rollups.addDirect(st.path, agg, truncated)
	if add != nil {
		add(agg)
	}
}

// processQuickFileTask handles one parallel batch of sampled files. Unknown
// entry types are classified here; if one turns out to be a directory it is
// put back on the shared queue.
func (s *Scanner) processQuickFileTask(ctx context.Context, sess *scanSession, q *treeQueue, task *quickFileTask) (treeAggregate, error) {
	defer task.state.done(ctx, sess)
	for _, item := range task.items {
		if err := sess.wait(ctx); err != nil {
			return treeAggregate{}, err
		}
		infoStart := time.Now()
		info, err := os.Lstat(item.path)
		sess.controller.ObserveMetadata(time.Since(infoStart), err)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				task.state.addTransient()
				continue
			}
			return treeAggregate{}, fmt.Errorf("读取文件元数据失败 %s: %w", item.path, err)
		}
		mode := info.Mode()
		if mode.IsDir() {
			task.state.demoteToDir()
			sess.counters.files.Add(-1)
			if task.state.depth+1 > maxDepth {
				return treeAggregate{}, fmt.Errorf("目录层级超过 %d: %s", maxDepth, item.path)
			}
			q.push(treeTask{path: item.path, depth: task.state.depth + 1})
			continue
		}
		if mode&os.ModeSymlink != 0 {
			task.state.demoteToSymlink()
			sess.counters.files.Add(-1)
			continue
		}
		if !mode.IsRegular() {
			task.state.demoteToOther()
			sess.counters.files.Add(-1)
			continue
		}
		meta, exact := platform.Stat(info)
		if !exact {
			meta.Nlink = 1
			task.state.addTransient()
		}
		task.state.recordSample(meta, item.prob)
	}
	return treeAggregate{}, nil
}

// processQuickDirTask enumerates one directory, queues sampled files as small
// parallel batches, and finalizes the directory once all batches finish. The
// directory itself is still one queue task, so file batching does not change
// which paths are visited.
func (s *Scanner) processQuickDirTask(ctx context.Context, sess *scanSession, q *treeQueue, task treeTask, budget *quickBudget, add func(treeAggregate)) (treeAggregate, error) {
	state := &quickDirState{path: task.path, depth: task.depth, add: add}
	if budget.exceeded() {
		return treeAggregate{}, errQuickBudget
	}
	if task.depth > maxDepth {
		return treeAggregate{}, fmt.Errorf("目录层级超过 %d: %s", maxDepth, task.path)
	}
	if err := sess.wait(ctx); err != nil {
		return treeAggregate{}, err
	}
	openStart := time.Now()
	f, err := platform.OpenDir(task.path)
	sess.controller.ObserveMetadata(time.Since(openStart), err)
	if err != nil {
		if task.depth > 0 && errors.Is(err, os.ErrNotExist) {
			state.addTransient()
			state.seal(ctx, sess)
			return treeAggregate{}, nil
		}
		return treeAggregate{}, fmt.Errorf("打开目录失败 %s: %w", task.path, err)
	}
	defer f.Close()

	if err := sess.wait(ctx); err != nil {
		return treeAggregate{}, err
	}
	statStart := time.Now()
	info, statErr := f.Stat()
	sess.controller.ObserveMetadata(time.Since(statStart), statErr)
	if statErr != nil {
		state.addTransient()
	} else if meta, ok := platform.Stat(info); ok {
		state.addDirBlock(meta)
	} else {
		state.addTransient()
	}

	percent := sess.quickSamplePercent
	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}
	sampleProb := float64(percent) / 100
	var fileSeen int64
	batching := sess.quickMaxFiles <= 0
	var batch []quickFileItem
	budgetHit := false

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		state.queueBatch(q, batch)
		batch = nil
	}

readLoop:
	for {
		if err := sess.wait(ctx); err != nil {
			return treeAggregate{}, err
		}
		readStart := time.Now()
		entries, readErr := f.ReadDir(sess.batchSize())
		sess.controller.ObserveMetadata(time.Since(readStart), readErr)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if task.depth > 0 && errors.Is(readErr, os.ErrNotExist) {
				state.addTransient()
				state.seal(ctx, sess)
				return treeAggregate{}, nil
			}
			return treeAggregate{}, fmt.Errorf("读取目录失败 %s: %w", task.path, readErr)
		}
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(task.path, name)
			typ := entry.Type()
			if typ&os.ModeSymlink != 0 {
				state.addSymlink()
				continue
			}
			if typ.IsDir() {
				state.addDir()
				if task.depth+1 > maxDepth {
					return treeAggregate{}, fmt.Errorf("目录层级超过 %d: %s", maxDepth, path)
				}
				q.push(treeTask{path: path, depth: task.depth + 1})
				continue
			}
			if typ != 0 && !typ.IsRegular() {
				continue
			}

			fileSeen = state.countFile()
			sess.counters.files.Add(1)
			state.setFirstFile(path)
			if budget.add(1) {
				state.markTruncated()
				budgetHit = true
				break readLoop
			}

			census := fileSeen <= int64(sess.censusFiles())
			sample := census || quickSampled(path, sess.quickSalt, percent)
			if !sample {
				continue
			}
			prob := sampleProb
			if census {
				prob = 1
			}
			item := quickFileItem{path: path, prob: prob}
			if batching {
				batch = append(batch, item)
				if len(batch) >= 64 {
					flushBatch()
				}
			} else if err := s.processQuickFileItem(ctx, sess, q, state, item); err != nil {
				return treeAggregate{}, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	flushBatch()
	state.seal(ctx, sess)
	if budgetHit {
		return treeAggregate{}, errQuickBudget
	}
	return treeAggregate{}, nil
}

func (s *Scanner) processQuickFileItem(ctx context.Context, sess *scanSession, q *treeQueue, state *quickDirState, item quickFileItem) error {
	if err := sess.wait(ctx); err != nil {
		return err
	}
	infoStart := time.Now()
	info, err := os.Lstat(item.path)
	sess.controller.ObserveMetadata(time.Since(infoStart), err)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			state.addTransient()
			return nil
		}
		return fmt.Errorf("读取文件元数据失败 %s: %w", item.path, err)
	}
	mode := info.Mode()
	if mode.IsDir() {
		state.demoteToDir()
		sess.counters.files.Add(-1)
		if state.depth+1 > maxDepth {
			return fmt.Errorf("目录层级超过 %d: %s", maxDepth, item.path)
		}
		q.push(treeTask{path: item.path, depth: state.depth + 1})
		return nil
	}
	if mode&os.ModeSymlink != 0 {
		state.demoteToSymlink()
		sess.counters.files.Add(-1)
		return nil
	}
	if !mode.IsRegular() {
		state.demoteToOther()
		sess.counters.files.Add(-1)
		return nil
	}
	meta, exact := platform.Stat(info)
	if !exact {
		meta.Nlink = 1
		state.addTransient()
	}
	state.recordSample(meta, item.prob)
	return nil
}
