// Package mountinfo parses the local mount table. It never contacts the NFS
// server, so it is safe to call from read-only CLI queries.
package mountinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Mount describes one mounted filesystem.
type Mount struct {
	Source string
	Target string
	FSType string
}

// List returns the current mount table. On systems without /proc (for example
// macOS during development) it returns an empty list.
func List() ([]Mount, error) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var mounts []Mount
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mounts = append(mounts, Mount{
			Source: unescape(fields[0]),
			Target: unescape(fields[1]),
			FSType: unescape(fields[2]),
		})
	}
	return mounts, scanner.Err()
}

// InfoForPath returns the longest matching mount for an absolute path.
func InfoForPath(path string) (Mount, bool) {
	mounts, err := List()
	if err != nil || len(mounts) == 0 {
		return Mount{}, false
	}
	path = filepath.Clean(path)
	best := Mount{}
	found := false
	for _, m := range mounts {
		target := filepath.Clean(m.Target)
		if path == target || strings.HasPrefix(path, target+string(filepath.Separator)) {
			if !found || len(target) > len(best.Target) {
				best = m
				found = true
			}
		}
	}
	return best, found
}

// IsNFS reports whether a path resides on an NFS mount.
func IsNFS(path string) bool {
	m, ok := InfoForPath(path)
	if !ok {
		return false
	}
	fs := strings.ToLower(m.FSType)
	return strings.HasPrefix(fs, "nfs") || strings.Contains(m.Source, ":")
}

// Server returns the mount source for display, or an empty string.
func Server(path string) string {
	if m, ok := InfoForPath(path); ok {
		return m.Source
	}
	return ""
}

// FSType returns the mount filesystem type for display.
func FSType(path string) string {
	if m, ok := InfoForPath(path); ok {
		return m.FSType
	}
	return ""
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if value, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// IsMountPoint reports whether path is itself a mount target. It never
// contacts the filesystem; on systems without /proc it returns false.
func IsMountPoint(path string) bool {
	m, ok := InfoForPath(path)
	if !ok {
		return false
	}
	return filepath.Clean(m.Target) == filepath.Clean(path)
}

// IsWithinMount reports whether path is on a non-root mounted filesystem.
// The root itself does not count: when a network mount is missing, its
// subdirectories usually fall back to the root filesystem, and that must not
// be accepted as a protected scan root. A path that is itself a mount target
// or a subdirectory below one returns true.
func IsWithinMount(path string) bool {
	m, ok := InfoForPath(path)
	if !ok {
		return false
	}
	return mountCovers(m, path)
}

func mountCovers(m Mount, path string) bool {
	target := filepath.Clean(m.Target)
	cleanPath := filepath.Clean(path)
	if target == string(filepath.Separator) {
		return false
	}
	return cleanPath == target || strings.HasPrefix(cleanPath, target+string(filepath.Separator))
}
