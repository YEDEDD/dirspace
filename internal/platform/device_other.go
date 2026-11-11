//go:build !linux && !darwin

package platform

// DeviceID returns 0 on platforms without a Unix device ID.
func DeviceID(string) uint64 { return 0 }
