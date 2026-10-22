package claudecode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPMessageWithoutAHandlerIsAnError(t *testing.T) {
	_, p := connect(t, Handler{})
	p.send(`{"type":"control_request","request_id":"m1","request":{"subtype":"mcp_message","server_name":"x","message":{"jsonrpc":"2.0","id":1,"method":"tools/list"}}}`)
	res := p.next()["response"].(map[string]any)
	if res["subtype"] != "error" || !strings.Contains(res["error"].(string), "SDK MCP server not found: x") {
		t.Errorf("reply = %v", res)
	}
}

func TestMCPMessageRepliesWrapTheResponse(t *testing.T) {
	_, p := connect(t, Handler{OnMCPMessage: func(_ context.Context, _ string, msg json.RawMessage) (json.RawMessage, error) {
		var m struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(msg, &m)
		return json.RawMessage(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{}}`), nil
	}})

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
