package cli

import (
	"reflect"
	"testing"
)

func TestDetachArgsRemovesBackgroundAndForcesForeground(t *testing.T) {
	got := detachArgs([]string{"--db", "x", "quick", "--background", "--path", "/mnt"})
	want := []string{"--db", "x", "quick", "--path", "/mnt", "--foreground"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%v want=%v", got, want)
	}
}
