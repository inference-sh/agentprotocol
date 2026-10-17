package claudecode

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/inference-sh/agentprotocol/internal/jsonrpc"
)

// ToolServer is the smallest MCP server that offers tools to the CLI: the
// part of the protocol a host-served server (InitializeRequest.SDKMCPServers)
// needs, which is initialize, tools/list, tools/call and ping. Plug Handle
// into Handler.OnMCPMessage.
//
// It is not a general MCP implementation. Resources, prompts, tasks and
// elicitation are absent, and a method it does not know is answered with
// method-not-found, which the CLI reads as "not supported".
type ToolServer struct {
	// Name and Version identify the server in the initialize reply.
	Name    string
	Version string

	// Tools is what tools/list returns.
	Tools []MCPTool

	// Call runs a tool. ToolUseID is the model's tool_use id, which the CLI
	// sends with each call; it is empty from a CLI that does not. An error
	// is a failure of the call itself and reaches the model as an error
	// result, the same as IsError.
	Call func(ctx context.Context, call MCPToolCall) (MCPToolResult, error)
}

// MCPTool is one tool as tools/list describes it.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPToolCall is one tools/call.
type MCPToolCall struct {
	Name      string
	Arguments json.RawMessage
	ToolUseID string
}

// MCPToolResult is a tool's answer. Text is sent as one text block.
type MCPToolResult struct {
	Text    string
	IsError bool
}

// toolUseIDMeta is the _meta key the CLI puts the model's tool_use id under on
// a tools/call.
const toolUseIDMeta = "claudecode/toolUseId"

// mcpProtocolVersion is answered to a client that names no version.
const mcpProtocolVersion = "2025-06-18"

// Handle answers one JSON-RPC message. A notification returns nil.
func (s *ToolServer) Handle(ctx context.Context, server string, raw json.RawMessage) (json.RawMessage, error) {
	var msg jsonrpc.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("claudecode: decode mcp message for %s: %w", server, err)
	}
	if !msg.HasID() {
		return nil, nil
	}
	result, rpcErr := s.dispatch(ctx, msg)
	out := jsonrpc.Message{JSONRPC: "2.0", ID: msg.ID}
	if rpcErr != nil {
		out.Error = rpcErr
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		out.Result = b
	}
	return json.Marshal(out)
}

func (s *ToolServer) dispatch(ctx context.Context, msg jsonrpc.Message) (any, *jsonrpc.Error) {
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = mcpProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		tools := s.Tools
		if tools == nil {
			tools = []MCPTool{}
		}
		return map[string]any{"tools": tools}, nil

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      map[string]any  `json:"_meta"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Name == "" {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tools/call needs a tool name"}
		}
		if !s.has(p.Name) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "unknown tool: " + p.Name}
		}
		call := MCPToolCall{Name: p.Name, Arguments: p.Arguments}
		if id, ok := p.Meta[toolUseIDMeta].(string); ok {
			call.ToolUseID = id
		}
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			call.Arguments = json.RawMessage(`{}`)
		}
		var res MCPToolResult
		if s.Call == nil {
			res = MCPToolResult{Text: "no tool handler is attached", IsError: true}
		} else if r, err := s.Call(ctx, call); err != nil {
			res = MCPToolResult{Text: err.Error(), IsError: true}
		} else {
			res = r
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": res.Text}},
			"isError": res.IsError,
		}, nil

	default:
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found: " + msg.Method}
	}
}

func (s *ToolServer) has(name string) bool {
	for _, t := range s.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
