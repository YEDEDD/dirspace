//go:build linux

package platform

import (
	"os"
	"syscall"
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
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileMeta{Logical: info.Size(), MtimeNS: info.ModTime().UnixNano(), Mode: uint32(info.Mode())}, false
	}
	return FileMeta{
		Allocated: st.Blocks * 512,
		Logical:   st.Size,
		Inode:     st.Ino,
		Dev:       uint64(st.Dev),
		Nlink:     uint64(st.Nlink),
		MtimeNS:   st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		Mode:      uint32(st.Mode),
	}, true
}

// ProcessCPUTime returns cumulative user+system CPU time for this process.
func ProcessCPUTime() (time.Duration, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, err
	}
	return time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond +
		time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond, nil
}

// SetLowPriority lowers CPU scheduling priority. It is best-effort.
func SetLowPriority() error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
}
