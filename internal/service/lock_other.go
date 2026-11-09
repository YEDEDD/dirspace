//go:build !linux && !darwin

package service

import (
	"errors"
	"os"
)

var ErrRootLocked = errors.New("该扫描根目录已有扫描任务在运行")

type rootLock struct{ file *os.File }

func acquireRootLock(dbDir, scanRoot string) (*rootLock, error) {
	return nil, errors.New("当前平台不支持扫描任务锁")
}

func (l *rootLock) Close() error { return nil }
