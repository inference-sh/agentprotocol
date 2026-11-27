package runner

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/conformance/server"
	"github.com/inference-sh/agentprotocol/harness"
)

func TestCachePrefix(t *testing.T) {
	asked := func(path, body string) server.LogEntry {
		return server.LogEntry{Path: path, Body: []byte(body), ToolCall: true}
	}
	sent := func(path, body string) server.LogEntry {
		return server.LogEntry{Path: path, Body: []byte(body)}
	}
	const chat = "/v1/chat/completions"
	const msgs = "/v1/messages"
	const resp = "/v1/responses"
	const gem = "/v1beta/models/gemini-2.5-flash:streamGenerateContent"
	cases := []struct {
		name    string
		entries []server.LogEntry
		want    string // Key of the cache-prefix check, "" for not reported
		detail  string // text the message must contain
	}{
		{"no tool call, no check", []server.LogEntry{sent(chat, `{"messages":[]}`)}, "", ""},
		{"chat prefix kept", []server.LogEntry{
			asked(chat, `{"tools":[{"a":1}],"messages":[{"role":"system","content":"S"},{"role":"user","content":"Q"}]}`),
			sent(chat, `{"title":"side call"}`),
			sent(chat, `{"messages":[{"content":"S","role":"system"},{"role":"user","content":"Q"},{"role":"assistant","tool_calls":[{"id":"call_mock_1"}]},{"role":"tool","tool_call_id":"call_mock_1","content":"x"}],  "tools":[{"a":1}]}`),
		}, "pass", ""},
		{"chat system prompt rewritten", []server.LogEntry{
			asked(chat, `{"messages":[{"role":"system","content":"time 10:00"},{"role":"user","content":"Q"}]}`),
			sent(chat, `{"messages":[{"role":"system","content":"time 10:01"},{"role":"user","content":"Q"},{"tool_call_id":"call_mock_1"}]}`),
		}, "finding", "messages[0]"},
		{"chat tools reordered", []server.LogEntry{
			asked(chat, `{"tools":[{"n":"a"},{"n":"b"}],"messages":[]}`),
			sent(chat, `{"tools":[{"n":"b"},{"n":"a"}],"messages":[{"tool_call_id":"call_mock_1"}]}`),
		}, "finding", "tools"},
		{"chat history dropped", []server.LogEntry{
			asked(chat, `{"messages":[{"role":"user","content":"A"},{"role":"user","content":"Q"}]}`),
			sent(chat, `{"messages":[{"tool_call_id":"call_mock_1"}]}`),
		}, "finding", "messages[0]"},
		{"anthropic cache_control moved", []server.LogEntry{
			asked(msgs, `{"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"Q","cache_control":{"type":"ephemeral"}}]}]}`),
			sent(msgs, `{"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"Q"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_mock_1"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_mock_1","cache_control":{"type":"ephemeral"}}]}]}`),
		}, "pass", ""},
		{"responses previous_response_id", []server.LogEntry{
			asked(resp, `{"instructions":"I","input":[{"role":"user","content":"Q"}]}`),
			sent(resp, `{"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_mock_1"}]}`),
		}, "pass:incremental", ""},
		{"responses instructions changed", []server.LogEntry{
			asked(resp, `{"instructions":"I","input":[{"role":"user","content":"Q"}]}`),
			sent(resp, `{"instructions":"I2","input":[{"role":"user","content":"Q"},{"type":"function_call_output","call_id":"call_mock_1"}]}`),
		}, "finding", "instructions"},
		{"gemini prefix kept", []server.LogEntry{
			asked(gem, `{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"Q"}]}]}`),
			sent(gem, `{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"Q"}]},{"role":"user","parts":[{"functionResponse":{}}]}]}`),
		}, "pass", ""},
		{"ids renamed by the agent", []server.LogEntry{
			asked(chat, `{"messages":[{"role":"user","content":"Q"}]}`),
			sent(chat, `{"messages":[{"role":"user","content":"Q"},{"role":"assistant","tool_calls":[{"id":"call_abc_0"}]},{"role":"tool","tool_call_id":"call_abc_0","content":"x"}]}`),
		}, "pass", ""},
		{"result never sent", []server.LogEntry{
			asked(chat, `{"messages":[]}`),
			sent(chat, `{"messages":[{"role":"user","content":"other"}]}`),
		}, "skip:no-result", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &TestRunner{harness: harness.All["claude"]}
			r.checkCachePrefix("acp", c.entries)
			got, msg := "", ""
			for _, ch := range r.result.Checks {
				if strings.HasSuffix(ch.ID, "/cache-prefix") {
					got, msg = ch.Key(), ch.Message
				}
			}
			if got != c.want {
				t.Fatalf("got %q (%s), want %q", got, msg, c.want)
			}
			if c.detail != "" && !strings.Contains(msg, c.detail) {
				t.Errorf("message %q does not name %q", msg, c.detail)
			}
		})
	}
}
