package driver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
)

// The real-binary test drives the installed codex against harness-test's mock
// Responses API, never the real one. It runs only when CODEX_MOCK_URL names a
// running mock with the control shim in testdata/codexmock:
//
//	cd driver/testdata/codexmock
//	go mod edit -replace github.com/belt-sh/harness-test=/path/to/harness-test && go mod tidy
//	go run . &            # prints the URL
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
