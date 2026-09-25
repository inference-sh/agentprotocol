// Package jsonrpc is the newline-delimited JSON transport every agent
// protocol here runs over: one line reader with one size limit, a registry of
// calls waiting for replies, a locked line writer, and a JSON-RPC 2.0
// connection built from them. acp and codexapp speak JSON-RPC through Conn;
// claudecode and pirpc have their own envelopes and use the pieces.
package jsonrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// MaxLine caps one line. Agents inline whole files and tool output into a
// single message, so the limit is generous; it exists so a runaway child
// cannot make the reader allocate without bound.
const MaxLine = 64 << 20

// ErrLineTooLong ends reading when a line exceeds MaxLine. The stream is
// failed rather than the line skipped: a skipped line may be the reply a
// caller is waiting on, and failing ends that wait instead of leaving it to
// hang.
var ErrLineTooLong = errors.New("jsonrpc: line exceeds 64 MiB")

// ReadLines calls fn with each line of r until r ends, and returns nil at a
// clean EOF. Only LF ends a line: a JSON string may hold U+2028 or U+2029,
// which are not record boundaries. The LF and a trailing CR are removed, and
// blank lines are skipped. fn must not keep the slice.
func ReadLines(r io.Reader, fn func(line []byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var long []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(long)+len(chunk) > MaxLine {
			return ErrLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if len(long) > 0 {
			long = append(long, chunk...)
			line = long
		}
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(bytes.TrimSpace(line)) > 0 {
			fn(line)
		}
		long = long[:0]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// LineWriter writes one JSON value per line, whole, from any goroutine.
type LineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewLineWriter wraps w.
func NewLineWriter(w io.Writer) *LineWriter { return &LineWriter{w: w} }

// WriteJSON encodes v and writes it with its LF in one write.
func (l *LineWriter) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.Write(append(data, '\n'))
	return err
}

// Pending holds the calls waiting for a reply, keyed by the id the reply
// will carry. The zero value is ready to use.
type Pending[K comparable, V any] struct {
	mu     sync.Mutex
	m      map[K]chan V
	closed bool
}

// Add registers id and returns the channel its reply arrives on. The channel
// closes without a value if the stream ends first. ok is false once Close
// has run.
func (p *Pending[K, V]) Add(id K) (reply <-chan V, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false
	}
	if p.m == nil {
		p.m = make(map[K]chan V)
	}
	ch := make(chan V, 1)
	p.m[id] = ch
	return ch, true
}

// Deliver hands v to the call waiting on id, reporting whether there was one.
func (p *Pending[K, V]) Deliver(id K, v V) bool {
	p.mu.Lock()
	ch, ok := p.m[id]
	delete(p.m, id)
	p.mu.Unlock()
	if ok {
		ch <- v
	}
	return ok
}

// Forget drops a call that stopped waiting.
func (p *Pending[K, V]) Forget(id K) {
	p.mu.Lock()
	delete(p.m, id)
	p.mu.Unlock()
}

// Close fails every waiting call and refuses new ones.
func (p *Pending[K, V]) Close() {
	p.mu.Lock()
	p.closed = true
	m := p.m
	p.m = nil
	p.mu.Unlock()
	for _, ch := range m {
		close(ch)
	}
}
