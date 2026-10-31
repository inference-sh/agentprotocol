package driver

import (
	"encoding/json"
	"testing"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
)

func TestACPCallerToolMatching(t *testing.T) {
	tools := newACPCallerTools("tools", nil, []ap.Tool{{Function: ap.ToolFunction{Name: "echo"}}})
	for title, want := range map[string]bool{
		"mcp__tools__echo": true, "mcp_tools_echo": true, "tools__echo": true,
		"tools_echo": true, "tools/echo": true, "tools.echo": true, "tools:echo": true,
		"echo":             false, // could be the agent's own echo
		"mcp__other__echo": false,
		"tools_bash":       false,
		"tools_":           false,
		"":                 false,
	} {
		bare, ok := tools.match(title, nil)
		if ok != want || (ok && bare != "echo") {
			t.Errorf("match(%q) = %q, %v; want %v", title, bare, ok, want)
		}
	}
}

// droid 1.1.0 titles our echo "tools: Echo" and names it in _meta.
func TestACPCallerToolMatchingByMeta(t *testing.T) {
	tools := newACPCallerTools("tools", nil, []ap.Tool{{Function: ap.ToolFunction{Name: "echo"}}})
	for meta, want := range map[string]bool{
		`{"factory":{"source":"mcp","server":"tools","tool":"echo"}}`:     true,
		`{"factory":{"source":"mcp","server":"other","tool":"echo"}}`:     false,
		`{"factory":{"source":"mcp","server":"tools","tool":"bash"}}`:     false,
		`{"factory":{"source":"builtin","server":"tools","tool":"echo"}}`: false,
		`{"other":{}}`: false,
		`not json`:     false,
	} {
		bare, ok := tools.match("tools: Echo", json.RawMessage(meta))
		if ok != want || (ok && bare != "echo") {
			t.Errorf("match(%s) = %q, %v; want %v", meta, bare, ok, want)
		}
	}
	// The permission request follows the tool_call with the same id.
	meta := json.RawMessage(`{"factory":{"source":"mcp","server":"tools","tool":"echo"}}`)
	if bare, ok := tools.observe(acp.SessionUpdate{Kind: acp.UpdateKindToolCall, ToolCallID: "c1", Title: "tools: Echo", Meta: meta}); !ok || bare != "echo" {
		t.Fatal("droid's tool_call not recognised")
	}
	if !tools.owns(acp.PermissionRequest{ToolCall: &acp.PermissionToolRef{ToolCallID: "c1", Title: "tools: Echo"}}) {
		t.Error("droid's permission for our tool not recognised")
	}
	if !tools.owns(acp.PermissionRequest{ToolCall: &acp.PermissionToolRef{ToolCallID: "c9", Title: "tools: Echo", Meta: meta}}) {
		t.Error("a permission whose _meta names our tool not recognised")
	}
}

func TestACPCallerToolIDIsForgottenWhenReused(t *testing.T) {
	tools := newACPCallerTools("tools", nil, []ap.Tool{{Function: ap.ToolFunction{Name: "echo"}}})
	if bare, ok := tools.observe(acp.SessionUpdate{Kind: acp.UpdateKindToolCall, ToolCallID: "c1", Title: "tools_echo"}); !ok || bare != "echo" {
		t.Fatalf("tool_call not recognised")
	}
	if bare, ok := tools.observe(acp.SessionUpdate{Kind: acp.UpdateKindToolCallUpdate, ToolCallID: "c1", Title: "Echoing hi"}); !ok || bare != "echo" {
		t.Error("a retitled update lost the call")
	}
	if !tools.owns(acp.PermissionRequest{ToolCall: &acp.PermissionToolRef{ToolCallID: "c1", Title: "Echoing hi"}}) {
		t.Error("a permission for an announced call is ours")
	}
	// The same id reused by the agent's own tool.
	if _, ok := tools.observe(acp.SessionUpdate{Kind: acp.UpdateKindToolCall, ToolCallID: "c1", Title: "bash"}); ok {
		t.Error("the agent's bash was taken for ours")
	}
	if tools.owns(acp.PermissionRequest{ToolCall: &acp.PermissionToolRef{ToolCallID: "c1", Title: "bash"}}) {
		t.Error("the agent's bash would be auto-allowed")
	}
	// A finished call is forgotten too.
	tools.observe(acp.SessionUpdate{Kind: acp.UpdateKindToolCall, ToolCallID: "c2", Title: "tools_echo"})
	tools.observe(acp.SessionUpdate{Kind: acp.UpdateKindToolCallUpdate, ToolCallID: "c2", Status: "completed"})
	if tools.owns(acp.PermissionRequest{ToolCall: &acp.PermissionToolRef{ToolCallID: "c2", Title: "bash"}}) {
		t.Error("a finished call's id still counted as ours")
	}
}
