//go:build !linux && !darwin

package platform

import (
	"os"
	"time"
)

// FileMeta is the metadata needed for usage accounting.
type FileMeta struct {
	Allocated int64
	Logical   int64
	Inode     uint64
	Dev       uint64
	Nlink     uint64
	MtimeNS   int64
	Mode      uint32
}

func Stat(info os.FileInfo) (FileMeta, bool) {
	return FileMeta{
		Logical: info.Size(),
		MtimeNS: info.ModTime().UnixNano(),
		Mode:    uint32(info.Mode()),
	}, false
}

func ProcessCPUTime() (time.Duration, error) { return 0, nil }

func SetLowPriority() error { return nil }
