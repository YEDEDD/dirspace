//go:build linux

package performance

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type hostSampler struct {
	prevTotal uint64
	prevIdle  uint64
	ready     bool
}

type HostMetrics struct {
	Load1      float64
	CPUPercent float64
	Supported  bool
}

func (s *hostSampler) Sample() HostMetrics {
	metrics := HostMetrics{}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 0 {
			if value, err := strconv.ParseFloat(fields[0], 64); err == nil {
				metrics.Load1 = value
				metrics.Supported = true
			}
		}
	}
	f, err := os.Open("/proc/stat")
	if err != nil {
		return metrics
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return metrics
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return metrics
	}
	var total, idle uint64
	for i, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			continue
		}
		total += value
		if i == 3 || i == 4 { // idle + iowait
			idle += value
		}
	}
	if s.ready && total > s.prevTotal && idle >= s.prevIdle {
		deltaTotal := total - s.prevTotal
		deltaIdle := idle - s.prevIdle
		if deltaTotal > 0 {
			metrics.CPUPercent = float64(deltaTotal-deltaIdle) / float64(deltaTotal) * 100
			metrics.Supported = true
		}
	}
	s.prevTotal = total
	s.prevIdle = idle
	s.ready = true
	return metrics
}
