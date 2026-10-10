package agentproc

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// stderrCopier copies the child's stderr from a pipe this package owns to
// the caller's writer, and can flush: report once everything the child wrote
// to stderr before the call has reached the writer.
//
// stdout and stderr are two pipes read by two goroutines, so an agent that
// writes an error to stderr and then answers on stdout can have its answer
// read first. A caller that quotes stderr when an answer arrives flushes
// before it reads what it collected.
//
// Flush sets the read end's deadline to now, which ends the copier's read.
// The copier then clears the deadline, reads whatever the pipe holds without
// waiting for more (drainNow), and releases the flushes that were waiting
// when that drain began: everything written before them was in the pipe by
// then. The deadline cannot do the draining itself: a read that starts after
// it fails at once, data waiting or not.
type stderrCopier struct {
	r    *os.File
	w    io.Writer
	done chan struct{}

	mu      sync.Mutex
	waiting []chan struct{}
}

func newStderrCopier(r *os.File, w io.Writer) *stderrCopier {
	c := &stderrCopier{r: r, w: w, done: make(chan struct{})}
	go c.run()
	return c
}

func (c *stderrCopier) run() {
	defer func() {
		// done closes under the lock flush registers under, so a flush
		// either sees it or is in waiting and is released here.
		c.mu.Lock()
		for _, ch := range c.waiting {
			close(ch)
		}
		c.waiting = nil
		close(c.done)
		c.mu.Unlock()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := c.r.Read(buf)
		if n > 0 {
			_, _ = c.w.Write(buf[:n])
		}
		switch {
		case err == nil:
		case errors.Is(err, os.ErrDeadlineExceeded):
			_ = c.r.SetReadDeadline(time.Time{})
			c.mu.Lock()
			armed := len(c.waiting)
			c.mu.Unlock()
			if !drainNow(c.r, buf, c.w) {
				return
			}
			c.release(armed)
		default:
			return // EOF, or the read end was closed
		}
	}
}

// release closes the first armed flushes. One that arrived during the drain
// is not covered by it, so the deadline is set again to start another.
func (c *stderrCopier) release(armed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.waiting[:armed] {
		close(ch)
	}
	c.waiting = c.waiting[armed:]
	if len(c.waiting) > 0 {
		_ = c.r.SetReadDeadline(time.Now())
	}
}

// flush returns once everything written to stderr before the call has been
// copied, or the copier has stopped. Where the pipe has no deadlines it
// returns at once.
func (c *stderrCopier) flush() {
	ch := make(chan struct{})
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return
	default:
	}
	c.waiting = append(c.waiting, ch)
	err := c.r.SetReadDeadline(time.Now())
	if err != nil {
		c.waiting = c.waiting[:len(c.waiting)-1]
	}
	c.mu.Unlock()
	if err != nil {
		return
	}
	<-ch
}
