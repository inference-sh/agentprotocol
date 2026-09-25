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
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/inference-sh/agentprotocol/internal/jsonrpc"
)

// Message is one JSON-RPC message in either direction. Which fields are set
// says what it is: Method and ID a request, Method alone a notification, ID
// with Result or Error a response.
type Message = jsonrpc.Message

// Error is a JSON-RPC error object. Its text includes the data codex
// attached, which is where the cause usually is.
type Error = jsonrpc.Error

// JSON-RPC error codes this client sends.
const (
	CodeMethodNotFound = jsonrpc.CodeMethodNotFound
	CodeInternal       = jsonrpc.CodeInternal
)

// ErrClosed is returned by calls made after the connection ended, and by calls
// still waiting when it did.
var ErrClosed = jsonrpc.ErrClosed

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
	conn   *jsonrpc.Conn
	h      Handler
	ctx    context.Context
	cancel context.CancelFunc
}

// NewClient wraps a stream. Call Start to begin reading.
func NewClient(r io.Reader, w io.Writer, h Handler) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{h: h, ctx: ctx, cancel: cancel}
	jh := jsonrpc.Handler{OnNotification: h.OnNotification}
	if h.OnRequest != nil {
		jh.OnRequest = func(m Message) { go c.answer(m) }
	}
	c.conn = jsonrpc.NewConn(r, w, "", jh)
	return c
}

// Start runs the read loop until the stream ends.
func (c *Client) Start() {
	c.conn.Start()
	go func() {
		<-c.conn.Done()
		c.cancel()
	}()
}

// Done closes when the stream has ended.
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

// Err reports why the stream ended, once Done is closed. A clean EOF is nil.
func (c *Client) Err() error { return c.conn.Err() }

func (c *Client) answer(m Message) {
	result, rerr := c.h.OnRequest(c.ctx, m.ID, m.Method, m.Params)
	if rerr != nil {
		_ = c.conn.ReplyError(m.ID, rerr)
		return
	}
	_ = c.conn.Reply(m.ID, result)
}

// Call sends a request and decodes the result into out, which may be nil.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	raw, err := c.conn.Call(ctx, method, params)
	if err != nil {
		if _, peer := err.(*Error); peer || err == ctx.Err() {
			return err
		}
		return fmt.Errorf("codex app-server: %s: %w", method, err)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("codex app-server: decode %s result: %w", method, err)
	}
	return nil
}

// Notify sends a notification. params may be nil.
func (c *Client) Notify(method string, params any) error {
	return c.conn.Notify(method, params)
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
