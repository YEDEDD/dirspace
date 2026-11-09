package scanner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/YEDEDD/dirspace/internal/storage"
)

// inventoryWriter persists the optional per-file manifest in bounded batches.
// It is deliberately off by default: writing millions of local SQLite rows costs
// CPU and disk and does not reduce NFS metadata requests.
type inventoryWriter struct {
	store  *storage.Store
	runID  int64
	rootID int64
	batch  int
	cancel context.CancelFunc
	log    *slog.Logger

	ch   chan storage.InventoryRecord
	done chan struct{}

	mu     sync.Mutex
	err    error
	closed sync.Once
}

func newInventoryWriter(ctx context.Context, cancel context.CancelFunc, store *storage.Store,
	runID, rootID int64, batch int, log *slog.Logger) *inventoryWriter {
	if batch < 100 {
		batch = 100
	}
	if batch > 2000 {
		batch = 2000
	}
	w := &inventoryWriter{
		store:  store,
		runID:  runID,
		rootID: rootID,
		batch:  batch,
		cancel: cancel,
		log:    log,
		ch:     make(chan storage.InventoryRecord, batch*2),
		done:   make(chan struct{}),
	}
	go w.loop(ctx)
	return w
}

func (w *inventoryWriter) loop(ctx context.Context) {
	defer close(w.done)
	buffer := make([]storage.InventoryRecord, 0, w.batch)
	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := w.store.AddInventoryBatch(writeCtx, buffer); err != nil {
			return err
		}
		buffer = buffer[:0]
		return nil
	}
	fail := func(err error) {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
		w.cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case record, ok := <-w.ch:
			if !ok {
				if err := flush(); err != nil {
					fail(err)
				}
				return
			}
			buffer = append(buffer, record)
			if len(buffer) >= w.batch {
				if err := flush(); err != nil {
					fail(err)
					return
				}
			}
		}
	}
}

func (w *inventoryWriter) Add(ctx context.Context, record storage.InventoryRecord) error {
	w.mu.Lock()
	err := w.err
	w.mu.Unlock()
	if err != nil {
		return fatal(err)
	}
	record.RootID = w.rootID
	record.RunID = w.runID
	select {
	case w.ch <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.err != nil {
			return fatal(w.err)
		}
		return errors.New("文件清单写入器已关闭")
	}
}

func (w *inventoryWriter) Close(ctx context.Context) error {
	w.closed.Do(func() { close(w.ch) })
	select {
	case <-w.done:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(35 * time.Second):
		return fatal(context.DeadlineExceeded)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return fatal(w.err)
	}
	return nil
}
