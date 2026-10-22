package driver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
	"github.com/inference-sh/agentprotocol/protocol/codexapp"
	"github.com/inference-sh/agentprotocol/report/usage"
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

// Each turn's usage is the growth of the thread's total; the window's fill
// is the last call against the model's window; the account and limits are
// read once and the limits pushed after.
func TestCodexReportsUsage(t *testing.T) {
	sess := openCodex(t, "", driver.SessionConfig{})
	if err := sess.Prompt(context.Background(), driver.TextInput("one")); err != nil {
		t.Fatal(err)
	}
	first := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if err := sess.Prompt(context.Background(), driver.TextInput("two")); err != nil {
		t.Fatal(err)
	}
	second := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	all := append(append([]ap.AgentEvent{}, first...), second...)

	for i, events := range [][]ap.AgentEvent{first, second} {
		ev, ok := findEvent(events, ap.AgentEventUsageUpdated)
		if !ok {
			t.Fatalf("turn %d: no usage.updated", i+1)
		}
		u, _ := ap.PayloadAs[ap.UsageUpdatedPayload](ev, ap.AgentEventUsageUpdated)
		if u.TotalTokens != 15 || u.Status != ap.TurnUsageComplete {
			t.Errorf("turn %d usage = %+v, want the 15 tokens it added", i+1, u)
		}
	}
	ev, ok := findEvent(first, ap.AgentEventContextUpdated)
	if !ok {
		t.Fatal("no context.updated")
	}
	if c, _ := ap.PayloadAs[ap.ContextUpdatedPayload](ev, ap.AgentEventContextUpdated); c.UsedTokens != 15 || c.MaxTokens != 272000 {
		t.Errorf("context = %+v", c)
	}
	ev, ok = findEvent(all, ap.AgentEventAccountUpdated)
	if !ok {
		t.Fatal("no account.updated")
	}
	if a, _ := ap.PayloadAs[ap.AccountUpdatedPayload](ev, ap.AgentEventAccountUpdated); a.Account.Email != "a@x.dev" || a.Account.Plan != "pro" {
		t.Errorf("account = %+v", a.Account)
	}
	// The read and the pushes race; whichever comes first, the pushed
	// figure is the one that stands.
	var st usage.State
	n := 0
	for _, ev := range all {
		if l, ok := ap.PayloadAs[ap.LimitsUpdatedPayload](ev, ap.AgentEventLimitsUpdated); ok {
			st.ApplyUpdate(l, time.Now())
			n++
		}
	}
	if n == 0 || st.Limits == nil || len(st.Limits.Windows) != 2 {
		t.Fatalf("limits = %+v from %d updates", st.Limits, n)
	}
	if p, w := st.Limits.Windows[0], st.Limits.Windows[1]; p.ID != "primary" || p.UsedPercent != 38 || p.DurationMinutes != 300 || w.ID != "secondary" || w.UsedPercent != 8 {
		t.Errorf("limits = %+v", st.Limits.Windows)
	}
}

// A spawned agent is reported as a subagent of the call that spawned it; its
// own output reaches the caller only when asked for, and its approval
// always names it.
func TestCodexReportsSubagents(t *testing.T) {
	for _, output := range []bool{false, true} {
		b, _ := codexBackend(t, "")
		b.SubagentOutput = output
		sess, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Prompt(context.Background(), driver.TextInput("spawn")); err != nil {
			t.Fatal(err)
		}
		before := until(t, sess.Events(), ap.AgentEventApprovalRequired)
		ask, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](before[len(before)-1], ap.AgentEventApprovalRequired)
		if ask.SubagentID != "thr_child" {
			t.Errorf("output=%v: approval = %+v, want it to name thr_child", output, ask)
		}
		if err := sess.Resolve(context.Background(), ask.ToolInvocationID, driver.Resolution{Decision: ap.InterruptResolutionAllow}); err != nil {
			t.Fatal(err)
		}
		events := append(before, until(t, sess.Events(), ap.AgentEventTurnCompleted)...)

		ev, ok := findEvent(events, ap.AgentEventSubagentStarted)
		if !ok {
			t.Fatalf("output=%v: no subagent.started in %v", output, typesOf(events))
		}
		st, _ := ap.PayloadAs[ap.SubagentStartedPayload](ev, ap.AgentEventSubagentStarted)
		if st.SubagentID != "thr_child" || st.ParentToolInvocationID != "collab_1" || st.Description != "look around" {
			t.Errorf("started = %+v", st)
		}
		if countType(events, ap.AgentEventSubagentStarted) != 1 || countType(events, ap.AgentEventSubagentCompleted) != 1 {
			t.Errorf("output=%v: events = %v", output, typesOf(events))
		}
		ev, _ = findEvent(events, ap.AgentEventSubagentCompleted)
		if c, _ := ap.PayloadAs[ap.SubagentCompletedPayload](ev, ap.AgentEventSubagentCompleted); c.Status != ap.SubagentCompleted || c.Summary != "found it" {
			t.Errorf("completed = %+v", c)
		}
		var usage *ap.SubagentUsage
		var childText string
		var childTools, parentTools int
		for _, ev := range events {
			if p, ok := ap.PayloadAs[ap.SubagentUpdatedPayload](ev, ap.AgentEventSubagentUpdated); ok && p.Usage != nil {
				usage = p.Usage
			}
			if p, ok := ap.PayloadAs[ap.ContentDeltaPayload](ev, ap.AgentEventContentDelta); ok && p.SubagentID == "thr_child" {
				childText += p.Delta
			}
			if p, ok := ap.PayloadAs[ap.ToolStartedPayload](ev, ap.AgentEventToolStarted); ok {
				switch {
				case p.SubagentID == "thr_child":
					childTools++
				case p.ToolInvocationID == "collab_1" && p.ToolName == "spawnAgent":
					parentTools++
				case p.ToolInvocationID == "c_call":
					t.Errorf("output=%v: the child's tool call reached the caller as the agent's", output)
				}
			}
		}
		if usage == nil || usage.TotalTokens != 700 {
			t.Errorf("output=%v: subagent usage = %+v", output, usage)
		}
		if parentTools != 1 {
			t.Errorf("output=%v: the spawn call was reported %d times as a tool call", output, parentTools)
		}
		if output != (childText == "child speaking" && childTools == 1) {
			t.Errorf("output=%v: child text %q, %d child tool call(s)", output, childText, childTools)
		}
		if got := deltas(events); got != "the child found it" && !output {
			t.Errorf("the agent's text = %q", got)
		}
		sess.Close()
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

func TestCodexWriteStdinApprovalSaysSo(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("stdin y"))
	events := until(t, sess.Events(), ap.AgentEventApprovalRequired)
	req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)
	if req.Arguments["kind"] != "writeStdin" || req.Arguments["command"] != "y" {
		t.Errorf("arguments = %v, want the stdin kind named", req.Arguments)
	}
	_ = sess.Resolve(context.Background(), req.ToolInvocationID, driver.Allow())
	if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "decision=accept" {
		t.Errorf("codex received %q", got)
	}
}

func TestCodexLegacyApprovalDecisions(t *testing.T) {
	cases := []struct {
		name string
		res  driver.Resolution
		want string
	}{
		{"allow", driver.Allow(), `"approved"`},
		{"allow for session", driver.AllowFor(driver.ScopeSession), `"approved_for_session"`},
		{"deny", driver.Deny("not now"), `{"denied":{"rejection":"not now"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := openCodex(t, "normal", driver.SessionConfig{})
			_ = sess.Prompt(context.Background(), driver.TextInput("legacy touch x"))
			events := until(t, sess.Events(), ap.AgentEventApprovalRequired)
			req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)
			if err := sess.Resolve(context.Background(), req.ToolInvocationID, tc.res); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "decision="+tc.want {
				t.Errorf("codex received %q, want decision=%s", got, tc.want)
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
				e, found := findEvent(got, ap.AgentEventError)
				if !found {
					t.Errorf("stream closed without saying why; got %v", typesOf(got))
				} else if p, _ := ap.PayloadAs[ap.ErrorPayload](e, ap.AgentEventError); !strings.Contains(p.Message, "backend went away") {
					t.Errorf("exit error %q does not quote codex's stderr", p.Message)
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

func TestCodexSkipApprovalsSendsNever(t *testing.T) {
	sess := openCodex(t, "normal", driver.SessionConfig{Approvals: driver.ApprovalsSkip})
	_ = sess.Prompt(context.Background(), driver.TextInput("policy?"))
	if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != `policy="never" reviewer=user` {
		t.Errorf("thread started with %s, want the never policy", got)
	}
}

func TestCodexPassesThreadConfigOnStartAndResume(t *testing.T) {
	for _, resume := range []string{"", "thr_old"} {
		b, _ := codexBackend(t, "normal")
		b.Config = map[string]any{"bypass_hook_trust": true, "model_reasoning_effort": "low"}
		sess, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir(), ResumeSessionID: resume})
		if err != nil {
			t.Fatalf("open (resume %q): %v", resume, err)
		}
		_ = sess.Prompt(context.Background(), driver.TextInput("config?"))
		got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted))
		if want := `config={"bypass_hook_trust":true,"model_reasoning_effort":"low"}`; got != want {
			t.Errorf("resume %q: thread got %s, want %s", resume, got, want)
		}
		_ = sess.Close()
	}

	// No Config sends none, so the user's own configuration stands.
	sess := openCodex(t, "normal", driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("config?"))
	if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "config=" {
		t.Errorf("default thread config = %s, want none", got)
	}
}

func TestCodexBackendCapabilities(t *testing.T) {
	b := &driver.CodexBackend{}
	if b.Kind() != driver.KindCodex {
		t.Errorf("kind = %q", b.Kind())
	}
	want := driver.Capabilities{Steer: true, Approvals: true, Interrupt: true, Resume: true, Tools: true, OnlyCallerTools: true, Images: true}
	if c := b.Capabilities(); c != want {
		t.Errorf("capabilities = %+v, want %+v", c, want)
	}
	// codex before 0.146.0 always offers update_plan.
	b.Version = "codex-cli 0.137.0"
	if c := b.Capabilities(); !c.Tools || c.OnlyCallerTools {
		t.Errorf("capabilities on 0.137.0 = %+v", c)
	}
}

func codexEchoTool() []ap.Tool {
	required := []string{"text"}
	return []ap.Tool{{Type: "function", Function: ap.ToolFunction{
		Name: "echo", Description: "Echo the text back.",
		Parameters: ap.ToolParameters{Type: "object", Properties: ap.ToolParameterProperties{
			"text": {Type: "string", Description: "what to echo"},
		}, Required: &required},
	}}}
}

// A caller tool is a codex dynamic tool: declared on thread/start with the
// experimental API on, called with item/tool/call, run by CallTool with no
// approval, and reported once under its own name and codex's call id.
func TestCodexCallerToolRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		out     driver.ToolOutput
		err     error
		success bool
		text    string
		status  ap.ToolInvocationStatus
	}{
		{name: "ok", out: driver.ToolOutput{Text: "echo: PING"}, success: true, text: "echo: PING", status: ap.ToolInvocationStatusCompleted},
		{name: "is error", out: driver.ToolOutput{Text: "no such word", IsError: true}, text: "no such word", status: ap.ToolInvocationStatusFailed},
		{name: "error", err: errors.New("lookup service down"), text: "lookup service down", status: ap.ToolInvocationStatusFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []driver.ToolCall
			sess := openCodex(t, "", driver.SessionConfig{
				Tools: codexEchoTool(),
				CallTool: func(_ context.Context, call driver.ToolCall) (driver.ToolOutput, error) {
					mu.Lock()
					calls = append(calls, call)
					mu.Unlock()
					return c.out, c.err
				},
			})
			if err := sess.Prompt(context.Background(), driver.TextInput(`tool {"text":"PING"}`)); err != nil {
				t.Fatal(err)
			}
			events := until(t, sess.Events(), ap.AgentEventTurnCompleted)

			if _, asked := findEvent(events, ap.AgentEventApprovalRequired); asked {
				t.Error("a caller tool raised an approval")
			}
			mu.Lock()
			if len(calls) != 1 || calls[0].Name != "echo" || calls[0].ToolInvocationID != "call_dyn" || string(calls[0].Arguments) != `{"text":"PING"}` {
				t.Errorf("calls = %+v", calls)
			}
			mu.Unlock()

			if n, m := countType(events, ap.AgentEventToolStarted), countType(events, ap.AgentEventToolCompleted); n != 1 || m != 1 {
				t.Errorf("%d tool.started and %d tool.completed, want one each", n, m)
			}
			started, _ := findEvent(events, ap.AgentEventToolStarted)
			sp, _ := ap.PayloadAs[ap.ToolStartedPayload](started, ap.AgentEventToolStarted)
			if sp.ToolName != "echo" || sp.ToolInvocationID != "call_dyn" || sp.Arguments["text"] != "PING" {
				t.Errorf("tool started = %+v", sp)
			}
			done, _ := findEvent(events, ap.AgentEventToolCompleted)
			dp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
			if dp.ToolName != "echo" || dp.ToolInvocationID != "call_dyn" || dp.Status != c.status || dp.Result != c.text || dp.DurationMs != 7 {
				t.Errorf("tool completed = %+v", dp)
			}

			// The fake replies with the response codex got.
			var res codexapp.DynamicToolCallResponse
			if err := json.Unmarshal([]byte(strings.TrimPrefix(deltas(events), "response=")), &res); err != nil {
				t.Fatalf("response %q: %v", deltas(events), err)
			}
			if res.Success != c.success || len(res.ContentItems) != 1 {
				t.Fatalf("response = %+v", res)
			}
			if item, ok := res.ContentItems[0].AsInputTextDynamicToolCallOutputContentItem(); !ok || item.Text != c.text {
				t.Errorf("content = %s", res.ContentItems[0].Raw)
			}
		})
	}
}

func TestCodexCallerToolsDeclaredWithExperimentalAPI(t *testing.T) {
	sess := openCodex(t, "", driver.SessionConfig{
		Tools:    codexEchoTool(),
		CallTool: func(context.Context, driver.ToolCall) (driver.ToolOutput, error) { return driver.ToolOutput{}, nil },
	})
	_ = sess.Prompt(context.Background(), driver.TextInput("init?"))
	got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted))
	want := `capabilities={"experimentalApi":true} tools=[{"description":"Echo the text back.",` +
		`"inputSchema":{"properties":{"text":{"description":"what to echo","title":"","type":"string"}},"required":["text"],"type":"object"},` +
		`"name":"echo","type":"function"}]`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// Without tools nothing experimental is asked for.
	plain := openCodex(t, "", driver.SessionConfig{})
	_ = plain.Prompt(context.Background(), driver.TextInput("init?"))
	if got := deltas(until(t, plain.Events(), ap.AgentEventTurnCompleted)); got != "capabilities= tools=" {
		t.Errorf("without tools: %s", got)
	}
}

func TestCodexCallerToolsNeedCallTool(t *testing.T) {
	b, _ := codexBackend(t, "")
	_, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir(), Tools: codexEchoTool()})
	if err == nil || !strings.Contains(err.Error(), "CallTool") {
		t.Errorf("open without CallTool = %v", err)
	}
}

// Interrupting the turn ends the context of a caller tool still running.
func TestCodexInterruptCancelsACallerTool(t *testing.T) {
	running := make(chan struct{})
	ended := make(chan error, 1)
	sess := openCodex(t, "", driver.SessionConfig{
		Tools: codexEchoTool(),
		CallTool: func(ctx context.Context, _ driver.ToolCall) (driver.ToolOutput, error) {
			close(running)
			<-ctx.Done()
			ended <- ctx.Err()
			return driver.ToolOutput{}, ctx.Err()
		},
	})
	_ = sess.Prompt(context.Background(), driver.TextInput(`tool {"text":"slow"}`))
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the caller tool never ran")
	}
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ctx ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted call's context never ended")
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if done, ok := findEvent(events, ap.AgentEventToolCompleted); !ok {
		t.Error("the interrupted call was left open")
	} else if p, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted); p.Status != ap.ToolInvocationStatusCancelled {
		t.Errorf("interrupted call = %+v", p)
	}
}

// OnlyCallerTools turns codex's own tools off at launch, starts the thread
// and every turn with no environment, and disables the user's MCP servers.
func TestCodexOnlyCallerToolsTurnsCodexsOwnOff(t *testing.T) {
	b, _ := codexBackend(t, "")
	b.Args = []string{"-c", `model="x"`}
	b.Config = map[string]any{"model_reasoning_effort": "low"}
	sess, err := b.Open(context.Background(), driver.SessionConfig{
		WorkDir: t.TempDir(), OnlyCallerTools: true, Tools: codexEchoTool(),
		CallTool: func(context.Context, driver.ToolCall) (driver.ToolOutput, error) { return driver.ToolOutput{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.Prompt(context.Background(), driver.TextInput("envs?"))
	got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted))
	for _, want := range []string{"thread=[] turn=[] ", `-c model="x" -c features.shell_tool=false`, "-c tools.update_plan.enabled=false", "-c orchestrator.skills.enabled=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	_ = sess.Prompt(context.Background(), driver.TextInput("config?"))
	if got := deltas(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != `config={"mcp_servers.outside.enabled":false,"model_reasoning_effort":"low"}` {
		t.Errorf("thread %s", got)
	}

	// Without it the environment is codex's default.
	plain := openCodex(t, "", driver.SessionConfig{})
	_ = plain.Prompt(context.Background(), driver.TextInput("envs?"))
	if got := deltas(until(t, plain.Events(), ap.AgentEventTurnCompleted)); !strings.HasPrefix(got, "thread= turn= args=app-server") || strings.Contains(got, "shell_tool") {
		t.Errorf("default: %s", got)
	}
}

// An older codex keeps update_plan, so OnlyCallerTools is refused on it even
// when Version did not say which codex this is.
func TestCodexOnlyCallerToolsRefusedOnOldCodex(t *testing.T) {
	b, _ := codexBackend(t, "old")
	_, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir(), OnlyCallerTools: true})
	if err == nil || !strings.Contains(err.Error(), "0.146.0") || !strings.Contains(err.Error(), "0.137.0") {
		t.Errorf("open on 0.137.0 = %v", err)
	}
	b.Version = "codex-cli 0.137.0"
	if _, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir(), OnlyCallerTools: true}); err == nil {
		t.Error("open with Version 0.137.0 succeeded")
	}
}

// --- the fake app-server ---

type fakeCodex struct {
	out  *json.Encoder
	mode string

	thread string
	policy json.RawMessage
	review string
	config json.RawMessage
	turns  int
	// What initialize, thread/start and the last turn/start carried, for
	// the tests to read back.
	initCaps     json.RawMessage
	dynamicTools json.RawMessage
	threadEnvs   json.RawMessage
	turnEnvs     json.RawMessage
	// used is the thread's running token total, as codex reports it.
	used int

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
		var p struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.initCaps = p.Capabilities
		ua := "agentprotocol/0.156.1 (fake)"
		if f.mode == "old" {
			ua = "agentprotocol/0.137.0 (fake)"
		}
		f.reply(m.ID, map[string]any{"userAgent": ua, "codexHome": "/tmp/fake", "platformFamily": "unix", "platformOs": "linux"})
	case codexapp.MethodConfigRead:
		f.reply(m.ID, map[string]any{"origins": map[string]any{}, "config": map[string]any{
			"model": "x", "mcp_servers": map[string]any{"outside": map[string]any{"url": "http://127.0.0.1:1/mcp", "enabled": true}},
		}})
	case codexapp.MethodInitialized:
		f.notify("remoteControl/status/changed", map[string]any{"status": "disabled"})
	case codexapp.MethodThreadStart, codexapp.MethodThreadResume:
		var p struct {
			ThreadID          string          `json:"threadId"`
			ApprovalPolicy    json.RawMessage `json:"approvalPolicy"`
			ApprovalsReviewer string          `json:"approvalsReviewer"`
			Config            json.RawMessage `json:"config"`
			DynamicTools      json.RawMessage `json:"dynamicTools"`
			Environments      json.RawMessage `json:"environments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.policy, f.review, f.config = p.ApprovalPolicy, p.ApprovalsReviewer, p.Config
		f.dynamicTools, f.threadEnvs = p.DynamicTools, p.Environments
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
		var envs struct {
			Environments json.RawMessage `json:"environments"`
		}
		_ = json.Unmarshal(m.Params, &envs)
		f.turnEnvs = envs.Environments
		text := inputText(p.Input)
		turn := "turn_" + text
		f.reply(m.ID, map[string]any{"turn": map[string]any{"id": turn, "items": []any{}, "status": "inProgress"}})
		if f.mode == "exit-on-turn" {
			os.Stderr.WriteString("codex: backend went away\n")
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
		case strings.HasPrefix(text, "stdin "):
			// A codex 0.156 approval for input to a running terminal.
			f.request(codexapp.MethodItemCommandExecutionRequestApproval, map[string]any{
				"threadId": f.thread, "turnId": turn, "itemId": "call_1", "startedAtMs": 1,
				"command": strings.TrimPrefix(text, "stdin "), "kind": "writeStdin",
			}, func(result json.RawMessage) {
				var r struct{ Decision string }
				_ = json.Unmarshal(result, &r)
				f.finish(turn, "decision="+r.Decision)
			})
			return
		case strings.HasPrefix(text, "legacy "):
			// A legacy-API approval; the reply is the raw decision.
			f.request(codexapp.MethodExecCommandApproval, map[string]any{
				"conversationId": f.thread, "callId": "call_legacy", "cwd": "/w",
				"command": []string{"sh", "-c", strings.TrimPrefix(text, "legacy ")}, "parsedCmd": []any{},
			}, func(result json.RawMessage) {
				var r struct{ Decision json.RawMessage }
				_ = json.Unmarshal(result, &r)
				f.finish(turn, "decision="+string(r.Decision))
			})
			return
		case text == "spawn":
			// codex 0.156.1's schema: a collabAgentToolCall spawns a
			// thread, whose own notifications carry its thread id.
			collab := func(status string, states map[string]any) map[string]any {
				return map[string]any{"type": "collabAgentToolCall", "id": "collab_1", "tool": "spawnAgent", "status": status,
					"senderThreadId": f.thread, "receiverThreadIds": []string{"thr_child"}, "prompt": "look around\nin detail",
					"agentsStates": states}
			}
			f.notify(codexapp.MethodItemStarted, map[string]any{"threadId": f.thread, "turnId": turn, "startedAtMs": 1, "item": collab("inProgress", map[string]any{})})
			child := "thr_child"
			f.notify(codexapp.MethodItemAgentMessageDelta, map[string]any{"threadId": child, "turnId": "t_child", "itemId": "c_msg", "delta": "child speaking"})
			cmd := map[string]any{"type": "commandExecution", "id": "c_call", "command": "ls", "cwd": "/w", "commandActions": []any{}, "status": "inProgress"}
			f.notify(codexapp.MethodItemStarted, map[string]any{"threadId": child, "turnId": "t_child", "startedAtMs": 1, "item": cmd})
			f.notify(codexapp.MethodThreadTokenUsageUpdated, map[string]any{"threadId": child, "turnId": "t_child", "tokenUsage": map[string]any{
				"total": map[string]any{"totalTokens": 700, "inputTokens": 600, "cachedInputTokens": 0, "outputTokens": 100, "reasoningOutputTokens": 0},
				"last":  map[string]any{"totalTokens": 700, "inputTokens": 600, "cachedInputTokens": 0, "outputTokens": 100, "reasoningOutputTokens": 0}}})
			f.request(codexapp.MethodItemCommandExecutionRequestApproval, map[string]any{
				"threadId": child, "turnId": "t_child", "itemId": "c_call", "startedAtMs": 1, "command": "ls", "cwd": "/w",
			}, func(json.RawMessage) {
				cmd["status"] = "completed"
				f.notify(codexapp.MethodItemCompleted, map[string]any{"threadId": child, "turnId": "t_child", "completedAtMs": 2, "item": cmd})
				f.notify(codexapp.MethodItemCompleted, map[string]any{"threadId": f.thread, "turnId": turn, "completedAtMs": 3,
					"item": collab("completed", map[string]any{child: map[string]any{"status": "completed", "message": "found it"}})})
				f.finish(turn, "the child found it")
			})
			return
		case text == "config?":
			f.finish(turn, "config="+string(f.config))
		case text == "init?":
			f.finish(turn, "capabilities="+string(f.initCaps)+" tools="+string(f.dynamicTools))
		case text == "envs?":
			f.finish(turn, "thread="+string(f.threadEnvs)+" turn="+string(f.turnEnvs)+" args="+strings.Join(os.Args[1:], " "))
		case strings.HasPrefix(text, "tool "):
			// codex 0.156.1: a dynamicToolCall item around the
			// item/tool/call request, its id the call id.
			args := strings.TrimPrefix(text, "tool ")
			item := func(status string, extra map[string]any) map[string]any {
				it := map[string]any{"type": "dynamicToolCall", "id": "call_dyn", "tool": "echo",
					"arguments": json.RawMessage(args), "status": status}
				for k, v := range extra {
					it[k] = v
				}
				return it
			}
			f.notify(codexapp.MethodItemStarted, map[string]any{"threadId": f.thread, "turnId": turn, "startedAtMs": 1, "item": item("inProgress", nil)})
			f.request(codexapp.MethodItemToolCall, map[string]any{
				"threadId": f.thread, "turnId": turn, "callId": "call_dyn", "tool": "echo", "arguments": json.RawMessage(args),
			}, func(result json.RawMessage) {
				if f.active != turn {
					return // interrupted meanwhile
				}
				var r codexapp.DynamicToolCallResponse
				_ = json.Unmarshal(result, &r)
				status := "completed"
				if !r.Success {
					status = "failed"
				}
				f.notify(codexapp.MethodItemCompleted, map[string]any{"threadId": f.thread, "turnId": turn, "completedAtMs": 2,
					"item": item(status, map[string]any{"contentItems": r.ContentItems, "success": r.Success, "durationMs": 7})})
				f.finish(turn, "response="+string(result))
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
	case codexapp.MethodAccountRead:
		// Shapes from codex 0.156.1's schema (GetAccountResponse).
		f.reply(m.ID, map[string]any{"account": map[string]any{"type": "chatgpt", "email": "a@x.dev", "planType": "pro"}, "requiresOpenaiAuth": true})
	case codexapp.MethodAccountRateLimitsRead:
		f.reply(m.ID, map[string]any{"accountId": "acct-1", "rateLimits": map[string]any{"limitId": "codex",
			"primary":   map[string]any{"usedPercent": 37, "resetsAt": 1791651600, "windowDurationMins": 300},
			"secondary": map[string]any{"usedPercent": 8, "windowDurationMins": 10080}}})
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
	f.used += 15
	f.notify(codexapp.MethodThreadTokenUsageUpdated, map[string]any{"threadId": f.thread, "turnId": turn, "tokenUsage": map[string]any{
		"total":              map[string]any{"totalTokens": f.used, "inputTokens": f.used * 2 / 3, "cachedInputTokens": 0, "outputTokens": f.used / 3, "reasoningOutputTokens": 0},
		"last":               map[string]any{"totalTokens": 15, "inputTokens": 10, "cachedInputTokens": 0, "outputTokens": 5, "reasoningOutputTokens": 0},
		"modelContextWindow": 272000}})
	f.notify(codexapp.MethodAccountRateLimitsUpdated, map[string]any{"rateLimits": map[string]any{"limitId": "codex",
		"primary": map[string]any{"usedPercent": 38}}})
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
