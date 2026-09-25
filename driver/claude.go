package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/claudecode"
	"github.com/inference-sh/agentprotocol/harness"
)

// KindClaude names the native Claude Code backend: the claude binary driven
// over its stream-json control protocol, with no SDK or adapter in between.
const KindClaude = string(harness.DriverClaudeCode)

// ClaudeBackend runs Claude Code as a local process and speaks the protocol
// the Claude Agent SDK speaks to it.
//
// The user's own login is used: the child inherits the environment, and
// selecting another account means pointing CLAUDE_CONFIG_DIR at a profile the
// user logged into, which is what Env is for. Nothing here reads, writes or
// passes a credential.
//
// Tool permissions always reach the caller. The backend launches the CLI with
// --permission-prompt-tool stdio, so every tool the user's settings do not
// already allow raises an approval-required event, and it refuses to launch
// in bypassPermissions mode.
type ClaudeBackend struct {
	// Command is the claude binary. Empty means "claude" on PATH.
	Command string

	// Env is the child environment. Nil inherits the parent's.
	Env []string

	// PermissionMode is passed as --permission-mode when set: default,
	// acceptEdits, plan or dontAsk. Empty leaves the user's configured mode.
	// bypassPermissions is refused.
	PermissionMode string

	// ExtraArgs are appended to the CLI's arguments. Arguments that skip
	// permission checks are refused.
	ExtraArgs []string

	// OnDiagnostic receives protocol-level observations that are not
	// failures: the session ID the CLI settled on, a request subtype this
	// client does not implement, a malformed line, how the CLI is
	// authenticated. Nil discards them.
	OnDiagnostic func(string)

	// Stderr receives the CLI's stderr as it is written. Nil keeps only a
	// short tail. The tail is kept either way and quoted in the error when
	// Open fails and when claude exits mid-turn.
	Stderr io.Writer

	// InitTimeout bounds the initialize handshake. Zero means 60 seconds.
	InitTimeout time.Duration

	// ShutdownGrace is how long Close lets the CLI finish writing its
	// transcript after input ends before it is killed. Zero means
	// claudecode.DefaultShutdownGrace.
	ShutdownGrace time.Duration
}

// Kind implements Backend.
func (b *ClaudeBackend) Kind() string { return KindClaude }

// Capabilities implements Backend. Each was checked against claude 2.1.281:
//
//   - Steer: a prompt sent while a turn runs is folded into that turn; the
//     CLI hands it to the model with the next tool result and the turn ends
//     with one result.
//   - Approvals: can_use_tool arrives for every tool call the user's rules do
//     not already settle.
//   - Interrupt: the interrupt control request ends the turn and the next
//     prompt runs normally in the same process.
//   - Resume: --resume with the session ID after the process has exited
//     brings the conversation back; the next request carries the earlier
//     turns.
//   - Tools: an HTTP MCP server named in Metadata (mcp_url, mcp_name,
//     mcp_token) is passed with --mcp-config and its tools are offered to the
//     model, behind the same approvals.
func (b *ClaudeBackend) Capabilities() Capabilities {
	return Capabilities{
		Steer:     true,
		Approvals: true,
		Interrupt: true,
		Resume:    true,
		Tools:     true,
	}
}

func (b *ClaudeBackend) diagnose(msg string) {
	if b.OnDiagnostic != nil {
		b.OnDiagnostic(msg)
	}
}

// Open launches the CLI, completes the handshake and returns a live session.
//
// A new session gets its ID up front with --session-id, because the CLI does
// not name a session until the first prompt; ID is therefore usable as soon
// as Open returns. A resumed session starts with the ID it was resumed by.
// Either way the ID is replaced by whatever system/init later reports, should
// the two ever differ.
func (b *ClaudeBackend) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if b.PermissionMode == claudecode.PermissionModeBypassPermissions {
		return nil, errors.New("driver: ClaudeBackend does not run in bypassPermissions mode")
	}

	s := &claudeSession{
		backend: b,
		runID:   cfg.RunID,
		chatID:  cfg.ChatID,
		model:   cfg.Model,
		pending: make(map[string]*claudePending),
		tools:   make(map[string]string),
		denied:  make(map[string]bool),
		pump:    newEventPump(),
	}

	opts := claudecode.Options{
		Command:        b.Command,
		Dir:            cfg.WorkDir,
		Env:            b.Env,
		Model:          cfg.Model,
		PermissionMode: b.PermissionMode,
		ExtraArgs:      b.ExtraArgs,
		MCPConfig:      claudeMCPConfig(cfg.Metadata),
	}
	if cfg.ResumeSessionID != "" {
		opts.ResumeSessionID = cfg.ResumeSessionID
		s.id = cfg.ResumeSessionID
	} else {
		s.id = claudecode.NewSessionID()
		opts.SessionID = s.id
	}

	opts.Stderr, s.stderr = agentStderr(b.Stderr)

	proc, err := claudecode.Spawn(opts, claudecode.Handler{
		OnMessage:    s.onMessage,
		OnPermission: s.onPermission,
		OnUnhandled: func(subtype string, _ json.RawMessage) {
			b.diagnose("claude sent a control request this client does not implement: " + subtype)
		},
		OnMalformed: func(line []byte, err error) {
			b.diagnose(fmt.Sprintf("claude wrote a line that is not JSON (%v): %.200s", err, line))
		},
	})
	if err != nil {
		return nil, s.stderr.quoteErr(fmt.Errorf("driver: launch claude: %w", err))
	}
	s.proc = proc

	timeout := b.InitTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	init, err := proc.Initialize(ictx, claudecode.InitializeRequest{AppendSystemPrompt: cfg.Instructions})
	cancel()
	if err != nil {
		_ = proc.Kill()
		_ = proc.ExitErr()
		return nil, errors.New(s.stderr.quote(fmt.Sprintf("driver: initialize claude: %v", err)))
	}
	b.diagnose(fmt.Sprintf("claude session %s ready (pid %d, permission mode %s, auth token=%s api key=%s)",
		s.id, init.PID, init.CurrentPermissionMode, orNone(init.Account.TokenSource), orNone(init.Account.APIKeySource)))

	go s.pump.run()
	go s.watch()

	s.emit(ap.NewEvent(ap.AgentEventRunStarted, s.runID, s.chatID, ap.RunStartedPayload{}))
	return s, nil
}

// claudePending is a can_use_tool request parked until Resolve answers it.
type claudePending struct {
	req    claudecode.PermissionRequest
	answer chan claudecode.PermissionResult
}

type claudeSession struct {
	backend *ClaudeBackend
	proc    *claudecode.Process
	runID   string
	chatID  string
	pump    *eventPump
	stderr  *tailBuffer

	closeOnce sync.Once
	closeErr  error

	mu      sync.Mutex
	id      string
	model   string
	pending map[string]*claudePending
	tools   map[string]string // tool_use id -> tool name, for results
	denied  map[string]bool   // tool_use ids a person refused

	// Turn state. A turn opens on Prompt, or on output arriving while none is
	// open (a prompt the CLI queued rather than folded in), and closes on the
	// result line.
	turnActive   bool
	turnIndex    int
	toolCount    int
	hasOutput    bool
	streamedText bool
	interrupting bool
}

func (s *claudeSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *claudeSession) Events() <-chan ap.AgentEvent { return s.pump.out }

// Prompt sends user input and returns at once. The turn ends with
// turn.completed, or with an error event if it failed.
//
// A prompt sent while a turn is running steers that turn: the CLI folds it
// into the running turn, and no second turn.started is emitted.
func (s *claudeSession) Prompt(ctx context.Context, in Input) error {
	select {
	case <-s.proc.Done():
		return claudecode.ErrClosed
	default:
	}
	text := in.Text
	if len(in.Files) > 0 {
		var b strings.Builder
		b.WriteString(text)
		b.WriteString("\n\nAttached files:")
		for _, f := range in.Files {
			name := f.Filename
			if name == "" {
				name = f.URI
			}
			fmt.Fprintf(&b, "\n- %s (%s)", name, f.URI)
		}
		text = b.String()
	}

	s.mu.Lock()
	s.openTurnLocked()
	s.mu.Unlock()

	if err := s.proc.SendUser(text); err != nil {
		return fmt.Errorf("driver: send prompt to claude: %w", err)
	}
	return nil
}

// openTurnLocked starts a turn if none is running. Caller holds s.mu.
func (s *claudeSession) openTurnLocked() {
	if s.turnActive {
		return
	}
	s.turnActive = true
	s.turnIndex++
	s.toolCount = 0
	s.hasOutput = false
	s.streamedText = false
	s.emit(ap.NewEvent(ap.AgentEventTurnStarted, s.runID, s.chatID, ap.TurnStartedPayload{
		TurnIndex: s.turnIndex,
		Model:     s.model,
	}))
}

// Interrupt aborts the running turn. The CLI withdraws any permission request
// the turn was waiting on, ends the turn, and stays ready for the next
// prompt.
func (s *claudeSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	if s.turnActive {
		s.interrupting = true
	}
	s.mu.Unlock()
	return s.proc.Interrupt(ctx)
}

// Resolve answers a parked permission request. requestID is the
// ToolInvocationID of the approval-required event, which is Claude's
// tool_use id.
//
// Scope maps onto Claude's own permission rules:
//
//   - once: allow this call only.
//   - session: also add the rules Claude suggested, scoped to the session,
//     so matching calls stop asking until the process ends.
//   - always: add the suggested rules where Claude suggested them, normally
//     the project's .claude/settings.local.json.
//
// Only rule suggestions are applied. Claude also suggests switching the
// whole session to acceptEdits or widening the working directories, which
// grant more than the call being approved, so those are left out. When Claude
// suggests no rule the whole tool is allowed at that scope. When Claude says a
// persistent rule would be broader than the request, always is rounded down
// to session.
//
// Values, when given, are merged into the tool's input; that is how an
// AskUserQuestion prompt is answered.
func (s *claudeSession) Resolve(ctx context.Context, requestID string, res Resolution) error {
	s.mu.Lock()
	p, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
		if res.Decision != ap.InterruptResolutionAllow {
			s.denied[requestID] = true
		}
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("driver: no pending request %q", requestID)
	}

	var result claudecode.PermissionResult
	if res.Decision == ap.InterruptResolutionAllow {
		result = claudecode.AllowResult(mergeInput(p.req.Input, res.Values))
		scope := res.Scope
		if scope == ScopeAlways && p.req.SuppressAlwaysAllow {
			s.backend.diagnose(fmt.Sprintf("claude marked %s as too broad to always allow; allowing for the session instead", p.req.ToolName))
			scope = ScopeSession
		}
		result.UpdatedPermissions = permissionUpdates(p.req, scope)
	} else {
		result = claudecode.DenyResult(res.Reason)
	}

	p.answer <- result

	s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
		ToolInvocationID: requestID,
		ToolName:         p.req.ToolName,
		Decision:         string(res.Decision),
		Reason:           res.Reason,
	}))
	return nil
}

// Close ends input, lets the CLI write its transcript, and waits for it to
// exit. Events not yet read are discarded and the channel closes.
func (s *claudeSession) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.proc.Close(s.backend.ShutdownGrace)
		s.pump.stopNow()
	})
	return s.closeErr
}

// Kill implements Killer. The CLI gets no end of input and no grace, so the
// transcript holds only what it had already written. The session then ends
// as it does when claude crashes: a turn still open reports process_exited,
// and Events closes once the queued events are read. Close afterwards
// releases the rest.
func (s *claudeSession) Kill() error {
	if err := s.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("driver: kill claude: %w", err)
	}
	<-s.proc.Exited()
	return nil
}

var _ Killer = (*claudeSession)(nil)

// watch ends the event stream when the process does. A turn still open at
// that point failed, and says so.
func (s *claudeSession) watch() {
	<-s.proc.Exited()
	exitErr := s.proc.ExitErr()

	s.mu.Lock()
	open := s.turnActive
	s.turnActive = false
	s.mu.Unlock()

	if open {
		msg := "claude exited before the turn finished"
		if exitErr != nil {
			msg += ": " + exitErr.Error()
		}
		msg = s.stderr.quote(msg)
		s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{Message: msg, Code: "process_exited"}))
	}
	s.pump.end()
}

func (s *claudeSession) onMessage(m claudecode.Message) {
	switch m.Type {
	case claudecode.TypeSystem:
		s.onSystem(m)
	case claudecode.TypeStreamEvent:
		var ev claudecode.StreamEvent
		if m.Decode(&ev) == nil {
			s.onStream(ev)
		}
	case claudecode.TypeAssistant:
		var a claudecode.AssistantMessage
		if m.Decode(&a) == nil {
			s.onAssistant(a)
		}
	case claudecode.TypeUser:
		var u claudecode.UserMessage
		if m.Decode(&u) == nil {
			s.onUser(u)
		}
	case claudecode.TypeResult:
		var r claudecode.ResultMessage
		if m.Decode(&r) == nil {
			s.onResult(r)
		}
	}
}

func (s *claudeSession) onSystem(m claudecode.Message) {
	var sys claudecode.SystemMessage
	if m.Decode(&sys) != nil {
		return
	}
	switch sys.Subtype {
	case claudecode.SystemInit:
		s.mu.Lock()
		prev := s.id
		if sys.SessionID != "" {
			s.id = sys.SessionID
		}
		if sys.Model != "" {
			s.model = sys.Model
		}
		s.mu.Unlock()
		if sys.SessionID != "" && sys.SessionID != prev {
			s.backend.diagnose(fmt.Sprintf("claude reported session %s, not %s; using its ID", sys.SessionID, prev))
		}
		for _, srv := range sys.MCPServers {
			if srv.Status != "connected" {
				s.backend.diagnose(fmt.Sprintf("MCP server %s is %s", srv.Name, srv.Status))
			}
		}
	case claudecode.SystemCompactBoundary:
		if sys.CompactMeta != nil {
			s.emit(ap.NewEvent(ap.AgentEventContextCompacted, s.runID, s.chatID, ap.ContextCompactedPayload{
				BeforeTokens: sys.CompactMeta.PreTokens,
				AfterTokens:  sys.CompactMeta.PostTokens,
			}))
		}
	}
}

func (s *claudeSession) onStream(ev claudecode.StreamEvent) {
	if ev.ParentToolUseID != nil {
		// A subagent's narration belongs to the Task tool that spawned it,
		// not to this conversation's reply.
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Event.Type {
	case "message_start":
		s.openTurnLocked()
		s.streamedText = false
	case "content_block_delta":
		s.openTurnLocked()
		switch ev.Event.Delta.Type {
		case "text_delta":
			if ev.Event.Delta.Text == "" {
				return
			}
			s.streamedText = true
			s.hasOutput = true
			s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{
				Kind: ap.ContentDeltaText, Delta: ev.Event.Delta.Text,
			}))
		case "thinking_delta":
			if ev.Event.Delta.Thinking == "" {
				return
			}
			s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{
				Kind: ap.ContentDeltaReasoning, Delta: ev.Event.Delta.Thinking,
			}))
		}
	}
}

func (s *claudeSession) onAssistant(a claudecode.AssistantMessage) {
	if a.Error != "" {
		s.backend.diagnose(fmt.Sprintf("claude reported an assistant error: %s", a.Error))
	}
	if a.ParentToolUseID != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openTurnLocked()
	for _, block := range a.Message.Content {
		switch block.Type {
		case "text":
			// Streamed already when partial messages are on; the settled
			// copy is only emitted when no delta carried it.
			if block.Text != "" && !s.streamedText {
				s.hasOutput = true
				s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{
					Kind: ap.ContentDeltaText, Delta: block.Text,
				}))
			}
		case "tool_use", "server_tool_use", "mcp_tool_use":
			s.toolCount++
			s.tools[block.ID] = block.Name
			s.emit(ap.NewEvent(ap.AgentEventToolStarted, s.runID, s.chatID, ap.ToolStartedPayload{
				ToolInvocationID: block.ID,
				ToolName:         block.Name,
				DisplayName:      block.Name,
				Arguments:        claudeArguments(block.Input),
			}))
		}
	}
}

func (s *claudeSession) onUser(u claudecode.UserMessage) {
	if u.ParentToolUseID != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, block := range u.Message.Blocks() {
		if block.Type != "tool_result" {
			continue
		}
		status := ap.ToolInvocationStatusCompleted
		if block.IsError {
			status = ap.ToolInvocationStatusFailed
			if s.denied[block.ToolUseID] || s.interrupting {
				status = ap.ToolInvocationStatusCancelled
			}
		}
		delete(s.denied, block.ToolUseID)
		name := s.tools[block.ToolUseID]
		delete(s.tools, block.ToolUseID)
		s.emit(ap.NewEvent(ap.AgentEventToolCompleted, s.runID, s.chatID, ap.ToolCompletedPayload{
			ToolInvocationID: block.ToolUseID,
			ToolName:         name,
			Status:           status,
			Result:           block.ResultText(),
		}))
	}
}

func (s *claudeSession) onResult(r claudecode.ResultMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.turnActive {
		if r.NumTurns == 0 {
			// A result with no turn behind it, as a resumed session can
			// produce while it settles. Nothing ran.
			return
		}
		s.openTurnLocked()
	}
	interrupted := r.Interrupted() || s.interrupting
	s.turnActive = false
	s.interrupting = false

	// A request still parked when the turn ends was withdrawn with it.
	// onPermission notices that too, but on its own goroutine, which can lose
	// the race with this one and report the withdrawal after turn.completed,
	// when a caller has stopped reading the turn's events. Close them here,
	// on the reader, so they always precede the end of the turn.
	for id, p := range s.pending {
		delete(s.pending, id)
		s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
			ToolInvocationID: id,
			ToolName:         p.req.ToolName,
			Decision:         string(ap.InterruptResolutionDeny),
			Reason:           "withdrawn by claude",
		}))
	}

	u := r.Usage
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	s.emit(ap.NewEvent(ap.AgentEventUsageUpdated, s.runID, s.chatID, ap.UsageUpdatedPayload{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
		CostUSD:          r.TotalCostUSD,
	}))

	if !interrupted && (r.IsError || r.Subtype != claudecode.ResultSuccess) {
		s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{
			Message: resultError(r),
			Code:    r.Subtype,
		}))
		return
	}

	stop := r.StopReason
	if interrupted {
		stop = "interrupted"
	}
	s.emit(ap.NewEvent(ap.AgentEventTurnCompleted, s.runID, s.chatID, ap.TurnCompletedPayload{
		TurnIndex:  s.turnIndex,
		ToolCount:  s.toolCount,
		HasOutput:  s.hasOutput,
		StopReason: stop,
	}))
}

// resultError picks a message a person can read. Entries tagged
// [ede_diagnostic] are the CLI's internal telemetry, not an explanation.
func resultError(r claudecode.ResultMessage) string {
	if r.Result != "" {
		return r.Result
	}
	for _, e := range r.Errors {
		if !strings.HasPrefix(e, "[ede_diagnostic]") {
			return e
		}
	}
	if r.TerminalReason != "" {
		return "claude turn failed: " + r.TerminalReason
	}
	return "claude turn failed: " + r.Subtype
}

// onPermission raises can_use_tool as an approval-required event and parks
// until Resolve answers it or Claude withdraws the request.
func (s *claudeSession) onPermission(ctx context.Context, requestID string, req claudecode.PermissionRequest) (claudecode.PermissionResult, error) {
	id := req.ToolUseID
	p := &claudePending{req: req, answer: make(chan claudecode.PermissionResult, 1)}

	s.mu.Lock()
	if _, taken := s.pending[id]; id == "" || taken {
		// Without a distinct tool_use id, the control request's own ID is the
		// only thing a decision can be addressed to.
		id = requestID
	}
	s.pending[id] = p
	s.mu.Unlock()

	s.emit(ap.NewEvent(ap.AgentEventApprovalRequired, s.runID, s.chatID, ap.ApprovalRequiredPayload{
		ToolInvocationID: id,
		ToolName:         req.ToolName,
		Arguments:        claudeArguments(req.Input),
		Reason:           ap.InterruptReasonToolApproval,
	}))

	select {
	case res := <-p.answer:
		return res, nil
	case <-ctx.Done():
		// Claude withdrew the request (the turn was interrupted) or the
		// process is gone. Tell the caller, so nobody is left holding an
		// approval that can no longer be answered.
		s.mu.Lock()
		_, still := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if still {
			s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
				ToolInvocationID: id,
				ToolName:         req.ToolName,
				Decision:         string(ap.InterruptResolutionDeny),
				Reason:           "withdrawn by claude",
			}))
		}
		return claudecode.DenyResult("cancelled"), nil
	}
}

func (s *claudeSession) emit(ev ap.AgentEvent) { s.pump.push(ev) }

// permissionUpdates turns a scope into the rules sent back with an allow.
func permissionUpdates(req claudecode.PermissionRequest, scope Scope) []claudecode.PermissionUpdate {
	if scope != ScopeSession && scope != ScopeAlways {
		return nil
	}
	var out []claudecode.PermissionUpdate
	for _, sug := range req.Suggestions {
		if sug.Type != "addRules" || sug.Behavior != claudecode.BehaviorAllow || len(sug.Rules) == 0 {
			continue
		}
		u := sug
		u.Rules = append([]claudecode.PermissionRule(nil), sug.Rules...)
		if scope == ScopeSession {
			u.Destination = claudecode.DestinationSession
		}
		out = append(out, u)
	}
	if len(out) > 0 {
		return out
	}
	dest := claudecode.DestinationSession
	if scope == ScopeAlways {
		dest = claudecode.DestinationLocalSettings
	}
	return []claudecode.PermissionUpdate{{
		Type:        "addRules",
		Rules:       []claudecode.PermissionRule{{ToolName: req.ToolName}},
		Behavior:    claudecode.BehaviorAllow,
		Destination: dest,
	}}
}

// mergeInput overlays answer values on the tool input.
func mergeInput(input json.RawMessage, values map[string]any) json.RawMessage {
	if len(values) == 0 {
		return input
	}
	m := map[string]any{}
	_ = json.Unmarshal(input, &m)
	for k, v := range values {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		return input
	}
	return out
}

func claudeArguments(raw json.RawMessage) ap.StringEncodedMap {
	if len(raw) == 0 {
		return nil
	}
	var m ap.StringEncodedMap
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// claudeMCPConfig reads the same MCP metadata keys the ACP backend does.
func claudeMCPConfig(md map[string]string) string {
	servers := mcpServersFrom(md)
	if len(servers) == 0 {
		return ""
	}
	return claudecode.MCPConfigHTTP(servers[0].Name, servers[0].URL, servers[0].Headers)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
