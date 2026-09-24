package pirpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Handler receives what pi sends that is not a reply to one of our commands.
// Both callbacks run on the read loop, in order, and must not block for long.
type Handler struct {
	// OnRecord receives every session event and extension UI request, and
	// any record type this package does not know.
	OnRecord func(Record)

	// OnMalformed is told about a line that is not JSON.
	OnMalformed func(line []byte, err error)
}

// Client speaks the protocol over a pair of streams. Most callers use Spawn,
// which wires one to a child process.
type Client struct {
	r io.Reader
	h Handler

	wmu sync.Mutex
	w   io.Writer

	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan Response
	closed  bool

	done    chan struct{}
	readErr error
}

// ErrClosed is returned by commands sent after the stream ended.
var ErrClosed = errors.New("pirpc: connection closed")

// NewClient wraps a stream. Call Start to begin reading.
func NewClient(r io.Reader, w io.Writer, h Handler) *Client {
	return &Client{r: r, w: w, h: h, pending: make(map[string]chan Response), done: make(chan struct{})}
}

// Start runs the read loop until the stream ends.
func (c *Client) Start() { go c.readLoop() }

// Done closes when the read loop has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the read loop ended: nil for a clean EOF.
func (c *Client) Err() error {
	<-c.done
	return c.readErr
}

// Send writes one JSON record.
func (c *Client) Send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// NewID returns a command id unique to this client.
func (c *Client) NewID() string { return "go-" + strconv.FormatUint(c.seq.Add(1), 10) }

// Expect registers interest in the response to a command id that will be
// sent separately, and returns the channel it will arrive on. The channel
// closes without a value if the stream ends first. Forget releases it.
func (c *Client) Expect(id string) (<-chan Response, error) {
	ch := make(chan Response, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	c.pending[id] = ch
	return ch, nil
}

// Forget drops interest in a command's response.
func (c *Client) Forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// CommandError is a response with success false.
type CommandError struct {
	Command string
	Message string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("pirpc: %s failed: %s", e.Command, e.Message)
}

// Request sends a command and waits for its response. pi has no way to
// withdraw a command, so a ctx that ends first only stops the wait.
func (c *Client) Request(ctx context.Context, cmd Command) (Response, error) {
	if cmd.ID == "" {
		cmd.ID = c.NewID()
	}
	ch, err := c.Expect(cmd.ID)
	if err != nil {
		return Response{}, err
	}
	if err := c.Send(cmd); err != nil {
		c.Forget(cmd.ID)
		return Response{}, fmt.Errorf("pirpc: send %s: %w", cmd.Type, err)
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return Response{}, ErrClosed
		}
		if !res.Success {
			return res, &CommandError{Command: cmd.Type, Message: res.Error}
		}
		return res, nil
	case <-ctx.Done():
		c.Forget(cmd.ID)
		return Response{}, ctx.Err()
	}
}

// GetState asks for the session state.
func (c *Client) GetState(ctx context.Context) (State, error) {
	var st State
	res, err := c.Request(ctx, Command{Type: CmdGetState})
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(res.Data, &st); err != nil {
		return st, fmt.Errorf("pirpc: decode get_state: %w", err)
	}
	return st, nil
}

// Abort aborts the running operation. pi answers once the session is idle.
func (c *Client) Abort(ctx context.Context) error {
	_, err := c.Request(ctx, Command{Type: CmdAbort})
	return err
}

// Compact compacts the conversation now.
func (c *Client) Compact(ctx context.Context, instructions string) (CompactionResult, error) {
	var out CompactionResult
	res, err := c.Request(ctx, Command{Type: CmdCompact, CustomInstructions: instructions})
	if err != nil {
		return out, err
	}
	if len(res.Data) > 0 {
		_ = json.Unmarshal(res.Data, &out)
	}
	return out, nil
}

// Answer replies to an extension UI dialog.
func (c *Client) Answer(r UIResponse) error { return c.Send(r) }

func (c *Client) readLoop() {
	defer func() {
		c.mu.Lock()
		c.closed = true
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		close(c.done)
	}()
	br := bufio.NewReaderSize(c.r, 64<<10)
	for {
		// LF only: a JSON string may hold U+2028 or U+2029, which are not
		// record boundaries (rpc.md, Framing).
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r")))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.readErr = err
			}
			return
		}
	}
}

func (c *Client) dispatch(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		if c.h.OnMalformed != nil {
			c.h.OnMalformed(line, err)
		}
		return
	}
	raw := json.RawMessage(append([]byte(nil), line...))
	if head.Type == TypeResponse {
		var res Response
		if json.Unmarshal(raw, &res) == nil && res.ID != "" {
			c.mu.Lock()
			ch, ok := c.pending[res.ID]
			delete(c.pending, res.ID)
			c.mu.Unlock()
			if ok {
				ch <- res
				return
			}
		}
	}
	if c.h.OnRecord != nil {
		c.h.OnRecord(Record{Type: head.Type, Raw: raw})
	}
}
