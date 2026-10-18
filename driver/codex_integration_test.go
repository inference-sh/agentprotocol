package driver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
	"github.com/inference-sh/agentprotocol/harness"
)

// The real-binary test drives the installed codex against harness-test's mock
// Responses API, never the real one. It runs only when CODEX_MOCK_URL names a
// running mock with the control shim, conformance/cmd/drivermock:
//
//	cd conformance && go run ./cmd/drivermock &   # prints the URL
//	CODEX_MOCK_URL=http://127.0.0.1:PORT go test -race -run CodexReal ./driver/
//
// CODEX_HOME is a temp dir for this test only, so the user's own sessions are
// untouched; the key is the mock's placeholder.

func realCodex(t *testing.T) (mock string, b *driver.CodexBackend, home string) {
	t.Helper()
	mock = os.Getenv("CODEX_MOCK_URL")
	if mock == "" {
		t.Skip("CODEX_MOCK_URL not set; see the comment above realCodex")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	home = t.TempDir()
	return mock, codexAgainstMock(t, bin, mock, home), home
}

func codexAgainstMock(t *testing.T, bin, mock, home string) *driver.CodexBackend {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CODEX_HOME=") && !strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			env = append(env, kv)
		}
	}
	env = append(env, "CODEX_HOME="+home, "OPENAI_API_KEY=mock")
	return &driver.CodexBackend{
		Command: bin,
		Args: []string{
			"-c", `model="gpt-4o-mini"`,
			"-c", `model_provider="mock"`,
			"-c", `model_providers.mock.name="Mock"`,
			"-c", `model_providers.mock.base_url="` + mock + `/v1"`,
			"-c", `model_providers.mock.env_key="OPENAI_API_KEY"`,
			"-c", `model_providers.mock.wire_api="responses"`,
		},
		Env:          env,
		ClientName:   "agentprotocol-test",
		OnDiagnostic: func(m string) { t.Log("diagnostic:", m) },
	}
}

func mockCtl(t *testing.T, mock, method, path string, body any) []byte {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, mock+path, rd)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mock %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(res.Body)
	return out.Bytes()
}

// mockRequests returns the bodies of the model requests the mock received.
func mockRequests(t *testing.T, mock string) []string {
	var log []struct {
		Path string          `json:"path"`
		Body json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(mockCtl(t, mock, "GET", "/log", nil), &log)
	var out []string
	for _, e := range log {
		if strings.HasSuffix(e.Path, "/responses") {
			out = append(out, string(e.Body))
		}
	}
	return out
}

func untilWithin(t *testing.T, ch <-chan ap.AgentEvent, want ap.AgentEventType, d time.Duration) []ap.AgentEvent {
	t.Helper()
	var got []ap.AgentEvent
	deadline := time.After(d)
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
			if ev.Type == ap.AgentEventError && want != ap.AgentEventError {
				p, _ := ap.PayloadAs[ap.ErrorPayload](ev, ap.AgentEventError)
				t.Fatalf("error event while waiting for %s: %s", want, p.Message)
			}
		case <-deadline:
			t.Fatalf("no %s within %s; got %v", want, d, typesOf(got))
		}
	}
}

func TestCodexRealTurnApprovalInterruptSteerResume(t *testing.T) {
	mock, b, _ := realCodex(t)
	work := t.TempDir()
	mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
	mockCtl(t, mock, "DELETE", "/log", nil)

	sess, err := b.Open(context.Background(), driver.SessionConfig{RunID: "run_real", ChatID: "chat_real", WorkDir: work})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	threadID := sess.ID()
	t.Logf("thread %s", threadID)
	ev := sess.Events()
	untilWithin(t, ev, ap.AgentEventRunStarted, 10*time.Second)

	t.Run("text turn", func(t *testing.T) {
		_ = sess.Prompt(context.Background(), driver.TextInput("remember the word PINEAPPLE"))
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
		if got := deltas(events); !strings.Contains(got, "Hello from mock server") {
			t.Errorf("text = %q", got)
		}
	})

	approve := func(t *testing.T, file string, res driver.Resolution) {
		mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "exec_command", "Args": `{"cmd":"touch ` + file + `"}`})
		_ = sess.Prompt(context.Background(), driver.TextInput("create "+file))
		events := untilWithin(t, ev, ap.AgentEventApprovalRequired, 60*time.Second)
		req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)
		t.Logf("approval: %+v", req)
		if !strings.Contains(req.Arguments["command"].(string), file) {
			t.Errorf("approval does not show the command: %v", req.Arguments)
		}
		if _, err := os.Stat(filepath.Join(work, file)); err == nil {
			t.Fatal("command ran before anyone approved it")
		}
		if err := sess.Resolve(context.Background(), req.ToolInvocationID, res); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		rest := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
		t.Logf("after resolve: %v", typesOf(rest))
	}

	t.Run("approval allow runs the command", func(t *testing.T) {
		approve(t, "approved.txt", driver.Allow())
		if _, err := os.Stat(filepath.Join(work, "approved.txt")); err != nil {
			t.Errorf("approved command did not run: %v", err)
		}
	})

	t.Run("approval deny does not", func(t *testing.T) {
		approve(t, "denied.txt", driver.Deny("no"))
		if _, err := os.Stat(filepath.Join(work, "denied.txt")); err == nil {
			t.Error("denied command ran")
		}
	})

	t.Run("session scope is not asked again", func(t *testing.T) {
		approve(t, "session.txt", driver.AllowFor(driver.ScopeSession))
		_ = os.Remove(filepath.Join(work, "session.txt"))
		mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "exec_command", "Args": `{"cmd":"touch session.txt"}`})
		_ = sess.Prompt(context.Background(), driver.TextInput("again"))
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
		if n := countType(events, ap.AgentEventApprovalRequired); n != 0 {
			t.Errorf("asked %d more time(s) after a session-scoped approval", n)
		}
		if _, err := os.Stat(filepath.Join(work, "session.txt")); err != nil {
			t.Errorf("command approved for the session did not run: %v", err)
		}
	})

	t.Run("interrupt leaves the thread usable", func(t *testing.T) {
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 20000})
		_ = sess.Prompt(context.Background(), driver.TextInput("slow one"))
		untilWithin(t, ev, ap.AgentEventTurnStarted, 30*time.Second)
		time.Sleep(300 * time.Millisecond)
		if err := sess.Interrupt(context.Background()); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 15*time.Second)
		p, _ := ap.PayloadAs[ap.TurnCompletedPayload](events[len(events)-1], ap.AgentEventTurnCompleted)
		if p.StopReason != "interrupted" {
			t.Errorf("stop reason = %q", p.StopReason)
		}
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
		_ = sess.Prompt(context.Background(), driver.TextInput("fast one"))
		if got := deltas(untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)); !strings.Contains(got, "Hello from mock server") {
			t.Errorf("turn after interrupt: %q", got)
		}
	})

	t.Run("interrupt withdraws a parked approval", func(t *testing.T) {
		mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "exec_command", "Args": `{"cmd":"touch parked.txt"}`})
		_ = sess.Prompt(context.Background(), driver.TextInput("create parked.txt"))
		events := untilWithin(t, ev, ap.AgentEventApprovalRequired, 60*time.Second)
		req, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](events[len(events)-1], ap.AgentEventApprovalRequired)
		if err := sess.Interrupt(context.Background()); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		rest := untilWithin(t, ev, ap.AgentEventTurnCompleted, 15*time.Second)
		t.Logf("after interrupt: %v", typesOf(rest))
		if _, ok := findEvent(rest, ap.AgentEventApprovalResolved); !ok {
			t.Error("the parked approval was never withdrawn")
		}
		if tool, ok := findEvent(rest, ap.AgentEventToolCompleted); !ok {
			t.Error("the parked command's tool invocation was left open")
		} else if p, _ := ap.PayloadAs[ap.ToolCompletedPayload](tool, ap.AgentEventToolCompleted); p.Status != ap.ToolInvocationStatusCancelled {
			t.Errorf("parked command closed as %s, want cancelled", p.Status)
		}
		if err := sess.Resolve(context.Background(), req.ToolInvocationID, driver.Allow()); err == nil {
			t.Error("resolving a withdrawn approval succeeded")
		}
		if _, err := os.Stat(filepath.Join(work, "parked.txt")); err == nil {
			t.Error("the parked command ran")
		}
	})

	t.Run("prompt mid-turn steers the running turn", func(t *testing.T) {
		mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 2000})
		defer mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
		before := len(mockRequests(t, mock))
		_ = sess.Prompt(context.Background(), driver.TextInput("first part"))
		untilWithin(t, ev, ap.AgentEventTurnStarted, 30*time.Second)
		time.Sleep(300 * time.Millisecond)
		_ = sess.Prompt(context.Background(), driver.TextInput("STEER-MARKER"))
		events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
		if n := countType(events, ap.AgentEventTurnStarted); n != 0 {
			t.Errorf("steer started %d new turn(s)", n)
		}
		reqs := mockRequests(t, mock)[before:]
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

	t.Run("resume in a new process continues the conversation", func(t *testing.T) {
		mockCtl(t, mock, "DELETE", "/log", nil)
		again, err := b.Open(context.Background(), driver.SessionConfig{RunID: "run_2", WorkDir: work, ResumeSessionID: threadID})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		defer again.Close()
		if again.ID() != threadID {
			t.Errorf("resumed id = %q, want %q", again.ID(), threadID)
		}
		untilWithin(t, again.Events(), ap.AgentEventRunStarted, 10*time.Second)
		_ = again.Prompt(context.Background(), driver.TextInput("what was the word?"))
		untilWithin(t, again.Events(), ap.AgentEventTurnCompleted, 60*time.Second)
		reqs := mockRequests(t, mock)
		if len(reqs) == 0 || !strings.Contains(reqs[len(reqs)-1], "PINEAPPLE") {
			t.Errorf("the resumed request does not carry the earlier turn")
		}
	})

	t.Run("kill mid-turn, then resume", func(t *testing.T) {
		victim, err := b.Open(context.Background(), driver.SessionConfig{RunID: "run_3", WorkDir: work, ResumeSessionID: threadID})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		untilWithin(t, victim.Events(), ap.AgentEventRunStarted, 10*time.Second)
		mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "exec_command", "Args": `{"cmd":"touch killed.txt"}`})
		_ = victim.Prompt(context.Background(), driver.TextInput("create killed.txt"))
		untilWithin(t, victim.Events(), ap.AgentEventApprovalRequired, 60*time.Second)
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

		mockCtl(t, mock, "DELETE", "/log", nil)
		again, err := b.Open(context.Background(), driver.SessionConfig{RunID: "run_4", WorkDir: work, ResumeSessionID: threadID})
		if err != nil {
			t.Fatalf("resume after kill: %v", err)
		}
		defer again.Close()
		untilWithin(t, again.Events(), ap.AgentEventRunStarted, 10*time.Second)
		_ = again.Prompt(context.Background(), driver.TextInput("still there?"))
		untilWithin(t, again.Events(), ap.AgentEventTurnCompleted, 60*time.Second)
		reqs := mockRequests(t, mock)
		if len(reqs) == 0 || !strings.Contains(reqs[len(reqs)-1], "PINEAPPLE") {
			t.Errorf("the thread resumed after a kill lost the earlier turns")
		}
	})
}

// offeredTools lists the tools a Responses request offers the model: a
// function by its name, a namespace as namespace/name for each of its
// tools, and a hosted tool (web_search, image_generation...) by its type.
func offeredTools(t *testing.T, body string) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("model request: %v", err)
	}
	var out []string
	for _, tool := range req.Tools {
		switch {
		case tool.Type == "namespace":
			for _, sub := range tool.Tools {
				out = append(out, tool.Name+"/"+sub.Name)
			}
		case tool.Name != "":
			out = append(out, tool.Name)
		default:
			out = append(out, tool.Type)
		}
	}
	return out
}

// TestCodexRealCallerTools runs SessionConfig.Tools through the real codex
// as dynamic tools: the model is offered the tool under its own name, the
// call reaches CallTool with no approval, and with OnlyCallerTools it is the
// only tool offered, an MCP server in the user's config included. A codex
// older than harness.FeatureOnlyCallerTools must refuse OnlyCallerTools.
// Run it against another codex by putting that one first on PATH.
func TestCodexRealCallerTools(t *testing.T) {
	for _, only := range []bool{false, true} {
		t.Run(map[bool]string{false: "with codex's tools", true: "only caller tools"}[only], func(t *testing.T) {
			mock, b, home := realCodex(t)
			mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
			out, err := exec.Command(b.Command, "--version").Output()
			if err != nil {
				t.Fatalf("codex --version: %v", err)
			}
			installed := strings.TrimSpace(string(out))
			t.Logf("%s", installed)
			// An MCP server of the user's own, which OnlyCallerTools must
			// keep from the model as well.
			mcp := "[mcp_servers.outside]\nurl = \"" + mock + "/mcp\"\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(mcp), 0o600); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			var calls []driver.ToolCall
			required := []string{"word"}
			cfg := driver.SessionConfig{
				RunID: "run_tools", ChatID: "chat_tools", WorkDir: t.TempDir(), OnlyCallerTools: only,
				// A model codex has metadata for, which gives it apply_patch.
				Model: "gpt-5.4",
				Tools: []ap.Tool{{Type: "function", Function: ap.ToolFunction{
					Name: "lookup", Description: "Look a word up.",
					Parameters: ap.ToolParameters{Type: "object", Properties: ap.ToolParameterProperties{
						"word": {Type: "string"},
					}, Required: &required},
				}}},
				CallTool: func(_ context.Context, call driver.ToolCall) (driver.ToolOutput, error) {
					mu.Lock()
					calls = append(calls, call)
					mu.Unlock()
					return driver.ToolOutput{Text: "LOOKUP-RESULT-4412"}, nil
				},
			}
			sess, err := b.Open(context.Background(), cfg)
			if only && !harness.All["codex"].HasFeature(harness.FeatureOnlyCallerTools, installed) {
				// An older codex keeps a tool of its own (update_plan), so
				// Open refuses rather than run with it.
				if err == nil || !strings.Contains(err.Error(), "OnlyCallerTools needs codex") {
					t.Fatalf("open on %s = %v, want a refusal", installed, err)
				}
				t.Logf("refused on %s: %v", installed, err)
				return
			}
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			ev := sess.Events()
			untilWithin(t, ev, ap.AgentEventRunStarted, 10*time.Second)

			mockCtl(t, mock, "DELETE", "/log", nil)
			mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "lookup", "Args": `{"word":"orb"}`})
			mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "TOOLS-DONE"})
			// The canned text outlives the test; put back the mock's own.
			defer mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "Hello from mock server."})
			_ = sess.Prompt(context.Background(), driver.TextInput("look up orb"))
			events := untilWithin(t, ev, ap.AgentEventTurnCompleted, 60*time.Second)
			t.Logf("events: %v", typesOf(events))

			if _, asked := findEvent(events, ap.AgentEventApprovalRequired); asked {
				t.Error("a caller tool raised an approval")
			}
			mu.Lock()
			got := append([]driver.ToolCall(nil), calls...)
			mu.Unlock()
			if len(got) != 1 || got[0].Name != "lookup" || string(got[0].Arguments) != `{"word":"orb"}` || got[0].ToolInvocationID != "call_mock_1" {
				t.Fatalf("calls = %+v", got)
			}
			if n := countType(events, ap.AgentEventToolStarted); n != 1 {
				t.Errorf("%d tool.started events, want 1", n)
			}
			if n := countType(events, ap.AgentEventToolCompleted); n != 1 {
				t.Errorf("%d tool.completed events, want 1", n)
			}
			started, _ := findEvent(events, ap.AgentEventToolStarted)
			sp, _ := ap.PayloadAs[ap.ToolStartedPayload](started, ap.AgentEventToolStarted)
			if sp.ToolName != "lookup" || sp.ToolInvocationID != got[0].ToolInvocationID || sp.Arguments["word"] != "orb" {
				t.Errorf("tool started = %+v", sp)
			}
			done, _ := findEvent(events, ap.AgentEventToolCompleted)
			dp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
			if dp.ToolName != "lookup" || dp.ToolInvocationID != got[0].ToolInvocationID || dp.Status != ap.ToolInvocationStatusCompleted || dp.Result != "LOOKUP-RESULT-4412" {
				t.Errorf("tool completed = %+v", dp)
			}
			if text := deltas(events); !strings.Contains(text, "TOOLS-DONE") {
				t.Errorf("text = %q", text)
			}

			reqs := mockRequests(t, mock)
			if len(reqs) < 2 {
				t.Fatalf("%d model request(s), want the call and the answer", len(reqs))
			}
			if !strings.Contains(reqs[len(reqs)-1], "LOOKUP-RESULT-4412") {
				t.Error("the tool's result did not reach the model")
			}
			for i, req := range reqs {
				tools := offeredTools(t, req)
				t.Logf("request %d offers %v", i, tools)
				if !slices.Contains(tools, "lookup") {
					t.Errorf("request %d does not offer lookup: %v", i, tools)
				}
				if only && len(tools) != 1 {
					t.Errorf("request %d offers codex's own tools with OnlyCallerTools: %v", i, tools)
				}
				// Without OnlyCallerTools codex keeps its own, so the check
				// above is not passing for want of them. The shell is
				// always there; apply_patch is not (0.163.0 leaves it out
				// for a model it has no metadata for, such as the mock's).
				if !only && !slices.Contains(tools, "exec_command") {
					t.Errorf("request %d lacks codex's shell without OnlyCallerTools: %v", i, tools)
				}
			}

			// The thread keeps its tools when resumed in a new process, and
			// codex's own stay off: thread/resume restores the environment,
			// which each turn turns off again.
			threadID := sess.ID()
			_ = sess.Close()
			cfg.ResumeSessionID = threadID
			again, err := b.Open(context.Background(), cfg)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			defer again.Close()
			untilWithin(t, again.Events(), ap.AgentEventRunStarted, 10*time.Second)
			mockCtl(t, mock, "DELETE", "/log", nil)
			mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": "lookup", "Args": `{"word":"again"}`})
			_ = again.Prompt(context.Background(), driver.TextInput("look it up again"))
			untilWithin(t, again.Events(), ap.AgentEventTurnCompleted, 60*time.Second)
			mu.Lock()
			n := len(calls)
			mu.Unlock()
			if n != 2 {
				t.Errorf("the resumed thread made %d caller tool call(s) in all, want 2", n)
			}
			for i, req := range mockRequests(t, mock) {
				tools := offeredTools(t, req)
				t.Logf("resumed request %d offers %v", i, tools)
				if !slices.Contains(tools, "lookup") || (only && len(tools) != 1) {
					t.Errorf("resumed request %d offers %v", i, tools)
				}
			}
		})
	}
}
