package agentproc

import "syscall"

// setParentDeathSignal has the kernel send SIGTERM to the child when the host
// dies without calling Close (SIGKILL, OOM), which the group no longer covers
// once the child is out of the terminal's foreground group. The signal is tied
// to the OS thread that forked, so a goroutine that exits while holding
// runtime.LockOSThread would trigger it early; nothing in this module does.
func setParentDeathSignal(a *syscall.SysProcAttr) {
	a.Pdeathsig = syscall.SIGTERM
}
