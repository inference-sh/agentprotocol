package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
	"github.com/inference-sh/agentprotocol/harness"
	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/all"
)

// DefaultSettle is how long Harness waits, after the first tool call of a
// response, for more calls the model made with it.
const DefaultSettle = 500 * time.Millisecond

// Harness is any harness with a harness.Direct row as a Model, driven over
// its session driver (ACP, codex app-server, pi rpc).
//
// The caller's tools are served over loopback HTTP and given to the agent as
// a session MCP server. A tool call is held at the server's tools/call,
// whatever the agent's own approval rules: the response ends once the model's
// calls have arrived (Settle after the first), and a continued call answers
// them with the caller's results. An agent that runs a response's calls one
// after another sends the next only after the first is answered; that call
// is then the next response, with no text of its own.
//
// What differs from Claude:
//
//   - The model is given the caller's tools under the agent's MCP naming
//     (Direct.ToolName), not their own names; responses carry the tools' own
//     names.
//   - Reasoning comes back as text, without a signature, and is not sent
//     back.
//   - A restart whose written session ends in tool results has to prompt
//     the agent to go on (Direct.ContinuePrompt): the model gets that text
//     after the results.
//   - Usage is what the agent reports, which may be nothing.
type Harness struct {
	// Harness is the registry row. Its Direct says how to launch it.
	Harness harness.Harness

	// Backend builds the session backend for one conversation, before the
	// direct arguments are added. Nil is driver.ForHarness(Harness, Env).
	// harness-test passes its mock-pointed backends here.
	Backend func() (driver.Backend, error)

	// Env is the child environment for the default Backend. Nil inherits.
	Env []string

	// Home is where the agent keeps sessions, for writing the session a
	// restart resumes. Empty is the user's home.
	Home string

	// WorkRoot holds a working directory per conversation. Empty is
	// os.TempDir().
	WorkRoot string

	// Idle is as Claude.Idle.
	Idle time.Duration

	// Settle is how long after a response's first tool call more may
	// arrive. Zero is DefaultSettle.
	Settle time.Duration

	mu     sync.Mutex
	live   map[string]*harnessConv
	closed bool
}

var _ Model = (*Harness)(nil)

// Capabilities implements Model.
func (h *Harness) Capabilities() Capabilities {
	return Capabilities{Tools: true, ExactContinue: true}
}

// Close implements Model.
func (h *Harness) Close() error {
	h.mu.Lock()
	live := h.live
	h.live, h.closed = nil, true
	h.mu.Unlock()
	for _, c := range live {
		c.close()
	}
	return nil
}

// Generate implements Model.
func (h *Harness) Generate(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error) {
	if h.Harness.Direct == nil {
		return nil, fmt.Errorf("%w: %s has no direct mode", ErrUnsupported, h.Harness.Name)
	}
	if err := checkRequest(req); err != nil {
		return nil, err
	}
	if req.Effort != "" || req.MaxTokens > 0 {
		return nil, fmt.Errorf("%w: effort and max tokens on %s", ErrUnsupported, h.Harness.Name)
	}
	if onDelta == nil {
		onDelta = func(Delta) {}
	}
	if c := h.take(req.Key); c != nil {
		if rest, ok := c.continues(req); ok {
			resp, err := c.next(ctx, rest, onDelta)
			return h.settle(req, c, resp, err)
		}
		c.close()
	}
	c, err := h.start(ctx, req, onDelta)
	if err != nil {
		return nil, err
	}
	resp, err := c.wait(ctx)
	if resp != nil {
		resp.Restarted = true
	}
	return h.settle(req, c, resp, err)
}

func (h *Harness) settle(req Request, c *harnessConv, resp *Response, err error) (*Response, error) {
	if err != nil {
		c.close()
		return nil, err
	}
	c.answered(req, resp)
	idle := h.Idle
	if idle == 0 {
		idle = DefaultIdle
	}
	if req.Key == "" || idle < 0 {
		c.close()
		return resp, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		c.close()
		return resp, nil
	}
	if h.live == nil {
		h.live = map[string]*harnessConv{}
	}
	if old := h.live[req.Key]; old != nil {
		go old.close()
	}
	h.live[req.Key] = c
	c.idle = time.AfterFunc(idle, func() {
		h.mu.Lock()
		if h.live[req.Key] == c {
			delete(h.live, req.Key)
		}
		h.mu.Unlock()
		c.close()
	})
	return resp, nil
}

func (h *Harness) take(key string) *harnessConv {
	if key == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.live[key]
	if c == nil {
		return nil
	}
	delete(h.live, key)
	if c.idle != nil && !c.idle.Stop() {
		return nil
	}
	if c.gone() {
		return nil
	}
	return c
}

// directValues fills the placeholders of a Direct row.
type directValues struct {
	SystemPrompt     string
	SystemPromptFile string
	Dir              string
	WorkDir          string
	MCPURL           string
	MCPToken         string
	Server           string
	Tool             string
}

func expand(s string, v directValues) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New("direct").Option("missingkey=error").Parse(s)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// visibleName is the name the model is given for a caller's tool.
func visibleName(d *harness.Direct, tool string) string {
	if d.ToolName == "" {
		return tool
	}
	name, err := expand(d.ToolName, directValues{Server: claudeServer, Tool: tool})
	if err != nil {
		return tool
	}
	return name
}

// WithLaunch adds args and env to a session backend's launch.
func WithLaunch(b driver.Backend, args []string, env []string) error {
	add := func(cur []string) []string {
		if cur == nil {
			cur = os.Environ()
		}
		return append(slices.Clip(cur), env...)
	}
	switch b := b.(type) {
	case *driver.ACPBackend:
		b.Args, b.Env = append(slices.Clip(b.Args), args...), add(b.Env)
	case *driver.PiBackend:
		b.Args, b.Env = append(slices.Clip(b.Args), args...), add(b.Env)
	case *driver.CodexBackend:
		b.Args, b.Env = append(slices.Clip(b.Args), args...), add(b.Env)
	default:
		return fmt.Errorf("llm: no direct launch for a %T", b)
	}
	return nil
}

func (h *Harness) start(ctx context.Context, req Request, onDelta func(Delta)) (*harnessConv, error) {
	d := h.Harness.Direct
	root := h.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	dir, err := os.MkdirTemp(root, "llm-"+h.Harness.Name+"-")
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	home := h.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return nil, fmt.Errorf("llm: %w", err)
		}
	}
	settle := h.Settle
	if settle == 0 {
		settle = DefaultSettle
	}
	c := &harnessConv{dir: dir, settle: settle, names: map[string]string{}}
	fail := func(err error) (*harnessConv, error) {
		c.close()
		return nil, err
	}

	// The conversation's own files live beside the working directory, not
	// in it, so the agent does not take them for part of the project.
	own := dir + ".direct"
	if err := os.Mkdir(own, 0o700); err != nil {
		return fail(fmt.Errorf("llm: %w", err))
	}
	c.files = append(c.files, own)
	promptFile := filepath.Join(own, "system.md")
	if err := os.WriteFile(promptFile, []byte(req.System), 0o600); err != nil {
		return fail(fmt.Errorf("llm: %w", err))
	}

	tools := req.Tools
	if req.ToolChoice == "none" {
		tools = nil
	}
	for _, t := range tools {
		c.names[t.Name] = visibleName(d, t.Name)
	}
	c.server = &toolServer{name: claudeServer, tools: tools, call: c.toolCall}
	cfg := driver.SessionConfig{WorkDir: dir, Model: req.Model}
	vals := directValues{SystemPrompt: req.System, SystemPromptFile: promptFile, Dir: own, WorkDir: dir}
	if len(tools) > 0 {
		hs, err := serveHTTP(c.server)
		if err != nil {
			return fail(err)
		}
		c.http = hs
		vals.MCPURL, vals.MCPToken = hs.url, hs.token
		if !d.MCPInArgs {
			cfg.Metadata = map[string]string{"mcp_url": hs.url, "mcp_name": claudeServer, "mcp_token": hs.token}
		}
	}
	for name, content := range d.Files {
		v, err := expand(content, vals)
		if err != nil {
			return fail(fmt.Errorf("llm: %s direct file %s: %w", h.Harness.Name, name, err))
		}
		path, err := expand(name, vals)
		if err != nil {
			return fail(fmt.Errorf("llm: %s direct file %s: %w", h.Harness.Name, name, err))
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(own, path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fail(fmt.Errorf("llm: %w", err))
		}
		if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
			return fail(fmt.Errorf("llm: %w", err))
		}
	}
	var args, env []string
	for _, a := range d.Args {
		v, err := expand(a, vals)
		if err != nil {
			return fail(fmt.Errorf("llm: %s direct args: %w", h.Harness.Name, err))
		}
		args = append(args, v)
	}
	keys := make([]string, 0, len(d.Env))
	for k := range d.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := expand(d.Env[k], vals)
		if err != nil {
			return fail(fmt.Errorf("llm: %s direct env: %w", h.Harness.Name, err))
		}
		env = append(env, k+"="+v)
	}
	if c.http != nil {
		// The tool server is on loopback; an agent behind a proxy would
		// otherwise send it there (omp's fetch honours HTTP_PROXY).
		env = append(env, noProxy("NO_PROXY", h.Env), noProxy("no_proxy", h.Env))
	}

	cut := lastAssistant(req.Messages) + 1
	trailing := req.Messages[cut:]
	seed, send := req.Messages[:cut], trailing
	if slices.ContainsFunc(trailing, hasToolResult) {
		prompt := d.ContinuePrompt
		if prompt == "" {
			prompt = "Continue."
		}
		seed, send = req.Messages, []transcript.Entry{userText(prompt)}
	}
	if len(seed) > 0 {
		id, err := h.writeSession(ctx, home, dir, req.Model, c.named(seed))
		if err != nil {
			return fail(err)
		}
		cfg.ResumeSessionID = id
	}

	build := h.Backend
	if build == nil {
		build = func() (driver.Backend, error) { return driver.ForHarness(h.Harness, h.Env) }
	}
	b, err := build()
	if err != nil {
		return fail(fmt.Errorf("llm: %w", err))
	}
	if err := WithLaunch(b, args, env); err != nil {
		return fail(err)
	}
	if debug {
		fmt.Fprintf(os.Stderr, "llm %s: direct args %q env %q mcp %v\n", h.Harness.Name, args, env, cfg.Metadata != nil)
	}
	c.begin(onDelta)
	sess, err := b.Open(ctx, cfg)
	if err != nil {
		return fail(fmt.Errorf("llm: %s: %w", h.Harness.Name, err))
	}
	c.sess = sess
	c.done = make(chan struct{})
	go c.pump()
	if err := sess.Prompt(ctx, driver.Input{Text: promptText(send)}); err != nil {
		return fail(fmt.Errorf("llm: %s: %w", h.Harness.Name, err))
	}
	c.sig = signature(req)
	return c, nil
}

// writeSession writes entries as the harness's own session for dir.
func (h *Harness) writeSession(ctx context.Context, home, dir, model string, entries []transcript.Entry) (string, error) {
	st, ok, err := all.Open(h.Harness.Name, home)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("%w: %s has no session codec to restart from", ErrUnsupported, h.Harness.Name)
	}
	// A minute back: gemini will not load a session in the minute it was
	// created (conformance README), and nothing else minds.
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	es := make([]transcript.Entry, len(entries))
	for i, e := range entries {
		e.Time = at.Add(time.Duration(i) * time.Millisecond)
		if e.ID == "" {
			e.ID = "m" + strconv.Itoa(i)
		}
		es[i] = e
	}
	id, err := st.Write(ctx, &transcript.Session{Agent: "llm", CWD: dir, Model: model, Created: es[0].Time, Updated: es[len(es)-1].Time, Entries: es})
	if err != nil {
		return "", fmt.Errorf("llm: write %s session: %w", h.Harness.Name, err)
	}
	return id, nil
}

// promptText is user entries as one prompt's text. Images and files are
// named, as the session driver names any file it is not sent.
func promptText(es []transcript.Entry) string {
	var parts []string
	for _, b := range userBlocks(es) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// heldTool is a tools/call the server holds for the caller's result.
type heldTool struct {
	id     string
	answer chan toolResult
}

// harnessConv is one conversation's running agent session.
type harnessConv struct {
	sess   driver.Session
	server *toolServer
	http   *httpToolServer
	dir    string
	files  []string
	settle time.Duration
	idle   *time.Timer
	names  map[string]string // tool name to the name the model is given

	sig     string
	history []string
	pending []string
	ended   bool

	mu     sync.Mutex
	turn   *harnessTurn
	held   map[string]*heldTool // by our call id
	early  []heldToolCall       // calls that came between responses
	seq    int
	closed bool
	done   chan struct{}

	closeOnce sync.Once
}

// heldToolCall is a call that arrived before a response was being
// collected: an agent running a response's calls one at a time.
type heldToolCall struct {
	block transcript.Block
}

// named is entries with the caller's tool calls under the names the model
// is given for them.
func (c *harnessConv) named(es []transcript.Entry) []transcript.Entry {
	out := make([]transcript.Entry, len(es))
	for i, e := range es {
		e.Content = slices.Clone(e.Content)
		for k, b := range e.Content {
			if n, ok := c.names[b.Name]; ok && (b.Kind == transcript.BlockToolUse || b.Kind == transcript.BlockToolResult) {
				e.Content[k].Name = n
			}
			if b.Kind == transcript.BlockReasoning {
				e.Content[k].Signature = "" // not this agent's to check
			}
		}
		out[i] = e
	}
	return out
}

func (c *harnessConv) gone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// continues is claudeConv.continues for this conversation.
func (c *harnessConv) continues(req Request) ([]transcript.Entry, bool) {
	cc := claudeConv{sig: c.sig, history: c.history, pending: c.pending, ended: c.ended}
	return cc.continues(req)
}

func (c *harnessConv) answered(req Request, resp *Response) {
	cc := claudeConv{results: map[string]toolResult{}}
	cc.answered(req, resp)
	c.sig, c.history, c.pending, c.ended = signature(req), cc.history, cc.pending, cc.ended
}

func (c *harnessConv) next(ctx context.Context, rest []transcript.Entry, onDelta func(Delta)) (*Response, error) {
	c.begin(onDelta)
	if len(c.pending) == 0 {
		if err := c.sess.Prompt(ctx, driver.Input{Text: promptText(rest)}); err != nil {
			return nil, fmt.Errorf("llm: %w", err)
		}
		return c.wait(ctx)
	}
	results := toolResults(rest)
	c.mu.Lock()
	var answer []func()
	for _, id := range c.pending {
		h := c.held[id]
		if h == nil {
			c.mu.Unlock()
			return nil, errors.New("llm: a held tool call was given up by the agent")
		}
		delete(c.held, id)
		r := results[id]
		answer = append(answer, func() { h.answer <- r })
	}
	c.mu.Unlock()
	for _, a := range answer {
		a()
	}
	return c.wait(ctx)
}

// toolCall holds one tools/call until the caller's result comes.
func (c *harnessConv) toolCall(ctx context.Context, _ string, name string, args json.RawMessage) (toolResult, error) {
	c.mu.Lock()
	c.seq++
	h := &heldTool{id: "call_" + strconv.Itoa(c.seq), answer: make(chan toolResult, 1)}
	if c.held == nil {
		c.held = map[string]*heldTool{}
	}
	c.held[h.id] = h
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	call := transcript.Block{Kind: transcript.BlockToolUse, ToolID: h.id, Name: name, Input: args}
	if t := c.turn; t != nil && !t.finished {
		t.call(call, c.settle)
	} else {
		c.early = append(c.early, heldToolCall{call})
	}
	c.mu.Unlock()
	select {
	case r := <-h.answer:
		return r, nil
	case <-ctx.Done():
		// The agent gave up waiting: it holds something other than what
		// the caller does now, so the next call must restart.
		c.mu.Lock()
		delete(c.held, h.id)
		c.mu.Unlock()
		c.close()
		return toolResult{}, ctx.Err()
	}
}

func (c *harnessConv) begin(onDelta func(Delta)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &harnessTurn{onDelta: onDelta, done: make(chan struct{}), mu: &c.mu}
	c.turn = t
	for _, e := range c.early {
		t.call(e.block, c.settle)
	}
	c.early = nil
}

func (c *harnessConv) wait(ctx context.Context) (*Response, error) {
	c.mu.Lock()
	t := c.turn
	done := c.done
	c.mu.Unlock()
	select {
	case <-t.done:
		return t.resp, t.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		select {
		case <-t.done:
			return t.resp, t.err
		default:
		}
		return nil, errors.New("llm: the agent session ended before answering")
	}
}

// pump reads the session's events into the turn being collected.
func (c *harnessConv) pump() {
	defer close(c.done)
	for ev := range c.sess.Events() {
		switch ev.Type {
		case ap.AgentEventApprovalRequired:
			// The only tools are the caller's, and they hold at the
			// server; anything else the agent asks to run is refused.
			p, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](ev, ev.Type)
			res := driver.Deny("only the caller's tools may run")
			if c.ours(p.ToolName) {
				res = driver.Allow()
			}
			_ = c.sess.Resolve(context.Background(), p.ToolInvocationID, res)
			continue
		}
		c.mu.Lock()
		if t := c.turn; t != nil && !t.finished {
			t.event(ev)
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	if t := c.turn; t != nil && !t.finished {
		t.finish(errors.New("llm: the agent session ended before answering"))
	}
	c.mu.Unlock()
}

// ours reports whether an approval names one of the caller's tools. Agents
// title a request in their own way, so the name is looked for in it.
func (c *harnessConv) ours(title string) bool {
	for tool, visible := range c.names {
		if strings.Contains(title, visible) || strings.Contains(title, tool) {
			return true
		}
	}
	return false
}

func (c *harnessConv) close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		held := c.held
		c.held = nil
		c.mu.Unlock()
		if c.idle != nil {
			c.idle.Stop()
		}
		for _, h := range held {
			select {
			case h.answer <- toolResult{Content: []mcpContent{{Type: "text", Text: "cancelled"}}, IsError: true}:
			default:
			}
		}
		if c.sess != nil {
			if k, ok := c.sess.(driver.Killer); ok {
				_ = k.Kill()
			}
			_ = c.sess.Close()
		}
		if c.http != nil {
			c.http.close()
		}
		_ = os.RemoveAll(c.dir)
		for _, f := range c.files {
			_ = os.RemoveAll(f)
		}
	})
}

// harnessTurn collects one response from session events and tool calls.
type harnessTurn struct {
	onDelta func(Delta)
	done    chan struct{}
	mu      *sync.Mutex // the conversation's, held by every caller
	resp    *Response
	err     error

	content  []transcript.Block
	tools    int
	usage    ap.LLMUsage
	timer    *time.Timer
	finished bool
}

func (t *harnessTurn) event(ev ap.AgentEvent) {
	switch ev.Type {
	case ap.AgentEventContentDelta:
		p, ok := ap.PayloadAs[ap.ContentDeltaPayload](ev, ev.Type)
		if !ok || p.SubagentID != "" || p.Delta == "" {
			return
		}
		kind, dk := transcript.BlockText, DeltaText
		if p.Kind == ap.ContentDeltaReasoning {
			kind, dk = transcript.BlockReasoning, DeltaReasoning
		}
		n := len(t.content)
		if n == 0 || t.content[n-1].Kind != kind {
			t.content = append(t.content, transcript.Block{Kind: kind})
			n++
		}
		t.content[n-1].Text += p.Delta
		t.onDelta(Delta{Kind: dk, Index: n - 1, Text: p.Delta})
	case ap.AgentEventUsageUpdated:
		if p, ok := ap.PayloadAs[ap.UsageUpdatedPayload](ev, ev.Type); ok {
			t.usage.PromptTokens = p.PromptTokens
			t.usage.CachedTokens = p.CachedInputTokens
			t.usage.CacheWriteTokens = p.CacheCreationTokens
			t.usage.CompletionTokens = p.CompletionTokens
			t.usage.TotalTokens = p.TotalTokens
			t.usage.ReasoningTokens = p.ReasoningTokens
		}
	case ap.AgentEventTurnCompleted:
		if t.tools == 0 {
			t.finish(nil)
		}
	case ap.AgentEventError:
		p, _ := ap.PayloadAs[ap.ErrorPayload](ev, ev.Type)
		t.finish(fmt.Errorf("llm: agent error: %s", p.Message))
	}
}

// call adds a tool call to the response, which ends Settle after the first.
func (t *harnessTurn) call(b transcript.Block, settle time.Duration) {
	t.content = append(t.content, b)
	t.onDelta(Delta{Kind: DeltaToolCall, Index: len(t.content) - 1, ToolIndex: t.tools, ToolID: b.ToolID, ToolName: b.Name})
	t.onDelta(Delta{Kind: DeltaToolCall, Index: len(t.content) - 1, ToolIndex: t.tools, Text: string(b.Input)})
	t.tools++
	if t.timer == nil {
		t.timer = time.AfterFunc(settle, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.finish(nil)
		})
	}
}

func (t *harnessTurn) finish(err error) {
	if t.finished {
		return
	}
	t.finished = true
	if t.timer != nil {
		t.timer.Stop()
	}
	defer close(t.done)
	if err != nil {
		t.err = err
		return
	}
	stop := "end_turn"
	if t.tools > 0 {
		stop = "tool_use"
	}
	t.usage.StopReason = stop
	t.resp = &Response{Content: t.content, StopReason: stop, Usage: t.usage}
}

// noProxy is the variable key set to its value in env (or this process's
// environment) with loopback added.
func noProxy(key string, env []string) string {
	cur, ok := os.LookupEnv(key)
	for _, kv := range env {
		if v, found := strings.CutPrefix(kv, key+"="); found {
			cur, ok = v, true
		}
	}
	if ok && cur != "" {
		return key + "=" + cur + ",127.0.0.1,localhost"
	}
	return key + "=127.0.0.1,localhost"
}
