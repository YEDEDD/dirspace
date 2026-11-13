package mountinfo

import "testing"

func TestMountCoversSubdirectory(t *testing.T) {
	m := Mount{Target: "/data", Source: "server:/export", FSType: "nfs"}
	cases := []struct {
		path string
		want bool
	}{
		{"/data", true},
		{"/data/subdir", true},
		{"/database", false},
		{"/", false},
	}
	for _, tc := range cases {
		if got := mountCovers(m, tc.path); got != tc.want {
			t.Fatalf("mountCovers(%q)=%v want %v", tc.path, got, tc.want)
		}
	}
}

func TestMountCoversRejectsRootFilesystem(t *testing.T) {
	if mountCovers(Mount{Target: "/"}, "/etc/example") {
		t.Fatal("root filesystem must not count as a protected mount")
	}
}

func TestFSTypeClass(t *testing.T) {
	cases := map[string]string{
		"xfs":          "local",
		"ext4":         "local",
		"zfs":          "local",
		"nfs":          "network",
		"nfs4":         "network",
		"cifs":         "network",
		"ceph":         "network",
		"lustre":       "network",
		"fuse":         "fuse",
		"fuse.juicefs": "fuse",
		"":             "unknown",
	}
	for fs, want := range cases {
		if got := FSTypeClass(fs); got != want {
			t.Fatalf("FSTypeClass(%q)=%q want %q", fs, got, want)
		}
	}
}

func TestTargetCoversRootFilesystem(t *testing.T) {
	if !targetCovers("/", "/data") {
		t.Fatal("root filesystem must cover /data")
	}
	if !targetCovers("/", "/") {
		t.Fatal("root filesystem must cover itself")
	}
	if targetCovers("/data", "/database") {
		t.Fatal("prefix must respect path boundary")
	}
	if !targetCovers("/data", "/data/sub") {
		t.Fatal("subdirectory should match")
	}
}
