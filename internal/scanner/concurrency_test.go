package scanner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScanConcurrencyRaisesLimitWhileWaiting(t *testing.T) {
	sem := newScanConcurrency(1)
	defer sem.Release()

	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var active atomic.Int64
	acquired := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sem.Acquire(context.Background()); err != nil {
				return
			}
			active.Add(1)
			acquired <- struct{}{}
			active.Add(-1)
			sem.Release()
		}()
	}
	time.Sleep(30 * time.Millisecond)
	if got := sem.Active(); got != 1 {
		t.Fatalf("active before raise = %d, want 1", got)
	}
	sem.SetLimit(4)
	deadline := time.After(2 * time.Second)
	for i := 0; i < 4; i++ {
		select {
		case <-acquired:
		case <-deadline:
			t.Fatal("waiters were not woken after raising the limit")
		}
	}
	wg.Wait()
	sem.Release()
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sem.Active(); got != 1 {
		t.Fatalf("active after release = %d, want 1", got)
	}
}

func TestScanConcurrencyHonorsContext(t *testing.T) {
	sem := newScanConcurrency(1)
	defer sem.Release()
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sem.Acquire(ctx); err == nil {
		t.Fatal("expected context cancellation while waiting for a slot")
	}
}
