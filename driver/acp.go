package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/acp"
)

// KindACP names the ACP backend.
const KindACP = "acp"

// ACPBackend runs agents that speak the Agent Client Protocol as local
// processes: Claude Code, Codex, Gemini CLI, Cursor, Grok.
//
// The credential never passes through here. The agent is already logged in as
// the user; selecting which account it uses is a matter of pointing its own
// config-directory variable at a profile, which is what Env is for.
type ACPBackend struct {
	// Command and Args launch the agent in ACP mode.
	Command string
	Args    []string

	// Env is the child environment, and is where an account profile is
	// selected. Nil inherits the parent's.
	Env []string

	// ClientName identifies us to the agent during the handshake.
	ClientName string

	// ClientVersion is reported alongside ClientName.
	ClientVersion string

	// OnDiagnostic receives protocol-level problems that did not fail the
	// session: an error the agent could not attribute to any request, or a
	// method this client does not implement.
	//
	// These deliberately do not become events. An event of type error would
	// mark the run failed, and these are not failures; kiro rejects
	// session/close and finishes its work perfectly well. But discarding them
	// is how kiro's behaviour stayed invisible for months, so they are offered
	// here instead, and the caller decides whether that means a log line, a
	// counter, or something a person actually sees.
	//
	// Nil discards, which is a choice rather than an accident. Recorded and
	// seen are not the same thing.
	OnDiagnostic func(string)

	// EmitReplay publishes the history a resumed session replays as ordinary
	// events, rather than discarding it.
	//
	// Off by default: replayed updates describe work that already happened,
	// and a run that announces them looks like it is doing that work now.
	// Turn it on when the caller is building the conversation from scratch
	// and genuinely wants the history.
	EmitReplay bool

	// ReplayIdleGap is how long a resuming agent's replay must stay quiet
	// before the session counts as rebuilt. Zero means acp's default.
	ReplayIdleGap time.Duration

	// LoadTimeout bounds a whole resume. Zero means acp's default.
	LoadTimeout time.Duration
}

// Kind implements Backend.
func (b *ACPBackend) Kind() string { return KindACP }

// diagnose reports a non-fatal protocol problem, if anyone is listening.
func (b *ACPBackend) diagnose(msg string) {
	if b.OnDiagnostic != nil {
		b.OnDiagnostic(msg)
	}
}

// Capabilities implements Backend.
//
// Resume is true in the sense that the call is worth making: of twelve agents,
// eleven accept session/load into a new process and one refuses outright. What
// has not been established is whether the accepting eleven actually restore
// the conversation or merely open a fresh session without complaining, so a
// caller should offer resuming and must not assume the context survived.
// Tools is true because ACP carries MCP server declarations, which is how a
// caller injects its own.
func (b *ACPBackend) Capabilities() Capabilities {
	return Capabilities{
		Steer:     false,
		Approvals: true,
		Interrupt: true,
		Resume:    true,
		Tools:     true,
	}
}

// Open launches the agent and opens a session.
func (b *ACPBackend) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if b.Command == "" {
		return nil, errors.New("driver: ACPBackend has no command")
	}

	s := &acpSession{
		runID:      cfg.RunID,
		chatID:     cfg.ChatID,
		events:     make(chan ap.AgentEvent, 64),
		pending:    make(map[string]*pendingPermission),
		emitReplay: b.EmitReplay,
	}

	name := b.ClientName
	if name == "" {
		name = "agentprotocol"
	}

	proc, err := acp.Spawn(ctx, acp.ProcessConfig{
		Command: b.Command,
		Args:    b.Args,
		Dir:     cfg.WorkDir,
		Env:     b.Env,
	}, acp.ClientInfo{Name: name, Version: b.ClientVersion}, acp.Handler{
		OnUpdate:     s.onUpdate,
		OnPermission: s.onPermission,
		OnPeerError: func(e *acp.Error) {
			b.diagnose(fmt.Sprintf("agent reported an unattributed error: %s (code %d)", e.Message, e.Code))
		},
		OnUnhandled: func(method string, _ json.RawMessage) {
			b.diagnose("agent called a method this client does not implement: " + method)
		},
	})
	if err != nil {
		return nil, err
	}
	s.proc = proc

	servers := mcpServersFrom(cfg.Metadata)

	if cfg.ResumeSessionID != "" {
		proc.ReplayIdleGap = b.ReplayIdleGap
		proc.LoadTimeout = b.LoadTimeout
		res, err := proc.LoadSession(ctx, cfg.ResumeSessionID, cfg.WorkDir, servers)
		if err != nil {
			_ = proc.Kill()
			return nil, fmt.Errorf("driver: resume acp session: %w", err)
		}
		s.id = cfg.ResumeSessionID
		b.diagnose(fmt.Sprintf("resumed session %s in %s: %d update(s) replayed, %d of them conversation, %s; agent %s",
			cfg.ResumeSessionID, res.Elapsed.Round(time.Millisecond),
			res.Replayed, res.Conversation, restoredWord(res.RestoredConversation),
			answeredWord(res.Answered)))
	} else {
		sid, err := proc.NewSession(ctx, cfg.WorkDir, servers)
		if err != nil {
			_ = proc.Kill()
			return nil, fmt.Errorf("driver: open acp session: %w", err)
		}
		s.id = sid
	}

	s.emit(ap.NewEvent(ap.AgentEventRunStarted, s.runID, s.chatID, ap.RunStartedPayload{}))
	return s, nil
}

// pendingPermission is one unanswered question from the agent. The agent's
// request is parked on a channel until a human decides, which is what makes an
// approval on our side able to answer a prompt on someone's laptop.
type pendingPermission struct {
	req    acp.PermissionRequest
	answer chan acp.PermissionResponse
	once   sync.Once
}

type acpSession struct {
	proc   *acp.Process
	id     string
	runID  string
	chatID string

	events     chan ap.AgentEvent
	closeOnce  sync.Once
	emitReplay bool

	mu      sync.Mutex
	pending map[string]*pendingPermission
}

func (s *acpSession) ID() string { return s.id }

func (s *acpSession) Events() <-chan ap.AgentEvent { return s.events }

func (s *acpSession) Prompt(ctx context.Context, in Input) error {
	s.emit(ap.NewEvent(ap.AgentEventTurnStarted, s.runID, s.chatID, ap.TurnStartedPayload{}))

	go func() {
		_, err := s.proc.Prompt(context.WithoutCancel(ctx), in.Text)
		if err != nil {
			s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{
				Message: err.Error(),
			}))
			return
		}
		s.emit(ap.NewEvent(ap.AgentEventTurnCompleted, s.runID, s.chatID, ap.TurnCompletedPayload{}))
	}()
	return nil
}

func (s *acpSession) Interrupt(ctx context.Context) error {
	return s.proc.Cancel(ctx)
}

// Resolve answers a parked permission request.
//
// It reports an error for an unknown request rather than ignoring it: a caller
// that has lost track of which requests are outstanding would otherwise wait
// for a turn that can never finish.
func (s *acpSession) Resolve(ctx context.Context, requestID string, res Resolution) error {
	s.mu.Lock()
	p, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
	}
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("driver: no pending request %q", requestID)
	}

	response := acp.ResponseForResolution(p.req, res.Decision)
	if res.Decision == ap.InterruptResolutionAllow && res.Scope != ScopeOnce {
		if id, found := p.req.PickOption(acp.OptionKindAllowAlways, acp.OptionKindAllowOnce); found {
			response = acp.Selected(id)
		}
	}

	p.once.Do(func() { p.answer <- response })

	s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
		ToolInvocationID: requestID,
		ToolName:         toolNameOf(p.req),
	}))
	return nil
}

func (s *acpSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.failPending()
		err = s.proc.Wait()
		close(s.events)
	})
	return err
}

// failPending releases every parked request so the agent is not left waiting
// on a decision that will never arrive.
func (s *acpSession) failPending() {
	s.mu.Lock()
	pending := s.pending
	s.pending = map[string]*pendingPermission{}
	s.mu.Unlock()
	for _, p := range pending {
		p.once.Do(func() { p.answer <- acp.Cancelled() })
	}
}

func (s *acpSession) onUpdate(n acp.UpdateNotification) {
	if n.Replay && !s.emitReplay {
		// History from a resumed session, not progress. Emitting it would
		// announce last week's tool calls as if they were running now, which
		// is wrong in every display and wrong again in anything that counts
		// tool use. The updates are still delivered to a caller that asks for
		// them by setting EmitReplay.
		return
	}
	if ev, ok := acp.EventForUpdate(n.Update, s.runID, s.chatID); ok {
		s.emit(ev)
	}
}

// restoredWord reports whether the replay proved anything. An agent that
// accepts the load and sends only its usual session openers has given us no
// evidence it found the session at all, and that is worth saying out loud
// rather than reporting a resume that may be a blank slate.
func restoredWord(restored bool) string {
	if restored {
		return "history restored"
	}
	return "NO user turn replayed, so the history is unproven"
}

func answeredWord(answered bool) string {
	if answered {
		return "answered the call"
	}
	return "replayed and went quiet without answering"
}

// onPermission raises the agent's question as an approval-required event and
// blocks until Resolve answers it, the context ends, or the session closes.
//
// Blocking here is the point. The ACP request stays open on the agent's side
// for as long as this call takes, which is what lets a human on a different
// machine decide whether a tool runs.
func (s *acpSession) onPermission(ctx context.Context, req acp.PermissionRequest) (acp.PermissionResponse, error) {
	payload := acp.ApprovalForPermission(req)
	id := payload.ToolInvocationID
	if id == "" {
		// Some agents omit a tool call ID. Without one there is nothing to
		// address a decision to, so fall back to the session: one unanswered
		// question at a time is better than a question nobody can answer.
		id = s.id
		payload.ToolInvocationID = id
	}

	p := &pendingPermission{req: req, answer: make(chan acp.PermissionResponse, 1)}
	s.mu.Lock()
	s.pending[id] = p
	s.mu.Unlock()

	s.emit(ap.NewEvent(ap.AgentEventApprovalRequired, s.runID, s.chatID, payload))

	select {
	case res := <-p.answer:
		return res, nil
	case <-ctx.Done():
		s.dropPending(id)
		return acp.Cancelled(), nil
	case <-s.proc.Done():
		s.dropPending(id)
		return acp.Cancelled(), nil
	}
}

func (s *acpSession) dropPending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// emit publishes an event, dropping it if the consumer has stopped reading.
// A slow reader must not wedge the agent's transport, and a session that is
// closing has no one left to tell.
func (s *acpSession) emit(ev ap.AgentEvent) {
	defer func() {
		// A send on a channel closed by Close races with an in-flight update
		// from the read loop. Recovering keeps a late event from taking the
		// process down.
		_ = recover()
	}()
	select {
	case s.events <- ev:
	default:
	}
}

func toolNameOf(r acp.PermissionRequest) string {
	if r.ToolCall == nil {
		return ""
	}
	return r.ToolCall.Title
}

// mcpServersFrom reads MCP server declarations out of session metadata. This
// is how a caller injects its own tools into an agent it does not own: the
// agent connects to the server and the tools appear alongside its native ones.
func mcpServersFrom(md map[string]string) []acp.MCPServer {
	url := md["mcp_url"]
	if url == "" {
		return nil
	}
	server := acp.MCPServer{Name: md["mcp_name"], URL: url}
	if server.Name == "" {
		server.Name = "host"
	}
	if token := md["mcp_token"]; token != "" {
		server.Headers = map[string]string{"Authorization": "Bearer " + token}
	}
	return []acp.MCPServer{server}
}
