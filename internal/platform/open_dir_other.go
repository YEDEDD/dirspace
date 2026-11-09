//go:build !linux && !darwin

package platform

import "os"

// OpenDir is a portable fallback. The caller still classifies entries with
// Lstat; on these platforms there is no O_NOFOLLOW equivalent exposed here.
func OpenDir(path string) (*os.File, error) {
	return os.Open(path)
}
