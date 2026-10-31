package driver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/driver"
	ap "github.com/inference-sh/agentprotocol/model"
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
	// load is the call that loads a deferred MCP tool, run in a turn of its
	// own before the tool is called; empty when the agent offers it outright.
	load, loadArgs string
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

// realHermes is hermes against the mock as an OpenAI-compatible endpoint.
// hermes takes only stdio MCP servers, so the tools reach it through this
// test binary as its StdioBridge (TestMain runs mcpbridge.Main).
func realHermes(t *testing.T, mock string) acpRealAgent {
	bin := os.Getenv("HERMES_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("hermes"); err != nil {
			t.Skip("hermes not installed")
		}
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		// provider custom: with openrouter, hermes keeps openrouter.ai and
		// drops base_url.
		"config.yaml": "model:\n  default: gpt-4o-mini\n  provider: custom\n  base_url: " + mock + "/v1\n  api_key: mock-key\nhooks: {}\nhooks_auto_accept: true\n",
	} {
		if err := os.WriteFile(filepath.Join(home, ".hermes", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return acpRealAgent{
		name: "hermes",
		backend: &driver.ACPBackend{
			Command: bin, Args: []string{"acp"},
			Env:         isolatedEnv(home, "HERMES_", "OPENROUTER_", "OPENAI_"),
			StdioBridge: []string{exe, "-test.run=^$"},
		},
		toolName: hermesToolName(t, bin),
		reqPath:  "/chat/completions",
	}
}

// realDroid is droid against the mock as a custom OpenAI model. droid takes
// only stdio MCP servers, so the tools reach it through this test binary as
// its StdioBridge, as belt's remote sessions do.
func realDroid(t *testing.T, mock string) acpRealAgent {
	bin := os.Getenv("DROID_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("droid"); err != nil {
			t.Skip("droid not installed")
		}
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"customModels":[{"model":"gpt-4o-mini","displayName":"Mock","baseUrl":"` + mock + `/v1","apiKey":"mock-key","provider":"openai","maxOutputTokens":4096,"maxContextLimit":128000}],"sessionDefaultSettings":{"model":"custom:gpt-4o-mini"}}`
	if err := os.WriteFile(filepath.Join(home, ".factory", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(isolatedEnv(home, "FACTORY_", "DROID_"),
		"FACTORY_API_BASE_URL="+mock, "FACTORY_API_KEY=fk-mock-key-0123456789abcdef0123", "FACTORY_DISABLE_KEYRING=1")
	return acpRealAgent{
		name: "droid",
		backend: &driver.ACPBackend{
			Command: bin, Args: []string{"exec", "--output-format", "acp", "-m", "custom:gpt-4o-mini"},
			Env:         env,
			StdioBridge: []string{exe, "-test.run=^$"},
		},
		toolName: func(server, tool string) string { return server + "___" + tool },
		reqPath:  "/responses",
		// droid 1.1.0 defers an MCP tool behind ToolSearch unless its
		// server's name contains "github" or "agentbrowser", and drops a
		// call of one not loaded yet.
		load: "ToolSearch", loadArgs: `{"query":"select:tools___lookup"}`,
	}
}

// hermesToolName is how the installed hermes names an MCP tool:
// mcp_<server>_<tool> before 0.19, mcp__<server>__<tool> from 0.19 (measured
// on 0.14.0 and 0.19.0).
func hermesToolName(t *testing.T, bin string) func(server, tool string) string {
	out, _ := exec.Command(bin, "--version").Output()
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(strings.Fields(string(out) + " x x x")[2], "v"), "%d.%d", &major, &minor); err != nil {
		t.Fatalf("hermes --version: %q", out)
	}
	if major == 0 && minor < 19 {
		return func(server, tool string) string { return "mcp_" + server + "_" + tool }
	}
	return func(server, tool string) string { return "mcp__" + server + "__" + tool }
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
		"hermes":          realHermes,
		"droid":           realDroid,
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

			if agent.load != "" {
				mockCtl(t, mock, "POST", "/ctl/toolcall", map[string]any{"Name": agent.load, "Args": agent.loadArgs})
				mockCtl(t, mock, "POST", "/text", map[string]any{"Text": "LOADED"})
				if err := sess.Prompt(context.Background(), driver.TextInput("load the lookup tool")); err != nil {
					t.Fatalf("prompt: %v", err)
				}
				untilWithin(t, sess.Events(), ap.AgentEventTurnCompleted, 120*time.Second)
			}
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
// 0.199.0 and hermes 0.14.0 when this was written) with no StdioBridge: Open
// must fail rather than drop the tools. Only initialize runs, so no model is called.
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
