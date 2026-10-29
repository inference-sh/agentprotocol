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

	"github.com/inference-sh/agentprotocol/driver"
	"github.com/inference-sh/agentprotocol/mcpbridge"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
)

// runFakeStdioTools is an agent that takes only stdio MCP servers, as hermes
// and droid do: its initialize declares no mcpCapabilities.http. On a prompt
// it launches the tools server session/new declared, calls echo through it,
// and says what came back.
func runFakeStdioTools() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 1<<20)
	send := func(v any) {
		data, _ := json.Marshal(v)
		os.Stdout.Write(append(data, '\n'))
	}
	reply := func(id int, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	var srv acp.MCPServer
	var decl string
	for in.Scan() {
		var msg acp.Message
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		switch msg.Method {
		case acp.MethodInitialize:
			reply(*msg.ID, map[string]any{"protocolVersion": acp.ProtocolVersion})
		case acp.MethodSessionNew:
			var p struct {
				MCPServers []json.RawMessage `json:"mcpServers"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			for _, raw := range p.MCPServers {
				_ = json.Unmarshal(raw, &srv)
				decl = string(raw)
			}
			reply(*msg.ID, acp.NewSessionResult{SessionID: "sess_fake"})
		case acp.MethodSessionPrompt:
			got := callOverStdio(srv)
			report, _ := json.Marshal(map[string]string{"decl": decl, "call": got})
			text, _ := json.Marshal(string(report))
			send(map[string]any{"jsonrpc": "2.0", "method": acp.MethodSessionUpdate,
				"params": acp.UpdateNotification{SessionID: "sess_fake", Update: acp.SessionUpdate{
					Kind: acp.UpdateKindAgentMessageChunk, Content: json.RawMessage(`{"type":"text","text":` + string(text) + `}`),
				}}})
			reply(*msg.ID, map[string]any{"stopReason": "end_turn"})
		case acp.MethodSessionClose:
			if msg.ID != nil {
				reply(*msg.ID, map[string]any{})
			}
			return
		}
	}
}

// callOverStdio launches srv as an agent launches a stdio MCP server, with
// its own environment plus the declared one, and returns its answers to
// initialize, a notification and a call of echo.
func callOverStdio(srv acp.MCPServer) string {
	if srv.Command == "" {
		return "no stdio server declared"
	}
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Env = os.Environ()
	for k, v := range srv.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return "start: " + err.Error()
	}
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
	} {
		stdin.Write([]byte(m + "\n"))
	}
	var lines []string
	sc := bufio.NewScanner(stdout)
	for len(lines) < 2 && sc.Scan() {
		lines = append(lines, sc.Text())
	}
	stdin.Close()
	_ = cmd.Wait()
	return strings.Join(lines, "\n")
}

// An agent without HTTP MCP gets the caller tools as a stdio server running
// StdioBridge, which reaches the loopback server through its environment.
func TestACPCallerToolsOverAStdioBridge(t *testing.T) {
	b := backendRunning(t, "stdio-tools")
	exe, _ := os.Executable()
	b.StdioBridge = []string{exe, "-test.run=^$"}
	var mu sync.Mutex
	var got []driver.ToolCall
	sess, err := b.Open(context.Background(), driver.SessionConfig{
		WorkDir: t.TempDir(), Tools: echoTool(),
		CallTool: func(_ context.Context, call driver.ToolCall) (driver.ToolOutput, error) {
			mu.Lock()
			got = append(got, call)
			mu.Unlock()
			return driver.ToolOutput{Text: "ECHO-OVER-STDIO"}, nil
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	if err := sess.Prompt(context.Background(), driver.TextInput("use the tool")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	evs := until(t, sess.Events(), ap.AgentEventTurnCompleted)

	var report map[string]string
	if err := json.Unmarshal([]byte(textOf(evs)), &report); err != nil {
		t.Fatalf("fake report %q: %v", textOf(evs), err)
	}
	var decl struct {
		Type    string          `json:"type"`
		Name    string          `json:"name"`
		Command string          `json:"command"`
		Env     []acp.NameValue `json:"env"`
	}
	_ = json.Unmarshal([]byte(report["decl"]), &decl)
	if decl.Type != "" || decl.Name != "tools" || decl.Command != exe {
		t.Errorf("declaration = %s", report["decl"])
	}
	env := map[string]string{}
	for _, nv := range decl.Env {
		env[nv.Name] = nv.Value
	}
	if !strings.HasPrefix(env[mcpbridge.EnvURL], "http://127.0.0.1:") || !strings.HasPrefix(env[mcpbridge.EnvAuthorization], "Bearer ") {
		t.Errorf("env = %v", env)
	}
	if !strings.Contains(report["call"], `"serverInfo"`) || !strings.Contains(report["call"], "ECHO-OVER-STDIO") {
		t.Errorf("over the bridge = %s", report["call"])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Name != "echo" || string(got[0].Arguments) != `{"text":"hi"}` {
		t.Errorf("CallTool got %+v", got)
	}
}
