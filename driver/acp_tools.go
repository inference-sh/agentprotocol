package driver

import (
	"strings"
	"sync"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/acp"
	"github.com/inference-sh/agentprotocol/internal/mcptools"
)

// acpCallerTools is one session's SessionConfig.Tools: the loopback MCP server
// that runs them and what is needed to recognise them in the agent's tool
// calls and permission requests.
type acpCallerTools struct {
	server   string
	loopback *mcptools.Loopback
	names    map[string]bool

	closeOnce sync.Once

	mu sync.Mutex
	// calls maps an ACP toolCallId already recognised as one of ours to the
	// bare tool name, so a later tool_call_update or permission request for
	// the same call is ours whatever title it carries.
	calls map[string]string
}

func newACPCallerTools(server string, lb *mcptools.Loopback, tools []ap.Tool) *acpCallerTools {
	t := &acpCallerTools{server: server, loopback: lb, names: map[string]bool{}, calls: map[string]string{}}
	for _, tool := range tools {
		t.names[tool.Function.Name] = true
	}
	return t
}

func (t *acpCallerTools) close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() { _ = t.loopback.Close() })
}

// match reports whether a tool call title names one of our tools, qualified
// by our server name, and returns the bare name.
//
// The matching rule: the title is <prefix><tool>, where <tool> is exactly a
// name in SessionConfig.Tools and <prefix> is one of the server-qualified
// forms agents use for an MCP tool:
//
//	mcp__<server>__   claude-agent-acp (and Claude Code)
//	mcp_<server>_     gemini's FQN form
//	<server>__        gemini when tool names collide
//	<server>_         opencode (<server>_<tool>, measured on opencode 1.17.16)
//	<server>/  <server>.  <server>:
//
// A bare title equal to a tool name is not matched: an agent's own tool of
// that name (bash, read) would be taken for ours, and auto-allowed.
func (t *acpCallerTools) match(title string) (string, bool) {
	if t == nil {
		return "", false
	}
	srv := t.server
	for _, prefix := range []string{
		"mcp__" + srv + "__", "mcp_" + srv + "_", srv + "__", srv + "_",
		srv + "/", srv + ".", srv + ":",
	} {
		if rest, ok := strings.CutPrefix(title, prefix); ok && t.names[rest] {
			return rest, true
		}
	}
	return "", false
}

// observe records a tool_call or tool_call_update and returns the bare name
// when the call is one of ours.
//
// A toolCallId is the model's call id as the agent passes it on (opencode
// does), and is not unique for the session's life: a mock or a provider may
// reuse one in a later turn. So a tool_call that names something else forgets
// the id, and a call's terminal update forgets it too; otherwise the agent's
// own tool reusing the id would be renamed, and its permission auto-allowed.
func (t *acpCallerTools) observe(u acp.SessionUpdate) (string, bool) {
	if t == nil {
		return "", false
	}
	bare, ok := t.match(u.Title)
	if u.ToolCallID == "" {
		return bare, ok
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case ok:
		t.calls[u.ToolCallID] = bare
	case u.Kind == acp.UpdateKindToolCall:
		delete(t.calls, u.ToolCallID)
	default:
		// An update may retitle the call (a description, or nothing); the
		// call is still the one its tool_call announced.
		bare, ok = t.calls[u.ToolCallID]
	}
	if ok && isTerminalToolStatus(u.Status) {
		delete(t.calls, u.ToolCallID)
	}
	return bare, ok
}

func isTerminalToolStatus(s string) bool {
	switch s {
	case "completed", "success", "failed", "error", "cancelled", "canceled":
		return true
	}
	return false
}

// owns reports whether a permission request is for one of our tools: its
// title matches, or its toolCallId was announced in a tool_call that did.
func (t *acpCallerTools) owns(req acp.PermissionRequest) bool {
	if t == nil || req.ToolCall == nil {
		return false
	}
	if _, ok := t.match(req.ToolCall.Title); ok {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.calls[req.ToolCall.ToolCallID]
	return ok && req.ToolCall.ToolCallID != ""
}
