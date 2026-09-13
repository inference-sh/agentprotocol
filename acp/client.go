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

// DefaultCallTimeout bounds a protocol call that expects a reply. Agents
// occasionally accept a call and never answer; without a bound the caller
// blocks forever. Initialize and session/new are handshake traffic and should
// return in well under this.
const DefaultCallTimeout = 60 * time.Second

// DefaultPromptTimeout bounds a prompt.
//
// It is much longer than DefaultCallTimeout because it measures something
// different. Every other call is protocol chatter whose duration is the
// agent's own bookkeeping, but session/prompt does not return until the model
// has finished the turn, which for an agent running tools can be many minutes.
// Timing a prompt out on a protocol budget kills work that was progressing.
const DefaultPromptTimeout = 10 * time.Minute

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
	// OnUpdate receives streamed progress. It must not block for long; it runs
	// on the read loop, because an update's order is the content's order and
	// handing updates to goroutines would scramble a message into arbitrary
	// pieces. A slow handler stalls everything behind it, including responses
	// to calls already in flight.
	//
	// Agent-initiated requests are not subject to this — see OnPermission.
	OnUpdate func(UpdateNotification)

	// OnPermission decides whether the agent may proceed. Returning an error
	// cancels the request. Nil cancels every request, because silently
	// allowing an action nobody approved is the one outcome a default must
	// never produce.
	//
	// Unlike OnUpdate this may block for as long as it needs to, which is the
	// point: the answer can be a human's, arriving minutes later and from
	// another machine. Each request is handled on its own goroutine, so
	// waiting here does not stop updates arriving, does not stall replies to
	// calls in flight, and does not prevent a second permission request from
	// being raised alongside the first.
	//
	// One consequence: a permission request now races the updates around it
	// rather than being ordered among them. Text from the turn can reach
	// OnUpdate before or after the request reaches this handler, and both are
	// correct. Anything that assumed the serialised order is wrong.
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

	// OnPeerError receives an error the agent reported without attributing it
	// to any request, which JSON-RPC permits and agents do use. Nothing can be
	// replied to and no caller is waiting, so the only alternative is to
	// discard it; a handler here is how such failures stay visible. Nil
	// discards them.
	OnPeerError func(*Error)
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

	initRaw json.RawMessage
	initRes InitializeResult

	// Replay bookkeeping for LoadSession. loading is held for the duration of
	// a load; replayed counts updates seen during it and lastUpdate is when
	// the most recent one arrived, which together are how a load detects that
	// an agent has finished replaying without answering the call.
	loading      bool
	replayed     int
	conversation int
	userTurn     bool
	lastUpdate   time.Time

	closeOnce sync.Once
	done      chan struct{}
	readErr   error

	// CallTimeout bounds a protocol call. Zero means DefaultCallTimeout.
	CallTimeout time.Duration

	// PromptTimeout bounds a prompt specifically, whose duration belongs to
	// the model rather than the protocol. Zero means DefaultPromptTimeout.
	PromptTimeout time.Duration

	// ReplayIdleGap is how long session/update must stay quiet during a load
	// before the session counts as rebuilt. Zero means DefaultReplayIdleGap.
	ReplayIdleGap time.Duration

	// LoadTimeout bounds a whole load. Zero means DefaultLoadTimeout.
	LoadTimeout time.Duration
}

// NewClient wires a client to a duplex stream. Nothing is sent until
// Initialize runs.
func NewClient(r io.Reader, w io.WriteCloser, info ClientInfo, h Handler) *Client {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	return &Client{
		w:             w,
		sc:            sc,
		h:             h,
		info:          info,
		pending:       make(map[int]chan Message),
		done:          make(chan struct{}),
		CallTimeout:   DefaultCallTimeout,
		PromptTimeout: DefaultPromptTimeout,
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
	raw, err := c.Call(ctx, MethodInitialize, params)
	if err != nil {
		return nil, err
	}

	// A result we cannot parse is not a failed handshake. Agents put
	// unexpected shapes in here and the fields we read are advisory, so the
	// raw form is kept either way and the typed view is best-effort.
	var res InitializeResult
	_ = json.Unmarshal(raw, &res)

	c.mu.Lock()
	c.initRaw = raw
	c.initRes = res
	c.mu.Unlock()
	return raw, nil
}

// InitializeRaw is the agent's unmodified handshake result, or nil before
// Initialize has run. Spawn performs the handshake itself, so this is how a
// caller that used Spawn reaches a result it never saw returned.
func (c *Client) InitializeRaw() json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initRaw
}

// AgentInfo is the agent's self-description from the handshake.
func (c *Client) AgentInfo() AgentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initRes.AgentInfo
}

// AgentCapabilities is what the agent said it can do, zero before Initialize.
//
// It is a claim rather than a contract. See CanLoadSession.
func (c *Client) AgentCapabilities() AgentCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initRes.AgentCapabilities
}

// AuthMethods are the ways the agent will accept being authenticated. Empty is
// the common case: the user logged in through the vendor's own CLI and the
// agent asks nothing of us.
func (c *Client) AuthMethods() []AuthMethod {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initRes.AuthMethods
}

// CanLoadSession reports whether the agent declared support for resuming.
//
// Do not branch on it. Every agent measured declares it, and loads still fail
// for reasons the flag cannot see, so a false is not a refusal and a true is
// not a promise. The claim carries no information about the outcome.
//
// The only reliable test is to call LoadSession and handle the error. This is
// here so a caller can show a person what the agent said, and so the claim is
// on record when it turns out to be untrue.
func (c *Client) CanLoadSession() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initRes.AgentCapabilities.LoadSession
}

// Authenticate selects one of the agent's advertised auth methods.
//
// Most agents advertise none, because the user logged in with the vendor's CLI
// and the credential is already on disk where the agent expects it. Call this
// only for an agent that asks: an unsolicited authenticate is a call the agent
// never offered to answer.
func (c *Client) Authenticate(ctx context.Context, methodID string) error {
	_, err := c.Call(ctx, MethodAuthenticate, AuthenticateParams{MethodID: methodID})
	return err
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

// DefaultReplayIdleGap is how long session/update must stay quiet before a
// load concludes that the agent has finished replaying.
//
// Three seconds because replay arrives in a burst and the gaps within one are
// small, while the gap after it is unbounded. Too short ends a load mid-replay
// and the rest of the history is then delivered as live progress.
const DefaultReplayIdleGap = 3 * time.Second

// DefaultLoadTimeout bounds a whole load: no answer and no replay within it
// means the agent is not going to attach.
const DefaultLoadTimeout = 60 * time.Second

// LoadResult describes how a load ended, which is worth surfacing because the
// two successful endings are not the same thing.
type LoadResult struct {
	// Replayed counts every update the agent sent while rebuilding.
	//
	// On its own this number proves very little. An agent announces its mode
	// and its available commands when any session opens, so a handful of
	// updates is what opening a blank session looks like too.
	Replayed int

	// Conversation counts the replayed updates that carry something said or
	// done — messages, thoughts, tool calls — rather than the session
	// describing itself.
	Conversation int

	// RestoredConversation reports that the replay contained something the
	// user said.
	//
	// This is the honest test of whether a load did anything. A fresh session
	// cannot produce a user turn, because on a fresh session the user has not
	// spoken; an agent that ignored the session id and opened a blank one
	// would replay no user turn no matter how many updates it sent. False
	// with a nil error means the agent accepted the call and gave back
	// nothing that proves it found the session.
	RestoredConversation bool

	// Answered reports whether session/load returned. False means the agent
	// attached and replayed but never answered the call.
	//
	// Against a mock backend with short replays every agent measured has
	// answered. T3 Code hit the other case in production against real models,
	// where replays are far longer, so the idle-gap path stays. Read a false
	// as "this agent needed the fallback", not as an error.
	Answered bool

	// Elapsed is how long the load took.
	Elapsed time.Duration
}

// LoadSession reopens a session the agent persisted earlier and remembers its
// ID, the resuming counterpart to NewSession.
//
// The intent is that the conversation comes back: the agent rebuilds its state
// and replays the history as session/update notifications, and the next prompt
// continues the same thread. It happens in a new process. A session somebody
// is driving by hand in a terminal keeps its own process untouched; what
// transfers is the conversation, not the pipe.
//
// A load that returns without error proves only that the agent did not
// refuse. Whether the conversation came with it is a separate question, and
// LoadResult.RestoredConversation answers it: an agent that ignored the id and
// opened a blank session sends its usual openers and no user turn, which is
// otherwise indistinguishable from a real replay. Whether a given agent can
// resume at all has also been seen to depend on how the previous process
// ended and whether it had been reaped before the load was attempted.
//
// Replayed updates reach Handler.OnUpdate with Replay set, so a caller can
// take them as history. They are delivered rather than swallowed because a
// caller attaching to a conversation it has never seen usually wants it.
//
// The awkward part, and the reason this is not two lines: session/load does
// not reliably return. Some agents answer only once replay finishes, some
// never answer while streaming, and one refuses outright. So the call races a
// replay-idle timer, and an agent that replays and goes quiet is treated as
// attached even though the RPC is still outstanding.
func (c *Client) LoadSession(ctx context.Context, sessionID, cwd string, servers []MCPServer) (LoadResult, error) {
	if sessionID == "" {
		return LoadResult{}, errors.New("acp: load without a session id")
	}
	if servers == nil {
		servers = []MCPServer{}
	}

	c.mu.Lock()
	c.loading = true
	c.replayed = 0
	c.conversation = 0
	c.userTurn = false
	c.lastUpdate = time.Time{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.loading = false
		c.mu.Unlock()
	}()

	params := LoadSessionParams{SessionID: sessionID, CWD: cwd, MCPServers: servers}
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, MethodSessionLoad, params)
		done <- err
	}()

	start := time.Now()
	gap := c.ReplayIdleGap
	if gap <= 0 {
		gap = DefaultReplayIdleGap
	}
	budget := c.LoadTimeout
	if budget <= 0 {
		budget = DefaultLoadTimeout
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(budget)
	defer deadline.Stop()

	for {
		select {
		case err := <-done:
			if err != nil {
				return c.loadResult(start, false), fmt.Errorf("acp: load session %q: %w", sessionID, err)
			}
			c.setSessionID(sessionID)
			return c.loadResult(start, true), nil

		case <-ticker.C:
			c.mu.Lock()
			quiet := c.replayed > 0 && !c.lastUpdate.IsZero() && time.Since(c.lastUpdate) >= gap
			c.mu.Unlock()
			if quiet {
				c.setSessionID(sessionID)
				return c.loadResult(start, false), nil
			}

		case <-deadline.C:
			return c.loadResult(start, false),
				fmt.Errorf("acp: load session %q: no answer and no replay within %s", sessionID, budget)

		case <-c.done:
			return c.loadResult(start, false),
				fmt.Errorf("acp: load session %q: agent exited", sessionID)

		case <-ctx.Done():
			return c.loadResult(start, false), ctx.Err()
		}
	}
}

func (c *Client) loadResult(start time.Time, answered bool) LoadResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return LoadResult{
		Replayed:             c.replayed,
		Conversation:         c.conversation,
		RestoredConversation: c.userTurn,
		Answered:             answered,
		Elapsed:              time.Since(start),
	}
}

// ReplayedUpdates is how many updates arrived during the most recent load.
func (c *Client) ReplayedUpdates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replayed
}

func (c *Client) setSessionID(id string) {
	c.mu.Lock()
	c.sessionID = id
	c.mu.Unlock()
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
	return c.call(ctx, MethodSessionPrompt, PromptParams{SessionID: sid, Prompt: blocks}, c.promptTimeout())
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

// Call sends a request and waits for its response, bounded by CallTimeout.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.call(ctx, method, params, c.callTimeout())
}

func (c *Client) callTimeout() time.Duration {
	if c.CallTimeout > 0 {
		return c.CallTimeout
	}
	return DefaultCallTimeout
}

func (c *Client) promptTimeout() time.Duration {
	if c.PromptTimeout > 0 {
		return c.PromptTimeout
	}
	return DefaultPromptTimeout
}

func (c *Client) call(ctx context.Context, method string, params any, budget time.Duration) (json.RawMessage, error) {
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

	timer := time.NewTimer(budget)
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
		return nil, fmt.Errorf("acp: timeout after %s waiting for %s", budget, method)
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
		// Count it before handing it over, whether or not anyone is
		// listening: LoadSession decides that replay has finished by watching
		// these, and a client with no OnUpdate must still be able to load.
		var n UpdateNotification
		decoded := json.Unmarshal(msg.Params, &n) == nil

		c.mu.Lock()
		replay := c.loading
		if replay {
			c.replayed++
			c.lastUpdate = time.Now()
			if decoded {
				if IsConversation(n.Update) {
					c.conversation++
				}
				if IsUserTurn(n.Update) {
					c.userTurn = true
				}
			}
		}
		c.mu.Unlock()

		if c.h.OnUpdate != nil && decoded {
			n.Replay = replay
			c.h.OnUpdate(n)
		}
		return
	}

	if msg.IsNotification() {
		if c.h.OnUnhandled != nil {
			c.h.OnUnhandled(msg.Method, msg.Params)
		}
		return
	}

	if msg.ID == nil {
		// Neither an ID nor a method. JSON-RPC allows a peer to report a
		// failure it cannot attribute to any request by sending an error with
		// a null or absent id, and agents do: kiro answers a session/close
		// notification this way. There is nobody to deliver it to and nothing
		// to reply to, so surface it and move on.
		if msg.Error != nil && c.h.OnPeerError != nil {
			c.h.OnPeerError(msg.Error)
		}
		return
	}

	// An agent-initiated request gets its own goroutine, because answering one
	// can take as long as a human takes to decide. On the read loop a parked
	// permission request freezes the whole connection: no updates arrive, no
	// reply to any call in flight is delivered, and a second permission
	// request cannot even be read until the first is answered — which made the
	// driver's map of pending permissions unreachable past one entry.
	//
	// Responses carry the id they answer, so the agent does not care what
	// order they come back in. Notifications stay on the read loop, where
	// their order is the content's order and must be preserved.
	//
	// duringLoad is read here, on the read loop, not inside the goroutine.
	// The goroutine runs at an arbitrary later time, by which point a load may
	// have finished and cleared the flag; capturing it at arrival is what
	// makes "this request came in mid-replay" true of when it arrived rather
	// than of when it happened to be scheduled.
	c.mu.Lock()
	duringLoad := c.loading
	c.mu.Unlock()
	go c.handleRequest(msg, duringLoad)
}

// handleRequest answers an agent-initiated request. Every path replies. An
// unanswered request leaves the agent blocked on its own timeout, which
// presents as a hang far from the cause.
func (c *Client) handleRequest(msg Message, duringLoad bool) {
	if msg.ID == nil {
		// Unreachable via dispatch, which filters this case. Kept so that a
		// future dispatch path cannot reintroduce a panic in the read loop,
		// which would take down the whole host process.
		return
	}
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
		req.DuringLoad = duringLoad
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
