package driver

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// Every backend has a Stderr io.Writer with the same meaning: it receives
// the agent's stderr as it is written, and nil keeps only a tail. The tail,
// the last stderrTailBytes, is kept either way and quoted in the error when
// Open fails and in the process_exited error when the agent dies: it is
// usually the only clue to why.
const stderrTailBytes = 4 << 10

// agentStderr is where an agent's stderr goes: the caller's writer, if any,
// and the tail.
func agentStderr(w io.Writer) (io.Writer, *tailBuffer) {
	tail := &tailBuffer{max: stderrTailBytes}
	if w == nil {
		return tail, tail
	}
	return io.MultiWriter(w, tail), tail
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// quote appends the tail to msg, when there is one.
func (t *tailBuffer) quote(msg string) string {
	if t == nil {
		return msg
	}
	if s := strings.TrimSpace(t.String()); s != "" {
		return msg + "; agent stderr: " + s
	}
	return msg
}

// quoteErr is err with the tail appended, still wrapping err.
func (t *tailBuffer) quoteErr(err error) error {
	if t == nil || err == nil {
		return err
	}
	if s := strings.TrimSpace(t.String()); s != "" {
		return fmt.Errorf("%w; agent stderr: %s", err, s)
	}
	return err
}
