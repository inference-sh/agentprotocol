package driver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/driver"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
)

// runFakeGrok plays grok's side of its ACP extensions, in the shapes its
// source sends them (grok-build 2026-09-29):
//
//   - grok-question: the prompt asks _x.ai/ask_user_question and says what
//     it got back;
//   - grok-background: the prompt starts a background command and ends;
//     the command then finishes and grok starts a wake turn about it.
func runFakeGrok(script string) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 1<<20)
	send := func(v any) {
		data, _ := json.Marshal(v)
		os.Stdout.Write(append(data, '\n'))
	}
	reply := func(id int, result any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
	notify := func(method string, params any) {
		send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
	update := func(u acp.SessionUpdate, meta map[string]any) {
		params := map[string]any{"sessionId": "sess_grok", "update": u}
		if meta != nil {
			params["_meta"] = meta
		}
		notify(acp.MethodSessionUpdate, params)
	}
	text := func(s string) acp.SessionUpdate {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
		return acp.SessionUpdate{Kind: acp.UpdateKindAgentMessageChunk, Content: b}
	}

	prompt := 0
	for in.Scan() {
		var msg acp.Message
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		switch {
		case msg.Method == acp.MethodInitialize:
			reply(*msg.ID, map[string]any{"protocolVersion": acp.ProtocolVersion})
		case msg.Method == acp.MethodSessionNew:
			reply(*msg.ID, map[string]any{"sessionId": "sess_grok", "models": map[string]any{
				"currentModelId": "grok-4.6",
				"availableModels": []any{
					map[string]any{"modelId": "grok-4.3", "_meta": map[string]any{"totalContextTokens": 1000000}},
					map[string]any{"modelId": "grok-4.6", "_meta": map[string]any{"totalContextTokens": 500000, "contextWindows": []int{256000, 500000}}},
				},
			}})
		case msg.Method == acp.MethodSessionPrompt:
			prompt = *msg.ID
			switch script {
			case "grok-question":
				send(map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "_x.ai/ask_user_question", "params": map[string]any{
					"sessionId": "sess_grok", "toolCallId": "call-q1", "mode": "default",
					"questions": []any{map[string]any{
						"question": "Which database?", "multiSelect": true,
						"options": []any{
							map[string]any{"label": "Redis", "description": "In-memory store"},
							map[string]any{"label": "Postgres"},
						},
					}},
				}})
			case "grok-background":
				update(acp.SessionUpdate{Kind: acp.UpdateKindToolCall, ToolCallID: "call-bg", Title: "[bg] make build (01234567)", Status: "completed",
					RawOutput: json.RawMessage(`{"type":"BackgroundTaskStarted","task_id":"0190-task","task_type":"bash","status":"running","command":"make build","summary":"build"}`)}, nil)
				reply(prompt, map[string]any{"stopReason": "end_turn"})
				time.Sleep(50 * time.Millisecond)
				notify("_x.ai/task_completed", map[string]any{"sessionId": "sess_grok", "update": map[string]any{
					"sessionUpdate": "task_completed", "will_wake": true,
					"task_snapshot": map[string]any{"task_id": "0190-task", "command": "make build", "exit_code": 2, "completed": true, "kind": "bash", "explicitly_killed": false, "output": "error: no rule"},
				}})
				update(text("the build failed"), map[string]any{"promptId": "task-completed-0190-task"})
				notify("_x.ai/session/prompt_complete", map[string]any{"sessionId": "sess_grok", "promptId": "task-completed-0190-task", "stopReason": "end_turn"})
			}
		case msg.Method == acp.MethodSessionClose:
			os.Exit(0)
		case msg.ID != nil && *msg.ID == 9001:
			// The answer to the question: say it back and end the turn.
			update(text(string(msg.Result)), nil)
			reply(prompt, map[string]any{"stopReason": "end_turn"})
		}
	}
	lingerIfAsked()
}

// collectUntil drains events up to and including the n-th one of type
// last, or until the deadline passes.
func collectUntil(t *testing.T, ch <-chan ap.AgentEvent, last ap.AgentEventType, n int, timeout time.Duration) []ap.AgentEvent {
	t.Helper()
	var got []ap.AgentEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
			if ev.Type == last {
				if n--; n == 0 {
					return got
				}
			}
		case <-deadline:
			return got
		}
	}
}

func grokBackend(t *testing.T, script string) *driver.ACPBackend {
	b := backendRunning(t, script)
	b.Extension = func() driver.ACPExtension { return driver.ACPExtensionFor("grok") }
	return b
}

// A question grok asks reaches the person as a form, and their answer goes
// back in grok's shape: keyed by the question, with their own words as
// Other and its notes.
func TestGrokQuestionIsAFormAndTheAnswerGoesBackInGroksShape(t *testing.T) {
	sess, err := grokBackend(t, "grok-question").Open(context.Background(), driver.SessionConfig{RunID: "r", ChatID: "c", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	opened := collectUntil(t, sess.Events(), ap.AgentEventContextUpdated, 1, 3*time.Second)
	ctxEv, ok := findEvent(opened, ap.AgentEventContextUpdated)
	if !ok {
		t.Fatalf("no context window at open; got %v", typesOf(opened))
	}
	if c, _ := ap.PayloadAs[ap.ContextUpdatedPayload](ctxEv, ap.AgentEventContextUpdated); c.MaxTokens != 500000 {
		t.Fatalf("context window = %d, want the current model's 500000", c.MaxTokens)
	}

	if err := sess.Prompt(context.Background(), driver.TextInput("set up a cache")); err != nil {
		t.Fatal(err)
	}
	events := collectUntil(t, sess.Events(), ap.AgentEventApprovalRequired, 1, 3*time.Second)
	ev, ok := findEvent(events, ap.AgentEventApprovalRequired)
	if !ok {
		t.Fatalf("no question; got %v", typesOf(events))
	}
	p, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](ev, ap.AgentEventApprovalRequired)
	if p.Reason != ap.InterruptReasonInput || p.Input == nil || p.ToolInvocationID != "call-q1" {
		t.Fatalf("question payload = %+v", p)
	}
	var schema struct {
		Properties map[string]struct {
			Type  string `json:"type"`
			Items struct {
				AnyOf []struct {
					Const string `json:"const"`
				} `json:"anyOf"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(p.Input.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if q := schema.Properties["q1"]; q.Type != "array" || len(q.Items.AnyOf) != 2 || q.Items.AnyOf[0].Const != "Redis" {
		t.Fatalf("multi-select question became %+v", q)
	}

	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Resolution{
		Decision: ap.InterruptResolutionAllow,
		Values:   map[string]any{"q1": []any{"Redis"}, "q1_other": "and SQLite for tests"},
	}); err != nil {
		t.Fatal(err)
	}
	rest := collectUntil(t, sess.Events(), ap.AgentEventTurnCompleted, 1, 3*time.Second)
	var said string
	for _, e := range rest {
		if d, ok := ap.PayloadAs[ap.ContentDeltaPayload](e, ap.AgentEventContentDelta); ok {
			said += d.Delta
		}
	}
	var got struct {
		Outcome     string                       `json:"outcome"`
		Answers     map[string][]string          `json:"answers"`
		Annotations map[string]map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal([]byte(said), &got); err != nil {
		t.Fatalf("grok was answered %q: %v", said, err)
	}
	if got.Outcome != "accepted" || len(got.Answers["Which database?"]) != 2 || got.Answers["Which database?"][1] != "Other" ||
		got.Annotations["Which database?"]["notes"] != "and SQLite for tests" {
		t.Fatalf("grok was answered %s", said)
	}
}

// A declined question is cancelled for grok.
func TestGrokDeclinedQuestionIsCancelled(t *testing.T) {
	sess, err := grokBackend(t, "grok-question").Open(context.Background(), driver.SessionConfig{RunID: "r", ChatID: "c", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	if err := sess.Prompt(context.Background(), driver.TextInput("set up a cache")); err != nil {
		t.Fatal(err)
	}
	events := collectUntil(t, sess.Events(), ap.AgentEventApprovalRequired, 1, 3*time.Second)
	ev, ok := findEvent(events, ap.AgentEventApprovalRequired)
	if !ok {
		t.Fatalf("no question; got %v", typesOf(events))
	}
	p, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](ev, ap.AgentEventApprovalRequired)
	if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Deny("")); err != nil {
		t.Fatal(err)
	}
	rest := collectUntil(t, sess.Events(), ap.AgentEventTurnCompleted, 1, 3*time.Second)
	d, ok := findEvent(rest, ap.AgentEventContentDelta)
	if !ok {
		t.Fatalf("no answer said back; got %v", typesOf(rest))
	}
	if c, _ := ap.PayloadAs[ap.ContentDeltaPayload](d, ap.AgentEventContentDelta); c.Delta != `{"outcome":"cancelled"}` {
		t.Fatalf("grok was answered %s", c.Delta)
	}
}

// A background command is background work of its own, which ends when grok
// says it finished; the turn grok then starts about it is a turn of the
// session, marked as the agent's.
func TestGrokBackgroundWorkAndItsWakeTurn(t *testing.T) {
	sess, err := grokBackend(t, "grok-background").Open(context.Background(), driver.SessionConfig{RunID: "r", ChatID: "c", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	if err := sess.Prompt(context.Background(), driver.TextInput("build it in the background")); err != nil {
		t.Fatal(err)
	}
	events := collectUntil(t, sess.Events(), ap.AgentEventTurnCompleted, 2, 3*time.Second)

	var order []ap.AgentEventType
	for _, e := range events {
		switch e.Type {
		case ap.AgentEventTurnStarted, ap.AgentEventTurnCompleted, ap.AgentEventBackgroundStarted, ap.AgentEventBackgroundCompleted, ap.AgentEventContentDelta:
			order = append(order, e.Type)
		}
	}
	want := []ap.AgentEventType{
		ap.AgentEventTurnStarted, ap.AgentEventBackgroundStarted, ap.AgentEventTurnCompleted,
		ap.AgentEventBackgroundCompleted, ap.AgentEventTurnStarted, ap.AgentEventContentDelta, ap.AgentEventTurnCompleted,
	}
	if len(order) != len(want) {
		t.Fatalf("events %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("events %v, want %v", order, want)
		}
	}

	started, _ := findEvent(events, ap.AgentEventBackgroundStarted)
	s, _ := ap.PayloadAs[ap.BackgroundStartedPayload](started, ap.AgentEventBackgroundStarted)
	if s.TaskID != "0190-task" || s.Kind != ap.BackgroundCommand || s.ToolInvocationID != "call-bg" || s.Command != "make build" {
		t.Fatalf("background started %+v", s)
	}
	done, _ := findEvent(events, ap.AgentEventBackgroundCompleted)
	c, _ := ap.PayloadAs[ap.BackgroundCompletedPayload](done, ap.AgentEventBackgroundCompleted)
	if c.Status != ap.BackgroundFailed || c.ExitCode == nil || *c.ExitCode != 2 || c.Output != "error: no rule" {
		t.Fatalf("background completed %+v", c)
	}
	var wake ap.TurnStartedPayload
	n := 0
	for _, e := range events {
		if e.Type == ap.AgentEventTurnStarted {
			n++
			if n == 2 {
				wake, _ = ap.PayloadAs[ap.TurnStartedPayload](e, ap.AgentEventTurnStarted)
			}
		}
	}
	if wake.Origin != ap.TurnOriginBackground || wake.TaskID != "0190-task" {
		t.Fatalf("wake turn %+v", wake)
	}
}
