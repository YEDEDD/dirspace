//go:build linux || darwin

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/YEDEDD/dirspace/internal/platform"
)

// ErrRootLocked means another process is already scanning this filesystem.
// The lock is filesystem-wide, not database-local, so two binaries using
// different --db paths still cannot scan overlapping roots concurrently.
var ErrRootLocked = errors.New("该文件系统已有扫描任务在运行")

type rootLock struct {
	file *os.File
	path string
}

func acquireScanLock(scanRoot string) (*rootLock, error) {
	lockDir, err := scanLockDirectory()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建锁目录失败: %w", err)
	}
	key := scanLockKey(scanRoot)
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:12]) + ".lock"
	path := filepath.Join(lockDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开扫描锁失败: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrRootLocked
		}
		return nil, fmt.Errorf("锁定扫描任务失败: %w", err)
	}
	return &rootLock{file: f, path: path}, nil
}

func scanLockKey(scanRoot string) string {
	if dev := platform.DeviceID(scanRoot); dev != 0 {
		return fmt.Sprintf("fs-%d", dev)
	}
	return "root-" + scanRoot
}

func scanLockDirectory() (string, error) {
	if dir := os.Getenv("DIRSPACE_LOCK_DIR"); dir != "" {
		return dir, nil
	}
	for _, dir := range []string{"/run/dirspace/locks", filepath.Join(os.TempDir(), "dirspace-locks")} {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return dir, nil
		}
	}
	return "", errors.New("无法创建 dirspace 锁目录")
}

func (l *rootLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
