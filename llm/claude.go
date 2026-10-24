package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/claudecode"
	"github.com/inference-sh/agentprotocol/transcript"
	claudetx "github.com/inference-sh/agentprotocol/transcript/claude"
)

// DefaultIdle is how long a conversation's process waits for its next call
// when Claude.Idle is zero.
const DefaultIdle = 15 * time.Minute

// claudeServer names the SDK MCP server that offers the caller's tools.
const claudeServer = "tools"

// Claude is Claude Code as a Model, on the user's own login.
//
// It launches the user's claude binary with nothing of its own switched on:
// no built-in tools (--tools ""), no MCP servers but ours
// (--strict-mcp-config), no settings files (--setting-sources ""), no skills
// or slash commands, no CLAUDE.md, memory, attachments or compaction, and
// the caller's system prompt in place of Claude Code's. The caller's tools
// are an SDK MCP server on the control channel, offered under their own
// names (CLAUDE_AGENT_SDK_MCP_NO_PREFIX), so a tool named like one of Claude
// Code's built-ins (Bash, Read, Edit) is best avoided.
//
// A tool call holds at can_use_tool: the response is returned once the
// model's message ends, and the process waits. A continued call allows the
// held calls and returns their results from the MCP server, so the model
// sees each result as the tool's own. A restart writes the conversation as
// a Claude Code session and resumes it. A new user turn after a restart is
// sent as it is. Tool results after a restart are part of the written
// session, and Claude Code continues from them with the user message it
// adds to a resumed turn (CLAUDE_CODE_RESUME_INTERRUPTED_TURN): "Continue
// from where you left off." follows the results in the same user message.
type Claude struct {
	// Command is the claude binary. Empty is claudecode.DefaultCommand.
	Command string

	// Env is the child environment. Nil inherits this process's, which is
	// how the user's login is used; CLAUDE_CONFIG_DIR selects another.
	Env []string

	// Home is where Claude Code keeps sessions (~/.claude under it), for
	// writing the session a restart resumes. Empty is the user's home.
	Home string

	// WorkRoot holds a working directory per conversation, so its sessions
	// are filed apart from the user's projects. Empty is os.TempDir(). A
	// conversation's directory is named after its key, so a restart, here
	// or on another machine with the same WorkRoot, sends what the live
	// process did.
	WorkRoot string

	// Idle is how long a conversation's process waits for its next call.
	// Zero is DefaultIdle; negative ends it with each call.
	Idle time.Duration

	// Stderr receives the CLI's diagnostics. Nil discards them.
	Stderr io.Writer

	mu       sync.Mutex
	live     map[string]*claudeConv
	contexts map[string]probed // by model; see contextFor
	closed   bool
}

var _ Model = (*Claude)(nil)

// Capabilities implements Model.
func (c *Claude) Capabilities() Capabilities {
	return Capabilities{Tools: true, Images: true, Reasoning: true, Effort: true, MaxTokens: true, ExactContinue: true}
}

// Close implements Model.
func (c *Claude) Close() error {
	c.mu.Lock()
	live := c.live
	c.live, c.closed = nil, true
	c.mu.Unlock()
	for _, conv := range live {
		conv.close()
	}
	return nil
}

// Generate implements Model.
func (c *Claude) Generate(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error) {
	if err := checkRequest(req); err != nil {
		return nil, err
	}
	if onDelta == nil {
		onDelta = func(Delta) {}
	}
	if conv := c.take(req.Key); conv != nil {
		if rest, ok := conv.continues(req); ok {
			resp, err := conv.next(ctx, req, rest, onDelta)
			return c.settle(req, conv, resp, err)
		}
		conv.close()
	}
	conv, err := c.start(ctx, req, onDelta)
	if err != nil {
		return nil, err
	}
	resp, err := conv.wait(ctx)
	if resp != nil {
		resp.Restarted = true
	}
	return c.settle(req, conv, resp, err)
}

// settle keeps a conversation for the next call on its key, or ends it.
func (c *Claude) settle(req Request, conv *claudeConv, resp *Response, err error) (*Response, error) {
	if err != nil {
		conv.close()
		return nil, err
	}
	conv.answered(req, resp)
	idle := c.Idle
	if idle == 0 {
		idle = DefaultIdle
	}
	if req.Key == "" || idle < 0 {
		conv.close()
		return resp, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		conv.close()
		return resp, nil
	}
	if c.live == nil {
		c.live = map[string]*claudeConv{}
	}
	if old := c.live[req.Key]; old != nil {
		go old.close()
	}
	c.live[req.Key] = conv
	conv.idle = time.AfterFunc(idle, func() {
		c.mu.Lock()
		if c.live[req.Key] == conv {
			delete(c.live, req.Key)
		}
		c.mu.Unlock()
		conv.close()
	})
	return resp, nil
}

// take removes the live conversation on key, for one call's exclusive use.
func (c *Claude) take(key string) *claudeConv {
	if key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.live[key]
	if conv == nil {
		return nil
	}
	delete(c.live, key)
	if conv.idle != nil && !conv.idle.Stop() {
		return nil // already ending
	}
	select {
	case <-conv.proc.Done():
		return nil
	default:
	}
	return conv
}

func checkRequest(req Request) error {
	switch req.ToolChoice {
	case "", "auto", "none":
	default:
		return fmt.Errorf("%w: tool choice %q", ErrUnsupported, req.ToolChoice)
	}
	if len(req.Messages) == 0 {
		return errors.New("llm: no messages")
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role == transcript.RoleAssistant {
		return errors.New("llm: the conversation ends with the assistant; there is nothing to answer")
	}
	return nil
}

// start launches the CLI for a call that does not continue a live
// conversation.
func (c *Claude) start(ctx context.Context, req Request, onDelta func(Delta)) (*claudeConv, error) {
	root := c.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	dir, err := workDir(root, req.Key)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	home := c.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return nil, fmt.Errorf("llm: %w", err)
		}
	}
	conv := &claudeConv{
		dir:     dir,
		home:    home,
		held:    map[string]*heldCall{},
		results: map[string]toolResult{},
	}
	cleanup := func(err error) (*claudeConv, error) {
		conv.removeFiles()
		return nil, err
	}

	// The turn after the last assistant message is what this call answers.
	// A user turn is sent; tool results go in the written session, and
	// Claude Code continues the turn they belong to when it resumes.
	cut := lastAssistant(req.Messages) + 1
	trailing := req.Messages[cut:]
	resume := slices.ContainsFunc(trailing, hasToolResult)
	seed, send := req.Messages[:cut], trailing
	if resume {
		seed, send = req.Messages, nil
	}

	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	env = append(slices.Clip(env),
		"CLAUDE_AGENT_SDK_MCP_NO_PREFIX=1",
		"ENABLE_TOOL_SEARCH=false",
		"DISABLE_COMPACT=1",
		"DISABLE_AUTO_COMPACT=1",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_CODE_DISABLE_ATTACHMENTS=1",
		"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS=1",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1",
		"CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1",
		"CLAUDE_CODE_DISABLE_CRON=1",
	)
	if req.MaxTokens > 0 {
		env = append(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS="+strconv.Itoa(req.MaxTokens))
	}
	if resume {
		env = append(env, "CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1")
	}
	args := []string{"--tools", "", "--strict-mcp-config", "--setting-sources", "", "--disable-slash-commands"}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	opts := claudecode.Options{
		Command:   c.Command,
		Dir:       dir,
		Env:       env,
		Model:     req.Model,
		ExtraArgs: args,
		Stderr:    c.Stderr,
	}
	if len(seed) > 0 {
		date := seed[0].Time
		if date.IsZero() {
			date = time.Now()
		}
		attachments := c.contextFor(ctx, env, req.Model, dir, date)
		id, err := writeClaudeSession(ctx, home, dir, req.Model, seed, attachments)
		if err != nil {
			return cleanup(err)
		}
		opts.ResumeSessionID = id
	} else {
		opts.SessionID = claudecode.NewSessionID()
	}
	conv.session = opts.ResumeSessionID + opts.SessionID

	tools := req.Tools
	if req.ToolChoice == "none" {
		tools = nil
	}
	if len(tools) > 0 {
		// Declared at launch, not at initialize: a resumed turn starts as
		// soon as the CLI is up, and must find the tools already there.
		// No timeout: toolCall answers from the request at once.
		opts.MCPConfig = claudecode.MCPConfig(map[string]any{claudeServer: claudecode.MCPServerSDK(claudeServer, 0)})
	}
	conv.server = &toolServer{name: claudeServer, tools: tools, call: conv.toolCall}
	conv.begin(onDelta)
	proc, err := claudecode.Spawn(opts, claudecode.Handler{
		OnMessage:    conv.onMessage,
		OnPermission: conv.onPermission,
		OnMCPMessage: func(ctx context.Context, server string, msg json.RawMessage) (json.RawMessage, error) {
			if server != claudeServer {
				return nil, fmt.Errorf("SDK MCP server not found: %s", server)
			}
			// nil for a notification: the client acknowledges it.
			return conv.server.handle(ctx, msg), nil
		},
	})
	if err != nil {
		return cleanup(fmt.Errorf("llm: %w", err))
	}
	conv.proc = proc
	init := claudecode.InitializeRequest{Subtype: claudecode.SubtypeInitialize, SystemPrompt: []string{req.System}}
	if _, err := proc.Initialize(ctx, init); err != nil {
		conv.close()
		return nil, fmt.Errorf("llm: claude initialize: %w", err)
	}
	if len(send) > 0 {
		if err := proc.SendUserBlocks(userBlocks(send)); err != nil {
			conv.close()
			return nil, fmt.Errorf("llm: %w", err)
		}
	}
	conv.sig = signature(req)
	return conv, nil
}

// writeClaudeSession writes entries as a Claude Code session for dir, with
// the context attachments Claude Code records after the first prompt, and
// returns its id. Entries are dated a millisecond apart, ending now: Claude
// Code resumes from the newest row.
func writeClaudeSession(ctx context.Context, home, dir, model string, entries []transcript.Entry, attachments []json.RawMessage) (string, error) {
	now := time.Now().UTC()
	es := make([]transcript.Entry, len(entries))
	for i, e := range entries {
		e.Time = now.Add(time.Duration(i-len(entries)) * time.Millisecond)
		if e.ID == "" {
			e.ID = "m" + strconv.Itoa(i)
		}
		es[i] = e
	}
	s := &transcript.Session{Agent: "llm", CWD: dir, Model: model, Created: es[0].Time, Updated: now, Entries: es}
	id, err := claudetx.WriteHosted(ctx, home, s, attachments)
	if err != nil {
		return "", fmt.Errorf("llm: write session: %w", err)
	}
	return id, nil
}

func lastAssistant(es []transcript.Entry) int {
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Role == transcript.RoleAssistant {
			return i
		}
	}
	return -1
}

func hasToolResult(e transcript.Entry) bool {
	return slices.ContainsFunc(e.Content, func(b transcript.Block) bool { return b.Kind == transcript.BlockToolResult })
}

// userBlocks is the content of user entries as one prompt.
func userBlocks(es []transcript.Entry) []claudecode.ContentBlock {
	var out []claudecode.ContentBlock
	for _, e := range es {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				out = append(out, claudecode.ContentBlock{Type: "text", Text: b.Text})
			case transcript.BlockImage:
				if len(b.Data) > 0 {
					out = append(out, claudecode.ImageBlock(b.MediaType, base64.StdEncoding.EncodeToString(b.Data)))
				} else if b.URI != "" {
					out = append(out, claudecode.ContentBlock{Type: "text", Text: "[image: " + b.URI + "]"})
				}
			case transcript.BlockFile:
				name := b.Name
				if name == "" {
					name = b.URI
				}
				out = append(out, claudecode.ContentBlock{Type: "text", Text: "[file: " + name + "]"})
			}
		}
	}
	if len(out) == 0 {
		out = []claudecode.ContentBlock{{Type: "text", Text: " "}}
	}
	return out
}

// signature fingerprints the settings a live process was launched with; a
// call with other settings restarts.
func signature(req Request) string {
	b, _ := json.Marshal([]any{req.System, req.Tools, req.ToolChoice == "none", req.Model, req.Effort, req.MaxTokens})
	return string(b)
}

// fingerprint is what must match for an entry the caller sends back to be
// the one the agent holds: who said it, its text and its tool call ids.
// Reasoning, argument formatting and media are left out, since a caller may
// store them differently from how they came.
func fingerprint(e transcript.Entry) string {
	role := e.Role
	if role == transcript.RoleTool {
		role = transcript.RoleUser
	}
	var text strings.Builder
	var ids []string
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			text.WriteString(b.Text)
		case transcript.BlockToolUse:
			ids = append(ids, "u:"+b.ToolID)
		case transcript.BlockToolResult:
			ids = append(ids, "r:"+b.ToolID)
			text.WriteString(b.Text)
		}
	}
	slices.Sort(ids)
	return string(role) + "\x00" + strings.TrimSpace(text.String()) + "\x00" + strings.Join(ids, ",")
}

// heldCall is a tool call parked at can_use_tool.
type heldCall struct {
	input  json.RawMessage
	answer chan claudecode.PermissionResult
}

// claudeConv is one conversation's running CLI and what it holds.
type claudeConv struct {
	proc    *claudecode.Process
	dir     string
	home    string
	session string // the Claude Code session it resumed or started
	server  *toolServer
	idle    *time.Timer

	// sig, history, pending and ended describe what the agent holds after
	// the last call: the settings, a fingerprint per entry of the
	// conversation including the last response, the tool calls that
	// response made, and whether it ended the turn.
	sig     string
	history []string
	pending []string
	ended   bool

	mu      sync.Mutex
	turn    *claudeTurn
	held    map[string]*heldCall
	results map[string]toolResult

	closeOnce sync.Once
}

// continues reports whether req extends what the agent holds, and returns
// the entries it adds.
func (c *claudeConv) continues(req Request) ([]transcript.Entry, bool) {
	if signature(req) != c.sig || len(req.Messages) <= len(c.history) {
		return nil, false
	}
	for i, fp := range c.history {
		if fingerprint(req.Messages[i]) != fp {
			return nil, false
		}
	}
	rest := req.Messages[len(c.history):]
	if len(c.pending) > 0 {
		// Only the results of the calls the agent holds, every one of them.
		got := map[string]bool{}
		for _, e := range rest {
			if e.Role != transcript.RoleTool && e.Role != transcript.RoleUser {
				return nil, false
			}
			for _, b := range e.Content {
				switch b.Kind {
				case transcript.BlockToolResult:
					got[b.ToolID] = true
				case transcript.BlockImage, transcript.BlockFile:
					if b.ToolID == "" {
						return nil, false
					}
				default:
					return nil, false
				}
			}
		}
		if len(got) != len(c.pending) {
			return nil, false
		}
		for _, id := range c.pending {
			if !got[id] {
				return nil, false
			}
		}
		return rest, true
	}
	if !c.ended {
		return nil, false
	}
	for _, e := range rest {
		if e.Role != transcript.RoleUser || hasToolResult(e) {
			return nil, false
		}
	}
	return rest, true
}

// next serves a call that continues the conversation with rest.
func (c *claudeConv) next(ctx context.Context, req Request, rest []transcript.Entry, onDelta func(Delta)) (*Response, error) {
	c.begin(onDelta)
	if len(c.pending) == 0 {
		if err := c.proc.SendUserBlocks(userBlocks(rest)); err != nil {
			return nil, fmt.Errorf("llm: %w", err)
		}
		return c.wait(ctx)
	}
	results := toolResults(rest)
	c.mu.Lock()
	for id, r := range results {
		c.results[id] = r
	}
	var allow []*heldCall
	for _, id := range c.pending {
		if h := c.held[id]; h != nil {
			delete(c.held, id)
			allow = append(allow, h)
		}
	}
	c.mu.Unlock()
	// A call not yet asked about is allowed when it is: onPermission finds
	// its result waiting.
	for _, h := range allow {
		h.answer <- claudecode.AllowResult(h.input)
	}
	return c.wait(ctx)
}

// toolResults gathers each tool_result with the media returned with it.
func toolResults(es []transcript.Entry) map[string]toolResult {
	out := map[string]toolResult{}
	for _, e := range es {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolResult:
				r := out[b.ToolID]
				if b.Text != "" {
					r.Content = append(r.Content, mcpContent{Type: "text", Text: b.Text})
				}
				r.IsError = b.Status == transcript.StatusError
				out[b.ToolID] = r
			case transcript.BlockImage:
				if b.ToolID != "" && len(b.Data) > 0 {
					r := out[b.ToolID]
					r.Content = append(r.Content, mcpContent{Type: "image", Data: base64.StdEncoding.EncodeToString(b.Data), MimeType: b.MediaType})
					out[b.ToolID] = r
				}
			}
		}
	}
	for id, r := range out {
		if len(r.Content) == 0 {
			r.Content = []mcpContent{{Type: "text", Text: ""}}
			out[id] = r
		}
	}
	return out
}

// answered records what the agent holds after resp.
func (c *claudeConv) answered(req Request, resp *Response) {
	c.history = c.history[:0]
	for _, e := range req.Messages {
		c.history = append(c.history, fingerprint(e))
	}
	c.history = append(c.history, fingerprint(transcript.Entry{Role: transcript.RoleAssistant, Content: resp.Content}))
	c.pending = nil
	for _, b := range resp.ToolCalls() {
		c.pending = append(c.pending, b.ToolID)
	}
	// After a stop other than these two the agent may have gone on, so it
	// no longer holds what the caller does.
	c.ended = resp.StopReason == "end_turn"
	if resp.StopReason != "tool_use" {
		c.pending = nil
	}
	c.mu.Lock()
	clear(c.results)
	c.mu.Unlock()
}

func (c *claudeConv) onPermission(ctx context.Context, _ string, r claudecode.PermissionRequest) (claudecode.PermissionResult, error) {
	c.mu.Lock()
	if _, ok := c.results[r.ToolUseID]; ok {
		c.mu.Unlock()
		return claudecode.AllowResult(r.Input), nil
	}
	h := &heldCall{input: r.Input, answer: make(chan claudecode.PermissionResult, 1)}
	c.held[r.ToolUseID] = h
	c.mu.Unlock()
	select {
	case res := <-h.answer:
		return res, nil
	case <-ctx.Done():
		return claudecode.PermissionResult{}, ctx.Err()
	}
}

// toolCall answers tools/call with the result the caller sent. A call is
// only allowed once its result is here, so a miss is a call the caller did
// not send a result for.
func (c *claudeConv) toolCall(_ context.Context, id, name string, _ json.RawMessage) (toolResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.results[id]; ok {
		return r, nil
	}
	return toolResult{}, fmt.Errorf("no result was given for %s call %s", name, id)
}

// begin starts collecting the next response.
func (c *claudeConv) begin(onDelta func(Delta)) {
	c.mu.Lock()
	c.turn = &claudeTurn{onDelta: onDelta, done: make(chan struct{}), blocks: map[int]*turnBlock{}}
	c.mu.Unlock()
}

// wait returns the response being collected.
func (c *claudeConv) wait(ctx context.Context) (*Response, error) {
	c.mu.Lock()
	t := c.turn
	c.mu.Unlock()
	select {
	case <-t.done:
		return t.resp, t.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.proc.Done():
		select {
		case <-t.done:
			return t.resp, t.err
		default:
		}
		c.proc.FlushStderr()
		err := c.proc.ExitErr()
		if err == nil {
			err = errors.New("exited")
		}
		return nil, fmt.Errorf("llm: claude ended before answering: %w", err)
	}
}

func (c *claudeConv) close() {
	c.closeOnce.Do(func() {
		if c.idle != nil {
			c.idle.Stop()
		}
		if c.proc != nil {
			_ = c.proc.Kill()
			<-c.proc.Done()
		}
		c.removeFiles()
	})
}

// removeFiles deletes the session this process kept, and its project and
// working directory once nothing else is in them: a later process of the
// same conversation uses the same ones, and may already be running.
func (c *claudeConv) removeFiles() {
	project := claudetx.Codec.Layout.Dir(c.home, c.dir)
	if c.session != "" {
		_ = os.Remove(filepath.Join(project, c.session+".jsonl"))
		_ = os.RemoveAll(filepath.Join(project, c.session))
	}
	_ = os.Remove(project)
	_ = os.Remove(c.dir)
}

// claudeTurn collects one response from the stream.
type claudeTurn struct {
	onDelta func(Delta)
	done    chan struct{}
	resp    *Response
	err     error

	msgID    string
	model    string
	blocks   map[int]*turnBlock
	order    []int
	tools    int
	stop     string
	usage    claudecode.Usage
	started  time.Time
	firstTok time.Time
	stopped  bool // message_stop seen; waiting for the result to end the turn
	finished bool
}

type turnBlock struct {
	kind      transcript.BlockKind
	text      strings.Builder
	signature string
	toolID    string
	toolName  string
	toolIndex int
}

func (c *claudeConv) onMessage(m claudecode.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.turn
	if t == nil || t.finished {
		return
	}
	switch m.Type {
	case claudecode.TypeStreamEvent:
		var ev claudecode.StreamEvent
		if m.Decode(&ev) != nil || ev.ParentToolUseID != nil {
			return
		}
		t.stream(ev.Event)
		if t.stopped && t.stop != "end_turn" {
			// A tool_use turn goes on to the tools, which hold. Any other
			// stop is final here; what Claude Code does after it (a
			// max_tokens recovery) is not part of this response.
			t.finish(nil)
		}
	case claudecode.TypeAssistant:
		var a claudecode.AssistantMessage
		if m.Decode(&a) == nil && a.ParentToolUseID == nil && a.Error != "" && t.msgID == "" {
			t.finish(fmt.Errorf("llm: claude: %s", a.Error))
		}
	case claudecode.TypeResult:
		var r claudecode.ResultMessage
		_ = m.Decode(&r)
		if t.stopped {
			t.finish(nil)
			return
		}
		msg := r.Result
		if len(r.Errors) > 0 {
			msg = strings.Join(r.Errors, "; ")
		}
		if msg == "" {
			msg = r.Subtype
		}
		t.finish(fmt.Errorf("llm: claude ended the turn without a response: %s", msg))
	}
}

func (t *claudeTurn) stream(ev claudecode.StreamAPI) {
	switch ev.Type {
	case "message_start":
		if ev.Message == nil || t.stopped {
			return
		}
		if t.msgID != "" && t.msgID != ev.Message.ID {
			// A retried request: start the response over.
			t.blocks, t.order, t.tools, t.stop, t.stopped = map[int]*turnBlock{}, nil, 0, "", false
		}
		t.msgID, t.model, t.started = ev.Message.ID, ev.Message.Model, time.Now()
		if ev.Message.Usage != nil {
			t.usage = *ev.Message.Usage
		}
	case "content_block_start":
		cb := ev.ContentBlock
		if cb == nil {
			return
		}
		b := &turnBlock{}
		switch cb.Type {
		case "text":
			b.kind = transcript.BlockText
		case "thinking":
			b.kind = transcript.BlockReasoning
			b.signature = cb.Signature
		case "tool_use":
			b.kind = transcript.BlockToolUse
			b.toolID, b.toolName, b.toolIndex = cb.ID, cb.Name, t.tools
			t.tools++
			t.onDelta(Delta{Kind: DeltaToolCall, Index: ev.Index, ToolIndex: b.toolIndex, ToolID: b.toolID, ToolName: b.toolName})
		default:
			return // redacted thinking, server tools: not ours to return
		}
		t.blocks[ev.Index] = b
		t.order = append(t.order, ev.Index)
	case "content_block_delta":
		b := t.blocks[ev.Index]
		if b == nil {
			return
		}
		if t.firstTok.IsZero() {
			t.firstTok = time.Now()
		}
		switch ev.Delta.Type {
		case "text_delta":
			b.text.WriteString(ev.Delta.Text)
			t.onDelta(Delta{Kind: DeltaText, Index: ev.Index, Text: ev.Delta.Text})
		case "thinking_delta":
			b.text.WriteString(ev.Delta.Thinking)
			t.onDelta(Delta{Kind: DeltaReasoning, Index: ev.Index, Text: ev.Delta.Thinking})
		case "signature_delta":
			b.signature += ev.Delta.Signature
		case "input_json_delta":
			b.text.WriteString(ev.Delta.PartialJSON)
			t.onDelta(Delta{Kind: DeltaToolCall, Index: ev.Index, ToolIndex: b.toolIndex, Text: ev.Delta.PartialJSON})
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			t.stop = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			t.usage.OutputTokens = ev.Usage.OutputTokens
		}
	case "message_stop":
		if t.msgID != "" {
			t.stopped = true
		}
	}
}

// finish ends the turn with the response collected, or with err.
func (t *claudeTurn) finish(err error) {
	if t.finished {
		return
	}
	t.finished = true
	defer close(t.done)
	if err != nil {
		t.err = err
		return
	}
	resp := &Response{StopReason: t.stop, Model: t.model}
	for _, i := range t.order {
		b := t.blocks[i]
		switch b.kind {
		case transcript.BlockToolUse:
			in := json.RawMessage(b.text.String())
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			resp.Content = append(resp.Content, transcript.Block{Kind: b.kind, ToolID: b.toolID, Name: b.toolName, Input: in})
		case transcript.BlockReasoning:
			resp.Content = append(resp.Content, transcript.Block{Kind: b.kind, Text: b.text.String(), Signature: b.signature})
		default:
			resp.Content = append(resp.Content, transcript.Block{Kind: b.kind, Text: b.text.String()})
		}
	}
	u := t.usage
	resp.Usage = ap.LLMUsage{
		StopReason:       t.stop,
		PromptTokens:     u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		CachedTokens:     u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		CompletionTokens: u.OutputTokens,
	}
	resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	if !t.started.IsZero() && !t.firstTok.IsZero() {
		resp.Usage.TimeToFirstToken = t.firstTok.Sub(t.started).Seconds()
		if d := time.Since(t.firstTok).Seconds(); d > 0 {
			resp.Usage.TokensPerSecond = float64(u.OutputTokens) / d
		}
	}
	t.resp = resp
}
