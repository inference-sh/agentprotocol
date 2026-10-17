package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func echoServer() *ToolServer {
	return &ToolServer{
		Name: "t", Version: "1",
		Tools: []MCPTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Call: func(_ context.Context, c MCPToolCall) (MCPToolResult, error) {
			if strings.Contains(string(c.Arguments), "boom") {
				return MCPToolResult{}, errors.New("it broke")
			}
			return MCPToolResult{Text: "echo " + string(c.Arguments) + " " + c.ToolUseID}, nil
		},
	}
}

func handle(t *testing.T, s *ToolServer, msg string) map[string]any {
	t.Helper()
	raw, err := s.Handle(context.Background(), "t", json.RawMessage(msg))
	if err != nil {
		t.Fatalf("handle %s: %v", msg, err)
	}
	if raw == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

func TestToolServerAnswersTheToolMethods(t *testing.T) {
	s := echoServer()

	init := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-11-25" || res["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("initialize = %v", init)
	}
	if init["id"] != float64(1) || init["jsonrpc"] != "2.0" {
		t.Errorf("envelope = %v", init)
	}

	if got := handle(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); got != nil {
		t.Errorf("notification answered %v", got)
	}

	list := handle(t, s, `{"jsonrpc":"2.0","id":"l","method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Errorf("tools/list = %v", list)
	}

	call := handle(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1},"_meta":{"claudecode/toolUseId":"tu1"}}}`)
	content := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["text"] != `echo {"a":1} tu1` || call["result"].(map[string]any)["isError"] != false {
		t.Errorf("tools/call = %v", call)
	}
}

func TestToolServerReportsFailures(t *testing.T) {
	s := echoServer()

	broke := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"x":"boom"}}}`)
	r := broke["result"].(map[string]any)
	if r["isError"] != true || r["content"].([]any)[0].(map[string]any)["text"] != "it broke" {
		t.Errorf("failed call = %v", broke)
	}

	unknown := handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope"}}`)
	if e := unknown["error"].(map[string]any); e["code"] != float64(-32602) {
		t.Errorf("unknown tool = %v", unknown)
	}

	missing := handle(t, s, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	if e := missing["error"].(map[string]any); e["code"] != float64(-32601) {
		t.Errorf("unknown method = %v", missing)
	}

	noArgs := handle(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo"}}`)
	if text := noArgs["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "echo {} " {
		t.Errorf("a call with no arguments got %q", text)
	}
}

func TestMCPMessageWithoutAHandlerIsAnError(t *testing.T) {
	_, p := connect(t, Handler{})
	p.send(`{"type":"control_request","request_id":"m1","request":{"subtype":"mcp_message","server_name":"x","message":{"jsonrpc":"2.0","id":1,"method":"tools/list"}}}`)
	res := p.next()["response"].(map[string]any)
	if res["subtype"] != "error" || !strings.Contains(res["error"].(string), "SDK MCP server not found: x") {
		t.Errorf("reply = %v", res)
	}
}

func TestMCPMessageRepliesWrapTheResponse(t *testing.T) {
	s := echoServer()
	_, p := connect(t, Handler{OnMCPMessage: s.Handle})

	p.send(`{"type":"control_request","request_id":"m1","request":{"subtype":"mcp_message","server_name":"t","message":{"jsonrpc":"2.0","id":7,"method":"ping"}}}`)
	res := p.next()["response"].(map[string]any)
	if res["subtype"] != "success" || res["request_id"] != "m1" {
		t.Fatalf("reply = %v", res)
	}
	mcp := res["response"].(map[string]any)["mcp_response"].(map[string]any)
	if mcp["id"] != float64(7) || mcp["result"] == nil {
		t.Errorf("mcp_response = %v", mcp)
	}

	p.send(`{"type":"control_request","request_id":"m2","request":{"subtype":"mcp_message","server_name":"t","message":{"jsonrpc":"2.0","method":"notifications/cancelled"}}}`)
	ack := p.next()["response"].(map[string]any)["response"].(map[string]any)["mcp_response"].(map[string]any)
	if ack["id"] != float64(0) {
		t.Errorf("notification ack = %v", ack)
	}
}
