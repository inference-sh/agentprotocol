package driver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/codexapp"
	"github.com/inference-sh/agentprotocol/driver"
)

// The codex tests use the same trick as the ACP ones: this test binary is
// launched as `codex app-server` and plays a scripted server. It switches in
// init rather than TestMain so the ACP fake's TestMain stays untouched.

const codexFakeEnv = "AGENTPROTOCOL_FAKE_CODEX"

func init() {
	if mode, ok := os.LookupEnv(codexFakeEnv); ok {
		runFakeCodex(mode)
		lingerIfAsked()
		os.Exit(0)
	}
}

func codexBackend(t *testing.T, mode string) (*driver.CodexBackend, *[]string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	var mu sync.Mutex
	diags := &[]string{}
	return &driver.CodexBackend{
		Command: exe,
		// The race runtime sleeps a second on exit unless told not to, which
		// would otherwise add a second to every Close under -race.
		Env: append(os.Environ(), codexFakeEnv+"="+mode, "GORACE=atexit_sleep_ms=0"),
		OnDiagnostic: func(m string) {
			mu.Lock()
			*diags = append(*diags, m)
			mu.Unlock()
		},
	}, diags
}

func openCodex(t *testing.T, mode string, cfg driver.SessionConfig) driver.Session {
	t.Helper()
	b, _ := codexBackend(t, mode)
	if cfg.RunID == "" {
		cfg.RunID, cfg.ChatID = "run_1", "chat_1"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	sess, err := b.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func deltas(events []ap.AgentEvent) string {
	var b strings.Builder
	for _, e := range events {
		if p, ok := ap.PayloadAs[ap.ContentDeltaPayload](e, ap.AgentEventContentDelta); ok && p.Kind == ap.ContentDeltaText {
			b.WriteString(p.Delta)
		}
	}
	return b.String()
}

func TestCodexOpenInitializesAndStartsAThread(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{})
	if sess.ID() != "thr_fake" {
		t.Errorf("ID = %q, want the thread id codex assigned", sess.ID())
	}
	events := until(t, sess.Events(), ap.AgentEventRunStarted)
	if events[0].RunID != "run_1" || events[0].ChatID != "chat_1" {
		t.Errorf("event not stamped: run=%q chat=%q", events[0].RunID, events[0].ChatID)
	}
}

func TestCodexTurnStreamsTextAndCompletes(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{})
	if err := sess.Prompt(context.Background(), driver.TextInput("hello")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if got := deltas(events); got != "you said hello" {
		t.Errorf("text = %q", got)
	}
	if _, ok := findEvent(events, ap.AgentEventTurnStarted); !ok {
		t.Error("no turn.started")
	}
	if _, ok := findEvent(events, ap.AgentEventUsageUpdated); !ok {
		t.Error("no usage.updated")
	}
	done, _ := findEvent(events, ap.AgentEventTurnCompleted)
	p, _ := ap.PayloadAs[ap.TurnCompletedPayload](done, ap.AgentEventTurnCompleted)
	if p.StopReason != "completed" || !p.HasOutput {
		t.Errorf("turn completed = %+v", p)
	}
	for _, e := range events {
		if e.RunID != "run_1" {
			t.Errorf("%s not stamped with run id", e.Type)
		}
	}
}

func TestCodexUnknownNotificationsAreTolerated(t *testing.T) {
	// The fake sends an unknown notification and a known one carrying
	// fields no schema mentions, before and during every turn.
	sess := openCodex(t, "normal", driver.SessionConfig{})
	for i := 0; i < 2; i++ {
		if err := sess.Prompt(context.Background(), driver.TextInput("again")); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		until(t, sess.Events(), ap.AgentEventTurnCompleted)
	}
}

func TestCodexApprovalAllowAndDeny(t *testing.T) {
	cases := []struct {
		name     string
		res      driver.Resolution
		decision string
		status   ap.ToolInvocationStatus
	}{
		{"allow", driver.Allow(), "accept", ap.ToolInvocationStatusCompleted},
		{"allow for session", driver.AllowFor(driver.ScopeSession), "acceptForSession", ap.ToolInvocationStatusCompleted},
		{"always rounds to session", driver.AllowFor(driver.ScopeAlways), "acceptForSession", ap.ToolInvocationStatusCompleted},
		{"deny", driver.Deny("no"), "decline", ap.ToolInvocationStatusCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := openCodex(t, "normal", driver.SessionConfig{})
			if err := sess.Prompt(context.Background(), driver.TextInput("run touch x")); err != nil {
				t.Fatalf("prompt: %v", err)
			}
			events := until(t, sess.Events(), ap.AgentEventApprovalRequired)
			if _, ok := findEvent(events, ap.AgentEventToolStarted); !ok {
				t.Error("no tool.started before the approval")
			}
			req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)
			if req.ToolInvocationID != "call_1" {
				t.Errorf("invocation id = %q, want the item id", req.ToolInvocationID)
			}
			if req.Arguments["command"] != "touch x" {
				t.Errorf("arguments = %v", req.Arguments)
			}

			if err := sess.Resolve(context.Background(), req.ToolInvocationID, tc.res); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if err := sess.Resolve(context.Background(), req.ToolInvocationID, tc.res); err == nil {
				t.Error("resolving the same request twice succeeded")
			}

			rest := until(t, sess.Events(), ap.AgentEventTurnCompleted)
			if got := deltas(rest); got != "decision="+tc.decision {
				t.Errorf("codex received %q, want decision=%s", got, tc.decision)
			}
			resolved, ok := findEvent(rest, ap.AgentEventApprovalResolved)
			if !ok {
				t.Fatal("no approval.resolved")
			}
			rp, _ := ap.PayloadAs[ap.ApprovalResolvedPayload](resolved, ap.AgentEventApprovalResolved)
			if rp.Decision != string(tc.res.Decision) {
				t.Errorf("resolved decision = %q", rp.Decision)
			}
			tool, ok := findEvent(rest, ap.AgentEventToolCompleted)
			if !ok {
				t.Fatal("no tool.completed")
			}
			tp, _ := ap.PayloadAs[ap.ToolCompletedPayload](tool, ap.AgentEventToolCompleted)
			if tp.Status != tc.status || tp.ToolInvocationID != "call_1" {
				t.Errorf("tool completed = %+v", tp)
			}
		})
	}
}

func TestCodexResolveUnknownIsAnError(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{})
	if err := sess.Resolve(context.Background(), "nope", driver.Allow()); err == nil {
		t.Error("resolved a request nobody asked")
	}
}

func TestCodexInterruptEndsTheTurnAndKeepsTheThread(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{})
	if err := sess.Prompt(context.Background(), driver.TextInput("hang")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	until(t, sess.Events(), ap.AgentEventTurnStarted)
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	p, _ := ap.PayloadAs[ap.TurnCompletedPayload](events[len(events)-1], ap.AgentEventTurnCompleted)
	if p.StopReason != "interrupted" {
		t.Errorf("stop reason = %q, want interrupted", p.StopReason)
	}

	if err := sess.Prompt(context.Background(), driver.TextInput("after")); err != nil {
		t.Fatalf("prompt after interrupt: %v", err)
	}
	if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "you said after" {
		t.Errorf("text after interrupt = %q", got)
	}
}

func TestCodexInterruptWithdrawsAParkedApproval(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("run touch x"))
	events := until(t, sess.Events(), ap.AgentEventApprovalRequired)
	req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)

	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	rest := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	resolved, ok := findEvent(rest, ap.AgentEventApprovalResolved)
	if !ok {
		t.Fatalf("parked approval never withdrawn; got %v", typesOf(rest))
	}
	rp, _ := ap.PayloadAs[ap.ApprovalResolvedPayload](resolved, ap.AgentEventApprovalResolved)
	if rp.Decision != "cancelled" {
		t.Errorf("decision = %q, want cancelled", rp.Decision)
	}
	if err := sess.Resolve(context.Background(), req.ToolInvocationID, driver.Allow()); err == nil {
		t.Error("resolving a withdrawn request succeeded")
	}
}

func TestCodexPromptMidTurnSteers(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("hang"))
	until(t, sess.Events(), ap.AgentEventTurnStarted)
	if err := sess.Prompt(context.Background(), driver.TextInput("change course")); err != nil {
		t.Fatalf("steer: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if got := deltas(events); got != "steered: change course" {
		t.Errorf("text = %q, want the steer acknowledged in the same turn", got)
	}
	if _, ok := findEvent(events, ap.AgentEventTurnStarted); ok {
		t.Error("steering started a second turn")
	}
}

func TestCodexResumeByThreadID(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{ResumeSessionID: "thr_old"})
	if sess.ID() != "thr_old" {
		t.Errorf("ID = %q, want the resumed thread", sess.ID())
	}
	_ = sess.Prompt(context.Background(), driver.TextInput("still there?"))
	if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "you said still there? (history: 1 turn)" {
		t.Errorf("text = %q", got)
	}

	b, _ := codexBackend(t, "normal")
	if _, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir(), ResumeSessionID: "thr_missing"}); err == nil {
		t.Error("resuming an unknown thread succeeded")
	}
}

func TestCodexProcessExitClosesEvents(t *testing.T) {
	sess := openCodex(t, "exit-on-turn", driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("die"))
	var got []ap.AgentEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				if _, found := findEvent(got, ap.AgentEventError); !found {
					t.Errorf("stream closed without saying why; got %v", typesOf(got))
				}
				return
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("events never closed after codex exited; got %v", typesOf(got))
		}
	}
}

func TestCodexCloseClosesEvents(t *testing.T) {
	b, _ := codexBackend(t, "normal")
	sess, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	_ = sess.Close()
	select {
	case _, open := <-drain(sess.Events()):
		if open {
			t.Error("still open")
		}
	case <-time.After(3 * time.Second):
		t.Error("events never closed")
	}
	if err := sess.Prompt(context.Background(), driver.TextInput("x")); err == nil {
		t.Error("prompt after close succeeded")
	}
}

func TestCodexKillEndsAWedgedAgentAtOnce(t *testing.T) {
	b, _ := codexBackend(t, "normal")
	b.Env = append(b.Env, ignoreEOFEnv+"=1")
	sess, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Prompt(context.Background(), driver.TextInput("run touch x"))
	events := until(t, sess.Events(), ap.AgentEventApprovalRequired)
	req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)

	got := killMidWork(t, sess)
	errEv, ok := findEvent(got, ap.AgentEventError)
	if !ok {
		t.Fatalf("killed with no error event: %v", typesOf(got))
	}
	if p, _ := ap.PayloadAs[ap.ErrorPayload](errEv, ap.AgentEventError); p.Code != "process_exited" {
		t.Errorf("error = %+v", p)
	}
	if err := sess.Resolve(context.Background(), req.ToolInvocationID, driver.Allow()); err == nil {
		t.Error("resolving a request of a killed agent succeeded")
	}
}

func TestCodexKillAfterCloseIsHarmless(t *testing.T) {
	killAfterClose(t, openCodex(t, "normal", driver.SessionConfig{}))
}

func TestCodexSendsApprovalPolicyThatAsks(t *testing.T) {
	// The fake echoes the policy and reviewer it was given as the reply text.
	sess := openCodex(t, "normal", driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("policy?"))
	if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != `policy="untrusted" reviewer=user` {
		t.Errorf("thread started with %s; the default must ask a person", got)
	}
}

func TestCodexBackendCapabilities(t *testing.T) {
	b := &driver.CodexBackend{}
	if b.Kind() != driver.KindCodex {
		t.Errorf("kind = %q", b.Kind())
	}
	c := b.Capabilities()
	if !c.Steer || !c.Approvals || !c.Interrupt || !c.Resume || c.Tools {
		t.Errorf("capabilities = %+v", c)
	}
}

// --- the fake app-server ---

type fakeCodex struct {
	out  *json.Encoder
	mode string

	thread string
	policy json.RawMessage
	review string
	turns  int

	active     string
	nextServer int
	// waiting is the server request awaiting a client response, and what to
	// do with the answer.
	waiting map[string]func(result json.RawMessage)
}

func runFakeCodex(mode string) {
	f := &fakeCodex{out: json.NewEncoder(os.Stdout), mode: mode, waiting: map[string]func(json.RawMessage){}}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for in.Scan() {
		var m codexapp.Message
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" {
			if k := f.waiting[string(m.ID)]; k != nil {
				delete(f.waiting, string(m.ID))
				k(m.Result)
			}
			continue
		}
		f.handle(m)
	}
}

func (f *fakeCodex) reply(id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	_ = f.out.Encode(codexapp.Message{ID: id, Result: raw})
}

func (f *fakeCodex) fail(id json.RawMessage, msg string) {
	_ = f.out.Encode(codexapp.Message{ID: id, Error: &codexapp.Error{Code: -32600, Message: msg}})
}

func (f *fakeCodex) notify(method string, params any) {
	raw, _ := json.Marshal(params)
	_ = f.out.Encode(codexapp.Message{Method: method, Params: raw})
}

func (f *fakeCodex) request(method string, params any, then func(json.RawMessage)) {
	id := json.RawMessage(strings.TrimSpace(mustMarshal(f.nextServer)))
	f.nextServer++
	f.waiting[string(id)] = then
	raw, _ := json.Marshal(params)
	_ = f.out.Encode(codexapp.Message{ID: id, Method: method, Params: raw})
}

func mustMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *fakeCodex) threadJSON(id string, turns int) map[string]any {
	ts := make([]any, turns)
	for i := range ts {
		ts[i] = map[string]any{"id": "old_turn", "items": []any{}, "status": "completed"}
	}
	return map[string]any{
		"id": id, "sessionId": id, "preview": "", "ephemeral": false, "modelProvider": "fake",
		"createdAt": 1, "updatedAt": 1, "status": map[string]any{"type": "idle"}, "cwd": "/",
		"cliVersion": "0.0.0", "source": "vscode", "turns": ts,
		"someFutureField": map[string]any{"nested": true},
	}
}

func (f *fakeCodex) handle(m codexapp.Message) {
	switch m.Method {
	case codexapp.MethodInitialize:
		f.reply(m.ID, map[string]any{"userAgent": "fake", "codexHome": "/tmp/fake", "platformFamily": "unix", "platformOs": "linux"})
	case codexapp.MethodInitialized:
		f.notify("remoteControl/status/changed", map[string]any{"status": "disabled"})
	case codexapp.MethodThreadStart, codexapp.MethodThreadResume:
		var p struct {
			ThreadID          string          `json:"threadId"`
			ApprovalPolicy    json.RawMessage `json:"approvalPolicy"`
			ApprovalsReviewer string          `json:"approvalsReviewer"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.policy, f.review = p.ApprovalPolicy, p.ApprovalsReviewer
		f.thread, f.turns = "thr_fake", 0
		if m.Method == codexapp.MethodThreadResume {
			if p.ThreadID != "thr_old" {
				f.fail(m.ID, "no rollout found for thread id "+p.ThreadID)
				return
			}
			f.thread, f.turns = p.ThreadID, 1
		}
		f.reply(m.ID, map[string]any{
			"thread": f.threadJSON(f.thread, f.turns), "model": "fake-model", "modelProvider": "fake",
			"cwd": "/", "approvalPolicy": p.ApprovalPolicy, "approvalsReviewer": "user",
			"sandbox": map[string]any{"type": "readOnly"}, "runtimeWorkspaceRoots": []string{"/"},
		})
		f.notify(codexapp.MethodThreadStarted, map[string]any{"thread": f.threadJSON(f.thread, 0)})
	case codexapp.MethodTurnStart:
		var p codexapp.TurnStartParams
		_ = json.Unmarshal(m.Params, &p)
		text := inputText(p.Input)
		turn := "turn_" + text
		f.reply(m.ID, map[string]any{"turn": map[string]any{"id": turn, "items": []any{}, "status": "inProgress"}})
		if f.mode == "exit-on-turn" {
			os.Exit(3)
		}
		f.active = turn
		f.notify("some/unknown/notification", map[string]any{"threadId": f.thread, "x": 1})
		f.notify(codexapp.MethodTurnStarted, map[string]any{"threadId": f.thread, "turn": map[string]any{"id": turn, "items": []any{}, "status": "inProgress"}})
		f.notify(codexapp.MethodItemStarted, map[string]any{"threadId": f.thread, "turnId": turn, "startedAtMs": 1,
			"item": map[string]any{"type": "userMessage", "id": "u1", "content": []any{}}})
		switch {
		case text == "hang":
			return
		case strings.HasPrefix(text, "run "):
			cmd := strings.TrimPrefix(text, "run ")
			item := func(status string) map[string]any {
				return map[string]any{"type": "commandExecution", "id": "call_1", "command": cmd, "cwd": "/w",
					"commandActions": []any{}, "status": status}
			}
			f.notify(codexapp.MethodItemStarted, map[string]any{"threadId": f.thread, "turnId": turn, "startedAtMs": 1, "item": item("inProgress")})
			f.request(codexapp.MethodItemCommandExecutionRequestApproval, map[string]any{
				"threadId": f.thread, "turnId": turn, "itemId": "call_1", "startedAtMs": 1, "command": cmd, "cwd": "/w",
				"availableDecisions": []any{"accept", "cancel"},
			}, func(result json.RawMessage) {
				var r struct{ Decision string }
				_ = json.Unmarshal(result, &r)
				if f.active != turn {
					return // interrupted meanwhile
				}
				status := "completed"
				if !strings.HasPrefix(r.Decision, "accept") {
					status = "declined"
				}
				f.notify(codexapp.MethodServerRequestResolved, map[string]any{"threadId": f.thread, "requestId": 0})
				f.notify(codexapp.MethodItemCompleted, map[string]any{"threadId": f.thread, "turnId": turn, "completedAtMs": 2, "item": item(status)})
				f.finish(turn, "decision="+r.Decision)
			})
			return
		case text == "policy?":
			f.finish(turn, "policy="+string(f.policy)+" reviewer="+f.review)
		default:
			reply := "you said " + text
			if f.turns > 0 {
				reply += " (history: 1 turn)"
			}
			f.finish(turn, reply)
		}
	case codexapp.MethodTurnSteer:
		var p codexapp.TurnSteerParams
		_ = json.Unmarshal(m.Params, &p)
		if p.ExpectedTurnID != f.active || f.active == "" {
			f.fail(m.ID, "no active turn to steer")
			return
		}
		f.reply(m.ID, map[string]any{"turnId": f.active})
		f.finish(f.active, "steered: "+inputText(p.Input))
	case codexapp.MethodTurnInterrupt:
		var p codexapp.TurnInterruptParams
		_ = json.Unmarshal(m.Params, &p)
		f.reply(m.ID, map[string]any{})
		if p.TurnID == f.active {
			turn := f.active
			f.active = ""
			// Codex withdraws a pending approval when the turn ends.
			for id := range f.waiting {
				delete(f.waiting, id)
				f.notify(codexapp.MethodServerRequestResolved, map[string]any{"threadId": f.thread, "requestId": json.RawMessage(id)})
			}
			f.notify(codexapp.MethodTurnCompleted, map[string]any{"threadId": f.thread,
				"turn": map[string]any{"id": turn, "items": []any{}, "status": "interrupted"}})
		}
	default:
		f.fail(m.ID, "unknown method "+m.Method)
	}
}

func (f *fakeCodex) finish(turn, text string) {
	f.notify(codexapp.MethodItemStarted, map[string]any{"threadId": f.thread, "turnId": turn, "startedAtMs": 1,
		"item": map[string]any{"type": "agentMessage", "id": "msg_1", "text": ""}})
	f.notify(codexapp.MethodItemAgentMessageDelta, map[string]any{"threadId": f.thread, "turnId": turn, "itemId": "msg_1", "delta": text})
	f.notify(codexapp.MethodItemCompleted, map[string]any{"threadId": f.thread, "turnId": turn, "completedAtMs": 2,
		"item": map[string]any{"type": "agentMessage", "id": "msg_1", "text": text, "brandNewField": 1}})
	f.notify(codexapp.MethodThreadTokenUsageUpdated, map[string]any{"threadId": f.thread, "turnId": turn, "tokenUsage": map[string]any{
		"total": map[string]any{"totalTokens": 15, "inputTokens": 10, "cachedInputTokens": 0, "outputTokens": 5, "reasoningOutputTokens": 0},
		"last":  map[string]any{"totalTokens": 15, "inputTokens": 10, "cachedInputTokens": 0, "outputTokens": 5, "reasoningOutputTokens": 0}}})
	f.active = ""
	f.notify(codexapp.MethodTurnCompleted, map[string]any{"threadId": f.thread,
		"turn": map[string]any{"id": turn, "items": []any{}, "status": "completed"}})
}

func inputText(in []codexapp.UserInput) string {
	var parts []string
	for _, u := range in {
		if t, ok := u.AsTextUserInput(); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, " ")
}
