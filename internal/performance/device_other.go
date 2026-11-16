//go:build !linux

package performance

type procDeviceSampler struct{}

func newDeviceSampler(string) *procDeviceSampler { return nil }

func (s *procDeviceSampler) Sample() DeviceMetrics { return DeviceMetrics{} }
