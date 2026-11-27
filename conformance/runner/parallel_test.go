package runner

import (
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/conformance/server"
	"github.com/inference-sh/agentprotocol/harness"
)

func TestParallelTurn(t *testing.T) {
	calls := []server.ParallelCall{{ID: "call_0"}, {ID: "call_1"}}
	want := []string{"README-aa", "NOTES-bb"}
	asked := server.LogEntry{Path: "/v1/chat/completions", ToolCall: true, Body: []byte(`{}`)}
	sent := func(path, body string) server.LogEntry { return server.LogEntry{Path: path, Body: []byte(body)} }
	cases := []struct {
		name    string
		entries []server.LogEntry
		want    string
	}{
		{"not served", []server.LogEntry{sent("/v1/chat/completions", `{}`)}, "skip:not-served"},
		{"chat paired", []server.LogEntry{asked, sent("/v1/chat/completions",
			`{"messages":[{"role":"tool","tool_call_id":"call_0","content":"1\tREADME-aa"},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"NOTES-bb"}]}]}`)}, "pass"},
		{"chat swapped", []server.LogEntry{asked, sent("/v1/chat/completions",
			`{"messages":[{"role":"tool","tool_call_id":"call_0","content":"NOTES-bb"},{"role":"tool","tool_call_id":"call_1","content":"README-aa"}]}`)}, "finding:swapped"},
		{"reused ids: only new results count", []server.LogEntry{
			{Path: "/v1/chat/completions", ToolCall: true, Body: []byte(`{"messages":[{"role":"tool","tool_call_id":"call_0","content":"NOTES-bb"},{"role":"tool","tool_call_id":"call_1","content":"README-aa"}]}`)},
			sent("/v1/chat/completions", `{"messages":[{"role":"tool","tool_call_id":"call_0","content":"NOTES-bb"},{"role":"tool","tool_call_id":"call_1","content":"README-aa"},{"role":"tool","tool_call_id":"call_0","content":"README-aa"},{"role":"tool","tool_call_id":"call_1","content":"NOTES-bb"}]}`)}, "pass"},
		{"reused ids not run: the old results are not taken for new ones", []server.LogEntry{
			{Path: "/v1/messages", ToolCall: true, Body: []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_0","content":"README-aa"},{"type":"tool_result","tool_use_id":"call_1","content":"NOTES-bb"}]}]}`)},
			sent("/v1/messages", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_0","content":"README-aa"},{"type":"tool_result","tool_use_id":"call_1","content":"NOTES-bb"}]},{"role":"assistant","content":"[Tool use interrupted]"}]}`)}, "finding:no-result"},
		{"one call broken", []server.LogEntry{asked, sent("/v1/chat/completions",
			`{"messages":[{"role":"tool","tool_call_id":"call_0","content":"README-aa"},{"role":"tool","tool_call_id":"call_1","content":"Error: invalid JSON"}]}`)}, "finding:wrong"},
		{"ids renamed by the agent, paired in order", []server.LogEntry{asked, sent("/v1/chat/completions",
			`{"messages":[{"role":"tool","tool_call_id":"call_0__2","content":"README-aa"},{"role":"tool","tool_call_id":"call_1__2","content":"NOTES-bb"}]}`)}, "pass:renamed-ids"},
		{"ids renamed, results swapped", []server.LogEntry{asked, sent("/v1/chat/completions",
			`{"messages":[{"role":"tool","tool_call_id":"x_0","content":"NOTES-bb"},{"role":"tool","tool_call_id":"x_1","content":"README-aa"}]}`)}, "finding:swapped"},
		{"the tool failed for both", []server.LogEntry{asked, sent("/v1/chat/completions",
			`{"messages":[{"role":"tool","tool_call_id":"call_0","content":"bwrap: No permissions"},{"role":"tool","tool_call_id":"call_1","content":"bwrap: No permissions"}]}`)}, "skip:tool-failed"},
		{"no result", []server.LogEntry{asked, sent("/v1/chat/completions", `{"messages":[]}`)}, "finding:no-result"},
		{"responses paired", []server.LogEntry{{Path: "/v1/responses", ToolCall: true, Body: []byte(`{}`)}, sent("/v1/responses",
			`{"input":[{"type":"function_call_output","call_id":"call_1","output":"NOTES-bb"},{"type":"function_call_output","call_id":"call_0","output":"README-aa"}]}`)}, "pass"},
		{"anthropic paired", []server.LogEntry{{Path: "/v1/messages", ToolCall: true, Body: []byte(`{}`)}, sent("/v1/messages",
			`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_0","content":[{"type":"text","text":"README-aa"}]},{"type":"tool_result","tool_use_id":"call_1","content":"NOTES-bb"}]}]}`)}, "pass"},
		{"gemini by position", []server.LogEntry{{Path: "/v1beta/models/g:streamGenerateContent", ToolCall: true, Body: []byte(`{}`)}, sent("/v1beta/models/g:streamGenerateContent",
			`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"read_file","response":{"output":"README-aa"}}},{"functionResponse":{"name":"read_file","response":{"output":"NOTES-bb"}}}]}]}`)}, "pass"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &TestRunner{harness: harness.All["claude"]}
			r.reportParallelTurn("parallel.interleaved", c.entries, calls, want, "README-aa", "NOTES-bb")
			if len(r.result.Checks) != 1 {
				t.Fatalf("got %d checks", len(r.result.Checks))
			}
			ch := r.result.Checks[0]
			if !strings.HasSuffix(ch.ID, "/parallel.interleaved") || ch.Key() != c.want {
				t.Errorf("got %s %s (%s), want %s", ch.ID, ch.Key(), ch.Message, c.want)
			}
		})
	}
}
