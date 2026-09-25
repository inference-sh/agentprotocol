//go:build unix

package agentproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setGroup puts the child in a group of its own so kill reaches its
// descendants. That also takes it out of the terminal's foreground group, so
// Ctrl-C reaches only the host, which shuts sessions down through Close.
func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(cmd.SysProcAttr)
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
