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
