package acp_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/inference-sh/agentprotocol/protocol/acp"
)

// The ACP schema's McpServer: http and sse carry a type and headers as a
// name/value list; stdio has no type and env as a name/value list. Agents
// that validate the schema (opencode) reject the object forms.
func TestMCPServerWireForm(t *testing.T) {
	for _, c := range []struct {
		srv  acp.MCPServer
		want string
	}{
		{acp.MCPServer{Name: "tools", URL: "http://127.0.0.1:1/mcp", Headers: map[string]string{"Authorization": "Bearer x", "A": "b"}},
			`{"type":"http","name":"tools","url":"http://127.0.0.1:1/mcp","headers":[{"name":"A","value":"b"},{"name":"Authorization","value":"Bearer x"}]}`},
		{acp.MCPServer{Type: "sse", Name: "s", URL: "http://h/sse"},
			`{"type":"sse","name":"s","url":"http://h/sse","headers":[]}`},
		{acp.MCPServer{Name: "fs", Command: "mcp-fs", Env: map[string]string{"K": "v"}},
			`{"name":"fs","command":"mcp-fs","args":[],"env":[{"name":"K","value":"v"}]}`},
	} {
		got, err := json.Marshal(c.srv)
		if err != nil || string(got) != c.want {
			t.Errorf("marshal %+v = %s, %v; want %s", c.srv, got, err, c.want)
			continue
		}
		var back acp.MCPServer
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatal(err)
		}
		if c.srv.Type == "" && c.srv.Command == "" {
			c.srv.Type = "http"
		}
		if c.srv.Command != "" && c.srv.Args == nil {
			back.Args = nil
		}
		if !reflect.DeepEqual(back, c.srv) {
			t.Errorf("round trip %+v = %+v", c.srv, back)
		}
	}
	// The object form this package used to write still reads.
	var old acp.MCPServer
	if err := json.Unmarshal([]byte(`{"name":"h","url":"u","headers":{"Authorization":"Bearer t"}}`), &old); err != nil || old.Headers["Authorization"] != "Bearer t" {
		t.Errorf("object headers: %+v, %v", old, err)
	}
}
