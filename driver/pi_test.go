package driver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
)

// The pi backend launches a real process, so these tests launch this test
// binary as a fake pi speaking pi's RPC mode, with the records pi 0.87.1
// writes. init rather than TestMain, so the ACP fake's TestMain stays
// untouched.

const (
	fakePiEnv    = "AGENTPROTOCOL_FAKE_PI"
	fakePiRecord = "AGENTPROTOCOL_FAKE_PI_RECORD"
	fakePiFile   = "AGENTPROTOCOL_FAKE_PI_FILE"
)

func init() {
	if os.Getenv(fakePiEnv) != "" {
		runFakePi()
		lingerIfAsked()
		os.Exit(0)
	}
}

// fakePi is pi's side of the protocol. The prompt text picks the behaviour.
type fakePi struct {
	wmu    sync.Mutex
	out    *bufio.Writer
	record *os.File

	mu        sync.Mutex
	streaming bool
	abort     chan struct{}
	steer     chan string
	dialogs   map[string]chan map[string]any
}

func runFakePi() {
	f := &fakePi{out: bufio.NewWriter(os.Stdout), dialogs: map[string]chan map[string]any{}}
	if path := os.Getenv(fakePiRecord); path != "" {
		f.record, _ = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		args, _ := json.Marshal(map[string]any{"args": os.Args[1:]})
		f.record.Write(append(args, '\n'))
	}
	sessionID := "fake-session"
	for i, a := range os.Args {
		if a == "--session-id" && i+1 < len(os.Args) {
			sessionID = os.Args[i+1]
		}
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if f.record != nil {
			f.record.Write(append(line, '\n'))
		}
		var cmd map[string]any
		if json.Unmarshal(line, &cmd) != nil {
			f.send(map[string]any{"type": "response", "command": "parse", "success": false, "error": "bad json"})
			continue
		}
		id, _ := cmd["id"].(string)
		switch cmd["type"] {
		case "get_state":
			f.mu.Lock()
			streaming := f.streaming
			f.mu.Unlock()
			f.send(map[string]any{"id": id, "type": "response", "command": "get_state", "success": true, "data": map[string]any{
				"sessionId": sessionID, "sessionFile": os.Getenv(fakePiFile), "isStreaming": streaming,
				"model": map[string]any{"id": "fake-model", "provider": "fake"}, "messageCount": 0,
			}})
		case "prompt":
			msg, _ := cmd["message"].(string)
			f.mu.Lock()
			if f.streaming {
				if cmd["streamingBehavior"] != "steer" {
					f.mu.Unlock()
					f.send(map[string]any{"id": id, "type": "response", "command": "prompt", "success": false, "error": "Agent is already processing."})
					continue
				}
				ch := f.steer
				f.mu.Unlock()
				f.send(map[string]any{"type": "queue_update", "steering": []string{msg}, "followUp": []string{}})
				f.send(map[string]any{"id": id, "type": "response", "command": "prompt", "success": true})
				if ch != nil {
					ch <- msg
				}
				continue
			}
			f.mu.Unlock()
			f.prompt(id, msg)
		case "steer":
			msg, _ := cmd["message"].(string)
			f.mu.Lock()
			ch := f.steer
			f.mu.Unlock()
			f.send(map[string]any{"id": id, "type": "response", "command": "steer", "success": true})
			if ch != nil {
				ch <- msg
			}
		case "abort":
			f.mu.Lock()
			ch := f.abort
			f.abort = nil
			f.mu.Unlock()
			if ch != nil {
				close(ch)
				for {
					f.mu.Lock()
					s := f.streaming
					f.mu.Unlock()
					if !s {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			f.send(map[string]any{"id": id, "type": "response", "command": "abort", "success": true})
		case "compact":
			f.send(map[string]any{"type": "compaction_start", "reason": "manual"})
			result := map[string]any{"summary": "s", "firstKeptEntryId": "e1", "tokensBefore": 900, "estimatedTokensAfter": 100}
			f.send(map[string]any{"type": "compaction_end", "reason": "manual", "result": result, "aborted": false, "willRetry": false})
			f.send(map[string]any{"id": id, "type": "response", "command": "compact", "success": true, "data": result})
		case "extension_ui_response":
			f.mu.Lock()
			ch := f.dialogs[cmd["id"].(string)]
			delete(f.dialogs, cmd["id"].(string))
			f.mu.Unlock()
			if ch != nil {
				ch <- cmd
			}
		default:
			f.send(map[string]any{"id": id, "type": "response", "command": cmd["type"], "success": false, "error": "Unknown command"})
		}
	}
}

func (f *fakePi) send(v any) {
	data, _ := json.Marshal(v)
	f.wmu.Lock()
	f.out.Write(append(data, '\n'))
	f.out.Flush()
	f.wmu.Unlock()
}

func (f *fakePi) assistant(text, stop, errMsg string) {
	msg := map[string]any{"role": "assistant", "content": []any{}, "provider": "fake", "model": "fake-model", "stopReason": "pending"}
	f.send(map[string]any{"type": "message_start", "message": msg})
	if text != "" {
		f.send(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": text}})
	}
	end := map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}},
		"provider": "fake", "model": "fake-model", "stopReason": stop,
		"usage": map[string]any{"input": 10, "output": 5, "cacheRead": 2, "cacheWrite": 0, "totalTokens": 17, "cost": map[string]any{"total": 0.01}}}
	if errMsg != "" {
		end["errorMessage"] = errMsg
	}
	f.send(map[string]any{"type": "message_end", "message": end})
}

func (f *fakePi) settle() {
	f.send(map[string]any{"type": "agent_end", "messages": []any{}, "willRetry": false})
	f.mu.Lock()
	f.streaming = false
	f.mu.Unlock()
	f.send(map[string]any{"type": "agent_settled"})
}

func (f *fakePi) prompt(id, msg string) {
	respond := func() { f.send(map[string]any{"id": id, "type": "response", "command": "prompt", "success": true}) }
	switch msg {
	case "reject":
		f.send(map[string]any{"id": id, "type": "response", "command": "prompt", "success": false, "error": "No API key found for fake"})
		return
	case "handled":
		respond()
		return
	}
	f.mu.Lock()
	f.streaming = true
	abort := make(chan struct{})
	steer := make(chan string, 4)
	f.abort, f.steer = abort, steer
	f.mu.Unlock()
	respond()
	f.send(map[string]any{"type": "agent_start"})
	f.send(map[string]any{"type": "turn_start"})

	switch msg {
	case "die":
		f.out.Flush()
		os.Exit(3)
	case "hang":
		return
	case "fail":
		f.assistant("", "error", "429 rate limited")
		f.settle()
	case "tool":
		f.send(map[string]any{"type": "tool_execution_start", "toolCallId": "call_1", "toolName": "bash", "args": map[string]any{"command": "ls"}})
		f.send(map[string]any{"type": "tool_execution_end", "toolCallId": "call_1", "toolName": "bash",
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "file.txt"}}}, "isError": false})
		f.assistant("listed", "stop", "")
		f.settle()
	case "slow":
		go func() {
			select {
			case <-abort:
				f.assistant("", "aborted", "Request aborted")
			case <-time.After(10 * time.Second):
				f.assistant("too late", "stop", "")
			}
			f.settle()
		}()
	case "steerable":
		go func() {
			select {
			case s := <-steer:
				f.send(map[string]any{"type": "message_start", "message": map[string]any{"role": "user", "content": s}})
				f.assistant("steered by "+s, "stop", "")
			case <-time.After(10 * time.Second):
				f.assistant("never steered", "stop", "")
			}
			f.settle()
		}()
	case "dialog":
		answer := make(chan map[string]any, 1)
		f.mu.Lock()
		f.dialogs["dlg-1"] = answer
		f.mu.Unlock()
		f.send(map[string]any{"type": "extension_ui_request", "id": "dlg-1", "method": "select", "title": "Allow rm?", "options": []string{"Yes", "No"}})
		go func() {
			select {
			case a := <-answer:
				v, _ := json.Marshal(a)
				f.assistant("answer "+string(v), "stop", "")
			case <-abort:
				f.assistant("", "aborted", "")
			}
			f.settle()
		}()
	default:
		f.assistant("hello from fake pi", "stop", "")
		f.settle()
	}
}

func piRunning(t *testing.T, extraEnv ...string) (*driver.PiBackend, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	record := filepath.Join(t.TempDir(), "record.jsonl")
	env := append(os.Environ(), fakePiEnv+"=1", fakePiRecord+"="+record)
	return &driver.PiBackend{
		Command:       exe,
		Args:          []string{"--provider", "fake"},
		Env:           append(env, extraEnv...),
		ShutdownGrace: 2 * time.Second,
	}, record
}

func openPi(t *testing.T, b *driver.PiBackend, cfg driver.SessionConfig) driver.Session {
	t.Helper()
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	sess, err := b.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	until(t, sess.Events(), ap.AgentEventRunStarted)
	return sess
}

// piRecord is what the fake received: its arguments, then each stdin record.
func piRecord(t *testing.T, path string) (args []string, cmds []map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		_ = json.Unmarshal([]byte(line), &m)
		if i == 0 {
			for _, a := range m["args"].([]any) {
				args = append(args, a.(string))
			}
			continue
		}
		cmds = append(cmds, m)
	}
	return args, cmds
}

func commandsOf(cmds []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, c := range cmds {
		if c["type"] == typ {
			out = append(out, c)
		}
	}
	return out
}

func TestPiOpenNamesTheSessionAndPassesTheLaunchFlags(t *testing.T) {
	b, record := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{Model: "gpt-x", Instructions: "be brief"})
	if sess.ID() != "fake-session" {
		t.Errorf("ID = %q, want pi's own id", sess.ID())
	}
	args, cmds := piRecord(t, record)
	want := []string{"--mode", "rpc", "--model", "gpt-x", "--append-system-prompt", "be brief", "--provider", "fake"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
	if len(cmds) == 0 || cmds[0]["type"] != "get_state" {
		t.Errorf("first command = %v, want get_state", cmds)
	}
	if b.Capabilities().Approvals {
		t.Error("pi does not ask before tool calls; Approvals must be false")
	}
}

func TestPiTextTurn(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{RunID: "r", ChatID: "c"})
	if err := sess.Prompt(context.Background(), driver.TextInput("hi")); err != nil {
		t.Fatal(err)
	}
	evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if got := textOf(evs); got != "hello from fake pi" {
		t.Errorf("text = %q", got)
	}
	if evs[0].Type != ap.AgentEventTurnStarted || countType(evs, ap.AgentEventTurnStarted) != 1 {
		t.Errorf("events = %v", typesOf(evs))
	}
	u, ok := findEvent(evs, ap.AgentEventUsageUpdated)
	if !ok {
		t.Fatal("no usage")
	}
	if p, _ := ap.PayloadAs[ap.UsageUpdatedPayload](u, ap.AgentEventUsageUpdated); p.PromptTokens != 12 || p.CompletionTokens != 5 || p.CostUSD != 0.01 {
		t.Errorf("usage = %+v", p)
	}
	p, _ := ap.PayloadAs[ap.TurnCompletedPayload](evs[len(evs)-1], ap.AgentEventTurnCompleted)
	if !p.HasOutput || p.StopReason != "stop" {
		t.Errorf("turn = %+v", p)
	}
	if evs[len(evs)-1].RunID != "r" || evs[len(evs)-1].ChatID != "c" {
		t.Errorf("event not stamped: %+v", evs[len(evs)-1])
	}
}

func TestPiToolEvents(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("tool"))
	evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	st, ok := findEvent(evs, ap.AgentEventToolStarted)
	if !ok {
		t.Fatalf("no tool.started: %v", typesOf(evs))
	}
	sp, _ := ap.PayloadAs[ap.ToolStartedPayload](st, ap.AgentEventToolStarted)
	if sp.ToolInvocationID != "call_1" || sp.ToolName != "bash" || sp.Arguments["command"] != "ls" {
		t.Errorf("tool.started = %+v", sp)
	}
	done, _ := findEvent(evs, ap.AgentEventToolCompleted)
	cp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
	if cp.Status != ap.ToolInvocationStatusCompleted || cp.Result != "file.txt" {
		t.Errorf("tool.completed = %+v", cp)
	}
	tp, _ := ap.PayloadAs[ap.TurnCompletedPayload](evs[len(evs)-1], ap.AgentEventTurnCompleted)
	if tp.ToolCount != 1 {
		t.Errorf("tool count = %d", tp.ToolCount)
	}
}

func TestPiTurnEndsWithoutARun(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})

	t.Run("rejected prompt is an error", func(t *testing.T) {
		_ = sess.Prompt(context.Background(), driver.TextInput("reject"))
		evs := until(t, sess.Events(), ap.AgentEventError)
		p, _ := ap.PayloadAs[ap.ErrorPayload](evs[len(evs)-1], ap.AgentEventError)
		if p.Code != "prompt_rejected" || !strings.Contains(p.Message, "No API key") {
			t.Errorf("error = %+v", p)
		}
	})
	t.Run("prompt pi handled itself completes", func(t *testing.T) {
		_ = sess.Prompt(context.Background(), driver.TextInput("handled"))
		evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)
		p, _ := ap.PayloadAs[ap.TurnCompletedPayload](evs[len(evs)-1], ap.AgentEventTurnCompleted)
		if p.StopReason != "handled" || p.HasOutput {
			t.Errorf("turn = %+v", p)
		}
	})
	t.Run("failed model call is an error", func(t *testing.T) {
		_ = sess.Prompt(context.Background(), driver.TextInput("fail"))
		evs := until(t, sess.Events(), ap.AgentEventError)
		p, _ := ap.PayloadAs[ap.ErrorPayload](evs[len(evs)-1], ap.AgentEventError)
		if p.Code != "agent_error" || p.Message != "429 rate limited" {
			t.Errorf("error = %+v", p)
		}
	})
	t.Run("session still works", func(t *testing.T) {
		_ = sess.Prompt(context.Background(), driver.TextInput("hi"))
		if got := textOf(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "hello from fake pi" {
			t.Errorf("text = %q", got)
		}
	})
}

func TestPiInterrupt(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("slow"))
	until(t, sess.Events(), ap.AgentEventTurnStarted)
	time.Sleep(100 * time.Millisecond)
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	p, _ := ap.PayloadAs[ap.TurnCompletedPayload](evs[len(evs)-1], ap.AgentEventTurnCompleted)
	if p.StopReason != "interrupted" {
		t.Errorf("stop = %q", p.StopReason)
	}
	_ = sess.Prompt(context.Background(), driver.TextInput("hi"))
	if got := textOf(until(t, sess.Events(), ap.AgentEventTurnCompleted)); got != "hello from fake pi" {
		t.Errorf("after interrupt: %q", got)
	}
}

func TestPiPromptMidTurnSteers(t *testing.T) {
	b, record := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("steerable"))
	// Wait for the run to be under way, so the steer goes as a prompt.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, cmds := piRecord(t, record)
		if len(commandsOf(cmds, "prompt")) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first prompt never sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	_ = sess.Prompt(context.Background(), driver.TextInput("MARKER"))
	evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if got := textOf(evs); got != "steered by MARKER" {
		t.Errorf("text = %q", got)
	}
	if n := countType(evs, ap.AgentEventTurnStarted); n != 1 {
		t.Errorf("%d turn.started, want 1: %v", n, typesOf(evs))
	}
	_, cmds := piRecord(t, record)
	prompts := commandsOf(cmds, "prompt")
	steers := commandsOf(cmds, "steer")
	if len(prompts)+len(steers) != 2 {
		t.Fatalf("prompts %v steers %v", prompts, steers)
	}
	if len(prompts) == 2 && prompts[1]["streamingBehavior"] != "steer" {
		t.Errorf("mid-turn prompt = %v", prompts[1])
	}
}

func TestPiDialogIsAnApproval(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("dialog"))
	evs := until(t, sess.Events(), ap.AgentEventApprovalRequired)
	req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](evs[len(evs)-1], ap.AgentEventApprovalRequired)
	if req.ToolInvocationID != "dlg-1" || req.ToolName != "extension_select" || req.Arguments["title"] != "Allow rm?" {
		t.Errorf("approval = %+v", req)
	}
	if err := sess.Resolve(context.Background(), "nope", driver.Allow()); err == nil {
		t.Error("resolving an unknown request succeeded")
	}
	if err := sess.Resolve(context.Background(), "dlg-1", driver.Allow()); err == nil {
		t.Error("allowing a select with no value succeeded")
	}
	bad := driver.Allow()
	bad.Values = map[string]any{"value": "Maybe"}
	if err := sess.Resolve(context.Background(), "dlg-1", bad); err == nil {
		t.Error("allowing a select with a value not offered succeeded")
	}
	ok := driver.Allow()
	ok.Values = map[string]any{"value": "Yes"}
	if err := sess.Resolve(context.Background(), "dlg-1", ok); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rest := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	if got := textOf(rest); !strings.Contains(got, `"value":"Yes"`) {
		t.Errorf("pi got %q", got)
	}
	if _, ok := findEvent(rest, ap.AgentEventApprovalResolved); !ok {
		t.Error("no approval.resolved")
	}
	if err := sess.Resolve(context.Background(), "dlg-1", ok); err == nil {
		t.Error("resolving twice succeeded")
	}
}

func TestPiInterruptDismissesADialog(t *testing.T) {
	b, record := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("dialog"))
	until(t, sess.Events(), ap.AgentEventApprovalRequired)
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	r, ok := findEvent(evs, ap.AgentEventApprovalResolved)
	if !ok {
		t.Fatalf("dialog not withdrawn: %v", typesOf(evs))
	}
	if p, _ := ap.PayloadAs[ap.ApprovalResolvedPayload](r, ap.AgentEventApprovalResolved); p.Decision != "deny" {
		t.Errorf("resolved = %+v", p)
	}
	_, cmds := piRecord(t, record)
	answers := commandsOf(cmds, "extension_ui_response")
	if len(answers) != 1 || answers[0]["cancelled"] != true {
		t.Errorf("answers = %v", answers)
	}
	if err := sess.Resolve(context.Background(), "dlg-1", driver.Deny("")); err == nil {
		t.Error("resolving a withdrawn dialog succeeded")
	}
}

func TestPiCompactIsTheCompactCommand(t *testing.T) {
	b, record := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("/compact keep the names"))
	evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)
	c, ok := findEvent(evs, ap.AgentEventContextCompacted)
	if !ok {
		t.Fatalf("no context.compacted: %v", typesOf(evs))
	}
	if p, _ := ap.PayloadAs[ap.ContextCompactedPayload](c, ap.AgentEventContextCompacted); p.BeforeTokens != 900 || p.AfterTokens != 100 {
		t.Errorf("compacted = %+v", p)
	}
	p, _ := ap.PayloadAs[ap.TurnCompletedPayload](evs[len(evs)-1], ap.AgentEventTurnCompleted)
	if p.StopReason != "compacted" {
		t.Errorf("turn = %+v", p)
	}
	_, cmds := piRecord(t, record)
	cc := commandsOf(cmds, "compact")
	if len(cc) != 1 || cc[0]["customInstructions"] != "keep the names" || len(commandsOf(cmds, "prompt")) != 0 {
		t.Errorf("commands = %v", cmds)
	}
}

func TestPiResume(t *testing.T) {
	t.Run("an existing session reopens by id", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "s.jsonl")
		os.WriteFile(file, []byte("{}\n"), 0o644)
		b, record := piRunning(t, fakePiFile+"="+file)
		sess := openPi(t, b, driver.SessionConfig{ResumeSessionID: "old-1"})
		if sess.ID() != "old-1" {
			t.Errorf("ID = %q", sess.ID())
		}
		args, _ := piRecord(t, record)
		if i := slices.Index(args, "--session-id"); i < 0 || args[i+1] != "old-1" {
			t.Errorf("args = %q", args)
		}
	})
	t.Run("a missing session is an error, not a fresh one", func(t *testing.T) {
		b, _ := piRunning(t, fakePiFile+"="+filepath.Join(t.TempDir(), "absent.jsonl"))
		sess, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir(), ResumeSessionID: "gone"})
		if err == nil {
			sess.Close()
			t.Fatal("resuming a session pi does not have succeeded")
		}
		if !strings.Contains(err.Error(), "no session gone") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestPiKillMidTurn(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("hang"))
	until(t, sess.Events(), ap.AgentEventTurnStarted)
	if err := sess.(driver.Killer).Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	evs := drainAll(t, sess.Events(), 5*time.Second)
	e, ok := findEvent(evs, ap.AgentEventError)
	if !ok {
		t.Fatalf("no error after kill: %v", typesOf(evs))
	}
	if p, _ := ap.PayloadAs[ap.ErrorPayload](e, ap.AgentEventError); p.Code != "process_exited" {
		t.Errorf("error = %+v", p)
	}
	if err := sess.Prompt(context.Background(), driver.TextInput("hi")); err == nil {
		t.Error("prompt after kill succeeded")
	}
}

func TestPiExitMidTurn(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	_ = sess.Prompt(context.Background(), driver.TextInput("die"))
	evs := drainAll(t, sess.Events(), 5*time.Second)
	e, ok := findEvent(evs, ap.AgentEventError)
	if !ok {
		t.Fatalf("no error after exit: %v", typesOf(evs))
	}
	if p, _ := ap.PayloadAs[ap.ErrorPayload](e, ap.AgentEventError); p.Code != "process_exited" || !strings.Contains(p.Message, "exit status 3") {
		t.Errorf("error = %+v", p)
	}
}

func TestPiCloseEndsEvents(t *testing.T) {
	b, _ := piRunning(t)
	sess := openPi(t, b, driver.SessionConfig{})
	if err := sess.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	select {
	case _, ok := <-sess.Events():
		for ok {
			_, ok = <-sess.Events()
		}
	case <-time.After(3 * time.Second):
		t.Fatal("events still open after close")
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestPiStartupFailureQuotesStderr(t *testing.T) {
	b := &driver.PiBackend{
		Command:     writeScript(t, "#!/bin/sh\necho 'No models available' >&2\nexit 1\n"),
		InitTimeout: 5 * time.Second,
	}
	_, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "No models available") {
		t.Errorf("err = %v", err)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-pi")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
