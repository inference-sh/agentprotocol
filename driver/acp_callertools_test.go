package driver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/driver"
	ap "github.com/inference-sh/agentprotocol/model"
)

func echoTool() []ap.Tool {
	return []ap.Tool{{Type: "function", Function: ap.ToolFunction{
		Name: "echo", Description: "Echo text.",
		Parameters: ap.ToolParameters{Type: "object", Properties: ap.ToolParameterProperties{
			"text": {Type: "string"},
		}},
	}}}
}

// The fake agent (script caller-tools) does what an ACP agent does with an
// MCP server from session/new: lists its tools over HTTP, announces the call
// as mcp__tools__echo, asks permission, calls it once allowed, then asks about
// its own tool that is also called echo.
func TestACPCallerToolsRunOverTheLoopback(t *testing.T) {
	b := backendRunning(t, "caller-tools")
	var mu sync.Mutex
	var got []driver.ToolCall
	sess, err := b.Open(context.Background(), driver.SessionConfig{
		RunID: "run_1", ChatID: "chat_1", WorkDir: t.TempDir(),
		Tools: echoTool(),
		CallTool: func(_ context.Context, call driver.ToolCall) (driver.ToolOutput, error) {
			mu.Lock()
			got = append(got, call)
			mu.Unlock()
			return driver.ToolOutput{Text: "ECHO-RESULT-7"}, nil
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	if err := sess.Prompt(context.Background(), driver.TextInput("use the tool")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	evs := until(t, sess.Events(), ap.AgentEventApprovalRequired)

	// One approval, for the agent's own echo; ours was allowed unasked.
	if n := countType(evs, ap.AgentEventApprovalRequired); n != 1 {
		t.Errorf("%d approval(s) before the built-in asked", n)
	}
	asked, _ := findEvent(evs, ap.AgentEventApprovalRequired)
	ap1, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](asked, ap.AgentEventApprovalRequired)
	if ap1.ToolInvocationID != "call_builtin" || ap1.ToolName != "echo" {
		t.Errorf("approval = %+v, want the agent's own echo", ap1)
	}
	if err := sess.Resolve(context.Background(), "call_builtin", driver.Resolution{Decision: ap.InterruptResolutionDeny}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	evs = append(evs, until(t, sess.Events(), ap.AgentEventTurnCompleted)...)

	mu.Lock()
	if len(got) != 1 || got[0].Name != "echo" || string(got[0].Arguments) != `{"text":"hi"}` || got[0].ToolInvocationID != "" {
		t.Errorf("CallTool got %+v", got)
	}
	mu.Unlock()

	var started, completed []string
	for _, e := range evs {
		switch e.Type {
		case ap.AgentEventToolStarted:
			p, _ := ap.PayloadAs[ap.ToolStartedPayload](e, e.Type)
			started = append(started, p.ToolInvocationID+"="+p.ToolName)
		case ap.AgentEventToolCompleted:
			p, _ := ap.PayloadAs[ap.ToolCompletedPayload](e, e.Type)
			completed = append(completed, p.ToolInvocationID+"="+p.ToolName)
		}
	}
	if strings.Join(started, ",") != "call_ours=echo,call_builtin=echo" {
		t.Errorf("tool.started = %v", started)
	}
	if strings.Join(completed, ",") != "call_ours=echo,call_builtin=" {
		t.Errorf("tool.completed = %v", completed)
	}

	// The fake's report of what it saw.
	var report map[string]string
	if err := json.Unmarshal([]byte(textOf(evs)), &report); err != nil {
		t.Fatalf("fake report %q: %v", textOf(evs), err)
	}
	var decl map[string]any
	_ = json.Unmarshal([]byte(report["decl"]), &decl)
	if decl["type"] != "http" || decl["name"] != "tools" || !strings.HasPrefix(report["url"], "http://127.0.0.1:") {
		t.Errorf("declaration = %s", report["decl"])
	}
	headers, _ := decl["headers"].([]any)
	if h, _ := headers[0].(map[string]any); len(headers) != 1 || h["name"] != "Authorization" || !strings.HasPrefix(h["value"].(string), "Bearer ") || len(h["value"].(string)) < 20 {
		t.Errorf("headers = %v, want one bearer Authorization as a name/value list", decl["headers"])
	}
	if !strings.Contains(report["list"], `"name":"echo"`) {
		t.Errorf("tools/list = %s", report["list"])
	}
	if !strings.Contains(report["unauth"], "401") {
		t.Errorf("a call without the token got %s", report["unauth"])
	}
	if !strings.Contains(report["call"], "ECHO-RESULT-7") || report["option"] != "o_allow" {
		t.Errorf("tools/call = %s, permission answered %q", report["call"], report["option"])
	}

	if err := sess.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	_, err = http.Post(report["url"], "application/json", strings.NewReader(`{}`))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("after Close the loopback answered: %v", err)
	}
}

func TestACPCallerToolsNeedCallTool(t *testing.T) {
	b := backendRunning(t, "caller-tools")
	_, err := b.Open(context.Background(), driver.SessionConfig{WorkDir: t.TempDir(), Tools: echoTool()})
	if err == nil || !strings.Contains(err.Error(), "CallTool") {
		t.Errorf("open without CallTool: %v", err)
	}
}

func TestACPRefusesOnlyCallerTools(t *testing.T) {
	b := backendRunning(t, "caller-tools")
	if b.Capabilities().OnlyCallerTools {
		t.Error("ACP cannot turn an agent's own tools off")
	}
	_, err := b.Open(context.Background(), driver.SessionConfig{
		WorkDir: t.TempDir(), Tools: echoTool(), OnlyCallerTools: true,
		CallTool: func(context.Context, driver.ToolCall) (driver.ToolOutput, error) { return driver.ToolOutput{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "OnlyCallerTools") {
		t.Errorf("open with OnlyCallerTools: %v", err)
	}
}

// The echo script declares no mcpCapabilities.http.
func TestACPCallerToolsNeedAnAgentThatTakesHTTPMCP(t *testing.T) {
	b := backendRunning(t, "echo")
	start := time.Now()
	_, err := b.Open(context.Background(), driver.SessionConfig{
		WorkDir: t.TempDir(), Tools: echoTool(),
		CallTool: func(context.Context, driver.ToolCall) (driver.ToolOutput, error) { return driver.ToolOutput{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "mcpCapabilities.http") {
		t.Errorf("open: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("refusing took %s", d)
	}
}

func TestACPCallerToolServerNameMustNotClashWithMetadata(t *testing.T) {
	b := backendRunning(t, "caller-tools")
	_, err := b.Open(context.Background(), driver.SessionConfig{
		WorkDir: t.TempDir(), Tools: echoTool(),
		Metadata: map[string]string{"mcp_url": "http://127.0.0.1:1/mcp", "mcp_name": "tools"},
		CallTool: func(context.Context, driver.ToolCall) (driver.ToolOutput, error) { return driver.ToolOutput{}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ToolServerName") {
		t.Errorf("open: %v", err)
	}
}
