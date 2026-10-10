//go:build unix

package agentproc

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// drainNow copies what r holds right now to w, without waiting for more. It
// reads the descriptor directly through Control, which neither waits for
// data nor checks the deadline a flush may have set meanwhile; os.File.Read
// does both. It reports false once the pipe has ended or was closed.
func drainNow(r *os.File, buf []byte, w io.Writer) bool {
	rc, err := r.SyscallConn()
	if err != nil {
		return true
	}
	open := true
	if err := rc.Control(func(fd uintptr) {
		for {
			n, rerr := syscall.Read(int(fd), buf)
			switch {
			case n > 0:
				_, _ = w.Write(buf[:n])
			case errors.Is(rerr, syscall.EINTR):
			case n == 0 && rerr == nil:
				open = false // EOF
				return
			default:
				return // EAGAIN: empty for now
			}
		}
	}); err != nil {
		return false // closed
	}
	return open
}
