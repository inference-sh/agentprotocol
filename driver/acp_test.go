package driver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/acp"
	"github.com/inference-sh/agentprotocol/driver"
)

// The backend spawns a real process, so the tests need a real ACP agent. This
// test binary doubles as one: when the environment variable is set, TestMain
// hands control to a scripted agent instead of running tests. It is the
// standard Go trick for testing subprocess behaviour without shipping a second
// binary or depending on a coding agent being installed.

const agentEnv = "AGENTPROTOCOL_FAKE_AGENT"

func TestMain(m *testing.M) {
	if script := os.Getenv(agentEnv); script != "" {
		runFakeAgent(script)
		return
	}
	os.Exit(m.Run())
}

// backendRunning returns a backend that launches this test binary as an agent
// following the named script.
func backendRunning(t *testing.T, script string) *driver.ACPBackend {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return &driver.ACPBackend{
		Command:       exe,
		Env:           append(os.Environ(), agentEnv+"="+script),
		ClientName:    "driver-test",
		ClientVersion: "1",
	}
}

// collect drains events until the channel closes or the deadline passes.
func collect(t *testing.T, ch <-chan ap.AgentEvent, want int, timeout time.Duration) []ap.AgentEvent {
	t.Helper()
	var got []ap.AgentEvent
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

func typesOf(events []ap.AgentEvent) []ap.AgentEventType {
	out := make([]ap.AgentEventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func findEvent(events []ap.AgentEvent, want ap.AgentEventType) (ap.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == want {
			return e, true
		}
	}
	return ap.AgentEvent{}, false
}

func TestOpenStartsASessionAndReportsIt(t *testing.T) {
	b := backendRunning(t, "echo")

	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID:   "run_1",
		ChatID:  "chat_1",
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	if sess.ID() != "sess_fake" {
		t.Errorf("session ID = %q, want the one the agent assigned", sess.ID())
	}

	events := collect(t, sess.Events(), 1, 2*time.Second)
	if len(events) == 0 {
		t.Fatal("opening a session emitted no events")
	}
	if events[0].Type != ap.AgentEventRunStarted {
		t.Errorf("first event = %s, want run started", events[0].Type)
	}
	if events[0].RunID != "run_1" || events[0].ChatID != "chat_1" {
		t.Errorf("event not stamped with our identifiers: run=%q chat=%q",
			events[0].RunID, events[0].ChatID)
	}
}

func TestPromptStreamsContentAndCompletesTheTurn(t *testing.T) {
	b := backendRunning(t, "echo")

	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	if err := sess.Prompt(context.Background(), driver.TextInput("hello")); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	events := collect(t, sess.Events(), 4, 3*time.Second)
	types := typesOf(events)
	t.Logf("events: %v", types)

	if _, ok := findEvent(events, ap.AgentEventContentDelta); !ok {
		t.Error("no content delta; the agent's output never reached the caller")
	}
	if _, ok := findEvent(events, ap.AgentEventTurnCompleted); !ok {
		t.Error("no turn completed; a caller cannot tell the work finished")
	}

	delta, _ := findEvent(events, ap.AgentEventContentDelta)
	payload, ok := ap.PayloadAs[ap.ContentDeltaPayload](delta, ap.AgentEventContentDelta)
	if !ok {
		t.Fatal("content delta carried no decodable payload")
	}
	if payload.Delta != "you said hello" {
		t.Errorf("delta = %q, want the agent's reply", payload.Delta)
	}
}

func TestPermissionBecomesAnApprovalEventAndResolveAnswersIt(t *testing.T) {
	// This is the whole point of the backend: a question asked by a process on
	// one machine becomes an event a human can answer from somewhere else.
	b := backendRunning(t, "permission")

	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	if err := sess.Prompt(context.Background(), driver.TextInput("do something risky")); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	events := collect(t, sess.Events(), 3, 3*time.Second)
	approval, ok := findEvent(events, ap.AgentEventApprovalRequired)
	if !ok {
		t.Fatalf("no approval event; got %v", typesOf(events))
	}

	payload, ok := ap.PayloadAs[ap.ApprovalRequiredPayload](approval, ap.AgentEventApprovalRequired)
	if !ok {
		t.Fatal("approval event carried no decodable payload")
	}
	if payload.ToolName != "dangerous" {
		t.Errorf("tool name = %q, want the tool awaiting approval", payload.ToolName)
	}
	if payload.ToolInvocationID == "" {
		t.Fatal("approval carried no invocation ID, so nothing can be answered")
	}

	if err := sess.Resolve(context.Background(), payload.ToolInvocationID, driver.Allow()); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rest := collect(t, sess.Events(), 3, 3*time.Second)
	all := append(events, rest...)
	if _, ok := findEvent(all, ap.AgentEventApprovalResolved); !ok {
		t.Error("no approval-resolved event after answering")
	}
	if _, ok := findEvent(all, ap.AgentEventTurnCompleted); !ok {
		t.Errorf("turn never completed after approval; got %v", typesOf(all))
	}
}

func TestResolveUnknownRequestIsAnError(t *testing.T) {
	// Silently ignoring it would leave the caller waiting for a turn that can
	// never finish.
	b := backendRunning(t, "echo")
	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	if err := sess.Resolve(context.Background(), "never_asked", driver.Allow()); err == nil {
		t.Error("resolving an unknown request succeeded")
	}
}

func TestCloseEndsTheEventStream(t *testing.T) {
	b := backendRunning(t, "echo")
	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	_ = sess.Close()

	select {
	case _, open := <-drain(sess.Events()):
		if open {
			t.Error("event channel still open after close")
		}
	case <-time.After(2 * time.Second):
		t.Error("event channel never closed; a consumer would block forever")
	}
}

// drain reads until the channel closes and reports the final state.
func drain(ch <-chan ap.AgentEvent) <-chan ap.AgentEvent {
	out := make(chan ap.AgentEvent)
	go func() {
		for range ch {
		}
		close(out)
	}()
	return out
}

func TestBackendDeclaresWhatItCanDo(t *testing.T) {
	b := &driver.ACPBackend{Command: "irrelevant"}
	caps := b.Capabilities()

	if b.Kind() != driver.KindACP {
		t.Errorf("kind = %q", b.Kind())
	}
	if !caps.Approvals {
		t.Error("ACP carries permission requests; approvals should be true")
	}
	// This assertion used to read the other way, on the reasoning that a
	// session lives inside a process. It does not: the agent persists the
	// session and session/load brings the conversation into a new process.
	// Every agent measured accepts the call after a clean close.
	if !caps.Resume {
		t.Error("session/load resumes a persisted session; resume should be true")
	}
}

// Resuming must not replay history as live events. A run that announced last
// week's tool calls as happening now would be wrong in every display, and in
// the api it would raise an approval for each one.
func TestResumeReplaysHistoryWithoutEmittingIt(t *testing.T) {
	for _, script := range []string{"resume", "resume-quiet"} {
		t.Run(script, func(t *testing.T) {
			var diags []string
			b := backendRunning(t, script)
			b.OnDiagnostic = func(m string) { diags = append(diags, m) }
			b.ReplayIdleGap = 200 * time.Millisecond

			sess, err := b.Open(context.Background(), driver.SessionConfig{
				RunID:           "run-1",
				WorkDir:         t.TempDir(),
				ResumeSessionID: "ses_f647af",
			})
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			defer sess.Close()

			if sess.ID() != "ses_f647af" {
				t.Errorf("session id = %q, want the one we resumed", sess.ID())
			}

			// Only run_started, never the replayed chunks.
			select {
			case ev := <-sess.Events():
				if ev.Type != ap.AgentEventRunStarted {
					t.Errorf("first event = %s, want run_started; replay leaked into the stream", ev.Type)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no run_started event")
			}
			select {
			case ev := <-sess.Events():
				t.Errorf("unexpected event %s: replayed history was emitted", ev.Type)
			case <-time.After(300 * time.Millisecond):
			}

			if len(diags) == 0 {
				t.Error("a resume should report how much it replayed")
			}
		})
	}
}

func TestResumeCanOptIntoTheHistory(t *testing.T) {
	b := backendRunning(t, "resume")
	b.EmitReplay = true

	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID:           "run-1",
		WorkDir:         t.TempDir(),
		ResumeSessionID: "ses_f647af",
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer sess.Close()

	var kinds []ap.AgentEventType
	deadline := time.After(3 * time.Second)
	for len(kinds) < 3 {
		select {
		case ev := <-sess.Events():
			kinds = append(kinds, ev.Type)
		case <-deadline:
			t.Fatalf("only saw %v; want the replayed history too", kinds)
		}
	}
}

func TestOpenWithoutACommandFails(t *testing.T) {
	b := &driver.ACPBackend{}
	if _, err := b.Open(context.Background(), driver.SessionConfig{}); err == nil {
		t.Error("opened a session with no command to launch")
	}
}

// --- the fake agent ---

// runFakeAgent speaks just enough ACP to exercise the backend, following one
// of a few named scripts.
func runFakeAgent(script string) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 1<<20)
	out := os.Stdout

	send := func(v any) {
		data, _ := json.Marshal(v)
		out.Write(append(data, '\n'))
	}
	reply := func(id int, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	update := func(u acp.SessionUpdate) {
		send(map[string]any{
			"jsonrpc": "2.0",
			"method":  acp.MethodSessionUpdate,
			"params":  acp.UpdateNotification{SessionID: "sess_fake", Update: u},
		})
	}

	// pendingPrompt holds the prompt call ID while we wait for a permission
	// answer, so the turn only completes after the human decides.
	pendingPrompt := 0

	for in.Scan() {
		var msg acp.Message
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}

		switch {
		case msg.Method == acp.MethodInitialize:
			reply(*msg.ID, map[string]any{"protocolVersion": acp.ProtocolVersion})

		case msg.Method == acp.MethodSessionNew:
			reply(*msg.ID, acp.NewSessionResult{SessionID: "sess_fake"})

		case msg.Method == acp.MethodSessionLoad:
			// Replay two updates, as a resuming agent does. Under the
			// "resume-quiet" script the call is never answered, which is how
			// most real agents behave.
			update(acp.SessionUpdate{
				Kind:    acp.UpdateKindAgentMessageChunk,
				Content: json.RawMessage(`{"type":"text","text":"old question"}`),
			})
			update(acp.SessionUpdate{
				Kind:    acp.UpdateKindAgentMessageChunk,
				Content: json.RawMessage(`{"type":"text","text":"old answer"}`),
			})
			if script != "resume-quiet" {
				reply(*msg.ID, map[string]any{})
			}

		case msg.Method == acp.MethodSessionPrompt:
			var p acp.PromptParams
			_ = json.Unmarshal(msg.Params, &p)
			text := ""
			if len(p.Prompt) > 0 {
				text = p.Prompt[0].Text
			}

			if script == "diagnostics" {
				// An agent-initiated call this client does not implement,
				// then a normal turn. The id-less error comes at close.
				send(map[string]any{
					"jsonrpc": "2.0", "id": 8001,
					"method": "_kiro.dev/telemetry", "params": map[string]any{},
				})
				update(acp.SessionUpdate{
					Kind:    acp.UpdateKindAgentMessageChunk,
					Content: json.RawMessage(`{"type":"text","text":"done"}`),
				})
				reply(*msg.ID, map[string]any{"stopReason": "end_turn"})
				continue
			}

			if script == "permission" {
				pendingPrompt = *msg.ID
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      9001,
					"method":  acp.MethodRequestPermission,
					"params": acp.PermissionRequest{
						SessionID: "sess_fake",
						ToolCall: &acp.PermissionToolRef{
							ToolCallID: "call_danger",
							Title:      "dangerous",
							RawInput:   json.RawMessage(`{"target":"prod"}`),
						},
						Options: []acp.PermissionOption{
							{OptionID: "o_allow", Kind: acp.OptionKindAllowOnce},
							{OptionID: "o_reject", Kind: acp.OptionKindRejectOnce},
						},
					},
				})
				continue
			}

			update(acp.SessionUpdate{
				Kind:    acp.UpdateKindAgentMessageChunk,
				Content: json.RawMessage(`{"type":"text","text":"you said ` + text + `"}`),
			})
			reply(*msg.ID, map[string]any{"stopReason": "end_turn"})

		case msg.IsResponse() && msg.ID != nil && *msg.ID == 9001:
			// The permission answer came back; finish the turn.
			update(acp.SessionUpdate{
				Kind:    acp.UpdateKindAgentMessageChunk,
				Content: json.RawMessage(`{"type":"text","text":"done"}`),
			})
			if pendingPrompt != 0 {
				reply(pendingPrompt, map[string]any{"stopReason": "end_turn"})
			}

		case msg.Method == acp.MethodSessionClose:
			if script == "diagnostics" {
				// kiro rejects session/close with an error carrying no id.
				send(map[string]any{
					"jsonrpc": "2.0",
					"error": map[string]any{
						"code": -32601, "message": "Method not found", "data": "session/close",
					},
				})
			}
			// A real agent finishes its end-of-session work and exits, which
			// closes the stream and releases the shutdown grace early.
			os.Exit(0)

		case msg.Method == acp.MethodSessionCancel:
			// Notification; nothing to answer.

		default:
			if msg.ID != nil {
				reply(*msg.ID, map[string]any{})
			}
		}
	}
	os.Exit(0)
}

var _ = exec.Command // kept for clarity about what the backend does

func TestDiagnosticsReachTheCallerInsteadOfVanishing(t *testing.T) {
	// The client stopped discarding unattributable errors in v0.2.1, but the
	// backend then dropped them by not wiring the handler, which is the same
	// bug one layer up. kiro answers session/close with an id-less error, so
	// this is not hypothetical.
	var mu sync.Mutex
	var seen []string

	b := backendRunning(t, "diagnostics")
	b.OnDiagnostic = func(msg string) {
		mu.Lock()
		seen = append(seen, msg)
		mu.Unlock()
	}

	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sess.Prompt(context.Background(), driver.TextInput("go")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	collect(t, sess.Events(), 4, 3*time.Second)
	_ = sess.Close()

	mu.Lock()
	defer mu.Unlock()
	var peerErr, unhandled bool
	for _, m := range seen {
		if strings.Contains(m, "unattributed error") && strings.Contains(m, "Method not found") {
			peerErr = true
		}
		if strings.Contains(m, "does not implement") && strings.Contains(m, "_kiro.dev/telemetry") {
			unhandled = true
		}
	}
	if !peerErr {
		t.Errorf("an id-less agent error never reached the caller; saw %v", seen)
	}
	if !unhandled {
		t.Errorf("an unimplemented method call never reached the caller; saw %v", seen)
	}
}

func TestDiagnosticsAreOptional(t *testing.T) {
	// A nil hook must discard rather than crash, so a caller that does not
	// care is not forced to supply one.
	b := backendRunning(t, "diagnostics")
	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := sess.Prompt(context.Background(), driver.TextInput("go")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	collect(t, sess.Events(), 4, 3*time.Second)
	if err := sess.Close(); err != nil {
		t.Logf("close: %v", err)
	}
}
