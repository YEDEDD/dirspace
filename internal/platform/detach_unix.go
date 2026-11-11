//go:build linux || darwin

package platform

import (
	"os/exec"
	"syscall"
)

// Detach puts a child process in its own session so it survives the parent
// terminal and does not receive SIGHUP when the shell exits.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
