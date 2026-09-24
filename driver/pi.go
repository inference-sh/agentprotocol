package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/pirpc"
)

// KindPi names the native pi backend: the pi coding agent driven over its own
// RPC mode (pi --mode rpc), with no adapter in between.
const KindPi = "pi"

// PiBackend runs pi as a local process and speaks its RPC protocol.
//
// The user's own install and login are used: the child inherits the
// environment, so pi reads its own settings, auth.json and provider keys.
// PI_CODING_AGENT_DIR in Env selects another profile. Nothing here reads,
// writes or passes a credential, and nothing is fetched at session time.
//
// pi does not ask before running a tool; see Capabilities.
type PiBackend struct {
	// Command is the pi binary. Empty means "pi" on PATH.
	Command string

	// Args are passed after the RPC flags, for what SessionConfig has no
	// field for, such as --provider.
	Args []string

	// Env is the child environment. Nil inherits the parent's.
	Env []string

	// OnDiagnostic receives protocol-level observations that are not
	// failures: the session pi opened, retries, extension errors and
	// notifications, a line that is not JSON. Nil discards them.
	OnDiagnostic func(string)

	// Stderr receives pi's stderr. Nil keeps only a short tail, which is
	// quoted in the error when a launch fails.
	Stderr io.Writer

	// InitTimeout bounds startup: pi loads its extensions, skills and model
	// catalogue before it answers the first command. Zero means 60 seconds.
	InitTimeout time.Duration

	// ShutdownGrace is how long Close lets pi dispose of its session after
	// input ends before it is killed. Zero means pirpc.DefaultShutdownGrace.
	ShutdownGrace time.Duration
}

// Kind implements Backend.
func (b *PiBackend) Kind() string { return KindPi }

// Capabilities implements Backend. Each was checked against pi 0.87.1:
//
//   - Steer: a prompt sent while a run is active is queued as a steering
//     message (prompt with streamingBehavior "steer"). pi delivers it before
//     its next model call in the same run, and the turn ends with one
//     agent_settled.
//   - Approvals: false. pi runs its tools without asking anyone; its RPC
//     protocol has no permission request (docs/security.md: "it does not ask
//     for approval before every tool call"). The only questions pi's RPC
//     mode asks are an extension's dialogs (ctx.ui.select, confirm, input,
//     editor), which a user's own extension can use to gate tools. Those are
//     raised as approval-required events and answered through Resolve, so a
//     gate the user installed reaches the caller; pi never raises one on its
//     own.
//   - Interrupt: the abort command ends the run (the last assistant message
//     stops as "aborted"), pi settles, and the next prompt runs in the same
//     process.
//   - Resume: pi files sessions per working directory; --session-id with
//     the id reopens one in a new process, and the next request carries the
//     earlier turns.
//   - Tools: false. pi takes no tools from its client; tools come from its
//     built-ins and the user's extensions, and it has no MCP.
func (b *PiBackend) Capabilities() Capabilities {
	return Capabilities{
		Steer:     true,
		Approvals: false,
		Interrupt: true,
		Resume:    true,
		Tools:     false,
	}
}

func (b *PiBackend) diagnose(msg string) {
	if b.OnDiagnostic != nil {
		b.OnDiagnostic(msg)
	}
}

// Open launches pi, waits for it to answer, and returns a live session.
//
// The ID is pi's own session id, read with get_state before Open returns. A
// resume passes --session-id; pi would silently start a fresh session under
// that id if it had none, so Open checks that the session's file exists and
// fails otherwise. Resuming needs the WorkDir the session was started in.
func (b *PiBackend) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	s := &piSession{
		backend: b,
		runID:   cfg.RunID,
		chatID:  cfg.ChatID,
		dialogs: make(map[string]*piDialog),
		tools:   make(map[string]string),
		pump:    newEventPump(),
	}

	opts := pirpc.Options{
		Command:            b.Command,
		Dir:                cfg.WorkDir,
		Env:                b.Env,
		Model:              cfg.Model,
		SessionID:          cfg.ResumeSessionID,
		AppendSystemPrompt: cfg.Instructions,
		ExtraArgs:          b.Args,
	}
	tail := &tailBuffer{max: 4 << 10}
	if b.Stderr != nil {
		opts.Stderr = io.MultiWriter(b.Stderr, tail)
	} else {
		opts.Stderr = tail
	}

	proc, err := pirpc.Spawn(opts, pirpc.Handler{
		OnRecord: s.onRecord,
		OnMalformed: func(line []byte, err error) {
			b.diagnose(fmt.Sprintf("pi wrote a line that is not JSON (%v): %.200s", err, line))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("driver: launch pi: %w", err)
	}
	s.proc = proc

	fail := func(format string, args ...any) (Session, error) {
		_ = proc.Kill()
		_ = proc.ExitErr()
		msg := fmt.Sprintf(format, args...)
		if t := strings.TrimSpace(tail.String()); t != "" {
			msg += ": " + t
		}
		return nil, errors.New(msg)
	}

	timeout := b.InitTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	st, err := proc.GetState(ictx)
	cancel()
	if err != nil {
		return fail("driver: start pi: %v", err)
	}
	if cfg.ResumeSessionID != "" {
		if st.SessionID != cfg.ResumeSessionID {
			return fail("driver: pi opened session %q, not %q", st.SessionID, cfg.ResumeSessionID)
		}
		if st.SessionFile == "" {
			return fail("driver: pi keeps no session file here, so session %s cannot be resumed", cfg.ResumeSessionID)
		}
		if _, err := os.Stat(st.SessionFile); err != nil {
			return fail("driver: pi has no session %s for %s", cfg.ResumeSessionID, cfg.WorkDir)
		}
	}
	s.id = st.SessionID
	if st.Model != nil {
		s.model = st.Model.Provider + "/" + st.Model.ID
	}
	b.diagnose(fmt.Sprintf("pi session %s ready (pid %d, model %s, %d message(s), file %s)",
		s.id, proc.Pid(), orNone(s.model), st.MessageCount, orNone(st.SessionFile)))

	go s.pump.run()
	go s.watch()

	s.emit(ap.NewEvent(ap.AgentEventRunStarted, s.runID, s.chatID, ap.RunStartedPayload{}))
	return s, nil
}

// piDialog is an extension dialog parked until Resolve answers it.
type piDialog struct {
	req   pirpc.UIRequest
	turn  int
	timer *time.Timer
}

type piSession struct {
	backend *PiBackend
	proc    *pirpc.Process
	runID   string
	chatID  string
	id      string
	pump    *eventPump

	closeOnce sync.Once
	closeErr  error

	mu      sync.Mutex
	model   string
	dialogs map[string]*piDialog
	tools   map[string]string // toolCallId -> name

	// Turn state. A turn opens on Prompt, or on agent_start while none is
	// open (a prompt pi deferred until the previous run settled), and closes
	// on agent_settled, pi's "nothing more will happen on its own".
	turnActive   bool
	turnIndex    int
	runSeen      bool // agent_start arrived during this turn
	toolCount    int
	hasOutput    bool
	streamedText bool
	interrupting bool
	compacting   bool
	usage        pirpc.Usage
	lastStop     string
	lastError    string
}

func (s *piSession) ID() string { return s.id }

func (s *piSession) Events() <-chan ap.AgentEvent { return s.pump.out }

// Prompt sends user input and returns at once. The turn ends with
// turn.completed, or with an error event if it failed.
//
// A prompt sent while a turn runs steers it: pi queues it as a steering
// message and the model sees it before its next call in the same run.
//
// "/compact [instructions]" is pi's own compaction command. Its TUI handles
// it without the model; in RPC mode it is the compact command, which is what
// this sends. The turn it opens ends when compaction does.
func (s *piSession) Prompt(ctx context.Context, in Input) error {
	select {
	case <-s.proc.Exited():
		return pirpc.ErrClosed
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

	if instructions, ok := compactCommand(text); ok {
		return s.compact(instructions)
	}

	s.mu.Lock()
	wasActive, runSeen := s.turnActive, s.runSeen
	s.openTurnLocked()
	turn := s.turnIndex
	s.mu.Unlock()

	cmd := pirpc.Command{ID: s.proc.NewID(), Type: pirpc.CmdPrompt, Message: text}
	switch {
	case !wasActive:
	case runSeen:
		// pi queues this if its run is still going, and defers it to a run
		// of its own if the run is settling.
		cmd.StreamingBehavior = pirpc.StreamingSteer
	default:
		// The turn's prompt is still in pi's preflight (extension hooks), so
		// pi is not streaming yet and would start a second run beside it.
		// A steer is queued for the run about to start.
		cmd.Type = pirpc.CmdSteer
	}
	ch, err := s.proc.Expect(cmd.ID)
	if err != nil {
		return err
	}
	if err := s.proc.Send(cmd); err != nil {
		s.proc.Forget(cmd.ID)
		return fmt.Errorf("driver: send prompt to pi: %w", err)
	}
	go s.awaitPrompt(cmd.Type, turn, ch)
	return nil
}

// awaitPrompt reads pi's answer to a prompt. pi answers once the prompt is
// accepted, before any model work; the work itself arrives as events. Two
// answers need handling here. A rejection (no model, no key, compaction
// running) comes with no run, so it ends the turn. An acceptance with no run
// behind it is a prompt pi handled itself (an extension command, or an input
// hook that consumed it), and get_state tells that apart from a run about to
// start. Both only apply while the turn the prompt was sent in is still the
// current one.
func (s *piSession) awaitPrompt(command string, turn int, ch <-chan pirpc.Response) {
	res, ok := <-ch
	if !ok {
		return // pi exited; watch ends the turn
	}
	if !res.Success {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.turnActive && s.turnIndex == turn && !s.runSeen {
			s.failTurnLocked("pi rejected the prompt: "+res.Error, "prompt_rejected")
		} else {
			s.backend.diagnose(fmt.Sprintf("pi rejected a %s: %s", command, res.Error))
		}
		return
	}
	s.mu.Lock()
	check := s.turnActive && s.turnIndex == turn && !s.runSeen && !s.compacting
	s.mu.Unlock()
	if !check {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	st, err := s.proc.GetState(ctx)
	cancel()
	if err != nil || st.IsStreaming || st.IsCompacting {
		return
	}
	// Events precede this response on the same stream, so a run that did
	// start has been seen by now.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnActive && s.turnIndex == turn && !s.runSeen && !s.compacting {
		s.completeTurnLocked("handled")
	}
}

func (s *piSession) compact(instructions string) error {
	s.mu.Lock()
	if s.turnActive {
		s.mu.Unlock()
		return errors.New("driver: pi compacts between turns; interrupt or wait for the turn to end")
	}
	s.openTurnLocked()
	s.compacting = true
	s.mu.Unlock()

	go func() {
		_, err := s.proc.Compact(context.Background(), instructions)
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.compacting {
			return
		}
		s.compacting = false
		if !s.turnActive {
			return
		}
		if errors.Is(err, pirpc.ErrClosed) {
			return // watch reports the exit
		}
		if err != nil && s.interrupting {
			s.completeTurnLocked("interrupted")
			return
		}
		if err != nil {
			var ce *pirpc.CommandError
			if errors.As(err, &ce) {
				s.failTurnLocked("pi could not compact: "+ce.Message, "compaction_failed")
				return
			}
			s.failTurnLocked("pi could not compact: "+err.Error(), "compaction_failed")
			return
		}
		s.completeTurnLocked("compacted")
	}()
	return nil
}

// compactCommand recognises pi's /compact with optional instructions.
func compactCommand(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "/compact" {
		return "", true
	}
	if rest, ok := strings.CutPrefix(t, "/compact "); ok {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// openTurnLocked starts a turn if none is running. Caller holds s.mu.
func (s *piSession) openTurnLocked() {
	if s.turnActive {
		return
	}
	s.turnActive = true
	s.turnIndex++
	s.runSeen = false
	s.toolCount = 0
	s.hasOutput = false
	s.streamedText = false
	s.interrupting = false
	s.usage = pirpc.Usage{}
	s.lastStop = ""
	s.lastError = ""
	s.emit(ap.NewEvent(ap.AgentEventTurnStarted, s.runID, s.chatID, ap.TurnStartedPayload{
		TurnIndex: s.turnIndex,
		Model:     s.model,
	}))
}

// Interrupt aborts the running turn. Dialogs an extension is waiting on are
// dismissed first, since a tool call parked on one would otherwise keep the
// run, and so the abort, from finishing. pi answers once it is idle, after
// the turn has ended.
func (s *piSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	if s.turnActive {
		s.interrupting = true
	}
	for id := range s.dialogs {
		s.withdrawDialogLocked(id, "dismissed by interrupt")
	}
	s.mu.Unlock()
	if err := s.proc.Abort(ctx); err != nil {
		return fmt.Errorf("driver: abort pi: %w", err)
	}
	return nil
}

// Resolve answers an extension dialog. requestID is the ToolInvocationID of
// the approval-required event, which is pi's extension_ui_request id.
//
//   - confirm: allow confirms, deny declines.
//   - select: allow needs Values["value"], one of the offered options; deny
//     dismisses the dialog.
//   - input, editor: allow needs Values["value"], the text; deny dismisses.
//
// pi dialogs have no notion of scope; every answer is for this dialog only.
func (s *piSession) Resolve(ctx context.Context, requestID string, res Resolution) error {
	s.mu.Lock()
	d, ok := s.dialogs[requestID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("driver: no pending request %q", requestID)
	}
	var answer pirpc.UIResponse
	allow := res.Decision == ap.InterruptResolutionAllow
	value, hasValue := res.Values["value"].(string)
	switch {
	case d.req.Method == pirpc.UIConfirm:
		answer = pirpc.UIConfirmed(requestID, allow)
	case !allow:
		answer = pirpc.UICancelled(requestID)
	case !hasValue:
		s.mu.Unlock()
		return fmt.Errorf("driver: pi's %s dialog needs Values[\"value\"] to allow", d.req.Method)
	case d.req.Method == pirpc.UISelect && !contains(d.req.Options, value):
		s.mu.Unlock()
		return fmt.Errorf("driver: %q is not one of the options %q", value, d.req.Options)
	default:
		answer = pirpc.UIValue(requestID, value)
	}
	s.dropDialogLocked(requestID)
	s.mu.Unlock()

	if err := s.proc.Answer(answer); err != nil {
		return fmt.Errorf("driver: answer pi: %w", err)
	}
	s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
		ToolInvocationID: requestID,
		ToolName:         dialogName(d.req),
		Decision:         string(res.Decision),
		Reason:           res.Reason,
	}))
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Close ends input, lets pi dispose of its session, and waits for it to
// exit. Events not yet read are discarded and the channel closes.
func (s *piSession) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.proc.Close(s.backend.ShutdownGrace)
		s.pump.stopNow()
	})
	return s.closeErr
}

// Kill implements Killer. pi gets no end of input and no chance to dispose
// of its session; its file holds what pi had already appended. The session
// then ends as it does when pi crashes: a turn still open reports
// process_exited, and Events closes once the queued events are read.
func (s *piSession) Kill() error {
	if err := s.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("driver: kill pi: %w", err)
	}
	<-s.proc.Exited()
	return nil
}

var _ Killer = (*piSession)(nil)

// watch ends the event stream when pi exits, however that happens.
func (s *piSession) watch() {
	<-s.proc.Exited()
	exitErr := s.proc.ExitErr()

	s.mu.Lock()
	for id, d := range s.dialogs {
		s.dropDialogLocked(id)
		s.emitWithdrawn(d, "pi exited")
	}
	if s.turnActive {
		s.turnActive = false
		msg := "pi exited before the turn finished"
		if exitErr != nil {
			msg += ": " + exitErr.Error()
		}
		s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{Message: msg, Code: "process_exited"}))
	}
	s.mu.Unlock()
	s.pump.end()
}

func (s *piSession) onRecord(r pirpc.Record) {
	switch r.Type {
	case pirpc.EventAgentStart:
		s.mu.Lock()
		s.openTurnLocked()
		s.runSeen = true
		s.mu.Unlock()
	case pirpc.EventMessageStart:
		var m pirpc.MessageEvent
		if r.Decode(&m) == nil && m.Message.Role == "assistant" {
			s.mu.Lock()
			s.streamedText = false
			s.mu.Unlock()
		}
	case pirpc.EventMessageUpdate:
		var u pirpc.MessageUpdate
		if r.Decode(&u) == nil {
			s.onUpdate(u.AssistantMessageEvent)
		}
	case pirpc.EventMessageEnd:
		var m pirpc.MessageEvent
		if r.Decode(&m) == nil && m.Message.Role == "assistant" {
			s.onAssistant(m.Message)
		}
	case pirpc.EventToolStart, pirpc.EventToolEnd:
		var t pirpc.ToolEvent
		if r.Decode(&t) == nil {
			s.onTool(t)
		}
	case pirpc.EventCompactionEnd:
		var c pirpc.CompactionEnd
		if r.Decode(&c) != nil {
			return
		}
		switch {
		case c.Result != nil:
			s.emit(ap.NewEvent(ap.AgentEventContextCompacted, s.runID, s.chatID, ap.ContextCompactedPayload{
				BeforeTokens: c.Result.TokensBefore,
				AfterTokens:  c.Result.EstimatedTokensAfter,
			}))
		case c.Aborted:
			s.backend.diagnose(fmt.Sprintf("pi's %s compaction was aborted", c.Reason))
		default:
			s.backend.diagnose(fmt.Sprintf("pi's %s compaction failed: %s", c.Reason, c.ErrorMessage))
		}
	case pirpc.EventAutoRetryStart:
		var a pirpc.AutoRetry
		if r.Decode(&a) == nil {
			s.backend.diagnose(fmt.Sprintf("pi retries (attempt %d of %d in %dms): %s", a.Attempt, a.MaxAttempts, a.DelayMs, a.ErrorMessage))
		}
	case pirpc.EventExtensionError:
		var e pirpc.ExtensionError
		if r.Decode(&e) == nil {
			s.backend.diagnose(fmt.Sprintf("pi extension %s failed in %s: %s", e.ExtensionPath, e.Event, e.Error))
		}
	case pirpc.TypeExtensionUIRequest:
		var req pirpc.UIRequest
		if r.Decode(&req) == nil {
			s.onUIRequest(req)
		}
	case pirpc.EventAgentSettled:
		s.mu.Lock()
		s.onSettledLocked()
		s.mu.Unlock()
	case pirpc.TypeResponse:
		// A response nobody waits for: a parse error, or one for a command
		// whose waiter gave up.
		var res pirpc.Response
		if r.Decode(&res) == nil && !res.Success {
			s.backend.diagnose(fmt.Sprintf("pi rejected %s: %s", res.Command, res.Error))
		}
	}
}

func (s *piSession) onUpdate(e pirpc.AssistantMessageEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Type {
	case "text_delta":
		if e.Delta == "" {
			return
		}
		s.openTurnLocked()
		s.streamedText = true
		s.hasOutput = true
		s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{
			Kind: ap.ContentDeltaText, Delta: e.Delta,
		}))
	case "thinking_delta":
		if e.Delta == "" {
			return
		}
		s.openTurnLocked()
		s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{
			Kind: ap.ContentDeltaReasoning, Delta: e.Delta,
		}))
	}
}

func (s *piSession) onAssistant(m pirpc.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Usage != nil {
		s.usage.Input += m.Usage.Input
		s.usage.Output += m.Usage.Output
		s.usage.CacheRead += m.Usage.CacheRead
		s.usage.CacheWrite += m.Usage.CacheWrite
		s.usage.Reasoning += m.Usage.Reasoning
		s.usage.Cost.Total += m.Usage.Cost.Total
	}
	if m.Model != "" {
		s.model = m.Provider + "/" + m.Model
	}
	s.lastStop = m.StopReason
	s.lastError = m.ErrorMessage
	if !s.streamedText {
		// A provider that does not stream text still ends with it.
		if text := m.Text(); text != "" {
			s.openTurnLocked()
			s.hasOutput = true
			s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{
				Kind: ap.ContentDeltaText, Delta: text,
			}))
		}
	}
	s.streamedText = false
}

func (s *piSession) onTool(t pirpc.ToolEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openTurnLocked()
	if t.Type == pirpc.EventToolStart {
		s.toolCount++
		s.tools[t.ToolCallID] = t.ToolName
		s.emit(ap.NewEvent(ap.AgentEventToolStarted, s.runID, s.chatID, ap.ToolStartedPayload{
			ToolInvocationID: t.ToolCallID,
			ToolName:         t.ToolName,
			DisplayName:      t.ToolName,
			Arguments:        claudeArguments(t.Args),
		}))
		return
	}
	status := ap.ToolInvocationStatusCompleted
	if t.IsError {
		status = ap.ToolInvocationStatusFailed
		if s.interrupting {
			status = ap.ToolInvocationStatusCancelled
		}
	}
	name := t.ToolName
	if name == "" {
		name = s.tools[t.ToolCallID]
	}
	delete(s.tools, t.ToolCallID)
	s.emit(ap.NewEvent(ap.AgentEventToolCompleted, s.runID, s.chatID, ap.ToolCompletedPayload{
		ToolInvocationID: t.ToolCallID,
		ToolName:         name,
		Status:           status,
		Result:           t.Result.Text(),
	}))
}

// onSettledLocked ends the turn: pi will not continue on its own. The last
// assistant message says how the run ended. Caller holds s.mu.
func (s *piSession) onSettledLocked() {
	if !s.turnActive || s.compacting {
		// A compaction's own settle, or one with no turn behind it; the
		// compact command's answer closes a compaction turn.
		return
	}
	for id, d := range s.dialogs {
		if d.turn == s.turnIndex {
			s.withdrawDialogLocked(id, "withdrawn: pi's run ended")
		}
	}
	u := s.usage
	prompt := u.Input + u.CacheRead + u.CacheWrite
	if prompt+u.Output > 0 || u.Cost.Total > 0 {
		s.emit(ap.NewEvent(ap.AgentEventUsageUpdated, s.runID, s.chatID, ap.UsageUpdatedPayload{
			PromptTokens:     prompt,
			CompletionTokens: u.Output,
			TotalTokens:      prompt + u.Output,
			ReasoningTokens:  u.Reasoning,
			CostUSD:          u.Cost.Total,
		}))
	}
	switch {
	case s.lastStop == pirpc.StopAborted || s.interrupting:
		s.completeTurnLocked("interrupted")
	case s.lastStop == pirpc.StopError:
		msg := s.lastError
		if msg == "" {
			msg = "pi's model call failed"
		}
		s.failTurnLocked(msg, "agent_error")
	default:
		s.completeTurnLocked(s.lastStop)
	}
}

func (s *piSession) completeTurnLocked(stop string) {
	s.turnActive = false
	s.interrupting = false
	s.emit(ap.NewEvent(ap.AgentEventTurnCompleted, s.runID, s.chatID, ap.TurnCompletedPayload{
		TurnIndex:  s.turnIndex,
		ToolCount:  s.toolCount,
		HasOutput:  s.hasOutput,
		StopReason: stop,
	}))
}

func (s *piSession) failTurnLocked(msg, code string) {
	s.turnActive = false
	s.interrupting = false
	s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{Message: msg, Code: code}))
}

// onUIRequest raises an extension's dialog as an approval-required event.
// Fire-and-forget requests (status, widgets, titles) have no counterpart
// here; notify is passed on as a diagnostic.
func (s *piSession) onUIRequest(req pirpc.UIRequest) {
	if !pirpc.IsDialog(req.Method) {
		if req.Method == pirpc.UINotify {
			s.backend.diagnose(fmt.Sprintf("pi extension notice (%s): %s", orNone(req.NotifyType), req.Message))
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &piDialog{req: req, turn: s.turnIndex}
	if !s.turnActive {
		d.turn = 0
	}
	s.dialogs[req.ID] = d
	if req.Timeout > 0 {
		// pi answers with its default when the timeout expires and ignores
		// a later answer.
		d.timer = time.AfterFunc(time.Duration(req.Timeout)*time.Millisecond, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if cur, ok := s.dialogs[req.ID]; ok && cur == d {
				s.dropDialogLocked(req.ID)
				s.emitWithdrawn(d, "timed out; pi used the dialog's default")
			}
		})
	}
	args := ap.StringEncodedMap{"method": req.Method}
	for k, v := range map[string]string{"title": req.Title, "message": req.Message, "placeholder": req.Placeholder, "prefill": req.Prefill} {
		if v != "" {
			args[k] = v
		}
	}
	if len(req.Options) > 0 {
		args["options"] = req.Options
	}
	s.emit(ap.NewEvent(ap.AgentEventApprovalRequired, s.runID, s.chatID, ap.ApprovalRequiredPayload{
		ToolInvocationID: req.ID,
		ToolName:         dialogName(req),
		Arguments:        args,
		Reason:           ap.InterruptReasonConfirmation,
	}))
}

// dialogName names an extension dialog in events. The title, which is the
// question, is in the arguments.
func dialogName(req pirpc.UIRequest) string { return "extension_" + req.Method }

func (s *piSession) dropDialogLocked(id string) {
	if d, ok := s.dialogs[id]; ok {
		if d.timer != nil {
			d.timer.Stop()
		}
		delete(s.dialogs, id)
	}
}

// withdrawDialogLocked dismisses a dialog in pi and tells the caller.
func (s *piSession) withdrawDialogLocked(id, reason string) {
	d, ok := s.dialogs[id]
	if !ok {
		return
	}
	s.dropDialogLocked(id)
	_ = s.proc.Answer(pirpc.UICancelled(id))
	s.emitWithdrawn(d, reason)
}

func (s *piSession) emitWithdrawn(d *piDialog, reason string) {
	s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
		ToolInvocationID: d.req.ID,
		ToolName:         dialogName(d.req),
		Decision:         string(ap.InterruptResolutionDeny),
		Reason:           reason,
	}))
}

func (s *piSession) emit(ev ap.AgentEvent) { s.pump.push(ev) }
