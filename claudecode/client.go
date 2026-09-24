package claudecode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Handler receives what the CLI sends that is not a reply to one of our
// requests. Every field is optional.
type Handler struct {
	// OnMessage receives every conversation line: system, assistant, user,
	// result, stream_event, and any type this package does not know. It runs
	// on the read loop, in order, so it must not block for long.
	OnMessage func(Message)

	// OnPermission answers can_use_tool. It runs on its own goroutine and may
	// block until a person decides; ctx ends when the CLI withdraws the
	// request with control_cancel_request, or the client closes. A nil
	// handler denies every request, which is the safe default.
	OnPermission func(ctx context.Context, requestID string, req PermissionRequest) (PermissionResult, error)

	// OnHookCallback answers hook_callback, for hooks registered at
	// initialize. Nil answers with an empty object, which the CLI reads as
	// "no opinion".
	OnHookCallback func(ctx context.Context, req HookCallbackRequest) (json.RawMessage, error)

	// OnCancel is told when the CLI withdraws one of its own requests, after
	// the context passed to OnPermission has been cancelled.
	OnCancel func(requestID string)

	// OnUnhandled is told about a control request subtype this client does
	// not implement. The CLI gets an error reply either way.
	OnUnhandled func(subtype string, raw json.RawMessage)

	// OnMalformed is told about a line that is not JSON.
	OnMalformed func(line []byte, err error)
}

// Client speaks the control protocol over a pair of streams. Most callers use
// Spawn, which wires one to a child process.
type Client struct {
	r io.Reader
	h Handler

	wmu sync.Mutex
	w   io.Writer

	mu       sync.Mutex
	pending  map[string]chan ControlResponseBody
	inflight map[string]context.CancelFunc
	closed   bool

	done    chan struct{}
	readErr error
}

// ErrClosed is returned by requests made after the stream ended.
var ErrClosed = errors.New("claudecode: connection closed")

// NewClient wraps a stream. Call Start to begin reading.
func NewClient(r io.Reader, w io.Writer, h Handler) *Client {
	return &Client{
		r:        r,
		w:        w,
		h:        h,
		pending:  make(map[string]chan ControlResponseBody),
		inflight: make(map[string]context.CancelFunc),
		done:     make(chan struct{}),
	}
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

// Send writes one JSON line.
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

// SendUser writes a text prompt.
func (c *Client) SendUser(text string) error { return c.Send(NewUserMessage(text)) }

// Request sends a control request and waits for its reply. If ctx ends first
// the request is withdrawn with control_cancel_request, as the SDK does.
func (c *Client) Request(ctx context.Context, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	id := newRequestID()
	ch := make(chan ControlResponseBody, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.Send(ControlRequest{Type: TypeControlRequest, RequestID: id, Request: raw}); err != nil {
		c.forget(id)
		return nil, fmt.Errorf("claudecode: send control request: %w", err)
	}

	select {
	case res, ok := <-ch:
		if !ok {
			return nil, ErrClosed
		}
		if res.Subtype == ResponseError {
			return nil, &ControlError{Message: res.Error}
		}
		return res.Response, nil
	case <-ctx.Done():
		c.forget(id)
		_ = c.Send(ControlCancel{Type: TypeControlCancel, RequestID: id})
		return nil, ctx.Err()
	}
}

// ControlError is an error reply to one of our control requests.
type ControlError struct{ Message string }

func (e *ControlError) Error() string { return "claudecode: control request failed: " + e.Message }

// Initialize performs the handshake. It must be the first control request.
func (c *Client) Initialize(ctx context.Context, req InitializeRequest) (InitializeResponse, error) {
	req.Subtype = SubtypeInitialize
	var res InitializeResponse
	raw, err := c.Request(ctx, req)
	if err != nil {
		return res, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return res, fmt.Errorf("claudecode: decode initialize response: %w", err)
		}
	}
	return res, nil
}

// Interrupt aborts the running turn. The session stays usable; the turn ends
// with a result whose TerminalReason is aborted_*. With no turn running it is
// a no-op that still succeeds.
func (c *Client) Interrupt(ctx context.Context) error {
	_, err := c.Request(ctx, map[string]any{"subtype": SubtypeInterrupt})
	return err
}

// SetPermissionMode changes how tool permissions are handled. Selecting
// bypassPermissions is refused here: it needs a launch flag this package
// never passes, and approvals are meant to reach a person.
func (c *Client) SetPermissionMode(ctx context.Context, mode string) error {
	if mode == PermissionModeBypassPermissions {
		return errors.New("claudecode: bypassPermissions is not offered")
	}
	_, err := c.Request(ctx, map[string]any{"subtype": SubtypeSetPermissionMode, "mode": mode})
	return err
}

// SetModel switches the model for later turns. Empty resets to the default.
func (c *Client) SetModel(ctx context.Context, model string) error {
	body := map[string]any{"subtype": SubtypeSetModel}
	if model != "" {
		body["model"] = model
	}
	_, err := c.Request(ctx, body)
	return err
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) readLoop() {
	defer c.shutdown()

	sc := bufio.NewScanner(c.r)
	// Lines carry whole tool results and file contents; the SDK imposes no
	// line limit, so allow a generous one.
	sc.Buffer(make([]byte, 0, 256<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			if c.h.OnMalformed != nil {
				c.h.OnMalformed(append([]byte(nil), line...), err)
			}
			continue
		}
		raw := append(json.RawMessage(nil), line...)

		switch head.Type {
		case TypeControlResponse:
			var res ControlResponse
			if json.Unmarshal(raw, &res) == nil {
				c.deliver(res.Response)
			}
		case TypeControlRequest:
			var req ControlRequest
			if json.Unmarshal(raw, &req) == nil {
				c.serve(req)
			}
		case TypeControlCancel:
			var cancel ControlCancel
			if json.Unmarshal(raw, &cancel) == nil {
				c.withdraw(cancel.RequestID)
			}
		case TypeKeepAlive:
		default:
			if c.h.OnMessage != nil {
				c.h.OnMessage(Message{Type: head.Type, Subtype: head.Subtype, Raw: raw})
			}
		}
	}
	c.readErr = sc.Err()
}

func (c *Client) deliver(res ControlResponseBody) {
	c.mu.Lock()
	ch, ok := c.pending[res.RequestID]
	delete(c.pending, res.RequestID)
	c.mu.Unlock()
	if ok {
		ch <- res
	}
}

// serve answers a request from the CLI on its own goroutine, so a permission
// prompt waiting on a person does not stop the stream.
func (c *Client) serve(req ControlRequest) {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return
	}
	c.inflight[req.RequestID] = cancel
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, req.RequestID)
			c.mu.Unlock()
			cancel()
		}()

		result, err := c.answer(ctx, req)
		if ctx.Err() != nil {
			// Withdrawn or closing: the CLI no longer expects an answer.
			return
		}
		body := ControlResponseBody{Subtype: ResponseSuccess, RequestID: req.RequestID, Response: result}
		if err != nil {
			body = ControlResponseBody{Subtype: ResponseError, RequestID: req.RequestID, Error: err.Error()}
		}
		_ = c.Send(ControlResponse{Type: TypeControlResponse, Response: body})
	}()
}

func (c *Client) answer(ctx context.Context, req ControlRequest) (json.RawMessage, error) {
	switch sub := req.Subtype(); sub {
	case SubtypeCanUseTool:
		var p PermissionRequest
		if err := json.Unmarshal(req.Request, &p); err != nil {
			return nil, fmt.Errorf("decode can_use_tool: %w", err)
		}
		res := DenyResult("No permission handler is attached.")
		if c.h.OnPermission != nil {
			var err error
			if res, err = c.h.OnPermission(ctx, req.RequestID, p); err != nil {
				return nil, err
			}
		}
		if res.ToolUseID == "" {
			res.ToolUseID = p.ToolUseID
		}
		return json.Marshal(res)

	case SubtypeHookCallback:
		var h HookCallbackRequest
		if err := json.Unmarshal(req.Request, &h); err != nil {
			return nil, fmt.Errorf("decode hook_callback: %w", err)
		}
		if c.h.OnHookCallback == nil {
			return json.RawMessage(`{}`), nil
		}
		return c.h.OnHookCallback(ctx, h)

	default:
		if c.h.OnUnhandled != nil {
			c.h.OnUnhandled(sub, req.Request)
		}
		return nil, fmt.Errorf("Unsupported control request subtype: %s", sub)
	}
}

func (c *Client) withdraw(id string) {
	c.mu.Lock()
	cancel, ok := c.inflight[id]
	c.mu.Unlock()
	if !ok {
		return
	}
	cancel()
	if c.h.OnCancel != nil {
		c.h.OnCancel(id)
	}
}

// shutdown fails every waiting request and cancels every inbound one.
func (c *Client) shutdown() {
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = map[string]chan ControlResponseBody{}
	inflight := c.inflight
	c.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	for _, cancel := range inflight {
		cancel()
	}
	close(c.done)
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewSessionID returns a random UUID v4, the form --session-id requires.
func NewSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
