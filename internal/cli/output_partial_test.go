package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
	"github.com/YEDEDD/dirspace/internal/service"
)

func TestBuildPartialJSONMarksCurrentAndLastSources(t *testing.T) {
	now := time.Now()
	view := &service.PartialView{
		Path: "/mnt", Root: model.Root{Path: "/mnt"},
		Run: &model.Run{ID: 7, Mode: model.ModeQuick, Status: model.RunRunning},
		Dirs: []service.PartialDir{
			{Dir: model.DirStat{Name: "current", AllocBytes: 7 << 20, FileCount: 2, Accuracy: model.AccuracySampled, LastSuccessAt: &now}, Source: service.PartialSourceCurrent},
			{Dir: model.DirStat{Name: "carried", AllocBytes: 3 << 20, FileCount: 1, Accuracy: model.AccuracySampled, LastSuccessAt: &now}, Source: service.PartialSourceLast},
		},
		SizeBasis: model.BasisAllocated, FreshDirs: 1, CarriedDirs: 1,
		FreshBytes: 7 << 20, CarriedBytes: 3 << 20, Mixed: true,
	}
	out := buildPartialJSON(view)
	if !out.Mixed || out.FreshDirs != 1 || out.CarriedDirs != 1 {
		t.Fatalf("merge metadata: %+v", out)
	}
	if out.MergedBytes != 10<<20 || out.FreshBytes != 7<<20 || out.CarriedBytes != 3<<20 {
		t.Fatalf("merge totals: %+v", out)
	}
	if len(out.CompletedDirs) != 2 || out.CompletedDirs[0].Source != string(service.PartialSourceCurrent) ||
		out.CompletedDirs[1].Source != string(service.PartialSourceLast) {
		t.Fatalf("sources: %+v", out.CompletedDirs)
	}
	if !strings.Contains(out.Note, "混合合计") {
		t.Fatalf("note: %q", out.Note)
	}

	var text bytes.Buffer
	printPartial(&text, view)
	for _, want := range []string{"本轮已完成", "沿用上次", "混合合计", "上次"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, text.String())
		}
	}
}
