package driver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/driver"
)

// The real-binary ACP tests drive installed ACP agents against
// conformance/cmd/drivermock, never a real provider or the user's account.
// They run only when ACP_MOCK_URL names a running mock:
//
//	cd conformance && go build -o /tmp/apmock ./cmd/drivermock && /tmp/apmock &
//	ACP_MOCK_URL=http://127.0.0.1:PORT go test -race -run ACPReal ./driver/
//
// Each agent gets a temp HOME holding provider config that points at the mock,
// so the user's own settings, keys and sessions are untouched.

// acpRealAgent is one installed agent set up against the mock: the backend,
// how the model is to name our tool when the mock calls it, and the request
// path its model calls go to.
type acpRealAgent struct {
	name     string
	backend  *driver.ACPBackend
	toolName func(server, tool string) string
	reqPath  string
	// asks is set when the agent is configured to ask permission for every
	// tool, so a built-in tool must still raise an approval.
	asks bool
	// builtin is a call of one of the agent's own tools, for asks.
	builtin, builtinArgs string
}

func realOpencode(t *testing.T, mock string) acpRealAgent { return opencodeWith(t, mock, "") }

// realOpencodeAsking has opencode ask permission for every tool, so the
// caller tool's permission request reaches the backend.
func realOpencodeAsking(t *testing.T, mock string) acpRealAgent {
	a := opencodeWith(t, mock, `"permission":{"*":"ask","bash":"ask"},`)
	a.asks, a.builtin, a.builtinArgs = true, "bash", `{"command":"echo hi > made.txt","description":"make a file"}`
	return a
}

func opencodeWith(t *testing.T, mock, extra string) acpRealAgent {
	bin := os.Getenv("OPENCODE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("opencode"); err != nil {
			t.Skip("opencode not installed")
		}
	}
	home := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// opencode 1.17 reads the endpoint from provider.<id>.options; a
	// top-level baseURL is ignored and the request goes to api.openai.com.
	write(".config/opencode/opencode.json", `{`+extra+`"model":"openai/gpt-4o-mini","autoupdate":false,"share":"disabled","provider":{"openai":{"options":{"apiKey":"mock-key","baseURL":"`+mock+`/v1"},"models":{"gpt-4o-mini":{}}}}}`)
	write(".local/share/opencode/auth.json", `{"openai":{"type":"api","key":"mock-key"}}`)
	env := append(isolatedEnv(home, "OPENCODE", "OPENAI_"), "OPENAI_API_KEY=mock-key", "OPENAI_BASE_URL="+mock+"/v1")
	return acpRealAgent{
		name:     "opencode",
		backend:  &driver.ACPBackend{Command: bin, Args: []string{"acp"}, Env: env},
		toolName: func(server, tool string) string { return server + "_" + tool },
		reqPath:  "/responses",
	}
}

// isolatedEnv is the parent environment with HOME replaced, the XDG
// directories under it, and variables with the given prefixes dropped.
func isolatedEnv(home string, drop ...string) []string {
	env := []string{}
outer:
	for _, kv := range os.Environ() {
		for _, p := range append([]string{"HOME=", "XDG_"}, drop...) {
			if strings.HasPrefix(kv, p) {
				continue outer
			}
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local/share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local/state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"))
}

// acpMockRequests returns the bodies of the model requests whose path ends in
// suffix.
func acpMockRequests(t *testing.T, mock, suffix string) []string {
	var log []struct {
		Path string          `json:"path"`
		Body json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(mockCtl(t, mock, "GET", "/log", nil), &log)
	var out []string
	for _, e := range log {
		if strings.HasSuffix(e.Path, suffix) {
			out = append(out, string(e.Body))
		}
	}
	return out
}

// TestACPRealCallerTools runs SessionConfig.Tools through each installed ACP
// agent: our tool served on the loopback HTTP MCP server, the model (the mock)
// calling it under the agent's name for it, CallTool running it, no
// approval-required event, and the bare name in the tool events.
func TestACPRealCallerTools(t *testing.T) {
	mock := os.Getenv("ACP_MOCK_URL")
	if mock == "" {
		t.Skip("ACP_MOCK_URL not set; see the comment above acpRealAgent")
	}
	for name, setup := range map[string]func(*testing.T, string) acpRealAgent{
		"opencode":        realOpencode,
		"opencode-asking": realOpencodeAsking,
	} {
		t.Run(name, func(t *testing.T) {
			agent := setup(t, mock)
			var stderr bytes.Buffer
			b := agent.backend
			b.Stderr = &stderr
			b.FirstEventTimeout = 90 * time.Second
			b.OnDiagnostic = func(m string) { t.Log("diagnostic:", m) }
			defer func() {
				if t.Failed() && stderr.Len() > 0 {
					t.Logf("%s stderr:\n%s", agent.name, stderr.String())
				}
			}()

			mockCtl(t, mock, "POST", "/ctl/delay", map[string]any{"Ms": 0})
			mockCtl(t, mock, "DELETE", "/log", nil)

			var got []driver.ToolCall
			calls := make(chan struct{}, 4)
			sess, err := b.Open(context.Background(), driver.SessionConfig{
				RunID: "run-tools", ChatID: "chat-tools", WorkDir: t.TempDir(),
				Model: "gpt-4o-mini",
				Tools: []ap.Tool{{Type: "function", Function: ap.ToolFunction{
					Name: "lookup", Description: "Look a word up.",
					Parameters: ap.ToolParameters{Type: "object", Properties: ap.ToolParameterProperties{
						"word": {Type: "string"},
					}},
				}}},
				CallTool: func(_ context.Context, call driver.ToolCall) (driver.ToolOutput, error) {
					got = append(got, call)
					calls <- struct{}{}
					return driver.ToolOutput{Text: "LOOKUP-RESULT-4412"}, nil
				},
			})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer sess.Close()
			untilWithin(t, sess.Events(), ap.AgentEventRunStarted, 30*time.Second)

			mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": agent.toolName("tools", "lookup"), "Args": `{"word":"orb"}`})
			mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "TOOLS-DONE"})
			if err := sess.Prompt(context.Background(), driver.TextInput("look up orb")); err != nil {
				t.Fatalf("prompt: %v", err)
			}
			evs := untilWithin(t, sess.Events(), ap.AgentEventTurnCompleted, 120*time.Second)
			for _, e := range evs {
				t.Logf("event %s %s", e.Type, e.Payload)
			}
			reqs := acpMockRequests(t, mock, agent.reqPath)
			if !strings.Contains(strings.Join(reqs, "\n"), agent.toolName("tools", "lookup")) {
				for _, r := range reqs {
					t.Logf("request: %.3000s", r)
				}
				t.Fatalf("the model was not offered %s", agent.toolName("tools", "lookup"))
			}

			if _, asked := findEvent(evs, ap.AgentEventApprovalRequired); asked {
				t.Error("a caller tool raised an approval")
			}
			t.Logf("CallTool got %+v", got)
			if len(got) != 1 || got[0].Name != "lookup" || !strings.Contains(string(got[0].Arguments), "orb") {
				t.Errorf("calls = %+v", got)
			}
			start, ok := findEvent(evs, ap.AgentEventToolStarted)
			if !ok {
				t.Fatalf("no tool.started in %v", typesOf(evs))
			}
			sp, _ := ap.PayloadAs[ap.ToolStartedPayload](start, ap.AgentEventToolStarted)
			if sp.ToolName != "lookup" || sp.ToolInvocationID == "" {
				t.Errorf("tool.started = %+v", sp)
			}
			done, ok := findEvent(evs, ap.AgentEventToolCompleted)
			if !ok {
				t.Fatalf("no tool.completed in %v", typesOf(evs))
			}
			dp, _ := ap.PayloadAs[ap.ToolCompletedPayload](done, ap.AgentEventToolCompleted)
			if dp.ToolName != "lookup" || dp.ToolInvocationID != sp.ToolInvocationID || dp.Status != ap.ToolInvocationStatusCompleted {
				t.Errorf("tool.completed = %+v", dp)
			}
			if !strings.Contains(strings.Join(reqs, "\n"), "LOOKUP-RESULT-4412") {
				t.Error("the tool's result never reached the model")
			}

			if !agent.asks {
				return
			}
			// The agent asks for everything, so the caller tool above was
			// asked for too and allowed by the backend. Its own tool must
			// still reach the caller as an approval.
			mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": agent.builtin, "Args": agent.builtinArgs})
			mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "BUILTIN-DONE"})
			if err := sess.Prompt(context.Background(), driver.TextInput("make a file")); err != nil {
				t.Fatalf("prompt: %v", err)
			}
			asked := untilWithin(t, sess.Events(), ap.AgentEventApprovalRequired, 60*time.Second)
			for _, e := range asked {
				t.Logf("event %s %s", e.Type, e.Payload)
			}
			e := asked[len(asked)-1]
			ap2, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](e, ap.AgentEventApprovalRequired)
			t.Logf("approval for the built-in: %+v", ap2)
			if err := sess.Resolve(context.Background(), ap2.ToolInvocationID, driver.Resolution{Decision: ap.InterruptResolutionDeny}); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			untilWithin(t, sess.Events(), ap.AgentEventTurnCompleted, 120*time.Second)
			if len(got) != 1 {
				t.Errorf("CallTool ran %d times", len(got))
			}
		})
	}
}

// TestACPRealRefusesAgentsWithoutHTTPMCP opens a session with a caller tool on
// installed agents whose initialize declares no mcpCapabilities.http (droid
// 0.199.0 and hermes 0.14.0 when this was written): Open must fail rather
// than drop the tools. Only initialize runs, so no model is called.
func TestACPRealRefusesAgentsWithoutHTTPMCP(t *testing.T) {
	if os.Getenv("ACP_MOCK_URL") == "" {
		t.Skip("ACP_MOCK_URL not set; see the comment above acpRealAgent")
	}
	for name, cmd := range map[string][]string{
		"droid":  {"droid", "exec", "--output-format", "acp"},
		"hermes": {"hermes", "acp"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, err := exec.LookPath(cmd[0])
			if err != nil {
				t.Skip(cmd[0] + " not installed")
			}
			home := t.TempDir()
			b := &driver.ACPBackend{Command: bin, Args: cmd[1:], Env: isolatedEnv(home, "FACTORY_", "HERMES_", "OPENROUTER_")}
			sess, err := b.Open(context.Background(), driver.SessionConfig{
				WorkDir:  t.TempDir(),
				Tools:    []ap.Tool{{Type: "function", Function: ap.ToolFunction{Name: "lookup"}}},
				CallTool: func(context.Context, driver.ToolCall) (driver.ToolOutput, error) { return driver.ToolOutput{}, nil },
			})
			if err == nil {
				sess.Close()
				t.Fatal("opened a session with caller tools on an agent that takes no HTTP MCP server")
			}
			if !strings.Contains(err.Error(), "mcpCapabilities.http") {
				t.Errorf("error = %v", err)
			}
		})
	}
}
