package scanner

import (
	"context"

	"github.com/YEDEDD/dirspace/internal/storage"
)

// hardlinkRegistry records (device, inode) pairs across all first-level
// directories of one run. It is backed by SQLite so resume and cancellation do
// not lose correctness, and it only performs work for files with nlink > 1.
type hardlinkRegistry struct {
	store *storage.Store
	runID int64
}

func (h *hardlinkRegistry) Claim(ctx context.Context, dev, ino uint64, topDir string) (bool, error) {
	return h.store.ClaimHardlink(ctx, h.runID, dev, ino, topDir)
}
