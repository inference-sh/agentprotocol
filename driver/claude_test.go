package driver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/claudecode"
	"github.com/inference-sh/agentprotocol/driver"
)

// The Claude backend launches a real process, so these tests launch this test
// binary as a fake claude. init runs before TestMain, so the fake takes over
// without touching the ACP fake's TestMain.

const (
	fakeClaudeEnv    = "AGENTPROTOCOL_FAKE_CLAUDE"
	fakeClaudeRecord = "AGENTPROTOCOL_FAKE_CLAUDE_RECORD"
)

func init() {
	if script := os.Getenv(fakeClaudeEnv); script != "" {
		runFakeClaude(script)
		lingerIfAsked()
		os.Exit(0)
	}
}

func claudeRunning(t *testing.T, script string) (*driver.ClaudeBackend, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	record := t.TempDir() + "/record.jsonl"
	return &driver.ClaudeBackend{
		Command:       exe,
		Env:           append(os.Environ(), fakeClaudeEnv+"="+script, fakeClaudeRecord+"="+record),
		ShutdownGrace: 2 * time.Second,
	}, record
}

func openClaude(t *testing.T, b *driver.ClaudeBackend, cfg driver.SessionConfig) driver.Session {
	t.Helper()
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

// until reads events until one of the wanted type arrives, returning all of
// them including it.
func until(t *testing.T, ch <-chan ap.AgentEvent, want ap.AgentEventType) []ap.AgentEvent {
	t.Helper()
	var got []ap.AgentEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("events closed before %s; got %v", want, typesOf(got))
			}
			got = append(got, ev)
			if ev.Type == want {
				return got
			}
		case <-deadline:
			t.Fatalf("no %s within 5s; got %v", want, typesOf(got))
		}
	}
}

func textOf(events []ap.AgentEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if p, ok := ap.PayloadAs[ap.ContentDeltaPayload](ev, ap.AgentEventContentDelta); ok && p.Kind == ap.ContentDeltaText {
			b.WriteString(p.Delta)
		}
	}
	return b.String()
}

type fakeRecord struct {
	Args       []string        `json:"args"`
	Initialize json.RawMessage `json:"initialize"`
}

func readRecord(t *testing.T, path string) fakeRecord {
	t.Helper()
	var rec fakeRecord
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(data, &rec) == nil && rec.Initialize != nil {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake claude left no record at %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestClaudeHandshakeNamesTheSessionBeforeAnyPrompt(t *testing.T) {
	b, record := claudeRunning(t, "echo")
	sess := openClaude(t, b, driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", Model: "claude-test", Instructions: "Be brief.",
	})

	id := sess.ID()
	if !uuidRE.MatchString(id) {
		t.Fatalf("session ID %q is not the UUID passed to --session-id", id)
	}

	ev := until(t, sess.Events(), ap.AgentEventRunStarted)[0]
	if ev.RunID != "run_1" || ev.ChatID != "chat_1" {
		t.Errorf("event not stamped: run=%q chat=%q", ev.RunID, ev.ChatID)
	}

	rec := readRecord(t, record)
	for _, want := range []string{
		"--output-format stream-json", "--verbose", "--input-format stream-json",
		"--include-partial-messages", "--permission-prompt-tool stdio",
		"--model claude-test", "--session-id=" + id,
	} {
		if !strings.Contains(strings.Join(rec.Args, " "), want) {
			t.Errorf("argv %v lacks %q", rec.Args, want)
		}
	}
	for _, a := range rec.Args {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") || strings.HasPrefix(a, "--resume") {
			t.Errorf("argv carries %q", a)
		}
	}
	var init claudecode.InitializeRequest
	_ = json.Unmarshal(rec.Initialize, &init)
	if init.Subtype != "initialize" || init.AppendSystemPrompt != "Be brief." {
		t.Errorf("initialize = %s, want the instructions as appendSystemPrompt", rec.Initialize)
	}
}

func TestClaudeTurnStreamsTextAndCompletes(t *testing.T) {
	b, _ := claudeRunning(t, "echo")
	sess := openClaude(t, b, driver.SessionConfig{RunID: "r", ChatID: "c"})

	start := time.Now()
	if err := sess.Prompt(context.Background(), driver.TextInput("hello")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("Prompt blocked for the turn; it must return at once")
	}

	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	t.Logf("events: %v", typesOf(events))

	// The reply streams as deltas and the settled assistant copy must not
	// repeat it.
	if got := textOf(events); got != "you said hello" {
		t.Errorf("text = %q, want the streamed reply exactly once", got)
	}
	if _, ok := findEvent(events, ap.AgentEventTurnStarted); !ok {
		t.Error("no turn.started")
	}
	if _, ok := findEvent(events, ap.AgentEventUsageUpdated); !ok {
		t.Error("no usage.updated from the result")
	}
	done, _ := ap.PayloadAs[ap.TurnCompletedPayload](events[len(events)-1], ap.AgentEventTurnCompleted)
	if !done.HasOutput || done.StopReason != "end_turn" || done.TurnIndex != 1 {
		t.Errorf("turn completed = %+v", done)
	}
	for _, ev := range events {
		if ev.RunID != "r" || ev.ChatID != "c" {
			t.Errorf("%s not stamped", ev.Type)
		}
	}
}

// approve runs the permission script up to the approval and returns its
// payload.
func approvalFor(t *testing.T, sess driver.Session) ap.ApprovalRequiredPayload {
	t.Helper()
	if err := sess.Prompt(context.Background(), driver.TextInput("make a file")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventApprovalRequired)
	if _, ok := findEvent(events, ap.AgentEventToolStarted); !ok {
		t.Errorf("no tool.started before the approval; got %v", typesOf(events))
	}
	p, ok := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)
	if !ok {
		t.Fatal("approval payload did not decode")
	}
	return p
}

func TestClaudeApprovalAllowForSessionRunsTheTool(t *testing.T) {
	b, _ := claudeRunning(t, "permission")
	sess := openClaude(t, b, driver.SessionConfig{RunID: "r", ChatID: "c"})

	p := approvalFor(t, sess)
	if p.ToolInvocationID != "toolu_1" || p.ToolName != "Bash" || p.Reason != ap.InterruptReasonToolApproval {
		t.Errorf("approval = %+v", p)
	}
	if p.Arguments["command"] != "touch x" {
		t.Errorf("arguments = %v", p.Arguments)
	}

	// The ID in the event is exactly what Resolve takes.
	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.AllowFor(driver.ScopeSession)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)

	resolved, ok := findEvent(events, ap.AgentEventApprovalResolved)
	if !ok {
		t.Fatalf("no approval.resolved; got %v", typesOf(events))
	}
	rp, _ := ap.PayloadAs[ap.ApprovalResolvedPayload](resolved, ap.AgentEventApprovalResolved)
	if rp.Decision != "allow" || rp.ToolInvocationID != "toolu_1" {
		t.Errorf("resolved = %+v", rp)
	}
	done, ok := findEvent(events, ap.AgentEventToolCompleted)
	if !ok {
		t.Fatal("no tool.completed")
	}
	tp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
	if tp.Status != ap.ToolInvocationStatusCompleted || tp.ToolName != "Bash" {
		t.Errorf("tool completed = %+v", tp)
	}

	// The fake reports what it was sent: the rule Claude suggested, rescoped
	// to the session, and nothing broader (no setMode, no addDirectories).
	if got := textOf(events); got != "allow input=touch x updates=addRules:Bash(touch x)@session" {
		t.Errorf("fake saw %q", got)
	}

	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Allow()); err == nil {
		t.Error("resolving the same request twice succeeded")
	}
}

func TestClaudeCallerToolsRunOverStdio(t *testing.T) {
	b, record := claudeRunning(t, "hosttools")
	b.ToolTimeout = 90 * time.Second

	var calls []driver.ToolCall
	var mu sync.Mutex
	required := []string{"text"}
	sess := openClaude(t, b, driver.SessionConfig{
		RunID: "r", ChatID: "c",
		Tools: []ap.Tool{{Type: "function", Function: ap.ToolFunction{
			Name: "echo", Description: "Echo the text back.",
			Parameters: ap.ToolParameters{Type: "object", Properties: ap.ToolParameterProperties{
				"text": {Type: "string", Description: "what to echo"},
			}, Required: &required},
		}}},
		CallTool: func(ctx context.Context, call driver.ToolCall) (driver.ToolOutput, error) {
			mu.Lock()
			calls = append(calls, call)
			mu.Unlock()
			var args struct{ Text string }
			_ = json.Unmarshal(call.Arguments, &args)
			return driver.ToolOutput{Text: "echo: " + args.Text}, nil
		},
	})

	rec := readRecord(t, record)
	var init claudecode.InitializeRequest
	if err := json.Unmarshal(rec.Initialize, &init); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !slices.Equal(init.SDKMCPServers, []string{"tools"}) || init.SDKMCPServerConfigs["tools"].TimeoutMs != 90000 {
		t.Errorf("initialize declared %v %+v", init.SDKMCPServers, init.SDKMCPServerConfigs)
	}

	if err := sess.Prompt(context.Background(), driver.TextInput("go")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)

	if _, asked := findEvent(events, ap.AgentEventApprovalRequired); asked {
		t.Error("a caller tool raised an approval")
	}
	started, ok := findEvent(events, ap.AgentEventToolStarted)
	if !ok {
		t.Fatalf("no tool.started; got %v", typesOf(events))
	}
	sp, _ := ap.PayloadAs[ap.ToolStartedPayload](started, ap.AgentEventToolStarted)
	if sp.ToolName != "echo" || sp.ToolInvocationID != "toolu_h" {
		t.Errorf("tool started = %+v", sp)
	}
	done, _ := findEvent(events, ap.AgentEventToolCompleted)
	dp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
	if dp.ToolName != "echo" || dp.Status != ap.ToolInvocationStatusCompleted || dp.Result != "echo: PING" {
		t.Errorf("tool completed = %+v", dp)
	}
	if got := textOf(events); got != "host tool said echo: PING" {
		t.Errorf("text = %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0].Name != "echo" || calls[0].ToolInvocationID != "toolu_h" {
		t.Errorf("calls = %+v", calls)
	}

	data, err := os.ReadFile(record + ".mcp")
	if err != nil {
		t.Fatalf("fake left no mcp record: %v", err)
	}
	var seen map[string]json.RawMessage
	_ = json.Unmarshal(data, &seen)
	if string(seen["note"]) != `{"mcp_response":{"jsonrpc":"2.0","result":{},"id":0}}` {
		t.Errorf("notification answered %s", seen["note"])
	}
	var list struct {
		MCPResponse struct {
			Result struct {
				Tools []struct {
					Name        string
					InputSchema map[string]any `json:"inputSchema"`
				} `json:"tools"`
			} `json:"result"`
		} `json:"mcp_response"`
	}
	_ = json.Unmarshal(seen["list"], &list)
	tools := list.MCPResponse.Result.Tools
	if len(tools) != 1 || tools[0].Name != "echo" || tools[0].InputSchema["type"] != "object" {
		t.Fatalf("tools/list = %s", seen["list"])
	}
	if _, titled := tools[0].InputSchema["title"]; titled {
		t.Errorf("schema kept an empty title: %v", tools[0].InputSchema)
	}
	if req, _ := tools[0].InputSchema["required"].([]any); len(req) != 1 || req[0] != "text" {
		t.Errorf("schema required = %v", tools[0].InputSchema["required"])
	}
	var perm claudecode.PermissionResult
	_ = json.Unmarshal(seen["perm"], &perm)
	if perm.Behavior != "allow" {
		t.Errorf("caller tool permission = %+v", perm)
	}
}

func TestClaudeOnlyCallerToolsTurnsClaudesOwnOff(t *testing.T) {
	b, record := claudeRunning(t, "plain")
	b.ExtraArgs = []string{"--verbose-extra"}
	openClaude(t, b, driver.SessionConfig{
		Metadata:        map[string]string{"mcp_url": "http://127.0.0.1:1/mcp", "mcp_name": "other"},
		OnlyCallerTools: true,
		Tools:           []ap.Tool{{Type: "function", Function: ap.ToolFunction{Name: "echo"}}},
		CallTool: func(context.Context, driver.ToolCall) (driver.ToolOutput, error) {
			return driver.ToolOutput{}, nil
		},
	})
	args := strings.Join(readRecord(t, record).Args, " ")
	for _, want := range []string{"--verbose-extra", "--tools  --strict-mcp-config --setting-sources  --disable-slash-commands"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	if strings.Contains(args, "other") || !strings.Contains(args, `--mcp-config {"mcpServers":{"tools":{"name":"tools","timeout":1800000,"type":"sdk"}}}`) {
		t.Errorf("mcp-config should hold only the caller tool server: %s", args)
	}
}

func TestClaudeCallerToolsNeedCallTool(t *testing.T) {
	b, _ := claudeRunning(t, "")
	_, err := b.Open(context.Background(), driver.SessionConfig{
		WorkDir: t.TempDir(),
		Tools:   []ap.Tool{{Type: "function", Function: ap.ToolFunction{Name: "echo"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "CallTool") {
		t.Errorf("open without CallTool = %v", err)
	}
}

func TestClaudeApprovalAllowOnceSendsNoRules(t *testing.T) {
	b, _ := claudeRunning(t, "permission")
	sess := openClaude(t, b, driver.SessionConfig{})
	p := approvalFor(t, sess)
	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Allow()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := textOf(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "allow input=touch x updates=" {
		t.Errorf("fake saw %q", got)
	}
}

func TestClaudeApprovalAlwaysKeepsClaudesDestination(t *testing.T) {
	b, _ := claudeRunning(t, "permission")
	sess := openClaude(t, b, driver.SessionConfig{})
	p := approvalFor(t, sess)
	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.AllowFor(driver.ScopeAlways)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := textOf(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "allow input=touch x updates=addRules:Bash(touch x)@localSettings" {
		t.Errorf("fake saw %q", got)
	}
}

func TestClaudeApprovalDenyStopsTheTool(t *testing.T) {
	b, _ := claudeRunning(t, "permission")
	sess := openClaude(t, b, driver.SessionConfig{})

	p := approvalFor(t, sess)
	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Deny("not on prod")); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if got := textOf(events); got != "deny message=not on prod" {
		t.Errorf("fake saw %q", got)
	}
	done, ok := findEvent(events, ap.AgentEventToolCompleted)
	if !ok {
		t.Fatal("no tool.completed")
	}
	tp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
	if tp.Status != ap.ToolInvocationStatusCancelled {
		t.Errorf("denied tool status = %s, want cancelled", tp.Status)
	}
	rv, _ := findEvent(events, ap.AgentEventApprovalResolved)
	rp, _ := ap.PayloadAs[ap.ApprovalResolvedPayload](rv, ap.AgentEventApprovalResolved)
	if rp.Decision != "deny" || rp.Reason != "not on prod" {
		t.Errorf("resolved = %+v", rp)
	}
}

func TestClaudeResolveUnknownIsAnError(t *testing.T) {
	b, _ := claudeRunning(t, "echo")
	sess := openClaude(t, b, driver.SessionConfig{})
	if err := sess.Resolve(context.Background(), "never_asked", driver.Allow()); err == nil {
		t.Error("resolving an unknown request succeeded")
	}
}

func TestClaudeInterruptEndsTheTurnAndKeepsTheSession(t *testing.T) {
	b, _ := claudeRunning(t, "interrupt")
	sess := openClaude(t, b, driver.SessionConfig{})

	p := approvalFor(t, sess)
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	t.Logf("events: %v", typesOf(events))

	// Claude withdrew the approval; the caller is told rather than left
	// holding a question nobody can answer.
	rv, ok := findEvent(events, ap.AgentEventApprovalResolved)
	if !ok {
		t.Fatal("withdrawn approval was never resolved")
	}
	rp, _ := ap.PayloadAs[ap.ApprovalResolvedPayload](rv, ap.AgentEventApprovalResolved)
	if rp.ToolInvocationID != p.ToolInvocationID || rp.Decision != "deny" {
		t.Errorf("resolved = %+v", rp)
	}
	if _, ok := findEvent(events, ap.AgentEventError); ok {
		t.Error("an interrupt is not an error")
	}
	done, _ := ap.PayloadAs[ap.TurnCompletedPayload](events[len(events)-1], ap.AgentEventTurnCompleted)
	if done.StopReason != "interrupted" {
		t.Errorf("stop reason = %q, want interrupted", done.StopReason)
	}
	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Allow()); err == nil {
		t.Error("resolving a withdrawn request succeeded")
	}

	// The session is still usable.
	if err := sess.Prompt(context.Background(), driver.TextInput("again")); err != nil {
		t.Fatalf("prompt after interrupt: %v", err)
	}
	events = until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if got := textOf(events); got != "you said again" {
		t.Errorf("after interrupt: %q", got)
	}
	done, _ = ap.PayloadAs[ap.TurnCompletedPayload](events[len(events)-1], ap.AgentEventTurnCompleted)
	if done.TurnIndex != 2 || done.StopReason != "end_turn" {
		t.Errorf("second turn = %+v", done)
	}
}

func TestClaudeResumePassesTheSessionID(t *testing.T) {
	b, record := claudeRunning(t, "echo")
	sess := openClaude(t, b, driver.SessionConfig{ResumeSessionID: "0f1e2d3c-resume"})

	if sess.ID() != "0f1e2d3c-resume" {
		t.Errorf("ID = %q, want the resumed ID", sess.ID())
	}
	rec := readRecord(t, record)
	if !slices.Contains(rec.Args, "--resume=0f1e2d3c-resume") {
		t.Errorf("argv %v lacks --resume", rec.Args)
	}
	for _, a := range rec.Args {
		if strings.HasPrefix(a, "--session-id") {
			t.Errorf("a resume also passed %s", a)
		}
	}

	// The fake answers init with the ID it was resumed by; a prompt goes
	// through as usual.
	_ = sess.Prompt(context.Background(), driver.TextInput("still there"))
	if got := textOf(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "you said still there" {
		t.Errorf("text = %q", got)
	}
}

// The account from initialize, a pushed rate limit, the window's fill once
// per call (not once per content block), and per-turn cost from a running
// total.
func TestClaudeReportsUsage(t *testing.T) {
	b, _ := claudeRunning(t, "usage")
	sess := openClaude(t, b, driver.SessionConfig{})
	if err := sess.Prompt(context.Background(), driver.TextInput("hello")); err != nil {
		t.Fatal(err)
	}
	first := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if err := sess.Prompt(context.Background(), driver.TextInput("again")); err != nil {
		t.Fatal(err)
	}
	second := until(t, sess.Events(), ap.AgentEventTurnCompleted)

	acct, ok := findEvent(first, ap.AgentEventAccountUpdated)
	if !ok {
		t.Fatal("no account.updated")
	}
	if a, _ := ap.PayloadAs[ap.AccountUpdatedPayload](acct, ap.AgentEventAccountUpdated); a.Account.Status != ap.AccountAuthenticated {
		t.Errorf("account = %+v", a.Account)
	}

	lim, ok := findEvent(first, ap.AgentEventLimitsUpdated)
	if !ok {
		t.Fatal("no limits.updated")
	}
	l, _ := ap.PayloadAs[ap.LimitsUpdatedPayload](lim, ap.AgentEventLimitsUpdated)
	if len(l.Windows) != 1 || l.Windows[0].ID != "five_hour" || l.Windows[0].UsedPercent < 79.9 || l.Windows[0].UsedPercent > 80.1 {
		t.Errorf("limits = %+v", l)
	}

	var contexts []ap.ContextUpdatedPayload
	for _, ev := range first {
		if c, ok := ap.PayloadAs[ap.ContextUpdatedPayload](ev, ap.AgentEventContextUpdated); ok {
			contexts = append(contexts, c)
		}
	}
	// One for the call (its two blocks repeat the usage), one once the
	// result names the window.
	if len(contexts) != 2 || contexts[0].UsedTokens != 9543 || contexts[0].MaxTokens != 0 || contexts[1].MaxTokens != 200000 {
		t.Errorf("context updates = %+v", contexts)
	}

	usageOf := func(events []ap.AgentEvent) ap.UsageUpdatedPayload {
		ev, ok := findEvent(events, ap.AgentEventUsageUpdated)
		if !ok {
			t.Fatal("no usage.updated")
		}
		u, _ := ap.PayloadAs[ap.UsageUpdatedPayload](ev, ap.AgentEventUsageUpdated)
		return u
	}
	u1, u2 := usageOf(first), usageOf(second)
	if u1.PromptTokens != 9503 || u1.CachedInputTokens != 9000 || u1.CacheCreationTokens != 500 || u1.CompletionTokens != 40 || u1.CostUSD != 0.05 || u1.Status != ap.TurnUsageComplete {
		t.Errorf("first turn = %+v", u1)
	}
	if u2.CostUSD < 0.0299 || u2.CostUSD > 0.0301 {
		t.Errorf("second turn's cost = %v, want its share of the running total", u2.CostUSD)
	}
}

// A subagent is reported by its own events; what it says reaches the caller
// only when asked for, and then names it. Its approval always names it.
func TestClaudeReportsSubagents(t *testing.T) {
	for _, output := range []bool{false, true} {
		b, _ := claudeRunning(t, "subagent")
		b.SubagentOutput = output
		sess := openClaude(t, b, driver.SessionConfig{})
		if err := sess.Prompt(context.Background(), driver.TextInput("go")); err != nil {
			t.Fatal(err)
		}
		before := until(t, sess.Events(), ap.AgentEventApprovalRequired)
		ask, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](before[len(before)-1], ap.AgentEventApprovalRequired)
		if ask.ToolInvocationID != "toolu_sub" || ask.SubagentID != "task1" {
			t.Errorf("output=%v: approval = %+v, want the subagent's call naming task1", output, ask)
		}
		if err := sess.Resolve(context.Background(), "toolu_sub", driver.Resolution{Decision: ap.InterruptResolutionAllow}); err != nil {
			t.Fatal(err)
		}
		events := append(before, until(t, sess.Events(), ap.AgentEventTurnCompleted)...)

		started, ok := findEvent(events, ap.AgentEventSubagentStarted)
		if !ok {
			t.Fatalf("output=%v: no subagent.started in %v", output, typesOf(events))
		}
		st, _ := ap.PayloadAs[ap.SubagentStartedPayload](started, ap.AgentEventSubagentStarted)
		if st.SubagentID != "task1" || st.ParentToolInvocationID != "toolu_agent" || st.Kind != "Explore" || st.Description != "look around" {
			t.Errorf("started = %+v", st)
		}
		if countType(events, ap.AgentEventSubagentCompleted) != 1 {
			t.Errorf("output=%v: %d subagent.completed, want 1", output, countType(events, ap.AgentEventSubagentCompleted))
		}
		done, _ := findEvent(events, ap.AgentEventSubagentCompleted)
		c, _ := ap.PayloadAs[ap.SubagentCompletedPayload](done, ap.AgentEventSubagentCompleted)
		if c.Status != ap.SubagentCompleted || c.Summary != "found it" || c.Usage == nil || c.Usage.TotalTokens != 1500 {
			t.Errorf("completed = %+v", c)
		}

		// The reply is the agent's alone either way.
		var main, sub []string
		for _, ev := range events {
			if p, ok := ap.PayloadAs[ap.ContentDeltaPayload](ev, ap.AgentEventContentDelta); ok {
				if p.SubagentID == "" {
					main = append(main, p.Delta)
				} else if p.SubagentID == "task1" {
					sub = append(sub, p.Delta)
				}
			}
		}
		if strings.Join(main, "") != "the subagent found it" {
			t.Errorf("output=%v: the agent's text = %q", output, main)
		}
		var subTools int
		for _, ev := range events {
			if p, ok := ap.PayloadAs[ap.ToolStartedPayload](ev, ap.AgentEventToolStarted); ok && p.SubagentID == "task1" {
				subTools++
			}
			if p, ok := ap.PayloadAs[ap.ToolStartedPayload](ev, ap.AgentEventToolStarted); ok && p.ToolInvocationID == "toolu_sub" && p.SubagentID == "" {
				t.Errorf("output=%v: the subagent's tool call reached the caller as the agent's", output)
			}
		}
		if output && (len(sub) != 1 || sub[0] != "subagent thinking aloud" || subTools != 1) {
			t.Errorf("subagent output = %q, %d tool call(s)", sub, subTools)
		}
		if !output && (len(sub) != 0 || subTools != 0) {
			t.Errorf("subagent output sent without being asked for: %q, %d tool call(s)", sub, subTools)
		}
		sess.Close()
	}
}

func TestClaudeUnknownMessagesAreTolerated(t *testing.T) {
	var mu sync.Mutex
	var diags []string
	b, _ := claudeRunning(t, "unknown")
	b.OnDiagnostic = func(m string) { mu.Lock(); diags = append(diags, m); mu.Unlock() }
	sess := openClaude(t, b, driver.SessionConfig{})

	_ = sess.Prompt(context.Background(), driver.TextInput("hi"))
	events := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	// The fake says whether its unknown control request got an error reply,
	// which is what the CLI does for subtypes it does not know.
	if got := textOf(events); got != "future_request=error" {
		t.Errorf("text = %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(diags, "\n")
	if !strings.Contains(joined, "future_request") {
		t.Errorf("unimplemented request not diagnosed: %v", diags)
	}
	if !strings.Contains(joined, "not JSON") {
		t.Errorf("malformed line not diagnosed: %v", diags)
	}
}

func TestClaudeProcessExitClosesEvents(t *testing.T) {
	b, _ := claudeRunning(t, "exit")
	sess := openClaude(t, b, driver.SessionConfig{})

	_ = sess.Prompt(context.Background(), driver.TextInput("hi"))
	var got []ap.AgentEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				errEv, found := findEvent(got, ap.AgentEventError)
				if !found {
					t.Fatalf("turn cut short with no error event: %v", typesOf(got))
				}
				p, _ := ap.PayloadAs[ap.ErrorPayload](errEv, ap.AgentEventError)
				if p.Code != "process_exited" || !strings.Contains(p.Message, "backend went away") {
					t.Errorf("error = %+v, want process_exited quoting claude's stderr", p)
				}
				if got[len(got)-1].Type != ap.AgentEventError {
					t.Errorf("error is not the last event: %v", typesOf(got))
				}
				return
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("events never closed after the process exited; got %v", typesOf(got))
		}
	}
}

func TestClaudeCloseEndsTheEventStream(t *testing.T) {
	b, _ := claudeRunning(t, "echo")
	sess := openClaude(t, b, driver.SessionConfig{})
	if err := sess.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	select {
	case <-drain(sess.Events()):
	case <-time.After(3 * time.Second):
		t.Fatal("events still open after Close")
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestClaudeKillEndsAWedgedAgentAtOnce(t *testing.T) {
	b, _ := claudeRunning(t, "permission")
	b.Env = append(b.Env, ignoreEOFEnv+"=1")
	sess := openClaude(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("touch x"))
	approvalFor(t, sess)

	got := killMidWork(t, sess)
	errEv, ok := findEvent(got, ap.AgentEventError)
	if !ok {
		t.Fatalf("turn cut short by a kill with no error event: %v", typesOf(got))
	}
	if p, _ := ap.PayloadAs[ap.ErrorPayload](errEv, ap.AgentEventError); p.Code != "process_exited" {
		t.Errorf("error = %+v", p)
	}
}

func TestClaudeKillAfterCloseIsHarmless(t *testing.T) {
	b, _ := claudeRunning(t, "echo")
	killAfterClose(t, openClaude(t, b, driver.SessionConfig{}))
}

func TestClaudeSkipApprovalsLaunchesInBypassMode(t *testing.T) {
	b, record := claudeRunning(t, "echo")
	b.PermissionMode = claudecode.PermissionModeAcceptEdits
	sess := openClaude(t, b, driver.SessionConfig{Approvals: driver.ApprovalsSkip})
	until(t, sess.Events(), ap.AgentEventRunStarted)
	args := strings.Join(readRecord(t, record).Args, " ")
	if !strings.Contains(args, "--permission-mode bypassPermissions") || strings.Contains(args, "acceptEdits") {
		t.Errorf("argv = %s, want bypassPermissions over the backend's mode", args)
	}
}

func TestClaudeLaunchFailureQuotesStderr(t *testing.T) {
	b, _ := claudeRunning(t, "die")
	_, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "No conversation found") {
		t.Errorf("err = %v, want the CLI's own complaint", err)
	}
}

func TestClaudeBackendDeclaresWhatItCanDo(t *testing.T) {
	b := &driver.ClaudeBackend{}
	if b.Kind() != driver.KindClaude {
		t.Errorf("kind = %q", b.Kind())
	}
	want := driver.Capabilities{Steer: true, Approvals: true, Interrupt: true, Resume: true, Tools: true, Images: true, OnlyCallerTools: true}
	if got := b.Capabilities(); got != want {
		t.Errorf("capabilities = %+v, want %+v", got, want)
	}
}

// --- the fake claude ---

func runFakeClaude(script string) {
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		data, _ := json.Marshal(v)
		out.Write(append(data, '\n'))
		out.Flush()
	}
	sessionID := "fake-session"
	for _, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, "--session-id="); ok {
			sessionID = v
		}
		if v, ok := strings.CutPrefix(a, "--resume="); ok {
			sessionID = v
		}
	}
	if script == "die" {
		fmt.Fprintln(os.Stderr, "No conversation found with session ID: nope")
		os.Exit(1)
	}

	success := func(id string, body any) {
		send(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": id, "response": body,
		}})
	}
	initMsg := func() {
		send(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID,
			"model": "claude-fake", "tools": []string{"Bash"}, "permissionMode": "default", "some_new_field": 1})
	}
	reply := func(text string) {
		send(map[string]any{"type": "stream_event", "session_id": sessionID, "parent_tool_use_id": nil,
			"event": map[string]any{"type": "message_start", "message": map[string]any{"id": "m"}}})
		send(map[string]any{"type": "stream_event", "session_id": sessionID, "parent_tool_use_id": nil,
			"event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}})
		send(map[string]any{"type": "assistant", "session_id": sessionID, "parent_tool_use_id": nil,
			"message": map[string]any{"id": "m", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	}
	result := func(extra map[string]any) {
		r := map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1,
			"stop_reason": "end_turn", "terminal_reason": "completed", "session_id": sessionID,
			"total_cost_usd": 0.001, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
		for k, v := range extra {
			r[k] = v
		}
		send(r)
	}
	toolUse := func() {
		send(map[string]any{"type": "assistant", "session_id": sessionID, "parent_tool_use_id": nil,
			"message": map[string]any{"id": "m-tool", "role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "touch x"}}}}})
		send(map[string]any{"type": "control_request", "request_id": "perm1", "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": "Bash", "display_name": "Bash",
			"input": map[string]any{"command": "touch x"}, "tool_use_id": "toolu_1",
			"permission_suggestions": []any{
				map[string]any{"type": "addRules", "rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "touch x"}},
					"behavior": "allow", "destination": "localSettings"},
				map[string]any{"type": "addDirectories", "directories": []string{"/tmp"}, "destination": "session"},
				map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"},
			},
		}})
	}
	toolResult := func(content string, isError bool) {
		send(map[string]any{"type": "user", "session_id": sessionID, "parent_tool_use_id": nil,
			"message": map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "toolu_1", "content": content, "is_error": isError}}}})
	}

	hostMCP := map[string]json.RawMessage{}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for in.Scan() {
		var msg struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Response  struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		var sub struct {
			Subtype string `json:"subtype"`
		}
		_ = json.Unmarshal(msg.Request, &sub)

		switch {
		case msg.Type == "control_request" && sub.Subtype == "initialize":
			if path := os.Getenv(fakeClaudeRecord); path != "" {
				data, _ := json.Marshal(map[string]any{"args": os.Args[1:], "initialize": msg.Request})
				_ = os.WriteFile(path, data, 0o600)
			}
			success(msg.RequestID, map[string]any{"pid": os.Getpid(), "current_permission_mode": "default",
				"account": map[string]any{"tokenSource": "claude.ai"}, "commands": []any{}, "brand_new_field": true})

		case msg.Type == "control_request" && sub.Subtype == "interrupt":
			// As the real CLI does: withdraw the pending permission, answer,
			// then end the turn as aborted.
			send(map[string]any{"type": "control_cancel_request", "request_id": "perm1"})
			success(msg.RequestID, map[string]any{"still_queued": []string{}})
			toolResult("The user doesn't want to proceed with this tool use.", true)
			result(map[string]any{"subtype": "error_during_execution", "is_error": true,
				"terminal_reason": "aborted_tools", "stop_reason": "tool_use",
				"errors": []string{"[ede_diagnostic] result_type=user"}})

		case msg.Type == "control_request":
			send(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "error", "request_id": msg.RequestID, "error": "Unsupported control request subtype: " + sub.Subtype}})

		case msg.Type == "control_response" && msg.Response.RequestID == "perm1":
			var res claudecode.PermissionResult
			_ = json.Unmarshal(msg.Response.Response, &res)
			if res.Behavior == "allow" {
				var input struct{ Command string }
				_ = json.Unmarshal(res.UpdatedInput, &input)
				var ups []string
				for _, u := range res.UpdatedPermissions {
					for _, r := range u.Rules {
						ups = append(ups, fmt.Sprintf("%s:%s(%s)@%s", u.Type, r.ToolName, r.RuleContent, u.Destination))
					}
					if len(u.Rules) == 0 {
						ups = append(ups, u.Type+"@"+u.Destination)
					}
				}
				toolResult("ok", false)
				reply(fmt.Sprintf("allow input=%s updates=%s", input.Command, strings.Join(ups, ",")))
			} else {
				toolResult(res.Message, true)
				reply("deny message=" + res.Message)
			}
			result(nil)

		case msg.Type == "control_response" && msg.Response.RequestID == "subperm":
			// The subagent's tool ran; it reports and ends, and the Agent
			// call returns its summary.
			sub := "toolu_agent"
			send(map[string]any{"type": "user", "session_id": sessionID, "parent_tool_use_id": sub,
				"message": map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_sub", "content": "ok", "is_error": false}}}})
			send(map[string]any{"type": "system", "subtype": "task_progress", "session_id": sessionID, "task_id": "task1",
				"tool_use_id": sub, "description": "look around", "last_tool_name": "Bash",
				"usage": map[string]any{"total_tokens": 1200, "tool_uses": 1, "duration_ms": 900}})
			send(map[string]any{"type": "system", "subtype": "task_updated", "session_id": sessionID, "task_id": "task1",
				"patch": map[string]any{"status": "completed"}})
			send(map[string]any{"type": "system", "subtype": "task_notification", "session_id": sessionID, "task_id": "task1",
				"tool_use_id": sub, "status": "completed", "summary": "found it", "output_file": "/tmp/x",
				"usage": map[string]any{"total_tokens": 1500, "tool_uses": 1, "duration_ms": 1000}})
			send(map[string]any{"type": "user", "session_id": sessionID, "parent_tool_use_id": nil,
				"message": map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": sub, "content": "found it", "is_error": false}}}})
			reply("the subagent found it")
			result(nil)
		case msg.Type == "control_response" && msg.Response.RequestID == "mcp-init":
			// The CLI's side of a host-served MCP server, as 2.1.302 runs
			// it: initialize, the initialized notification, tools/list, a
			// tool_use, its permission check, then tools/call.
			send(map[string]any{"type": "control_request", "request_id": "mcp-note", "request": map[string]any{
				"subtype": "mcp_message", "server_name": "tools",
				"message": map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}}})
		case msg.Type == "control_response" && msg.Response.RequestID == "mcp-note":
			hostMCP["note"] = msg.Response.Response
			send(map[string]any{"type": "control_request", "request_id": "mcp-list", "request": map[string]any{
				"subtype": "mcp_message", "server_name": "tools",
				"message": map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}}})
		case msg.Type == "control_response" && msg.Response.RequestID == "mcp-list":
			hostMCP["list"] = msg.Response.Response
			send(map[string]any{"type": "assistant", "session_id": sessionID, "parent_tool_use_id": nil,
				"message": map[string]any{"id": "m-host", "role": "assistant", "content": []any{map[string]any{
					"type": "tool_use", "id": "toolu_h", "name": "mcp__tools__echo", "input": map[string]any{"text": "PING"}}}}})
			send(map[string]any{"type": "control_request", "request_id": "hperm", "request": map[string]any{
				"subtype": "can_use_tool", "tool_name": "mcp__tools__echo", "input": map[string]any{"text": "PING"}, "tool_use_id": "toolu_h"}})
		case msg.Type == "control_response" && msg.Response.RequestID == "hperm":
			hostMCP["perm"] = msg.Response.Response
			send(map[string]any{"type": "control_request", "request_id": "mcp-call", "request": map[string]any{
				"subtype": "mcp_message", "server_name": "tools",
				"message": map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
					"name": "echo", "arguments": map[string]any{"text": "PING"},
					"_meta": map[string]any{"claudecode/toolUseId": "toolu_h"}}}}})
		case msg.Type == "control_response" && msg.Response.RequestID == "mcp-call":
			hostMCP["call"] = msg.Response.Response
			var call struct {
				MCPResponse struct {
					Result struct {
						Content []struct{ Text string } `json:"content"`
						IsError bool                    `json:"isError"`
					} `json:"result"`
				} `json:"mcp_response"`
			}
			_ = json.Unmarshal(msg.Response.Response, &call)
			text := ""
			if len(call.MCPResponse.Result.Content) > 0 {
				text = call.MCPResponse.Result.Content[0].Text
			}
			send(map[string]any{"type": "user", "session_id": sessionID, "parent_tool_use_id": nil,
				"message": map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_h", "content": text, "is_error": call.MCPResponse.Result.IsError}}}})
			if path := os.Getenv(fakeClaudeRecord); path != "" {
				data, _ := json.Marshal(hostMCP)
				_ = os.WriteFile(path+".mcp", data, 0o600)
			}
			reply("host tool said " + text)
			result(nil)

		case msg.Type == "control_response" && msg.Response.RequestID == "future1":
			initMsg()
			reply("future_request=" + msg.Response.Subtype)
			result(nil)

		case msg.Type == "user":
			text := ""
			if len(msg.Message.Content) > 0 {
				text = msg.Message.Content[0].Text
			}
			initMsg()
			switch script {
			case "permission":
				toolUse()
			case "hosttools":
				send(map[string]any{"type": "control_request", "request_id": "mcp-init", "request": map[string]any{
					"subtype": "mcp_message", "server_name": "tools",
					"message": map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
						"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code"}}}}})
			case "interrupt":
				if text == "again" {
					reply("you said again")
					result(nil)
				} else {
					toolUse()
				}
			case "unknown":
				send(map[string]any{"type": "weird_future_thing", "payload": 1})
				send(map[string]any{"type": "system", "subtype": "brand_new_subtype"})
				send(map[string]any{"type": "keep_alive"})
				out.WriteString("this is not json\n")
				out.Flush()
				send(map[string]any{"type": "control_request", "request_id": "future1",
					"request": map[string]any{"subtype": "future_request"}})
			case "subagent":
				// sdk.d.ts 0.3.296: an Agent tool call, the task it
				// starts, and the subagent's own messages carrying the
				// call as parent_tool_use_id.
				send(map[string]any{"type": "assistant", "session_id": sessionID, "parent_tool_use_id": nil,
					"message": map[string]any{"id": "m-agent", "role": "assistant", "content": []any{map[string]any{
						"type": "tool_use", "id": "toolu_agent", "name": "Agent", "input": map[string]any{"description": "look around", "prompt": "look"}}}}})
				send(map[string]any{"type": "system", "subtype": "task_started", "session_id": sessionID, "task_id": "task1",
					"tool_use_id": "toolu_agent", "description": "look around", "subagent_type": "Explore", "prompt": "look"})
				send(map[string]any{"type": "assistant", "session_id": sessionID, "parent_tool_use_id": "toolu_agent",
					"message": map[string]any{"id": "m-sub", "role": "assistant", "content": []any{
						map[string]any{"type": "text", "text": "subagent thinking aloud"},
						map[string]any{"type": "tool_use", "id": "toolu_sub", "name": "Bash", "input": map[string]any{"command": "ls"}}}}})
				send(map[string]any{"type": "control_request", "request_id": "subperm", "request": map[string]any{
					"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "toolu_sub", "agent_id": "a-sub"}})
			case "usage":
				// Shapes from sdk.d.ts 0.3.296: rate_limit_event with a
				// fractional utilization and Unix-second reset; an assistant
				// call's usage repeated on each of its content blocks; a
				// result whose cost and modelUsage run for the session.
				send(map[string]any{"type": "rate_limit_event", "session_id": sessionID, "rate_limit_info": map[string]any{
					"status": "allowed_warning", "rateLimitType": "five_hour", "utilization": 0.8, "resetsAt": 1791651600}})
				call := map[string]any{"input_tokens": 3, "cache_read_input_tokens": 9000, "cache_creation_input_tokens": 500, "output_tokens": 40}
				for _, block := range []map[string]any{{"type": "thinking", "thinking": "hm"}, {"type": "text", "text": "done"}} {
					send(map[string]any{"type": "assistant", "session_id": sessionID, "parent_tool_use_id": nil,
						"message": map[string]any{"id": "m", "role": "assistant", "model": "claude-fake", "content": []any{block}, "usage": call}})
				}
				cost := 0.05
				if text == "again" {
					cost = 0.08
				}
				result(map[string]any{"total_cost_usd": cost,
					"usage":      map[string]any{"input_tokens": 3, "cache_read_input_tokens": 9000, "cache_creation_input_tokens": 500, "output_tokens": 40},
					"modelUsage": map[string]any{"claude-fake": map[string]any{"inputTokens": 3, "outputTokens": 40, "cacheReadInputTokens": 9000, "cacheCreationInputTokens": 500, "costUSD": cost, "contextWindow": 200000, "maxOutputTokens": 64000}}})
			case "exit":
				reply("partial")
				os.Stderr.WriteString("claude: backend went away\n")
				os.Exit(3)
			default:
				reply("you said " + text)
				result(nil)
			}
		}
	}
}
