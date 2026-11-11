//go:build !linux && !darwin

package platform

import "os/exec"

// Detach is a no-op on platforms without session detachment.
func Detach(cmd *exec.Cmd) {}
