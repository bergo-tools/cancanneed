//go:build darwin || linux

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup makes context cancellation terminate the agent and all
// commands it spawned, rather than leaving tests or submit helpers behind.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
