//go:build unix

package proc

import (
	"errors"
	"syscall"
)

// Alive reports whether a process with this pid exists. Signal 0 checks
// existence without delivering anything; EPERM means it exists under
// another user.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
