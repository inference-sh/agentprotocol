//go:build !unix

package agentproc

import (
	"io"
	"os"
)

// drainNow is never reached here: pipes on this platform take no deadline,
// so a flush returns at once and the copier is never interrupted.
func drainNow(*os.File, []byte, io.Writer) bool { return true }
