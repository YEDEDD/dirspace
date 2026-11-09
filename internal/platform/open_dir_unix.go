//go:build linux || darwin

package platform

import (
	"os"
	"syscall"
)

// OpenDir opens a directory without following a symlink at the final path
// component. This closes the readdir-to-open race where a directory entry can
// be replaced by a symlink between classification and traversal.
func OpenDir(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
