package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"sync"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/codexapp"
	"github.com/inference-sh/agentprotocol/harness"
)

// KindCodex names the native Codex backend.
const KindCodex = "codex"

// CodexBackend runs Codex through `codex app-server`, the JSON-RPC server in
// the codex CLI that OpenAI's IDE extension uses. No adapter sits in between.
//
// Codex runs as the user: it reads its own login from CODEX_HOME, which is
// where an account profile is selected through Env. Nothing here reads or
// passes a credential.
type CodexBackend struct {
	// Command is the codex binary. Empty means "codex" on PATH.
	Command string

	// Args follow "app-server", for config overrides such as
	// "-c", `model_provider="..."`.
	Args []string

	// Env is the child environment. Nil inherits the parent's. Set
	// CODEX_HOME here to select an account profile.
	Env []string

	// Version is the installed codex version (codex --version), when the
	// caller knows it; ForHarnessVersion sets it. Features the registry
	// records for codex with a version range (harness.Harness.Features) are
	// then used only on versions that have them. Empty assumes every feature.
	Version string

	// ClientName and ClientVersion identify us during initialize. Codex puts
	// them in its user agent.
	ClientName    string
	ClientVersion string

	// ApprovalPolicy is sent on thread start and resume. Empty means
	// "untrusted": codex asks before anything outside its known-safe set.
	//
	// It is always sent, so a user config that says "never" does not silently
	// turn approvals off for a session driven from here. Setting "never" is
	// possible, and is the caller's decision, never this package's default.
	ApprovalPolicy string

	// Sandbox is the sandbox mode: "read-only", "workspace-write" or
	// "danger-full-access". Empty leaves it to the user's codex config.
	Sandbox string

	// ApprovalsReviewer routes approval requests. Empty means "user": every
	// request reaches the caller as an approval-required event rather than
	// being decided by codex's automatic reviewer.
	ApprovalsReviewer string

	// Config is codex configuration for this session's thread, sent as the
	// config of thread/start and thread/resume. Keys are dotted config paths
	// as in `-c key=value` ("model_reasoning_effort",
	// "features.some_flag"); values are anything that encodes to JSON.
	//
	// Unlike Args, which configure the whole app-server process, this is
	// scoped to the thread, and it reaches settings codex accepts only per
	// thread. bypass_hook_trust is one: codex 0.156.1 honours it here and
	// ignores it as `-c` on app-server. It runs hooks nobody reviewed, so it
	// is the caller's decision and never set by this package. Nil sends none.
	Config map[string]any

	// Stderr receives codex's own logs. Nil discards them.
	Stderr io.Writer

	// OnDiagnostic receives protocol-level observations that are not events:
	// warnings codex reports, retries, server requests this client declines,
	// and notifications for other threads. Nil discards them.
	OnDiagnostic func(string)
}

// Kind implements Backend.
func (b *CodexBackend) Kind() string { return KindCodex }

// Capabilities implements Backend.
//
// Steer: a prompt during a running turn is sent as turn/steer against the
// active turn, which codex accepts and feeds into that turn's next model
// request. Measured on codex-cli 0.137.0 and 0.156.1.
//
// Resume: thread/resume by thread id in a fresh process restores the
// conversation; the next model request carries the earlier turns.
//
// Tools is false: codex can take MCP servers through its config, but this
// backend does not wire SessionConfig.Tools or MCP metadata into it yet.
func (b *CodexBackend) Capabilities() Capabilities {
	return Capabilities{
		Steer:     b.has(harness.FeatureTurnSteer),
		Approvals: true,
		Interrupt: true,
		Resume:    true,
		Tools:     false,
	}
}

// has reports whether the installed codex has a version-ranged feature,
// true when the version is not known.
func (b *CodexBackend) has(feature string) bool {
	return b.Version == "" || harness.All["codex"].HasFeature(feature, b.Version)
}

func (b *CodexBackend) diagnose(msg string) {
	if b.OnDiagnostic != nil {
		b.OnDiagnostic(msg)
	}
}

// Open implements Backend.
func (b *CodexBackend) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	threadConfig, err := encodeConfig(b.Config)
	if err != nil {
		return nil, err
	}

	s := &codexSession{
		backend:   b,
		runID:     cfg.RunID,
		chatID:    cfg.ChatID,
		events:    make(chan ap.AgentEvent, 256),
		pending:   map[string]*codexApproval{},
		byRPC:     map[string]string{},
		openTools: map[string]string{},
		prompts:   make(chan Input, 64),
		stop:      make(chan struct{}),
		watched:   make(chan struct{}),
	}

	name := b.ClientName
	if name == "" {
		name = "agentprotocol"
	}
	version := b.ClientVersion
	if version == "" {
		version = "0"
	}

	proc, err := codexapp.Spawn(ctx, codexapp.ProcessConfig{
		Command: b.Command,
		Args:    b.Args,
		Dir:     cfg.WorkDir,
		Env:     b.Env,
		Stderr:  b.Stderr,
	}, codexapp.ClientInfo{Name: name, Version: version}, codexapp.Handler{
		OnNotification: s.onNotification,
		OnRequest:      s.onRequest,
	})
	if err != nil {
		return nil, err
	}
	s.proc = proc

	policy := codexapp.AskForApprovalUntrusted
	if b.ApprovalPolicy != "" {
		policy = codexapp.AskForApproval(mustJSON(b.ApprovalPolicy))
	}
	reviewer := codexapp.ApprovalsReviewerUser
	if b.ApprovalsReviewer != "" {
		reviewer = codexapp.ApprovalsReviewer(b.ApprovalsReviewer)
	}
	var sandbox *codexapp.SandboxMode
	if b.Sandbox != "" {
		m := codexapp.SandboxMode(b.Sandbox)
		sandbox = &m
	}

	var thread codexapp.Thread
	if cfg.ResumeSessionID != "" {
		res, err := proc.ThreadResume(ctx, codexapp.ThreadResumeParams{
			ThreadID:              cfg.ResumeSessionID,
			Cwd:                   optional(cfg.WorkDir),
			Model:                 optional(cfg.Model),
			DeveloperInstructions: optional(cfg.Instructions),
			ApprovalPolicy:        policy,
			ApprovalsReviewer:     &reviewer,
			Sandbox:               sandbox,
			Config:                threadConfig,
		})
		if err != nil {
			_ = proc.Kill()
			return nil, fmt.Errorf("driver: resume codex thread %s: %w", cfg.ResumeSessionID, err)
		}
		thread, s.model = res.Thread, res.Model
		b.diagnose(fmt.Sprintf("resumed codex thread %s with %d turn(s) of history", thread.ID, len(thread.Turns)))
	} else {
		res, err := proc.ThreadStart(ctx, codexapp.ThreadStartParams{
			Cwd:                   optional(cfg.WorkDir),
			Model:                 optional(cfg.Model),
			DeveloperInstructions: optional(cfg.Instructions),
			ApprovalPolicy:        policy,
			ApprovalsReviewer:     &reviewer,
			Sandbox:               sandbox,
			Config:                threadConfig,
		})
		if err != nil {
			_ = proc.Kill()
			return nil, fmt.Errorf("driver: start codex thread: %w", err)
		}
		thread, s.model = res.Thread, res.Model
	}

	s.mu.Lock()
	s.id = thread.ID
	s.mu.Unlock()

	s.emit(ap.NewEvent(ap.AgentEventRunStarted, s.runID, s.chatID, ap.RunStartedPayload{}))
	go s.sendLoop()
	go s.watch()
	return s, nil
}

// encodeConfig turns CodexBackend.Config into the thread params' shape.
func encodeConfig(cfg map[string]any) (map[string]json.RawMessage, error) {
	if len(cfg) == 0 {
		return nil, nil
	}
	out := make(map[string]json.RawMessage, len(cfg))
	for k, v := range cfg {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("driver: codex config %q: %w", k, err)
		}
		out[k] = raw
	}
	return out, nil
}

// codexApproval is one server request waiting on a human.
type codexApproval struct {
	method string
	rpcID  string
	turnID string
	tool   string
	params json.RawMessage
	answer chan any
	once   sync.Once
}

func (p *codexApproval) settle(v any) { p.once.Do(func() { p.answer <- v }) }

type codexSession struct {
	backend *CodexBackend
	proc    *codexapp.Process
	runID   string
	chatID  string
	model   string

	prompts chan Input
	stop    chan struct{}
	watched chan struct{} // closed when watch has finished

	evMu     sync.Mutex
	events   chan ap.AgentEvent
	evClosed bool

	closeOnce sync.Once
	closing   bool

	mu         sync.Mutex
	id         string
	activeTurn string
	turnIndex  int
	toolCount  int
	hasOutput  bool
	turnError  string
	pending    map[string]*codexApproval
	byRPC      map[string]string
	// openTools are tool items started and not yet completed, by item id.
	// Codex does not complete an item whose turn was interrupted, so these
	// are closed as cancelled when the turn ends.
	openTools map[string]string
}

func (s *codexSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *codexSession) Events() <-chan ap.AgentEvent { return s.events }

// Prompt queues input and returns. The send loop delivers prompts in order:
// as a new turn when none is running, or as a steer into the running one. The
// turn ends with turn.completed, or with an error event if codex refused the
// input or the turn failed.
func (s *codexSession) Prompt(ctx context.Context, in Input) error {
	select {
	case <-s.stop:
		return errors.New("driver: codex session is closed")
	default:
	}
	select {
	case s.prompts <- in:
		return nil
	case <-s.stop:
		return errors.New("driver: codex session is closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *codexSession) sendLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.stop:
		case <-s.proc.Done():
		}
		cancel()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case in := <-s.prompts:
			if err := s.send(ctx, in); err != nil && ctx.Err() == nil {
				s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{Message: err.Error()}))
			}
		}
	}
}

func (s *codexSession) send(ctx context.Context, in Input) error {
	input := userInput(in)
	s.mu.Lock()
	thread, active := s.id, s.activeTurn
	s.mu.Unlock()

	if active != "" && !s.backend.has(harness.FeatureTurnSteer) {
		return fmt.Errorf("driver: codex %s has no turn/steer (added in %s); send the prompt after the turn ends",
			s.backend.Version, harness.All["codex"].Features[harness.FeatureTurnSteer].From)
	}
	if active != "" {
		_, err := s.proc.TurnSteer(ctx, codexapp.TurnSteerParams{ThreadID: thread, ExpectedTurnID: active, Input: input})
		if err == nil {
			return nil
		}
		var rpcErr *codexapp.Error
		if !errors.As(err, &rpcErr) {
			return fmt.Errorf("steer codex turn: %w", err)
		}
		// The turn ended between our look and codex's. Start a new one.
		s.backend.diagnose("turn/steer refused, starting a new turn instead: " + rpcErr.Message)
	}

	res, err := s.proc.TurnStart(ctx, codexapp.TurnStartParams{ThreadID: thread, Input: input})
	if err != nil {
		return fmt.Errorf("start codex turn: %w", err)
	}
	s.mu.Lock()
	if s.activeTurn == "" {
		s.activeTurn = res.Turn.ID
	}
	s.mu.Unlock()
	return nil
}

// userInput converts our input to codex's. Images go as images; any other
// attachment is named in the text, since codex reads files from the
// workspace itself.
func userInput(in Input) []codexapp.UserInput {
	text := in.Text
	var out []codexapp.UserInput
	for _, f := range in.Files {
		if strings.HasPrefix(f.ContentType, "image/") {
			if u, err := url.Parse(f.URI); err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "data") {
				if v, err := codexapp.NewUserInput(codexapp.ImageUserInput{Type: codexapp.UserInputTypeImage, URL: &f.URI}); err == nil {
					out = append(out, v)
					continue
				}
			} else if path := localPath(f.URI); path != "" {
				if v, err := codexapp.NewUserInput(codexapp.LocalImageUserInput{Type: codexapp.UserInputTypeLocalImage, Path: path}); err == nil {
					out = append(out, v)
					continue
				}
			}
		}
		name := f.Filename
		if name == "" {
			name = f.URI
		}
		text += fmt.Sprintf("\n\nAttached file %s: %s", name, f.URI)
	}
	return append([]codexapp.UserInput{codexapp.TextInput(text)}, out...)
}

func localPath(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		return strings.TrimPrefix(uri, "file://")
	}
	if strings.HasPrefix(uri, "/") {
		return uri
	}
	return ""
}

// Interrupt cancels the running turn with turn/interrupt. The turn then
// completes with stop reason "interrupted" and the thread takes new prompts.
// With no turn running it does nothing.
func (s *codexSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	thread, active := s.id, s.activeTurn
	s.mu.Unlock()
	if active == "" {
		return nil
	}
	return s.proc.TurnInterrupt(ctx, codexapp.TurnInterruptParams{ThreadID: thread, TurnID: active})
}

// Resolve answers a parked approval.
//
// Scope maps onto codex's session-scoped approval cache: ScopeSession and
// ScopeAlways both send acceptForSession (approved_for_session on the legacy
// methods, scope "session" for permission grants). Nothing here writes a
// persistent execpolicy rule, so ScopeAlways rounds down to the session.
// A denial is "decline", which lets the turn continue without the action.
func (s *codexSession) Resolve(ctx context.Context, requestID string, res Resolution) error {
	s.mu.Lock()
	p, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
		delete(s.byRPC, p.rpcID)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("driver: no pending request %q", requestID)
	}

	allow := res.Decision == ap.InterruptResolutionAllow
	session := res.Scope == ScopeSession || res.Scope == ScopeAlways
	p.settle(approvalAnswer(p, allow, session, res.Reason))

	s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
		ToolInvocationID: requestID,
		ToolName:         p.tool,
		Decision:         string(res.Decision),
		Reason:           res.Reason,
	}))
	return nil
}

// approvalAnswer builds the response body codex expects for the request kind.
//
// reason is only carried by the legacy methods, whose denial names a
// rejection; the item/* methods have no field for it.
func approvalAnswer(p *codexApproval, allow, session bool, reason string) any {
	switch p.method {
	case codexapp.MethodItemCommandExecutionRequestApproval:
		d := codexapp.CommandExecutionApprovalDecisionDecline
		if allow && session {
			d = codexapp.CommandExecutionApprovalDecisionAcceptForSession
		} else if allow {
			d = codexapp.CommandExecutionApprovalDecisionAccept
		}
		return codexapp.CommandExecutionRequestApprovalResponse{Decision: d}

	case codexapp.MethodItemFileChangeRequestApproval:
		d := codexapp.FileChangeApprovalDecisionDecline
		if allow && session {
			d = codexapp.FileChangeApprovalDecisionAcceptForSession
		} else if allow {
			d = codexapp.FileChangeApprovalDecisionAccept
		}
		return codexapp.FileChangeRequestApprovalResponse{Decision: d}

	case codexapp.MethodItemPermissionsRequestApproval:
		scope := codexapp.PermissionGrantScopeTurn
		if session {
			scope = codexapp.PermissionGrantScopeSession
		}
		var grant codexapp.GrantedPermissionProfile
		if allow {
			var req codexapp.PermissionsRequestApprovalParams
			if json.Unmarshal(p.params, &req) == nil {
				grant = codexapp.GrantedPermissionProfile{FileSystem: req.Permissions.FileSystem, Network: req.Permissions.Network}
			}
		}
		return codexapp.PermissionsRequestApprovalResponse{Permissions: grant, Scope: &scope}

	case codexapp.MethodExecCommandApproval, codexapp.MethodApplyPatchApproval:
		// Since codex 0.156 a denial is {"denied":{"rejection":...}}; it was
		// the bare string "denied" up to 0.137. Only turns started through the
		// legacy APIs raise these requests, and this driver starts turns with
		// turn/start, so the current shape is sent.
		if reason == "" {
			reason = "declined"
		}
		d := codexapp.ReviewDecision(mustJSON(codexapp.DeniedReviewDecision{Denied: codexapp.DeniedReviewDecisionDenied{Rejection: reason}}))
		if allow && session {
			d = codexapp.ReviewDecisionApprovedForSession
		} else if allow {
			d = codexapp.ReviewDecisionApproved
		}
		if p.method == codexapp.MethodExecCommandApproval {
			return codexapp.ExecCommandApprovalResponse{Decision: d}
		}
		return codexapp.ApplyPatchApprovalResponse{Decision: d}
	}
	return nil
}

// cancelAnswer is what a request gets when nobody will decide it: the session
// closed or codex withdrew it.
func cancelAnswer(p *codexApproval) any { return approvalAnswer(p, false, false, "cancelled") }

// Close ends the session. Parked approvals are declined, codex's stdin is
// closed, and the event channel closes once the process is gone.
func (s *codexSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		pending := s.pending
		s.pending = map[string]*codexApproval{}
		s.byRPC = map[string]string{}
		s.mu.Unlock()
		close(s.stop)
		for _, p := range pending {
			p.settle(cancelAnswer(p))
		}
		err = s.proc.Close()
		s.closeEvents()
	})
	// Codex was told to stop; how its exit status reads after that is not
	// something a caller can act on.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// Kill implements Killer. Codex gets no grace to finish writing its rollout.
// The session then ends as it does when codex crashes: an error event with
// code process_exited, then Events closes. After Close it does nothing.
func (s *codexSession) Kill() error {
	_ = s.proc.Kill()
	<-s.watched
	return s.Close()
}

var _ Killer = (*codexSession)(nil)

// watch closes the event stream when codex exits on its own, which is the
// signal to a caller that no more work is coming.
func (s *codexSession) watch() {
	<-s.proc.Done()
	s.mu.Lock()
	closing := s.closing
	pending := s.pending
	s.pending = map[string]*codexApproval{}
	s.byRPC = map[string]string{}
	s.mu.Unlock()
	for _, p := range pending {
		p.settle(cancelAnswer(p))
	}
	if !closing {
		msg := "codex app-server exited"
		if err := s.proc.Err(); err != nil {
			msg += ": " + err.Error()
		}
		s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{Message: msg, Code: "process_exited"}))
	}
	<-s.proc.Exited()
	s.closeEvents()
	close(s.watched)
}

func (s *codexSession) emit(ev ap.AgentEvent) {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if s.evClosed {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.backend.diagnose("event dropped, consumer is not reading: " + string(ev.Type))
	}
}

func (s *codexSession) closeEvents() {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if !s.evClosed {
		s.evClosed = true
		close(s.events)
	}
}

// --- server requests ---

func (s *codexSession) onRequest(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) (any, *codexapp.Error) {
	var (
		key, turnID, tool string
		args              ap.StringEncodedMap
	)
	switch method {
	case codexapp.MethodItemCommandExecutionRequestApproval:
		var p codexapp.CommandExecutionRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &codexapp.Error{Code: -32602, Message: err.Error()}
		}
		key, turnID, tool = deref(p.ApprovalID, p.ItemID), p.TurnID, codexapp.ThreadItemTypeCommandExecution
		args = ap.StringEncodedMap{"command": deref(p.Command, ""), "cwd": deref(p.Cwd, "")}
		if p.Kind != nil && *p.Kind != codexapp.CommandExecutionApprovalKindCommand {
			// writeStdin (codex 0.156): input for a terminal already running,
			// not a new command.
			args["kind"] = string(*p.Kind)
		}
		if p.Reason != nil {
			args["reason"] = *p.Reason
		}
	case codexapp.MethodItemFileChangeRequestApproval:
		var p codexapp.FileChangeRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &codexapp.Error{Code: -32602, Message: err.Error()}
		}
		key, turnID, tool = p.ItemID, p.TurnID, codexapp.ThreadItemTypeFileChange
		args = ap.StringEncodedMap{}
		if p.Reason != nil {
			args["reason"] = *p.Reason
		}
		if p.GrantRoot != nil {
			args["grant_root"] = *p.GrantRoot
		}
	case codexapp.MethodItemPermissionsRequestApproval:
		var p codexapp.PermissionsRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &codexapp.Error{Code: -32602, Message: err.Error()}
		}
		key, turnID, tool = p.ItemID, p.TurnID, "permissions"
		args = ap.StringEncodedMap{"cwd": p.Cwd}
		if raw, err := json.Marshal(p.Permissions); err == nil {
			args["permissions"] = string(raw)
		}
		if p.Reason != nil {
			args["reason"] = *p.Reason
		}
	case codexapp.MethodExecCommandApproval:
		var p codexapp.ExecCommandApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &codexapp.Error{Code: -32602, Message: err.Error()}
		}
		key, tool = deref(p.ApprovalID, p.CallID), codexapp.ThreadItemTypeCommandExecution
		args = ap.StringEncodedMap{"command": strings.Join(p.Command, " "), "cwd": p.Cwd}
	case codexapp.MethodApplyPatchApproval:
		var p codexapp.ApplyPatchApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &codexapp.Error{Code: -32602, Message: err.Error()}
		}
		key, tool = p.CallID, codexapp.ThreadItemTypeFileChange
		args = ap.StringEncodedMap{}
	default:
		s.backend.diagnose("codex made a request this client does not handle: " + method)
		return nil, &codexapp.Error{Code: codexapp.CodeMethodNotFound, Message: "not supported by this client: " + method}
	}

	p := &codexApproval{
		method: method,
		rpcID:  string(id),
		turnID: turnID,
		tool:   tool,
		params: params,
		answer: make(chan any, 1),
	}

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return cancelAnswer(p), nil
	}
	if key == "" {
		key = "request-" + string(id)
	}
	if _, taken := s.pending[key]; taken {
		key = key + "#" + string(id)
	}
	s.pending[key] = p
	s.byRPC[p.rpcID] = key
	s.mu.Unlock()

	s.emit(ap.NewEvent(ap.AgentEventApprovalRequired, s.runID, s.chatID, ap.ApprovalRequiredPayload{
		ToolInvocationID: key,
		ToolName:         tool,
		Arguments:        args,
		Reason:           ap.InterruptReasonToolApproval,
	}))

	select {
	case v := <-p.answer:
		return v, nil
	case <-ctx.Done():
		s.drop(key)
		return cancelAnswer(p), nil
	}
}

func (s *codexSession) drop(key string) {
	s.mu.Lock()
	if p, ok := s.pending[key]; ok {
		delete(s.byRPC, p.rpcID)
		delete(s.pending, key)
	}
	s.mu.Unlock()
}

// withdraw releases approvals codex no longer waits for, and tells the caller
// so a prompt on someone's screen does not stay open for nothing.
func (s *codexSession) withdraw(match func(*codexApproval) bool, reason string) {
	s.mu.Lock()
	var gone []string
	var ps []*codexApproval
	for key, p := range s.pending {
		if match(p) {
			gone = append(gone, key)
			ps = append(ps, p)
			delete(s.pending, key)
			delete(s.byRPC, p.rpcID)
		}
	}
	s.mu.Unlock()
	for i, key := range gone {
		ps[i].settle(cancelAnswer(ps[i]))
		s.emit(ap.NewEvent(ap.AgentEventApprovalResolved, s.runID, s.chatID, ap.ApprovalResolvedPayload{
			ToolInvocationID: key,
			ToolName:         ps[i].tool,
			Decision:         "cancelled",
			Reason:           reason,
		}))
	}
}

// --- notifications ---

func (s *codexSession) onNotification(method string, params json.RawMessage) {
	s.mu.Lock()
	thread := s.id
	s.mu.Unlock()

	// Every notification about a turn or item names its thread. Codex can
	// run sub-agent threads in the same process; their progress is not ours.
	var head struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &head)
	if head.ThreadID != "" && thread != "" && head.ThreadID != thread {
		return
	}

	switch method {
	case codexapp.MethodTurnStarted:
		var n codexapp.TurnStartedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		s.mu.Lock()
		s.activeTurn = n.Turn.ID
		s.turnIndex++
		s.toolCount, s.hasOutput, s.turnError = 0, false, ""
		index := s.turnIndex
		s.mu.Unlock()
		s.emit(ap.NewEvent(ap.AgentEventTurnStarted, s.runID, s.chatID, ap.TurnStartedPayload{TurnIndex: index, Model: s.model}))

	case codexapp.MethodTurnCompleted:
		var n codexapp.TurnCompletedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		s.mu.Lock()
		if s.activeTurn == n.Turn.ID || s.activeTurn == "" {
			s.activeTurn = ""
		}
		index, tools, output, lastErr := s.turnIndex, s.toolCount, s.hasOutput, s.turnError
		open := s.openTools
		s.openTools = map[string]string{}
		s.mu.Unlock()
		turnID := n.Turn.ID
		s.withdraw(func(p *codexApproval) bool { return p.turnID == turnID }, "turn ended")
		for id, name := range open {
			s.emit(ap.NewEvent(ap.AgentEventToolCompleted, s.runID, s.chatID, ap.ToolCompletedPayload{
				ToolInvocationID: id,
				ToolName:         name,
				Status:           ap.ToolInvocationStatusCancelled,
			}))
		}

		if n.Turn.Status == codexapp.TurnStatusFailed {
			msg := lastErr
			if n.Turn.Error != nil && n.Turn.Error.Message != "" {
				msg = n.Turn.Error.Message
			}
			if msg == "" {
				msg = "codex turn failed"
			}
			s.emit(ap.NewEvent(ap.AgentEventError, s.runID, s.chatID, ap.ErrorPayload{Message: msg, Code: "turn_failed"}))
			return
		}
		s.emit(ap.NewEvent(ap.AgentEventTurnCompleted, s.runID, s.chatID, ap.TurnCompletedPayload{
			TurnIndex:  index,
			ToolCount:  tools,
			HasOutput:  output,
			StopReason: string(n.Turn.Status),
		}))

	case codexapp.MethodItemAgentMessageDelta:
		var n codexapp.AgentMessageDeltaNotification
		if json.Unmarshal(params, &n) != nil || n.Delta == "" {
			return
		}
		s.mu.Lock()
		s.hasOutput = true
		s.mu.Unlock()
		s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{Kind: ap.ContentDeltaText, Delta: n.Delta}))

	case codexapp.MethodItemReasoningTextDelta:
		var n codexapp.ReasoningTextDeltaNotification
		if json.Unmarshal(params, &n) == nil && n.Delta != "" {
			s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{Kind: ap.ContentDeltaReasoning, Delta: n.Delta}))
		}

	case codexapp.MethodItemReasoningSummaryTextDelta:
		var n codexapp.ReasoningSummaryTextDeltaNotification
		if json.Unmarshal(params, &n) == nil && n.Delta != "" {
			s.emit(ap.NewEvent(ap.AgentEventContentDelta, s.runID, s.chatID, ap.ContentDeltaPayload{Kind: ap.ContentDeltaReasoning, Delta: n.Delta}))
		}

	case codexapp.MethodItemStarted:
		var n codexapp.ItemStartedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		if p, ok := toolStarted(n.Item); ok {
			s.mu.Lock()
			s.toolCount++
			s.openTools[p.ToolInvocationID] = p.ToolName
			s.mu.Unlock()
			s.emit(ap.NewEvent(ap.AgentEventToolStarted, s.runID, s.chatID, p))
		}

	case codexapp.MethodItemCompleted:
		var n codexapp.ItemCompletedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		if p, ok := toolCompleted(n.Item); ok {
			s.mu.Lock()
			delete(s.openTools, p.ToolInvocationID)
			s.mu.Unlock()
			s.emit(ap.NewEvent(ap.AgentEventToolCompleted, s.runID, s.chatID, p))
		}

	case codexapp.MethodThreadTokenUsageUpdated:
		var n codexapp.ThreadTokenUsageUpdatedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		u := n.TokenUsage.Last
		s.emit(ap.NewEvent(ap.AgentEventUsageUpdated, s.runID, s.chatID, ap.UsageUpdatedPayload{
			PromptTokens:     int(u.InputTokens),
			CompletionTokens: int(u.OutputTokens),
			TotalTokens:      int(u.TotalTokens),
			ReasoningTokens:  int(u.ReasoningOutputTokens),
		}))

	case codexapp.MethodThreadCompacted:
		s.emit(ap.NewEvent(ap.AgentEventContextCompacted, s.runID, s.chatID, ap.ContextCompactedPayload{}))

	case codexapp.MethodServerRequestResolved:
		var n codexapp.ServerRequestResolvedNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		rpc := string(n.RequestID)
		s.mu.Lock()
		_, stillPending := s.byRPC[rpc]
		s.mu.Unlock()
		if stillPending {
			s.withdraw(func(p *codexApproval) bool { return p.rpcID == rpc }, "codex withdrew the request")
		}

	case codexapp.MethodError:
		var n codexapp.ErrorNotification
		if json.Unmarshal(params, &n) != nil {
			return
		}
		if n.WillRetry {
			s.backend.diagnose("codex will retry after: " + n.Error.Message)
			return
		}
		s.mu.Lock()
		s.turnError = n.Error.Message
		s.mu.Unlock()
		s.backend.diagnose("codex reported: " + n.Error.Message)

	case codexapp.MethodWarning:
		var n codexapp.WarningNotification
		if json.Unmarshal(params, &n) == nil {
			s.backend.diagnose("codex warning: " + n.Message)
		}

	case codexapp.MethodConfigWarning:
		var n codexapp.ConfigWarningNotification
		if json.Unmarshal(params, &n) == nil {
			s.backend.diagnose("codex config warning: " + n.Summary)
		}
	}
	// Anything else, known or not, carries nothing the lifecycle models.
}

// toolStarted maps the items that are tool use. Messages, reasoning and the
// user's own input are not.
func toolStarted(item codexapp.ThreadItem) (ap.ToolStartedPayload, bool) {
	p := ap.ToolStartedPayload{ToolName: item.Type}
	switch item.Type {
	case codexapp.ThreadItemTypeCommandExecution:
		v, ok := item.AsCommandExecutionThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.DisplayName = v.ID, v.Command
		p.Arguments = ap.StringEncodedMap{"command": v.Command, "cwd": v.Cwd}
	case codexapp.ThreadItemTypeFileChange:
		v, ok := item.AsFileChangeThreadItem()
		if !ok {
			return p, false
		}
		paths := make([]string, 0, len(v.Changes))
		for _, c := range v.Changes {
			paths = append(paths, c.Path)
		}
		p.ToolInvocationID, p.DisplayName = v.ID, strings.Join(paths, ", ")
		p.Arguments = ap.StringEncodedMap{"paths": paths}
	case codexapp.ThreadItemTypeMCPToolCall:
		v, ok := item.AsMcpToolCallThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.ToolName, p.DisplayName = v.ID, v.Tool, v.Server+"/"+v.Tool
		p.Arguments = codexArguments(v.Arguments)
	case codexapp.ThreadItemTypeDynamicToolCall:
		v, ok := item.AsDynamicToolCallThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.ToolName, p.DisplayName = v.ID, v.Tool, v.Tool
		p.Arguments = codexArguments(v.Arguments)
	case codexapp.ThreadItemTypeWebSearch:
		v, ok := item.AsWebSearchThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.DisplayName = v.ID, v.Query
		p.Arguments = ap.StringEncodedMap{"query": v.Query}
	default:
		return p, false
	}
	return p, true
}

func toolCompleted(item codexapp.ThreadItem) (ap.ToolCompletedPayload, bool) {
	p := ap.ToolCompletedPayload{ToolName: item.Type}
	switch item.Type {
	case codexapp.ThreadItemTypeCommandExecution:
		v, ok := item.AsCommandExecutionThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.Status, p.Result = v.ID, itemStatus(string(v.Status)), deref(v.AggregatedOutput, "")
		if v.DurationMs != nil {
			p.DurationMs = *v.DurationMs
		}
	case codexapp.ThreadItemTypeFileChange:
		v, ok := item.AsFileChangeThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.Status = v.ID, itemStatus(string(v.Status))
	case codexapp.ThreadItemTypeMCPToolCall:
		v, ok := item.AsMcpToolCallThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.ToolName, p.Status = v.ID, v.Tool, itemStatus(string(v.Status))
		if v.Error != nil {
			p.Result = v.Error.Message
		} else if v.Result != nil {
			if raw, err := json.Marshal(v.Result.Content); err == nil {
				p.Result = string(raw)
			}
		}
		if v.DurationMs != nil {
			p.DurationMs = *v.DurationMs
		}
	case codexapp.ThreadItemTypeDynamicToolCall:
		v, ok := item.AsDynamicToolCallThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.ToolName, p.Status = v.ID, v.Tool, itemStatus(string(v.Status))
	case codexapp.ThreadItemTypeWebSearch:
		v, ok := item.AsWebSearchThreadItem()
		if !ok {
			return p, false
		}
		p.ToolInvocationID, p.Status = v.ID, ap.ToolInvocationStatusCompleted
	default:
		return p, false
	}
	if !p.Status.IsTerminal() {
		return p, false
	}
	return p, true
}

// itemStatus maps codex's item statuses. "declined" is a person saying no,
// which is a cancellation rather than a failure of the tool.
func itemStatus(s string) ap.ToolInvocationStatus {
	switch s {
	case "inProgress":
		return ap.ToolInvocationStatusInProgress
	case "completed":
		return ap.ToolInvocationStatusCompleted
	case "declined":
		return ap.ToolInvocationStatusCancelled
	default:
		return ap.ToolInvocationStatusFailed
	}
}

func deref(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// codexArguments decodes a tool's JSON arguments, keeping non-object values
// under "value" rather than dropping them.
func codexArguments(raw json.RawMessage) ap.StringEncodedMap {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m ap.StringEncodedMap
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		return ap.StringEncodedMap{"value": v}
	}
	return nil
}
