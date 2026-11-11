//go:build !linux && !darwin

package service

import "os"

var ErrRootLocked = os.ErrExist

type rootLock struct{ file *os.File }

func acquireScanLock(string) (*rootLock, error) {
	return &rootLock{}, nil
}

func (l *rootLock) Close() error { return nil }
