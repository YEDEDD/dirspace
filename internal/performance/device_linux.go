//go:build linux

package performance

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type procDeviceSampler struct {
	name string
	kind DeviceKind

	mu       sync.Mutex
	lastAt   time.Time
	last     diskCounters
	hasLast  bool
	lastRate DeviceMetrics
}

type diskCounters struct {
	reads       float64
	sectorsRead float64
	readMs      float64
	writes      float64
	sectorsWrit float64
	writeMs     float64
	ioMs        float64
	weightedMs  float64
}

func newDeviceSampler(source string) *procDeviceSampler {
	name, kind := resolveDevice(source)
	if name == "" {
		return nil
	}
	return &procDeviceSampler{name: name, kind: kind}
}

func resolveDevice(source string) (string, DeviceKind) {
	source = strings.TrimSpace(source)
	if source == "" || !strings.HasPrefix(source, "/dev/") {
		return "", DeviceUnknown
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		resolved = source
	}
	name := filepath.Base(resolved)
	if name == "" {
		return "", DeviceUnknown
	}
	// A partition has its own /proc/diskstats row on modern kernels, but the
	// parent disk is the useful queue/latency device. Walk one level up when
	// sysfs marks this as a partition.
	if _, err := os.Stat(filepath.Join("/sys/class/block", name, "partition")); err == nil {
		if target, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", name)); err == nil {
			parent := filepath.Base(filepath.Dir(target))
			if parent != "" && parent != "." {
				name = parent
			}
		}
	}
	kind := DeviceUnknown
	if strings.HasPrefix(name, "nvme") {
		kind = DeviceNVMe
	} else if data, err := os.ReadFile(filepath.Join("/sys/class/block", name, "queue", "rotational")); err == nil {
		if strings.TrimSpace(string(data)) == "1" {
			kind = DeviceHDD
		} else {
			kind = DeviceSSD
		}
	}
	return name, kind
}

func (s *procDeviceSampler) Sample() DeviceMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	counters, ok := readDiskCounters(s.name)
	if !ok {
		return DeviceMetrics{Name: s.name, Kind: s.kind}
	}
	now := time.Now()
	if !s.hasLast {
		s.last = counters
		s.lastAt = now
		s.hasLast = true
		return DeviceMetrics{Supported: true, Name: s.name, Kind: s.kind}
	}
	elapsed := now.Sub(s.lastAt).Seconds()
	if elapsed <= 0 {
		return s.lastRate
	}
	delta := diskCounters{
		reads:       counters.reads - s.last.reads,
		sectorsRead: counters.sectorsRead - s.last.sectorsRead,
		readMs:      counters.readMs - s.last.readMs,
		writes:      counters.writes - s.last.writes,
		sectorsWrit: counters.sectorsWrit - s.last.sectorsWrit,
		writeMs:     counters.writeMs - s.last.writeMs,
		ioMs:        counters.ioMs - s.last.ioMs,
		weightedMs:  counters.weightedMs - s.last.weightedMs,
	}
	s.last = counters
	s.lastAt = now
	metrics := DeviceMetrics{
		Supported: true,
		Valid:     true,
		Name:      s.name,
		Kind:      s.kind,
		ReadIOPS:  delta.reads / elapsed,
		WriteIOPS: delta.writes / elapsed,
	}
	if delta.reads > 0 {
		metrics.ReadAwait = delta.readMs / delta.reads
	}
	if delta.writes > 0 {
		metrics.WriteAwait = delta.writeMs / delta.writes
	}
	metrics.Queue = delta.weightedMs / (elapsed * 1000)
	metrics.Util = delta.ioMs / (elapsed * 1000) * 100
	if metrics.Util > 100 {
		metrics.Util = 100
	}
	s.lastRate = metrics
	return metrics
}

func readDiskCounters(name string) (diskCounters, bool) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return diskCounters{}, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 || fields[2] != name {
			continue
		}
		values := make([]float64, 11)
		for i := 0; i < 11; i++ {
			value, err := strconv.ParseFloat(fields[3+i], 64)
			if err != nil {
				return diskCounters{}, false
			}
			values[i] = value
		}
		return diskCounters{
			reads:       values[0],
			sectorsRead: values[2],
			readMs:      values[3],
			writes:      values[4],
			sectorsWrit: values[6],
			writeMs:     values[7],
			ioMs:        values[9],
			weightedMs:  values[10],
		}, true
	}
	return diskCounters{}, false
}
