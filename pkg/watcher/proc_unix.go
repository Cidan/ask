//go:build !windows

package watcher

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the script in its own process group so a timeout
// or a stop kills everything it started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
