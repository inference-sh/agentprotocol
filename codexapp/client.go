// Package codexapp is a client for `codex app-server`, the JSON-RPC server
// OpenAI ships inside the codex CLI and drives its IDE extension with.
//
// The wire is newline-delimited JSON over the child's stdio. Messages follow
// JSON-RPC 2.0 without the "jsonrpc" member, which codex neither sends nor
// requires. Both sides make requests: the client starts threads and turns, and
// the server asks the client to approve commands and file changes.
//
// The protocol types in protocol_gen.go are generated from the schema the
// codex binary publishes about itself; see generate.go. Unknown fields and
// unknown notifications are tolerated, because codex sends experimental
// fields to every client and adds notifications between releases.
package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Message is one JSON-RPC message in either direction. Which fields are set
// says what it is: Method and ID a request, Method alone a notification, ID
// with Result or Error a response.
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("codex app-server: %s (code %d)", e.Message, e.Code)
}

// JSON-RPC error codes this client sends.
const (
	CodeMethodNotFound = -32601
	CodeInternal       = -32603
)

// ErrClosed is returned by calls made after the connection ended, and by calls
// still waiting when it did.
var ErrClosed = errors.New("codex app-server: connection closed")

// Handler receives what the server sends unprompted.
type Handler struct {
	// OnNotification is called on the read loop, in arrival order. It must
	// not block: the next message is not read until it returns.
	OnNotification func(method string, params json.RawMessage)

	// OnRequest answers a request from the server, such as an approval. It
	// runs on its own goroutine and may block for as long as a human takes;
	// ctx ends when the connection does. The result is sent back as is; a
	// non-nil error is sent as a JSON-RPC error instead.
	//
	// Nil answers every request with method-not-found.
	OnRequest func(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) (any, *Error)
}

// Client is one JSON-RPC connection to an app-server.
type Client struct {
	r *bufio.Reader
	w io.Writer
	h Handler

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan Message

	done    chan struct{}
	doneErr error
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewClient wraps a stream. Call Start to begin reading.
func NewClient(r io.Reader, w io.Writer, h Handler) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		r:       bufio.NewReaderSize(r, 1<<16),
		w:       w,
		h:       h,
		pending: map[string]chan Message{},
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Start runs the read loop until the stream ends.
func (c *Client) Start() { go c.readLoop() }

// Done closes when the stream has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the stream ended, once Done is closed. A clean EOF is nil.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.doneErr
	default:
		return nil
	}
}

func (c *Client) readLoop() {
	var err error
	defer func() {
		c.mu.Lock()
		c.doneErr = err
		pending := c.pending
		c.pending = map[string]chan Message{}
		c.mu.Unlock()
		for _, ch := range pending {
			close(ch)
		}
		c.cancel()
		close(c.done)
	}()
	for {
		line, rerr := c.r.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				err = rerr
			}
			return
		}
	}
}

func (c *Client) dispatch(line []byte) {
	var m Message
	if json.Unmarshal(line, &m) != nil {
		// Not JSON-RPC. codex logs to stderr, so this is unexpected, but a
		// stray line must not end the session.
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		go c.answer(m)
	case m.Method != "":
		if c.h.OnNotification != nil {
			c.h.OnNotification(m.Method, m.Params)
		}
	case len(m.ID) > 0:
		key := string(m.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ok {
			ch <- m
		}
	}
}

func (c *Client) answer(m Message) {
	if c.h.OnRequest == nil {
		_ = c.write(Message{ID: m.ID, Error: &Error{Code: CodeMethodNotFound, Message: "method not supported by this client: " + m.Method}})
		return
	}
	result, rerr := c.h.OnRequest(c.ctx, m.ID, m.Method, m.Params)
	if rerr != nil {
		_ = c.write(Message{ID: m.ID, Error: rerr})
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		_ = c.write(Message{ID: m.ID, Error: &Error{Code: CodeInternal, Message: err.Error()}})
		return
	}
	_ = c.write(Message{ID: m.ID, Result: raw})
}

func (c *Client) write(m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// Call sends a request and decodes the result into out, which may be nil.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("codex app-server: encode %s params: %w", method, err)
	}
	ch := make(chan Message, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	c.pending[string(id)] = ch
	c.mu.Unlock()

	if err := c.write(Message{ID: id, Method: method, Params: raw}); err != nil {
		c.forget(id)
		return fmt.Errorf("codex app-server: send %s: %w", method, err)
	}

	select {
	case m, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: %w", method, ErrClosed)
		}
		if m.Error != nil {
			return m.Error
		}
		if out == nil || len(m.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(m.Result, out); err != nil {
			return fmt.Errorf("codex app-server: decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	}
}

func (c *Client) forget(id json.RawMessage) {
	c.mu.Lock()
	delete(c.pending, string(id))
	c.mu.Unlock()
}

// Notify sends a notification. params may be nil.
func (c *Client) Notify(method string, params any) error {
	m := Message{Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = raw
	}
	return c.write(m)
}

// Initialize performs the handshake: the initialize request, then the
// initialized notification the server waits for before serving anything else.
func (c *Client) Initialize(ctx context.Context, info ClientInfo) (InitializeResponse, error) {
	var res InitializeResponse
	if err := c.Call(ctx, MethodInitialize, InitializeParams{ClientInfo: info}, &res); err != nil {
		return res, err
	}
	return res, c.Notify(MethodInitialized, nil)
}

// ThreadStart opens a new thread.
func (c *Client) ThreadStart(ctx context.Context, p ThreadStartParams) (ThreadStartResponse, error) {
	var res ThreadStartResponse
	err := c.Call(ctx, MethodThreadStart, p, &res)
	return res, err
}

// ThreadResume reopens a persisted thread.
func (c *Client) ThreadResume(ctx context.Context, p ThreadResumeParams) (ThreadResumeResponse, error) {
	var res ThreadResumeResponse
	err := c.Call(ctx, MethodThreadResume, p, &res)
	return res, err
}

// TurnStart starts a turn.
//
// Measured on codex-cli 0.137.0: while a turn is running, turn/start does not
// queue a second turn. The input joins the running turn, and the returned turn
// ID never sees turn/started or turn/completed. Use TurnSteer for that.
func (c *Client) TurnStart(ctx context.Context, p TurnStartParams) (TurnStartResponse, error) {
	var res TurnStartResponse
	err := c.Call(ctx, MethodTurnStart, p, &res)
	return res, err
}

// TurnSteer adds input to the running turn. It fails when ExpectedTurnID is
// not the active turn.
func (c *Client) TurnSteer(ctx context.Context, p TurnSteerParams) (TurnSteerResponse, error) {
	var res TurnSteerResponse
	err := c.Call(ctx, MethodTurnSteer, p, &res)
	return res, err
}

// TurnInterrupt cancels a running turn. The turn then completes with status
// interrupted and the thread stays usable.
func (c *Client) TurnInterrupt(ctx context.Context, p TurnInterruptParams) error {
	return c.Call(ctx, MethodTurnInterrupt, p, nil)
}

// TextInput builds the text member of a turn's input.
func TextInput(text string) UserInput {
	u, _ := NewUserInput(TextUserInput{Type: UserInputTypeText, Text: text})
	return u
}
