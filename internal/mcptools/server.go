package mcptools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/inference-sh/agentprotocol/internal/jsonrpc"
)

// Package mcptools is the smallest MCP server that offers a caller's tools to
// an agent: initialize, tools/list, tools/call and ping. The drivers put it
// behind whatever transport the agent takes: Claude Code's control channel
// (mcp_message), or a loopback HTTP endpoint (Loopback) for agents that dial
// MCP servers themselves.
//
// It is not a general MCP implementation. Resources, prompts, tasks and
// elicitation are absent, and a method it does not know is answered with
// method-not-found, which the CLI reads as "not supported".
type Server struct {
	// Name and Version identify the server in the initialize reply.
	Name    string
	Version string

	// Tools is what tools/list returns.
	Tools []Tool

	// Run runs a tool. An error is a failure of the call itself and reaches
	// the model as an error result, the same as IsError.
	Run func(ctx context.Context, call Call) (Result, error)
}

// Tool is one tool as tools/list describes it.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Call is one tools/call.
type Call struct {
	Name      string
	Arguments json.RawMessage

	// ToolUseID is the model's id for this call, where the agent sends one
	// in _meta: Claude Code under claudecode/toolUseId, our own pi extension
	// under agentprotocol/toolCallId. Empty otherwise.
	ToolUseID string
}

// Result is a tool's answer. Text is sent as one text block.
type Result struct {
	Text    string
	IsError bool
}

// toolUseIDMeta is the _meta key the CLI puts the model's tool_use id under on
// a tools/call.
const toolUseIDMeta = "claudecode/toolUseId"

// ToolCallIDMeta is the agent-neutral _meta key for the model's call id, for
// clients this module writes itself (the pi driver's extension).
const ToolCallIDMeta = "agentprotocol/toolCallId"

// mcpProtocolVersion is answered to a client that names no version.
const mcpProtocolVersion = "2025-06-18"

// Handle answers one JSON-RPC message. A notification returns nil.
func (s *Server) Handle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var msg jsonrpc.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("mcptools: decode message: %w", err)
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

func (s *Server) dispatch(ctx context.Context, msg jsonrpc.Message) (any, *jsonrpc.Error) {
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
			tools = []Tool{}
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
		call := Call{Name: p.Name, Arguments: p.Arguments}
		if id, ok := p.Meta[ToolCallIDMeta].(string); ok {
			call.ToolUseID = id
		} else if id, ok := p.Meta[toolUseIDMeta].(string); ok {
			call.ToolUseID = id
		}
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			call.Arguments = json.RawMessage(`{}`)
		}
		var res Result
		if s.Run == nil {
			res = Result{Text: "no tool handler is attached", IsError: true}
		} else if r, err := s.Run(ctx, call); err != nil {
			res = Result{Text: err.Error(), IsError: true}
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

func (s *Server) has(name string) bool {
	for _, t := range s.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
