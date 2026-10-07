//go:build unix

package auth

import (
	"os/exec"
	"syscall"
)

// configureProcess runs the command in its own process group and, on
// cancellation, kills the whole group: a shell's children (the tool it
// started) would otherwise keep running and keep our output pipe open.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
