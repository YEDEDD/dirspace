//go:build !linux

package performance

type hostSampler struct{}

type HostMetrics struct {
	Load1      float64
	CPUPercent float64
	Supported  bool
}

func (s *hostSampler) Sample() HostMetrics { return HostMetrics{Supported: false} }
