//go:build unix && !linux

package agentproc

import "syscall"

// Only Linux has a parent-death signal.
func setParentDeathSignal(*syscall.SysProcAttr) {}
