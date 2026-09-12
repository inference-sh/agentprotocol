package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// DefaultCallTimeout bounds a single request that expects a reply. Agents
// occasionally accept a call and never answer; without a bound the caller
// blocks forever.
const DefaultCallTimeout = 60 * time.Second

// maxLineBytes caps one JSON-RPC line. Agents inline file contents and tool
// output into updates, so the default scanner limit is far too small.
const maxLineBytes = 8 << 20

// Handler is the policy half of the client. The transport decides nothing; it
// hands every agent-initiated request here and sends back whatever comes out.
//
// A nil Handler, or a nil field within one, is safe: the client answers with
// the conservative default described on each field. That means a caller can
// implement only what it cares about.
type Handler struct {
	// OnUpdate receives streamed progress. It must not block for long; the
	// read loop is single-threaded and a slow handler stalls everything
	// behind it, including responses to calls already in flight.
	OnUpdate func(UpdateNotification)

	// OnPermission decides whether the agent may proceed. Returning an error
	// cancels the request. Nil cancels every request, because silently
	// allowing an action nobody approved is the one outcome a default must
	// never produce.
	OnPermission func(context.Context, PermissionRequest) (PermissionResponse, error)

	// OnReadTextFile serves a file the agent asked for. Nil refuses, so an
	// agent cannot read the client's filesystem unless the client opted in.
	OnReadTextFile func(context.Context, ReadTextFileParams) (ReadTextFileResult, error)

	// OnWriteTextFile writes a file on the agent's behalf. Nil refuses, for
	// the same reason.
	OnWriteTextFile func(context.Context, WriteTextFileParams) error

	// OnElicitation answers a structured question. Nil declines, which lets
	// the agent continue without an answer rather than hanging on one.
	OnElicitation func(context.Context, ElicitationParams) (ElicitationResponse, error)

	// OnUnhandled is called for any other agent-initiated request. The client
	// always replies with method-not-found afterwards: an unanswered request
	// leaves the agent waiting on its own timeout, which looks like a hang
	// and has cost us a real debugging session before.
	OnUnhandled func(method string, params json.RawMessage)
}

// Client is one ACP conversation over a byte stream.
//
// It owns the stream but not the process. A caller that spawned a subprocess
// keeps responsibility for waiting on it and reaping it; this type only speaks
// the protocol. That split is what lets the same client run over a pipe, a
// socket, or an in-memory stream in a test.
type Client struct {
	w  io.WriteCloser
	sc *bufio.Scanner
	h  Handler

	info ClientInfo

	writeMu sync.Mutex

	mu        sync.Mutex
	nextID    int
	pending   map[int]chan Message
	sessionID string

	closeOnce sync.Once
	done      chan struct{}
	readErr   error

	CallTimeout time.Duration
}

// NewClient wires a client to a duplex stream. Nothing is sent until
// Initialize runs.
func NewClient(r io.Reader, w io.WriteCloser, info ClientInfo, h Handler) *Client {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	return &Client{
		w:           w,
		sc:          sc,
		h:           h,
		info:        info,
		pending:     make(map[int]chan Message),
		done:        make(chan struct{}),
		CallTimeout: DefaultCallTimeout,
	}
}

// Start begins reading. It returns immediately; the read loop runs until the
// stream ends or Close is called.
func (c *Client) Start() {
	go c.readLoop()
}

// Done is closed when the read loop stops, whether cleanly or by error.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the read loop stopped, or nil if it ended cleanly.
func (c *Client) Err() error {
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// SessionID is the session opened by NewSession, empty before that.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Initialize performs the ACP handshake. The advertised capabilities follow
// from the Handler: an agent is told it may ask permission or touch files only
// when the caller supplied something to answer with.
func (c *Client) Initialize(ctx context.Context) (json.RawMessage, error) {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientInfo:      c.info,
		Capabilities: ClientCapabilities{
			PermissionRequests: c.h.OnPermission != nil,
			FileSystem:         c.h.OnReadTextFile != nil || c.h.OnWriteTextFile != nil,
		},
	}
	return c.Call(ctx, MethodInitialize, params)
}

// NewSession opens a session rooted at cwd and remembers its ID.
func (c *Client) NewSession(ctx context.Context, cwd string, servers []MCPServer) (string, error) {
	if servers == nil {
		servers = []MCPServer{}
	}
	raw, err := c.Call(ctx, MethodSessionNew, NewSessionParams{CWD: cwd, MCPServers: servers})
	if err != nil {
		return "", err
	}
	var res NewSessionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("acp: decode session/new result: %w", err)
	}
	c.mu.Lock()
	c.sessionID = res.SessionID
	c.mu.Unlock()
	return res.SessionID, nil
}

// Prompt sends user input. It returns when the agent answers the prompt call,
// which for most agents is the end of the turn; progress arrives meanwhile
// through Handler.OnUpdate.
func (c *Client) Prompt(ctx context.Context, text string) (json.RawMessage, error) {
	return c.PromptBlocks(ctx, []ContentBlock{TextBlock(text)})
}

// PromptBlocks is Prompt for content that is not a single string.
func (c *Client) PromptBlocks(ctx context.Context, blocks []ContentBlock) (json.RawMessage, error) {
	sid := c.SessionID()
	if sid == "" {
		return nil, errors.New("acp: prompt before session/new")
	}
	return c.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: sid, Prompt: blocks})
}

// Cancel asks the agent to abandon the current turn, leaving the session open.
func (c *Client) Cancel(ctx context.Context) error {
	sid := c.SessionID()
	if sid == "" {
		return nil
	}
	return c.Notify(MethodSessionCancel, SessionRef{SessionID: sid})
}

// CloseSession ends the session without tearing down the stream, giving the
// agent a chance to run its own shutdown work.
func (c *Client) CloseSession() error {
	sid := c.SessionID()
	if sid == "" {
		return nil
	}
	return c.Notify(MethodSessionClose, SessionRef{SessionID: sid})
}

// Close shuts the write side, which ends the agent's read loop and in turn
// ours. It is safe to call more than once.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.w.Close() })
	return err
}

// Call sends a request and waits for its response.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(Request{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(c.CallTimeout)
	defer timer.Stop()

	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("acp: %s: %w", method, msg.Error)
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("acp: timeout after %s waiting for %s", c.CallTimeout, method)
	case <-c.done:
		if c.readErr != nil {
			return nil, fmt.Errorf("acp: stream ended during %s: %w", method, c.readErr)
		}
		return nil, fmt.Errorf("acp: stream ended during %s", method)
	}
}

// Notify sends a request that expects no reply.
func (c *Client) Notify(method string, params any) error {
	return c.write(Request{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(req Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("acp: encode %s: %w", req.Method, err)
	}
	data = append(data, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(data); err != nil {
		return fmt.Errorf("acp: write %s: %w", req.Method, err)
	}
	return nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	for c.sc.Scan() {
		line := c.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg Message
		if json.Unmarshal(line, &msg) != nil {
			// Agents print diagnostics to stdout alongside protocol
			// traffic. A line we cannot parse is noise, not a fatal error.
			continue
		}
		c.dispatch(msg)
	}
	if err := c.sc.Err(); err != nil {
		c.mu.Lock()
		c.readErr = err
		c.mu.Unlock()
	}
}

func (c *Client) dispatch(msg Message) {
	if msg.IsResponse() {
		c.mu.Lock()
		ch, ok := c.pending[*msg.ID]
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
		return
	}

	if msg.Method == MethodSessionUpdate {
		if c.h.OnUpdate != nil {
			var n UpdateNotification
			if json.Unmarshal(msg.Params, &n) == nil {
				c.h.OnUpdate(n)
			}
		}
		return
	}

	if msg.IsNotification() {
		if c.h.OnUnhandled != nil {
			c.h.OnUnhandled(msg.Method, msg.Params)
		}
		return
	}

	c.handleRequest(msg)
}

// handleRequest answers an agent-initiated request. Every path replies. An
// unanswered request leaves the agent blocked on its own timeout, which
// presents as a hang far from the cause.
func (c *Client) handleRequest(msg Message) {
	ctx := context.Background()
	id := *msg.ID

	switch msg.Method {
	case MethodRequestPermission:
		if c.h.OnPermission == nil {
			c.respond(id, Cancelled())
			return
		}
		var req PermissionRequest
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			c.respondError(id, ErrCodeInvalidRequest, err.Error())
			return
		}
		res, err := c.h.OnPermission(ctx, req)
		if err != nil {
			c.respond(id, Cancelled())
			return
		}
		c.respond(id, res)

	case MethodFsReadTextFile:
		if c.h.OnReadTextFile == nil {
			c.respondError(id, ErrCodeMethodNotFound, "client does not serve files")
			return
		}
		var p ReadTextFileParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			c.respondError(id, ErrCodeInvalidRequest, err.Error())
			return
		}
		res, err := c.h.OnReadTextFile(ctx, p)
		if err != nil {
			c.respondError(id, ErrCodeInvalidRequest, err.Error())
			return
		}
		c.respond(id, res)

	case MethodFsWriteTextFile:
		if c.h.OnWriteTextFile == nil {
			c.respondError(id, ErrCodeMethodNotFound, "client does not accept writes")
			return
		}
		var p WriteTextFileParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			c.respondError(id, ErrCodeInvalidRequest, err.Error())
			return
		}
		if err := c.h.OnWriteTextFile(ctx, p); err != nil {
			c.respondError(id, ErrCodeInvalidRequest, err.Error())
			return
		}
		c.respond(id, struct{}{})

	case MethodElicitationCreate:
		if c.h.OnElicitation == nil {
			c.respond(id, ElicitationResponse{Action: ElicitationDecline})
			return
		}
		var p ElicitationParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			c.respondError(id, ErrCodeInvalidRequest, err.Error())
			return
		}
		res, err := c.h.OnElicitation(ctx, p)
		if err != nil {
			c.respond(id, ElicitationResponse{Action: ElicitationCancel})
			return
		}
		c.respond(id, res)

	default:
		if c.h.OnUnhandled != nil {
			c.h.OnUnhandled(msg.Method, msg.Params)
		}
		c.respondError(id, ErrCodeMethodNotFound, "method not found: "+msg.Method)
	}
}

func (c *Client) respond(id int, result any) {
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		c.respondError(id, ErrCodeInvalidRequest, "client produced an unencodable result")
		return
	}
	c.writeRaw(append(data, '\n'))
}

func (c *Client) respondError(id, code int, message string) {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	c.writeRaw(append(data, '\n'))
}

func (c *Client) writeRaw(data []byte) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.w.Write(data)
}
