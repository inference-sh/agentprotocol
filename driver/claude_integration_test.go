package driver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// TestClaudeRealBinary drives the installed claude against a local mock of
// the Anthropic Messages API. It never reaches the real API: the child gets
// ANTHROPIC_BASE_URL pointed at the mock, a dummy key the mock ignores, and a
// throwaway CLAUDE_CONFIG_DIR so the user's sessions and settings are not
// touched.
//
// It skips unless two variables are set:
//
//	AGENTPROTOCOL_CLAUDE_MOCK_URL      the Messages API base, e.g. http://127.0.0.1:4000
//	AGENTPROTOCOL_CLAUDE_MOCK_CONTROL  the mock's control port
//
// The mock is harness-test's server package (github.com/belt-sh/harness-test/
// server) behind a small wrapper that exposes on the control port:
//
//	POST   /text  {"Text": "..."}               canned reply
//	POST   /tool  {"Name": "...", "Args": "..."} answer the next request with this tool call
//	GET    /log                                 the recorded requests
//	DELETE /log                                 clear them
//	POST   /mcp                                 an MCP server with one tool, echo
//	GET    /mcp/calls                           the MCP methods called
//
// harness-test depends on this module, so the wrapper cannot live here.
func TestClaudeRealBinary(t *testing.T) {
	mockURL := os.Getenv("AGENTPROTOCOL_CLAUDE_MOCK_URL")
	control := os.Getenv("AGENTPROTOCOL_CLAUDE_MOCK_CONTROL")
	if mockURL == "" || control == "" {
		t.Skip("set AGENTPROTOCOL_CLAUDE_MOCK_URL and AGENTPROTOCOL_CLAUDE_MOCK_CONTROL to run against the real claude")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}

	mock := &claudeMock{t: t, control: control}
	configDir := t.TempDir()
	workDir := t.TempDir()

	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAUDE") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"CLAUDE_CONFIG_DIR="+configDir,
		"ANTHROPIC_BASE_URL="+mockURL,
		"ANTHROPIC_API_KEY=sk-ant-mock-not-a-key",
		"DISABLE_AUTOUPDATER=1",
	)

	var stderr bytes.Buffer
	b := &driver.ClaudeBackend{
		Command:      bin,
		Env:          env,
		Stderr:       &stderr,
		OnDiagnostic: func(m string) { t.Log("diagnostic:", m) },
	}
	defer func() {
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("claude stderr:\n%s", stderr.String())
		}
	}()

	cfg := driver.SessionConfig{
		RunID: "run-int", ChatID: "chat-int", WorkDir: workDir,
		Model:        "claude-haiku-4-5-20251001",
		Instructions: "INSTRUCTION-MARKER-7731: answer tersely.",
		Metadata:     map[string]string{"mcp_url": control + "/mcp", "mcp_name": "host"},
	}
	sess, err := b.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := sess.ID()
	t.Logf("session %s", id)
	ev := until(t, sess.Events(), ap.AgentEventRunStarted)

	turn := func(prompt string) []ap.AgentEvent {
		t.Helper()
		if err := sess.Prompt(context.Background(), driver.TextInput(prompt)); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		return untilLong(t, sess.Events(), ap.AgentEventTurnCompleted)
	}
	approval := func(prompt string) ap.ApprovalRequiredPayload {
		t.Helper()
		if err := sess.Prompt(context.Background(), driver.TextInput(prompt)); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		evs := untilLong(t, sess.Events(), ap.AgentEventApprovalRequired)
		p, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](evs[len(evs)-1], ap.AgentEventApprovalRequired)
		return p
	}
	toolStatus := func(evs []ap.AgentEvent) ap.ToolCompletedPayload {
		t.Helper()
		e, ok := findEvent(evs, ap.AgentEventToolCompleted)
		if !ok {
			t.Fatalf("no tool.completed in %v", typesOf(evs))
		}
		p, _ := ap.PayloadAs[ap.ToolCompletedPayload](e, ap.AgentEventToolCompleted)
		return p
	}
	_ = ev

	t.Run("text turn", func(t *testing.T) {
		mock.clear()
		mock.text("TEXT-REPLY-ONE")
		evs := turn("FIRST-PROMPT-MARKER say something")
		if got := textOf(evs); got != "TEXT-REPLY-ONE" {
			t.Errorf("text = %q; events %v", got, typesOf(evs))
		}
		if sess.ID() != id {
			t.Errorf("ID changed from %s to %s after init", id, sess.ID())
		}
		if !strings.Contains(mock.lastRequest(), "INSTRUCTION-MARKER-7731") {
			t.Error("instructions did not reach the system prompt")
		}
	})

	t.Run("allow runs the tool", func(t *testing.T) {
		mock.tool("Bash", `{"command":"touch allowed.txt","description":"create a file"}`)
		mock.text("ALLOWED-DONE")
		p := approval("create allowed.txt")
		if p.ToolName != "Bash" || p.ToolInvocationID == "" {
			t.Fatalf("approval = %+v", p)
		}
		if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Allow()); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		evs := untilLong(t, sess.Events(), ap.AgentEventTurnCompleted)
		if s := toolStatus(evs); s.Status != ap.ToolInvocationStatusCompleted {
			t.Errorf("tool = %+v", s)
		}
		if _, err := os.Stat(filepath.Join(workDir, "allowed.txt")); err != nil {
			t.Errorf("approved command did not run: %v", err)
		}
	})

	t.Run("deny stops the tool", func(t *testing.T) {
		mock.tool("Bash", `{"command":"touch denied.txt","description":"create a file"}`)
		mock.text("DENIED-DONE")
		p := approval("create denied.txt")
		if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Deny("DENY-REASON-42")); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		evs := untilLong(t, sess.Events(), ap.AgentEventTurnCompleted)
		s := toolStatus(evs)
		if s.Status != ap.ToolInvocationStatusCancelled || !strings.Contains(s.Result, "DENY-REASON-42") {
			t.Errorf("tool = %+v", s)
		}
		if _, err := os.Stat(filepath.Join(workDir, "denied.txt")); err == nil {
			t.Error("denied command ran")
		}
	})

	t.Run("allow for session stops asking", func(t *testing.T) {
		mock.tool("Bash", `{"command":"touch session.txt","description":"create a file"}`)
		mock.text("SESSION-ONE")
		p := approval("create session.txt")
		if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.AllowFor(driver.ScopeSession)); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		untilLong(t, sess.Events(), ap.AgentEventTurnCompleted)

		// The same command again: the session rule answers it.
		mock.tool("Bash", `{"command":"touch session.txt","description":"create a file"}`)
		mock.text("SESSION-TWO")
		evs := turn("create session.txt again")
		if _, asked := findEvent(evs, ap.AgentEventApprovalRequired); asked {
			t.Error("asked again despite a session-scoped allow")
		}
		if s := toolStatus(evs); s.Status != ap.ToolInvocationStatusCompleted {
			t.Errorf("tool = %+v", s)
		}
	})

	t.Run("mid-turn prompt steers", func(t *testing.T) {
		// A session of its own: the mock gives every tool call the same id,
		// and a history holding several of them is rewritten by the CLI.
		scfg := cfg
		scfg.WorkDir = t.TempDir()
		scfg.Metadata = nil
		st, err := b.Open(context.Background(), scfg)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer st.Close()

		mock.clear()
		mock.tool("Bash", `{"command":"touch steer.txt","description":"create a file"}`)
		mock.text("STEERED")
		if err := st.Prompt(context.Background(), driver.TextInput("create steer.txt")); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		evs := untilLong(t, st.Events(), ap.AgentEventApprovalRequired)
		p, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](evs[len(evs)-1], ap.AgentEventApprovalRequired)
		if err := st.Prompt(context.Background(), driver.TextInput("STEER-MARKER-5 also mention cats")); err != nil {
			t.Fatalf("steer: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		if err := st.Resolve(context.Background(), p.ToolInvocationID, driver.Allow()); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		evs = untilLong(t, st.Events(), ap.AgentEventTurnCompleted)
		if n := countType(evs, ap.AgentEventTurnStarted); n != 0 {
			t.Errorf("steering opened %d new turn(s)", n)
		}
		if last := mock.lastRequest(); !strings.Contains(last, "STEER-MARKER-5") {
			t.Errorf("the steering prompt did not reach the running turn's next request; its messages end with %s", tailMessages(last, 2))
		}
		// Nothing further runs as a separate turn.
		select {
		case ev := <-st.Events():
			t.Errorf("unexpected %s after the steered turn", ev.Type)
		case <-time.After(2 * time.Second):
		}
	})

	t.Run("interrupt keeps the session", func(t *testing.T) {
		mock.tool("Bash", `{"command":"touch interrupted.txt","description":"create a file"}`)
		p := approval("create interrupted.txt")
		if err := sess.Interrupt(context.Background()); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		evs := untilLong(t, sess.Events(), ap.AgentEventTurnCompleted)
		t.Logf("interrupt events: %v", typesOf(evs))
		done, _ := ap.PayloadAs[ap.TurnCompletedPayload](evs[len(evs)-1], ap.AgentEventTurnCompleted)
		if done.StopReason != "interrupted" {
			t.Errorf("stop = %q", done.StopReason)
		}
		if _, ok := findEvent(evs, ap.AgentEventApprovalResolved); !ok {
			t.Error("the withdrawn approval was not resolved")
		}
		if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Allow()); err == nil {
			t.Error("resolved a withdrawn request")
		}
		if _, err := os.Stat(filepath.Join(workDir, "interrupted.txt")); err == nil {
			t.Error("interrupted command ran")
		}
		mock.text("AFTER-INTERRUPT")
		if got := textOf(turn("still there?")); got != "AFTER-INTERRUPT" {
			t.Errorf("after interrupt: %q", got)
		}
	})

	t.Run("mcp tool", func(t *testing.T) {
		mock.tool("mcp__host__echo", `{"text":"MCP-PING"}`)
		mock.text("MCP-DONE")
		p := approval("use the echo tool")
		if p.ToolName != "mcp__host__echo" {
			t.Fatalf("approval = %+v", p)
		}
		if err := sess.Resolve(context.Background(), p.ToolInvocationID, driver.Allow()); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		evs := untilLong(t, sess.Events(), ap.AgentEventTurnCompleted)
		if s := toolStatus(evs); s.Status != ap.ToolInvocationStatusCompleted || !strings.Contains(s.Result, "echo: MCP-PING") {
			t.Errorf("tool = %+v", s)
		}
	})

	if err := sess.Close(); err != nil {
		t.Errorf("close: %v", err)
	}

	t.Run("resume after close", func(t *testing.T) {
		mock.clear()
		mock.text("RESUMED-REPLY")
		cfg.ResumeSessionID = id
		cfg.Metadata = nil
		resumed, err := b.Open(context.Background(), cfg)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		defer resumed.Close()
		if resumed.ID() != id {
			t.Errorf("resumed ID = %s", resumed.ID())
		}
		if err := resumed.Prompt(context.Background(), driver.TextInput("RESUME-PROMPT what did I say first?")); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		evs := untilLong(t, resumed.Events(), ap.AgentEventTurnCompleted)
		if got := textOf(evs); got != "RESUMED-REPLY" {
			t.Errorf("text = %q", got)
		}
		if resumed.ID() != id {
			t.Errorf("after init the resumed ID is %s, want %s", resumed.ID(), id)
		}
		last := mock.lastRequest()
		if !strings.Contains(last, "FIRST-PROMPT-MARKER") || !strings.Contains(last, "RESUME-PROMPT") {
			t.Error("the resumed request does not carry the earlier conversation")
		}
	})

	t.Run("kill mid-turn, then resume", func(t *testing.T) {
		cfg.ResumeSessionID = id
		cfg.Metadata = nil
		victim, err := b.Open(context.Background(), cfg)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		mock.tool("Bash", `{"command":"touch killed.txt","description":"create a file"}`)
		if err := victim.Prompt(context.Background(), driver.TextInput("create killed.txt")); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		untilLong(t, victim.Events(), ap.AgentEventApprovalRequired)
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

		mock.clear()
		mock.text("AFTER-KILL-REPLY")
		again, err := b.Open(context.Background(), cfg)
		if err != nil {
			t.Fatalf("resume after kill: %v", err)
		}
		defer again.Close()
		if err := again.Prompt(context.Background(), driver.TextInput("AFTER-KILL-PROMPT")); err != nil {
			t.Fatalf("prompt: %v", err)
		}
		if got := textOf(untilLong(t, again.Events(), ap.AgentEventTurnCompleted)); got != "AFTER-KILL-REPLY" {
			t.Errorf("text = %q", got)
		}
		if !strings.Contains(mock.lastRequest(), "FIRST-PROMPT-MARKER") {
			t.Error("the session resumed after a kill lost the earlier conversation")
		}
	})
}

// drainAll reads events until the channel closes.
func drainAll(t *testing.T, ch <-chan ap.AgentEvent, d time.Duration) []ap.AgentEvent {
	t.Helper()
	var got []ap.AgentEvent
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("events still open after %s; got %v", d, typesOf(got))
		}
	}
}

func untilLong(t *testing.T, ch <-chan ap.AgentEvent, want ap.AgentEventType) []ap.AgentEvent {
	t.Helper()
	var got []ap.AgentEvent
	deadline := time.After(60 * time.Second)
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
			t.Fatalf("no %s within 60s; got %v", want, typesOf(got))
		}
	}
}

func countType(evs []ap.AgentEvent, want ap.AgentEventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == want {
			n++
		}
	}
	return n
}

type claudeMock struct {
	t       *testing.T
	control string
}

func (m *claudeMock) do(method, path string, body any) []byte {
	m.t.Helper()
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, m.control+path, r)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatalf("mock %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return out
}

func (m *claudeMock) text(s string) { m.do("POST", "/text", map[string]string{"Text": s}) }
func (m *claudeMock) tool(name, args string) {
	m.do("POST", "/tool", map[string]string{"Name": name, "Args": args})
}
func (m *claudeMock) clear() { m.do("DELETE", "/log", nil) }

// lastRequest is the body of the last Messages API request, as text.
func (m *claudeMock) lastRequest() string {
	var log []struct {
		Path string          `json:"path"`
		Body json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(m.do("GET", "/log", nil), &log)
	for i := len(log) - 1; i >= 0; i-- {
		if strings.HasSuffix(log[i].Path, "/messages") {
			return string(log[i].Body)
		}
	}
	return ""
}

// tailMessages returns the last n messages of a Messages API request body.
func tailMessages(body string, n int) string {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal([]byte(body), &req) != nil {
		return "(unparseable)"
	}
	if len(req.Messages) > n {
		req.Messages = req.Messages[len(req.Messages)-n:]
	}
	out, _ := json.Marshal(req.Messages)
	return string(out)
}
