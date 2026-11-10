package scanner

import (
	"fmt"
	"testing"
)

func TestQuickSampleIsHashedAndStable(t *testing.T) {
	const total = 10000
	sampled := 0
	for i := 0; i < total; i++ {
		path := fmt.Sprintf("/cfs/data/dir/file-%05d.bin", i)
		first := quickSampled(path, 42, 2)
		if first != quickSampled(path, 42, 2) {
			t.Fatalf("sample for %s is not deterministic", path)
		}
		if first {
			sampled++
		}
	}
	// p=2%, n=10000 => mean 200, sd ~14. A wide bound keeps the test stable
	// while still catching a broken threshold or an all-or-nothing sampler.
	if sampled < 140 || sampled > 260 {
		t.Fatalf("sampled=%d, want roughly 2%% of %d", sampled, total)
	}
	if !quickSampled("/cfs/data/dir/file.bin", 42, 100) {
		t.Fatal("100%% sample rejected a path")
	}
}

func TestQuickSampleSaltChangesSelection(t *testing.T) {
	changed := 0
	for i := 0; i < 1000; i++ {
		path := fmt.Sprintf("/cfs/data/dir/file-%04d.bin", i)
		if quickSampled(path, 1, 10) != quickSampled(path, 2, 10) {
			changed++
		}
	}
	if changed < 100 {
		t.Fatalf("salt changed only %d/1000 selections", changed)
	}
}
