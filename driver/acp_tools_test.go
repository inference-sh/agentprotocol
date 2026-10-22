package driver

import (
	"testing"

	ap "github.com/inference-sh/agentprotocol"
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
		bare, ok := tools.match(title)
		if ok != want || (ok && bare != "echo") {
			t.Errorf("match(%q) = %q, %v; want %v", title, bare, ok, want)
		}
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
