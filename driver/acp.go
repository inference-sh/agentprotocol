package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
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
//
// The client offers the agent no filesystem: fs/read_text_file and
// fs/write_text_file are not advertised, so the agent reads and writes the
// working directory itself. This is deliberate. Serving those calls would put
// this process between the agent and the user's files for no gain when both
// run on the same machine.
//
// An agent that exits on its own ends the session the way Kill does: a turn
// still running reports an error with code process_exited, and Events closes.
type ACPBackend struct {
	// Command and Args launch the agent in ACP mode.
	Command string
	Args    []string

	// Env is the child environment, and is where an account profile is
	// selected. Nil inherits the parent's.
	Env []string

	// Stderr receives the agent's stderr as it is written. Nil keeps only a
	// short tail. The tail is kept either way and quoted in the error when
	// Open fails, when the agent exits mid-turn and when a prompt gets no
	// answer (FirstEventTimeout): it is usually the only clue to why.
	Stderr io.Writer

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

	// FirstEventTimeout bounds how long a prompt may wait for the agent's
	// first sign of work: a message, thought, tool call or plan update, a
	// permission request, or the prompt's own response. When it passes
	// with none of them, the turn fails with an error event of code
	// no_response (the message quotes the agent's stderr tail) and
	// session/cancel is sent; the session stays open for the caller to
	// Close or Kill. An agent waiting on a backend that will never answer
	// otherwise leaves the turn open for acp's whole-prompt timeout.
	//
	// Only the time to the first sign is bounded; after it the turn runs as
	// long as the agent works. Session bookkeeping updates (available
	// commands, mode, usage, config options) and echoes of the user's own
	// prompt do not count: agents send those without the model having
	// started. Zero means DefaultFirstEventTimeout; negative disables the
	// bound.
	//
	// initialize and session/new are bounded separately, by acp's
	// DefaultCallTimeout, and a resume by LoadTimeout.
	FirstEventTimeout time.Duration
}

// DefaultFirstEventTimeout is ACPBackend.FirstEventTimeout's default.
//
// Measured in harness-test's container against its mock model (2026-09-25,
// the 13 ACP agents at their latest release, two runs, one prompt per run
// and a second for droid, gemini, grok and qwen), the first sign of work
// came 0.01s to 2.07s after session/prompt (cursor slowest, then opencode at
// 1.32s); session/new took at most 2.53s (hermes). A
// real model adds its own time to first token, and an agent may start MCP
// servers or index the repository before it sends anything, so the default
// is some sixty times the slowest measurement: it exists to turn a turn that
// would never end into an error, not to police slow ones.
const DefaultFirstEventTimeout = 2 * time.Minute

// CodeNoResponse is the error code of a turn the agent never started
// answering within ACPBackend.FirstEventTimeout.
const CodeNoResponse = "no_response"

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
// Resume is true in the sense that the call is worth making: every agent
// measured accepts session/load into a new process after a clean close. It is
// not a guarantee. Loads have been seen to fail for the same agent depending
// on how the previous process ended and whether it had been reaped, and a
// load that succeeds may still have restored nothing — the diagnostic says
// which. Offer resuming; do not assume the context survived.
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
		firstEvent: b.FirstEventTimeout,
		diagnose:   b.diagnose,
		waiting:    make(map[chan struct{}]struct{}),
	}
	if s.firstEvent == 0 {
		s.firstEvent = DefaultFirstEventTimeout
	}

	name := b.ClientName
	if name == "" {
		name = "agentprotocol"
	}

	var stderr io.Writer
	stderr, s.stderr = agentStderr(b.Stderr)

	proc, err := acp.Spawn(ctx, acp.ProcessConfig{
		Command: b.Command,
		Args:    b.Args,
		Dir:     cfg.WorkDir,
		Env:     b.Env,
		Stderr:  stderr,
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
		return nil, s.stderr.quoteErr(err)
	}
	s.proc = proc

	servers := mcpServersFrom(cfg.Metadata)

	if cfg.ResumeSessionID != "" {
		proc.ReplayIdleGap = b.ReplayIdleGap
		proc.LoadTimeout = b.LoadTimeout
		res, err := proc.LoadSession(ctx, cfg.ResumeSessionID, cfg.WorkDir, servers)
		if err != nil {
			_ = proc.Kill()
			return nil, s.stderr.quoteErr(fmt.Errorf("driver: resume acp session: %w", err))
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
			return nil, s.stderr.quoteErr(fmt.Errorf("driver: open acp session: %w", err))
		}
		s.id = sid
	}

	s.emit(ap.NewEvent(ap.AgentEventRunStarted, s.runID, s.chatID, ap.RunStartedPayload{}))
	go s.watch()
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
	evMu       sync.Mutex
	evClosed   bool
	closeOnce  sync.Once
	emitReplay bool
	// stderr is the last of the agent's stderr, for error messages.
	stderr *tailBuffer

	// firstEvent is the resolved FirstEventTimeout; <= 0 means unbounded.
	firstEvent time.Duration
	diagnose   func(string)

	mu      sync.Mutex
	pending map[string]*pendingPermission
	// turns counts prompts still waiting on the agent's answer.
	turns int
	// waiting holds a channel per prompt that has not yet seen a sign of
	// work; alive closes and removes them all. A prompt still in it when
	// its answer arrives ended without doing anything visible.
	waiting map[chan struct{}]struct{}
}

func (s *acpSession) ID() string { return s.id }

func (s *acpSession) Events() <-chan ap.AgentEvent { return s.events }

func (s *acpSession) Prompt(ctx context.Context, in Input) error {
	select {
	case <-s.proc.Done():
		return errors.New("driver: acp session has ended")
	default:
	}
	life := make(chan struct{})
	s.mu.Lock()
	s.turns++
	s.waiting[life] = struct{}{}
	s.mu.Unlock()
	s.emit(ap.NewEvent(ap.AgentEventTurnStarted, s.runID, s.chatID, ap.TurnStartedPayload{}))

	// settle lets exactly one of the prompt's answer and the first-event
	// timeout report the turn.
	var settled atomic.Bool
	settle := func() bool { return settled.CompareAndSwap(false, true) }

	go func() {
		res, err := s.proc.Prompt(context.WithoutCancel(ctx), in.Text)
		silent := s.release(life)
		if err != nil {
			select {
			case <-s.proc.Done():
				// The stream is gone: the agent died or the session is
				// closing. Whoever ends the session reports the turn, so
				// it is left open for them to see.
				return
			default:
			}
		}
		if !settle() {
			return // already reported as no_response
		}
		s.mu.Lock()
		s.turns--
		s.mu.Unlock()
		if err != nil {
			s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{
				Message: err.Error(),
			}))
			return
		}
		if silent {
			// hermes before 0.18.0 ends a turn whose model call failed this
			// way, with the provider's error on stderr only.
			s.diagnose(s.stderr.quote(fmt.Sprintf("turn ended (%s) with no message, thought, tool call or plan from the agent", stopReason(res))))
		}
		s.emit(ap.NewEvent(ap.AgentEventTurnCompleted, s.runID, s.chatID, ap.TurnCompletedPayload{}))
	}()
	if s.firstEvent > 0 {
		go s.awaitFirstEvent(life, settle)
	}
	return nil
}

// awaitFirstEvent fails the turn when the agent shows no sign of work within
// firstEvent of the prompt.
func (s *acpSession) awaitFirstEvent(life chan struct{}, settle func() bool) {
	timer := time.NewTimer(s.firstEvent)
	defer timer.Stop()
	select {
	case <-life:
		return
	case <-s.proc.Done():
		return
	case <-timer.C:
	}
	s.mu.Lock()
	_, still := s.waiting[life]
	delete(s.waiting, life)
	s.mu.Unlock()
	if !still || !settle() {
		return
	}
	s.mu.Lock()
	s.turns--
	s.mu.Unlock()
	_ = s.proc.Cancel(context.Background())
	msg := fmt.Sprintf("agent sent nothing for %s after session/prompt: no message, tool call, permission request or response; session/cancel sent", s.firstEvent)
	s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{
		Message: s.stderr.quote(msg), Code: CodeNoResponse,
	}))
}

// release ends the wait of the prompt that owns life, whose answer has
// arrived, and reports whether it had seen no sign of work until then.
func (s *acpSession) release(life chan struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.waiting[life]
	if ok {
		close(life)
		delete(s.waiting, life)
	}
	return ok
}

// stopReason reads session/prompt's stopReason, "no stopReason" when the
// result has none.
func stopReason(res json.RawMessage) string {
	var r struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(res, &r) != nil || r.StopReason == "" {
		return "no stopReason"
	}
	return "stopReason " + r.StopReason
}

// alive records a sign of work from the agent, releasing every prompt still
// waiting for its first one.
func (s *acpSession) alive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.waiting {
		close(ch)
		delete(s.waiting, ch)
	}
}

// isWork reports whether an update shows the agent working on a prompt, as
// opposed to describing the session (commands, mode, usage, config) or
// echoing what the user said, which agents send without the model having
// started.
func isWork(u acp.SessionUpdate) bool {
	if acp.IsUserTurn(u) {
		return false
	}
	return acp.IsConversation(u) || u.Kind == acp.UpdateKindPlan || acp.IsTurnDone(u)
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
		s.closeEvents()
	})
	return err
}

// Kill implements Killer. The agent gets no session/close and no grace; any
// request parked on a person is cancelled. The session then ends as it does
// when the agent crashes: a turn still running reports process_exited, and
// Events closes. After Close it does nothing.
func (s *acpSession) Kill() error {
	var err error
	s.closeOnce.Do(func() {
		s.failPending()
		err = s.proc.Kill()
		s.reportExit()
		s.closeEvents()
	})
	return err
}

// watch ends the session when the agent's stream ends without Close or Kill
// having asked for it: the agent crashed, was killed from outside, or quit.
// Without this a dead agent would look alive, with Events open forever.
func (s *acpSession) watch() {
	<-s.proc.Done()
	s.closeOnce.Do(func() {
		s.failPending()
		_ = s.proc.Kill() // reaps it; ExitState is then its own exit
		s.reportExit()
		s.closeEvents()
	})
}

// reportExit tells the caller a turn died with the agent. An idle agent that
// exits is reported only by Events closing, as with the other backends.
func (s *acpSession) reportExit() {
	s.mu.Lock()
	open := s.turns > 0
	s.turns = 0
	s.mu.Unlock()
	if !open {
		return
	}
	msg := "agent exited before the turn finished"
	if st := s.proc.ExitState(); st != nil {
		msg += ": " + st.String()
	}
	msg = s.stderr.quote(msg)
	s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{Message: msg, Code: "process_exited"}))
}

var _ Killer = (*acpSession)(nil)

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
	if !n.Replay && isWork(n.Update) {
		s.alive()
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
	if !req.DuringLoad {
		s.alive()
	}
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
	// The lock orders a late event from the read loop or a prompt goroutine
	// against the close in Close or Kill.
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if s.evClosed {
		return
	}
	select {
	case s.events <- ev:
	default:
	}
}

func (s *acpSession) closeEvents() {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if !s.evClosed {
		s.evClosed = true
		close(s.events)
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
