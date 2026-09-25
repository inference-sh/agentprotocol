//go:build unix

package agentproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// kill signals the group the child leads, which the kernel keeps reserved
// while it has members, including after the child itself is reaped.
func kill(p *os.Process) error {
	err := syscall.Kill(-p.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
