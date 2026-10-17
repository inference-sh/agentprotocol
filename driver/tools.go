package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/internal/mcptools"
)

// checkCallerTools refuses a SessionConfig the backend cannot honour, before
// anything is launched: tools nobody runs, or a promise that the agent has
// only the caller's tools that the backend cannot keep. A silent downgrade of
// the second would leave the agent its shell when the caller meant it not to.
func checkCallerTools(cfg SessionConfig, caps Capabilities) error {
	if len(cfg.Tools) > 0 && cfg.CallTool == nil {
		return errors.New("driver: SessionConfig.Tools needs CallTool to run them")
	}
	if len(cfg.Tools) > 0 && !caps.Tools {
		return errors.New("driver: this backend cannot take caller tools")
	}
	if cfg.OnlyCallerTools && !caps.OnlyCallerTools {
		return errors.New("driver: this backend cannot turn off the agent's own tools (OnlyCallerTools)")
	}
	return nil
}

// callerToolServer serves SessionConfig.Tools as an MCP server whose calls go
// to the caller. Each backend puts it behind the transport its agent takes.
func callerToolServer(tools []ap.Tool, call ToolCaller) (*mcptools.Server, error) {
	server := &mcptools.Server{Name: "agentprotocol", Version: "1"}
	seen := map[string]bool{}
	for _, t := range tools {
		name := t.Function.Name
		if name == "" {
			return nil, errors.New("driver: a tool in SessionConfig.Tools has no name")
		}
		if seen[name] {
			return nil, fmt.Errorf("driver: tool %q appears twice in SessionConfig.Tools", name)
		}
		seen[name] = true
		schema, err := toolInputSchema(t)
		if err != nil {
			return nil, fmt.Errorf("driver: tool %q: %w", name, err)
		}
		server.Tools = append(server.Tools, mcptools.Tool{
			Name: name, Description: t.Function.Description, InputSchema: schema,
		})
	}
	server.Run = func(ctx context.Context, c mcptools.Call) (mcptools.Result, error) {
		out, err := call(ctx, ToolCall{ToolInvocationID: c.ToolUseID, Name: c.Name, Arguments: c.Arguments})
		if err != nil {
			return mcptools.Result{}, err
		}
		return mcptools.Result{Text: out.Text, IsError: out.IsError}, nil
	}
	return server, nil
}

// toolInputSchema is a tool's parameters as the JSON Schema object MCP's
// inputSchema requires: type object always, no empty title, and the
// function-level required list when the parameters carry none.
func toolInputSchema(t ap.Tool) (json.RawMessage, error) {
	raw, err := json.Marshal(t.Function.Parameters)
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	if title, _ := schema["title"].(string); title == "" {
		delete(schema, "title")
	}
	schema["type"] = "object"
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	if _, ok := schema["required"]; !ok && t.Function.Required != nil {
		schema["required"] = *t.Function.Required
	}
	return json.Marshal(schema)
}
