package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
)

// Message is one JSON-RPC message in either direction. Which fields are set
// says what it is: Method and ID a request, Method alone a notification, ID
// with Result or Error a response.
type Message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// HasID reports whether the message carries an id; a null id is none.
func (m Message) HasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error renders the message and, when the peer attached one, its data.
//
// Agents put their real explanation in data and a bare category in message:
// gemini answers a failed load with message "Internal error" and data that
// says the session id was not found, where it looked, and to run
// --list-sessions. A formatter that printed only the message would discard
// the one sentence that identifies the cause. Data is raw JSON; it is
// appended verbatim rather than parsed, because its shape is the peer's.
func (e *Error) Error() string {
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return e.Message
	}
	return e.Message + ": " + string(e.Data)
}

// JSON-RPC error codes.
const (
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// ErrClosed is returned by calls made after the stream ended, and by calls
// still waiting when it did.
var ErrClosed = errors.New("connection closed")

// Handler receives what the peer sends that is not a reply. Every field is
// optional.
type Handler struct {
	// OnNotification runs on the read loop, in arrival order. It must not
	// block for long: the next message is not read until it returns.
	OnNotification func(method string, params json.RawMessage)

	// OnRequest runs on the read loop and must not block. It answers with
	// Reply or ReplyError, usually from a goroutine it starts, since an
	// answer can wait on a person. Nil answers method-not-found.
	OnRequest func(Message)

	// OnPeerError receives an error the peer sent without an id, which
	// JSON-RPC permits for a failure it cannot attribute to a request.
	OnPeerError func(*Error)
}

// Conn is one JSON-RPC connection over a line stream. Outgoing ids are
// integers.
type Conn struct {
	r       io.Reader
	w       *LineWriter
	h       Handler
	version string

	nextID  atomic.Int64
	pending Pending[int64, Message]

	done chan struct{}
	err  error
}

// NewConn wraps a stream. version is sent as the "jsonrpc" member; empty
// omits it, for peers such as codex that neither send nor require it. Call
// Start to begin reading.
func NewConn(r io.Reader, w io.Writer, version string, h Handler) *Conn {
	return &Conn{r: r, w: NewLineWriter(w), h: h, version: version, done: make(chan struct{})}
}

// Start runs the read loop until the stream ends.
func (c *Conn) Start() { go c.readLoop() }

// Done closes when the stream has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the stream ended once Done has closed: nil for a clean EOF.
// Before that it is nil.
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func (c *Conn) readLoop() {
	c.err = ReadLines(c.r, c.dispatch)
	c.pending.Close()
	close(c.done)
}

func (c *Conn) dispatch(line []byte) {
	var m Message
	if json.Unmarshal(line, &m) != nil {
		// Agents print diagnostics to stdout alongside protocol traffic. A
		// line that is not JSON-RPC is noise, not the end of the session.
		return
	}
	switch {
	case m.Method != "" && m.HasID():
		if c.h.OnRequest == nil {
			_ = c.ReplyError(m.ID, &Error{Code: CodeMethodNotFound, Message: "method not found: " + m.Method})
			return
		}
		c.h.OnRequest(m)
	case m.Method != "":
		if c.h.OnNotification != nil {
			c.h.OnNotification(m.Method, m.Params)
		}
	case m.HasID():
		if id, err := strconv.ParseInt(string(m.ID), 10, 64); err == nil {
			c.pending.Deliver(id, m)
		}
	case m.Error != nil:
		if c.h.OnPeerError != nil {
			c.h.OnPeerError(m.Error)
		}
	}
}

// Call sends a request and waits for its result. A peer error is returned as
// *Error; a stream that ends first as ErrClosed, wrapping the read error if
// there was one; a ctx that ends first as ctx.Err().
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	m := Message{JSONRPC: c.version, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode %s params: %w", method, err)
		}
		m.Params = raw
	}
	id := c.nextID.Add(1)
	m.ID = json.RawMessage(strconv.FormatInt(id, 10))
	ch, ok := c.pending.Add(id)
	if !ok {
		return nil, c.closedErr()
	}
	if err := c.w.WriteJSON(m); err != nil {
		c.pending.Forget(id)
		return nil, fmt.Errorf("send %s: %w", method, err)
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return nil, c.closedErr()
		}
		if res.Error != nil {
			return nil, res.Error
		}
		return res.Result, nil
	case <-ctx.Done():
		c.pending.Forget(id)
		return nil, ctx.Err()
	}
}

func (c *Conn) closedErr() error {
	if err := c.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return ErrClosed
}

// Notify sends a notification. params may be nil.
func (c *Conn) Notify(method string, params any) error {
	m := Message{JSONRPC: c.version, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode %s params: %w", method, err)
		}
		m.Params = raw
	}
	return c.w.WriteJSON(m)
}

// Reply answers a request with a result. A result that cannot be encoded is
// answered with an internal error, so the peer is never left waiting.
func (c *Conn) Reply(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return c.ReplyError(id, &Error{Code: CodeInternal, Message: "client produced an unencodable result: " + err.Error()})
	}
	return c.w.WriteJSON(Message{JSONRPC: c.version, ID: id, Result: raw})
}

// ReplyError answers a request with an error.
func (c *Conn) ReplyError(id json.RawMessage, e *Error) error {
	return c.w.WriteJSON(Message{JSONRPC: c.version, ID: id, Error: e})
}
