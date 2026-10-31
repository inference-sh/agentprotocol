package driver

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/inference-sh/agentprotocol/internal/mcptools"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
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
//	<server>_         opencode (measured, opencode 1.17.16)
//	mcp__<server>__   Claude Code's form (claude-agent-acp; not measured here)
//	mcp_<server>_  <server>__  <server>/  <server>.  <server>:
//	                  other agents' forms (not measured)
//
// A tool call whose _meta names our server and tool is ours whatever its
// title (see matchMeta). A permission request or later update whose
// toolCallId a matching tool_call announced is ours too (see observe). A bare
// title equal to a tool name is not matched: an agent's own tool of that name
// (bash, read) would be taken for ours, and auto-allowed. An agent whose title
// is a description rather than the tool's name, and whose _meta does not name
// the tool, is not recognised, and its permission requests for our tools
// reach the caller as approvals.
func (t *acpCallerTools) match(title string, meta json.RawMessage) (string, bool) {
	if t == nil {
		return "", false
	}
	if bare, ok := t.matchMeta(meta); ok {
		return bare, true
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

// matchMeta reads the MCP server and tool a tool call's _meta names. droid
// 1.1.0 titles a call of our lookup "tools: Lookup" and puts
// {"factory":{"source":"mcp","server":"tools","tool":"lookup"}} in _meta, on
// the tool_call and on its permission request.
func (t *acpCallerTools) matchMeta(meta json.RawMessage) (string, bool) {
	if len(meta) == 0 {
		return "", false
	}
	var m struct {
		Factory struct {
			Source, Server, Tool string
		} `json:"factory"`
	}
	if json.Unmarshal(meta, &m) != nil {
		return "", false
	}
	f := m.Factory
	if f.Source != "mcp" || f.Server != t.server || !t.names[f.Tool] {
		return "", false
	}
	return f.Tool, true
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
	bare, ok := t.match(u.Title, u.Meta)
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
// title or _meta matches, or its toolCallId was announced in a tool_call that did.
func (t *acpCallerTools) owns(req acp.PermissionRequest) bool {
	if t == nil || req.ToolCall == nil {
		return false
	}
	if _, ok := t.match(req.ToolCall.Title, req.ToolCall.Meta); ok {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.calls[req.ToolCall.ToolCallID]
	return ok && req.ToolCall.ToolCallID != ""
}
