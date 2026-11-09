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
)

// ErrRootLocked means another process is already scanning the same root.
var ErrRootLocked = errors.New("该扫描根目录已有扫描任务在运行")

type rootLock struct {
	file *os.File
	path string
}

func acquireRootLock(dbDir, scanRoot string) (*rootLock, error) {
	lockDir := filepath.Join(dbDir, "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建锁目录失败: %w", err)
	}
	sum := sha256.Sum256([]byte(scanRoot))
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

func (l *rootLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
