//go:build linux || darwin

package platform

import (
	"os"
	"syscall"
)

// DeviceID returns the filesystem device ID for path, or 0 when unavailable.
func DeviceID(path string) uint64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Dev)
}
