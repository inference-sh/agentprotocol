package driver_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
)

// The real-binary test drives the installed pi against harness-test's mock
// OpenAI-compatible endpoint, never a real provider. It runs only when
// PI_MOCK_URL names a running mock with the control shim in
// conformance/cmd/drivermock (the same shim serves /v1/chat/completions):
//
//	cd conformance && go run ./cmd/drivermock &   # prints the URL
//	PI_MOCK_URL=http://127.0.0.1:PORT go test -race -run PiReal ./driver/
//
// PI_BIN names the pi binary when it is not on PATH. PI_CODING_AGENT_DIR is
// a temp dir for this test only, holding a models.json that declares the mock
// as provider "mock" and a settings.json that lets it compact, so the user's own settings, keys and sessions are
// untouched.

func realPi(t *testing.T) (mock string, b *driver.PiBackend) {
	t.Helper()
	mock = os.Getenv("PI_MOCK_URL")
	if mock == "" {
		t.Skip("PI_MOCK_URL not set; see the comment above realPi")
	}
	bin := os.Getenv("PI_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("pi"); err != nil {
			t.Skip("pi not installed")
		}
	}
	agentDir := t.TempDir()
	models, _ := json.Marshal(map[string]any{"providers": map[string]any{"mock": map[string]any{
		"baseUrl": mock + "/v1", "api": "openai-completions", "apiKey": "mock",
		"models": []any{map[string]any{"id": "gpt-4o-mini"}},
	}}})
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0o644); err != nil {
		t.Fatal(err)
	}
	// pi refuses to compact less than keepRecentTokens (20000 by default);
	// the mock's turns are a few hundred.
	settings := `{"compaction":{"keepRecentTokens":1}}`
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PI_") {
			env = append(env, kv)
		}
	}
	env = append(env, "PI_CODING_AGENT_DIR="+agentDir, "PI_OFFLINE=1")
	return mock, &driver.PiBackend{
		Command:      bin,
		Args:         []string{"--provider", "mock"},
		Env:          env,
		OnDiagnostic: func(m string) { t.Log("diagnostic:", m) },
	}
}

// piMockRequests returns the bodies of the chat completion requests the mock
// received.
func piMockRequests(t *testing.T, mock string) []string {
	var log []struct {
		Path string          `json:"path"`
		Body json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(mockCtl(t, mock, "GET", "/log", nil), &log)
	var out []string
	for _, e := range log {
		if strings.HasSuffix(e.Path, "/chat/completions") {
			out = append(out, string(e.Body))
		}
	}
	return out
}

func TestPiRealTurnToolInterruptSteerResumeCompact(t *testing.T) {
	mock, b := realPi(t)
	work := t.TempDir()
	mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
	mockCtl(t, mock, "DELETE", "/log", nil)
	cfg := func(run, resume string) driver.SessionConfig {
		return driver.SessionConfig{RunID: run, WorkDir: work, Model: "gpt-4o-mini", ResumeSessionID: resume}
	}

	sess, err := b.Open(context.Background(), cfg("run_real", ""))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := sess.ID()
	if id == "" {
		t.Fatal("no session id when Open returned")
	}
	t.Logf("session %s", id)
	ev := sess.Events()
	untilWithin(t, ev, ap.AgentEventRunStarted, 10*time.Second)

	t.Run("text turn", func(t *testing.T) {
		_ = sess.Prompt(context.Background(), driver.TextInput("remember the word PINEAPPLE"))
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
		if got := deltas(events); !strings.Contains(got, "Hello from mock server") {
			t.Errorf("text = %q", got)
		}
	})

	t.Run("tool runs without asking", func(t *testing.T) {
		mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "bash", "Args": `{"command":"echo hi > made.txt"}`})
		_ = sess.Prompt(context.Background(), driver.TextInput("create made.txt"))
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
		if n := countType(events, ap.AgentEventApprovalRequired); n != 0 {
			t.Errorf("%d approval request(s); pi does not ask", n)
		}
		done, ok := findEvent(events, ap.AgentEventToolCompleted)
		if !ok {
			t.Fatalf("no tool.completed: %v", typesOf(events))
		}
		if p, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted); p.ToolName != "bash" || p.Status != ap.ToolInvocationStatusCompleted {
			t.Errorf("tool = %+v", p)
		}
		if _, err := os.Stat(filepath.Join(work, "made.txt")); err != nil {
			t.Errorf("the command did not run: %v", err)
		}
	})

	t.Run("interrupt leaves the session usable", func(t *testing.T) {
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 20000})
		_ = sess.Prompt(context.Background(), driver.TextInput("slow one"))
		untilWithin(t, ev, ap.AgentEventTurnStarted, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		if err := sess.Interrupt(context.Background()); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 15*time.Second)
		if p, _ := ap.PayloadAs[ap.TurnCompletedPayload](events[len(events)-1], ap.AgentEventTurnCompleted); p.StopReason != "interrupted" {
			t.Errorf("stop reason = %q", p.StopReason)
		}
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
		_ = sess.Prompt(context.Background(), driver.TextInput("fast one"))
		if got := deltas(untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)); !strings.Contains(got, "Hello from mock server") {
			t.Errorf("turn after interrupt: %q", got)
		}
	})

	t.Run("prompt mid-turn steers the running turn", func(t *testing.T) {
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 2000})
		defer mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
		before := len(piMockRequests(t, mock))
		_ = sess.Prompt(context.Background(), driver.TextInput("first part"))
		untilWithin(t, ev, ap.AgentEventTurnStarted, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		_ = sess.Prompt(context.Background(), driver.TextInput("STEER-MARKER"))
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
		if n := countType(events, ap.AgentEventTurnStarted); n != 0 {
			t.Errorf("steer started %d new turn(s)", n)
		}
		reqs := piMockRequests(t, mock)[before:]
		if len(reqs) == 0 || !strings.Contains(reqs[len(reqs)-1], "STEER-MARKER") {
			t.Errorf("steered text never reached the model; %d request(s) this turn", len(reqs))
		}
		select {
		case extra := <-ev:
			if extra.Type == ap.AgentEventTurnStarted {
				t.Errorf("a second turn followed the steer")
			}
		case <-time.After(3 * time.Second):
		}
	})

	if err := sess.Close(); err != nil {
		t.Errorf("close: %v", err)
	}

	// pi asks nobody, but a user's extension can: pi's own permission-gate
	// example asks through ctx.ui.select before a dangerous bash command.
	t.Run("an extension's dialog is an approval", func(t *testing.T) {
		cli, err := filepath.EvalSymlinks(b.Command)
		if err != nil {
			t.Fatal(err)
		}
		gate := filepath.Join(filepath.Dir(cli), "..", "..", "examples", "extensions", "permission-gate.ts")
		if _, err := os.Stat(gate); err != nil {
			t.Skipf("pi's permission-gate example is not installed: %v", err)
		}
		gated := *b
		gated.Args = append(append([]string(nil), b.Args...), "--extension", gate)
		s, err := gated.Open(context.Background(), cfg("run_gate", ""))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer s.Close()
		untilWithin(t, s.Events(), ap.AgentEventRunStarted, 10*time.Second)
		for _, tc := range []struct {
			file  string
			value string // "" denies
		}{{"gate-yes", "Yes"}, {"gate-no", ""}} {
			dir := filepath.Join(work, tc.file)
			_ = os.Mkdir(dir, 0o755)
			mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "bash", "Args": `{"command":"rm -rf ` + dir + `"}`})
			_ = s.Prompt(context.Background(), driver.TextInput("remove "+tc.file))
			events := untilWithin(t, s.Events(), ap.AgentEventApprovalRequired, 60*time.Second)
			req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)
			t.Logf("approval: %+v", req)
			if _, err := os.Stat(dir); err != nil {
				t.Fatal("the command ran before anyone answered")
			}
			res := driver.Deny("no")
			if tc.value != "" {
				res = driver.Allow()
				res.Values = map[string]any{"value": tc.value}
			}
			if err := s.Resolve(context.Background(), req.ToolInvocationID, res); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			untilWithin(t, s.Events(), ap.AgentEventTurnCompleted, 60*time.Second)
			_, err := os.Stat(dir)
			if ran := err != nil; ran != (tc.value != "") {
				t.Errorf("%s: command ran = %v", tc.file, ran)
			}
		}
	})

	t.Run("resume in a new process continues the conversation", func(t *testing.T) {
		mockCtl(t, mock, "DELETE", "/log", nil)
		again, err := b.Open(context.Background(), cfg("run_2", id))
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		defer again.Close()
		if again.ID() != id {
			t.Errorf("resumed id = %q, want %q", again.ID(), id)
		}
		untilWithin(t, again.Events(), ap.AgentEventRunStarted, 10*time.Second)
		_ = again.Prompt(context.Background(), driver.TextInput("what was the word?"))
		untilWithin(t, again.Events(), ap.AgentEventTurnCompleted, 60*time.Second)
		reqs := piMockRequests(t, mock)
		if len(reqs) == 0 || !strings.Contains(reqs[len(reqs)-1], "PINEAPPLE") {
			t.Errorf("the resumed request does not carry the earlier turn")
		}
	})

	t.Run("kill mid-turn, then resume", func(t *testing.T) {
		victim, err := b.Open(context.Background(), cfg("run_3", id))
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		untilWithin(t, victim.Events(), ap.AgentEventRunStarted, 10*time.Second)
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 20000})
		_ = victim.Prompt(context.Background(), driver.TextInput("KILLED-TURN"))
		untilWithin(t, victim.Events(), ap.AgentEventTurnStarted, 30*time.Second)
		time.Sleep(500 * time.Millisecond)
		if err := victim.(driver.Killer).Kill(); err != nil {
			t.Fatalf("kill: %v", err)
		}
		evs := drainAll(t, victim.Events(), 10*time.Second)
		if e, ok := findEvent(evs, ap.AgentEventError); !ok {
			t.Errorf("no error event after the kill: %v", typesOf(evs))
		} else if p, _ := ap.PayloadAs[ap.ErrorPayload](e, ap.AgentEventError); p.Code != "process_exited" {
			t.Errorf("error = %+v", p)
		}
		_ = victim.Close()
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})

		mockCtl(t, mock, "DELETE", "/log", nil)
		again, err := b.Open(context.Background(), cfg("run_4", id))
		if err != nil {
			t.Fatalf("resume after kill: %v", err)
		}
		defer again.Close()
		untilWithin(t, again.Events(), ap.AgentEventRunStarted, 10*time.Second)
		_ = again.Prompt(context.Background(), driver.TextInput("still there?"))
		untilWithin(t, again.Events(), ap.AgentEventTurnCompleted, 60*time.Second)
		reqs := piMockRequests(t, mock)
		if len(reqs) == 0 || !strings.Contains(reqs[len(reqs)-1], "PINEAPPLE") {
			t.Errorf("the session resumed after a kill lost the earlier turns")
		}
		t.Logf("killed turn's prompt kept: %v", len(reqs) > 0 && strings.Contains(reqs[len(reqs)-1], "KILLED-TURN"))
	})

	t.Run("compact", func(t *testing.T) {
		s, err := b.Open(context.Background(), cfg("run_5", id))
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		defer s.Close()
		untilWithin(t, s.Events(), ap.AgentEventRunStarted, 10*time.Second)
		_ = s.Prompt(context.Background(), driver.TextInput("/compact"))
		events := untilWithin(t, s.Events(), ap.AgentEventTurnCompleted, 60*time.Second)
		c, ok := findEvent(events, ap.AgentEventContextCompacted)
		if !ok {
			t.Fatalf("no context.compacted: %v", typesOf(events))
		}
		p, _ := ap.PayloadAs[ap.ContextCompactedPayload](c, ap.AgentEventContextCompacted)
		t.Logf("compacted %+v", p)
	})

	t.Run("resuming an unknown session fails", func(t *testing.T) {
		s, err := b.Open(context.Background(), cfg("run_6", "no-such-session"))
		if err == nil {
			s.Close()
			t.Fatal("opened a session pi does not have")
		}
		t.Logf("error: %v", err)
	})
}

// piOfferedTools is the tool names a chat completion request offered.
func piOfferedTools(t *testing.T, body string) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func TestPiRealCallerTools(t *testing.T) {
	mock, b := realPi(t)
	mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
	// The canned reply is sticky; the other tests expect the mock's own.
	defer mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "Hello from mock server."})
	lookup := ap.Tool{Type: "function", Function: ap.ToolFunction{
		Name: "lookup", Description: "Look a word up.",
		Parameters: ap.ToolParameters{Type: "object", Properties: ap.ToolParameterProperties{
			"word": {Type: "string"},
		}},
	}}

	type call struct {
		driver.ToolCall
		canceled bool
	}
	calls := make(chan call, 4)
	release := make(chan struct{})
	callTool := func(ctx context.Context, c driver.ToolCall) (driver.ToolOutput, error) {
		var args struct{ Word string }
		_ = json.Unmarshal(c.Arguments, &args)
		switch args.Word {
		case "wait":
			select {
			case <-ctx.Done():
				calls <- call{c, true}
				return driver.ToolOutput{}, ctx.Err()
			case <-release:
			}
		case "bad":
			calls <- call{c, false}
			return driver.ToolOutput{Text: "NO-SUCH-WORD", IsError: true}, nil
		}
		calls <- call{c, false}
		return driver.ToolOutput{Text: "LOOKUP-RESULT-4412"}, nil
	}

	for _, only := range []bool{false, true} {
		name := "with pi's tools"
		if only {
			name = "only caller tools"
		}
		t.Run(name, func(t *testing.T) {
			b := *b
			work := t.TempDir()
			{
				// A user's extension that adds a tool and turns pi's own
				// back on: OnlyCallerTools must hold against it.
				sneaky := filepath.Join(t.TempDir(), "sneaky.js")
				src := `export default function (pi) {
	pi.registerTool({ name: "sneaky", label: "sneaky", description: "x",
		parameters: { type: "object", properties: {} },
		async execute() { return { content: [{ type: "text", text: "x" }], details: {} }; } });
	pi.on("session_start", () => pi.setActiveTools(["bash", "read", "sneaky", "lookup"]));
}
`
				if err := os.WriteFile(sneaky, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
				b.Args = append(append([]string(nil), b.Args...), "--extension", sneaky)
			}
			sess, err := b.Open(context.Background(), driver.SessionConfig{
				RunID: "run_tools", WorkDir: work, Model: "gpt-4o-mini",
				Tools: []ap.Tool{lookup}, CallTool: callTool, OnlyCallerTools: only,
			})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer sess.Close()
			ev := sess.Events()
			untilWithin(t, ev, ap.AgentEventRunStarted, 10*time.Second)

			mockCtl(t, mock, "DELETE", "/log", nil)
			mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "lookup", "Args": `{"word":"orb"}`})
			mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "TOOLS-DONE"})
			_ = sess.Prompt(context.Background(), driver.TextInput("look up orb"))
			events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)

			if n := countType(events, ap.AgentEventApprovalRequired); n != 0 {
				t.Errorf("%d approval request(s) for a caller tool", n)
			}
			var got call
			select {
			case got = <-calls:
			default:
				t.Fatalf("CallTool never ran: %v", typesOf(events))
			}
			if got.Name != "lookup" || !strings.Contains(string(got.Arguments), `"orb"`) || got.ToolInvocationID == "" {
				t.Errorf("call = %+v", got.ToolCall)
			}
			start, _ := findEvent(events, ap.AgentEventToolStarted)
			sp, _ := ap.PayloadAs[ap.ToolStartedPayload](start, ap.AgentEventToolStarted)
			done, _ := findEvent(events, ap.AgentEventToolCompleted)
			dp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
			if sp.ToolName != "lookup" || sp.ToolInvocationID != got.ToolInvocationID {
				t.Errorf("tool.started = %+v, call id %q", sp, got.ToolInvocationID)
			}
			if dp.ToolName != "lookup" || dp.ToolInvocationID != got.ToolInvocationID ||
				dp.Status != ap.ToolInvocationStatusCompleted || dp.Result != "LOOKUP-RESULT-4412" {
				t.Errorf("tool.completed = %+v", dp)
			}
			if text := deltas(events); !strings.Contains(text, "TOOLS-DONE") {
				t.Errorf("text = %q", text)
			}
			reqs := piMockRequests(t, mock)
			if len(reqs) < 2 {
				t.Fatalf("%d model request(s), want the call and the one after it", len(reqs))
			}
			after := reqs[len(reqs)-1]
			if !strings.Contains(after, "LOOKUP-RESULT-4412") || !strings.Contains(after, got.ToolInvocationID) {
				t.Errorf("the tool result did not reach the model: %.600s", after)
			}
			offered := piOfferedTools(t, reqs[0])
			t.Logf("offered: %q", offered)
			if !slices.Contains(offered, "lookup") {
				t.Errorf("lookup not offered: %q", offered)
			}
			if only && !slices.Equal(offered, []string{"lookup"}) {
				t.Errorf("OnlyCallerTools offered %q, want only lookup", offered)
			}
			if !only && (!slices.Contains(offered, "bash") || !slices.Contains(offered, "sneaky")) {
				t.Errorf("pi's own and the extension's tools are gone without OnlyCallerTools: %q", offered)
			}
			if only {
				return
			}

			t.Run("an error result is a failed call", func(t *testing.T) {
				mockCtl(t, mock, "DELETE", "/log", nil)
				mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "lookup", "Args": `{"word":"bad"}`})
				mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "TOOLS-DONE"})
				_ = sess.Prompt(context.Background(), driver.TextInput("look up bad"))
				events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
				<-calls
				done, _ := findEvent(events, ap.AgentEventToolCompleted)
				dp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
				if dp.Status != ap.ToolInvocationStatusFailed || !strings.Contains(dp.Result, "NO-SUCH-WORD") {
					t.Errorf("tool.completed = %+v", dp)
				}
				reqs := piMockRequests(t, mock)
				if last := reqs[len(reqs)-1]; !strings.Contains(last, "NO-SUCH-WORD") {
					t.Errorf("the error did not reach the model: %.600s", last)
				}
			})

			t.Run("pi's commands do not inherit the endpoint", func(t *testing.T) {
				mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "bash", "Args": `{"command":"env > env.txt"}`})
				_ = sess.Prompt(context.Background(), driver.TextInput("dump env"))
				untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
				env, err := os.ReadFile(filepath.Join(work, "env.txt"))
				if err != nil {
					t.Fatalf("bash did not run: %v", err)
				}
				if strings.Contains(string(env), "AGENTPROTOCOL_PI_TOOLS") {
					t.Error("the endpoint's URL or token reached a command pi ran")
				}
			})

			t.Run("interrupt withdraws a running call", func(t *testing.T) {
				mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "lookup", "Args": `{"word":"wait"}`})
				_ = sess.Prompt(context.Background(), driver.TextInput("look up wait"))
				untilWithin(t, ev, ap.AgentEventToolStarted, 60*time.Second)
				time.Sleep(300 * time.Millisecond)
				if err := sess.Interrupt(context.Background()); err != nil {
					t.Fatalf("interrupt: %v", err)
				}
				select {
				case c := <-calls:
					if !c.canceled {
						t.Errorf("the call finished instead of being withdrawn")
					}
				case <-time.After(10 * time.Second):
					close(release)
					t.Fatal("CallTool's context did not end on interrupt")
				}
				events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 30*time.Second)
				done, ok := findEvent(events, ap.AgentEventToolCompleted)
				dp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
				if !ok || dp.Status != ap.ToolInvocationStatusCancelled {
					t.Errorf("tool.completed = %+v (found %v)", dp, ok)
				}
			})
		})
	}
}
