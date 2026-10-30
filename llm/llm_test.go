package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

func toolUse(id, name string) transcript.Block {
	return transcript.Block{Kind: transcript.BlockToolUse, ToolID: id, Name: name, Input: json.RawMessage(`{}`)}
}

func toolResult1(id, text string) transcript.Entry {
	return transcript.Entry{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: id, Text: text}}}
}

// held is a conversation as the agent holds it after req and resp.
func held(req Request, resp *Response) *claudeConv {
	c := &claudeConv{sig: signature(req), results: map[string]toolResult{}}
	c.answered(req, resp)
	return c
}

func TestContinues(t *testing.T) {
	base := Request{Key: "k", System: "s", Tools: calcTools, Messages: []transcript.Entry{user("hi")}}
	calls := &Response{StopReason: "tool_use", Content: []transcript.Block{
		{Kind: transcript.BlockReasoning, Text: "", Signature: "sig"},
		{Kind: transcript.BlockText, Text: "Let me add."},
		toolUse("t1", "add"), toolUse("t2", "add"),
	}}
	// What the caller sends back: the response as it stored it (reasoning
	// dropped, text padded), then the results.
	echoed := transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{
		{Kind: transcript.BlockText, Text: "Let me add. "}, toolUse("t2", "add"), toolUse("t1", "add"),
	}}
	with := func(extra ...transcript.Entry) Request {
		r := base
		r.Messages = append(append(append([]transcript.Entry(nil), base.Messages...), echoed), extra...)
		return r
	}

	cases := []struct {
		name string
		req  Request
		want bool
	}{
		{"every result", with(toolResult1("t1", "1"), toolResult1("t2", "2")), true},
		{"results in one entry", with(transcript.Entry{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "t2"}, {Kind: transcript.BlockToolResult, ToolID: "t1"},
		}}), true},
		{"a result missing", with(toolResult1("t1", "1")), false},
		{"a result for another call", with(toolResult1("t1", "1"), toolResult1("t3", "3")), false},
		{"text after the results", with(toolResult1("t1", "1"), toolResult1("t2", "2"), user("and?")), false},
		{"nothing new", with(), false},
		{"other system prompt", func() Request { r := with(toolResult1("t1", "1"), toolResult1("t2", "2")); r.System = "x"; return r }(), false},
		{"history edited", func() Request {
			r := with(toolResult1("t1", "1"), toolResult1("t2", "2"))
			r.Messages[0] = user("hello")
			return r
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := held(base, calls).continues(tc.req)
			if ok != tc.want {
				t.Errorf("continues = %v, want %v", ok, tc.want)
			}
		})
	}

	ended := &Response{StopReason: "end_turn", Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hello"}}}
	next := base
	next.Messages = append(append([]transcript.Entry(nil), base.Messages...), transcript.Entry{Role: transcript.RoleAssistant, Content: ended.Content}, user("more"))
	if rest, ok := held(base, ended).continues(next); !ok || len(rest) != 1 {
		t.Errorf("a user turn after end_turn: continues = %v (%d new)", ok, len(rest))
	}
	cut := &Response{StopReason: "max_tokens", Content: ended.Content}
	if _, ok := held(base, cut).continues(next); ok {
		t.Error("after max_tokens the agent may have gone on; a call must restart")
	}
}

func TestToolResults(t *testing.T) {
	got := toolResults([]transcript.Entry{{Role: transcript.RoleTool, Content: []transcript.Block{
		{Kind: transcript.BlockToolResult, ToolID: "a", Text: "out", Status: transcript.StatusError},
		{Kind: transcript.BlockImage, ToolID: "a", MediaType: "image/png", Data: []byte{1, 2}},
		{Kind: transcript.BlockToolResult, ToolID: "b"},
	}}})
	a := got["a"]
	if !a.IsError || len(a.Content) != 2 || a.Content[0].Text != "out" || a.Content[1].Type != "image" || a.Content[1].Data != "AQI=" {
		t.Errorf("a = %+v", a)
	}
	if b := got["b"]; len(b.Content) != 1 || b.Content[0].Type != "text" {
		t.Errorf("an empty result still answers the call: %+v", b)
	}
}

func TestToolServer(t *testing.T) {
	s := &toolServer{name: "tools", tools: calcTools, call: func(_ context.Context, id, name string, args json.RawMessage) (toolResult, error) {
		if id == "bad" {
			return toolResult{}, errors.New("no result")
		}
		return toolResult{Content: []mcpContent{{Type: "text", Text: id + ":" + name + ":" + string(args)}}}, nil
	}}
	ctx := context.Background()
	if r := s.handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); r != nil {
		t.Errorf("a notification is not answered here: %s", r)
	}
	for _, tc := range []struct{ msg, want string }{
		{`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`, `"capabilities":{"tools":{}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, `"name":"multiply"`},
		{`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{"a":1},"_meta":{"claudecode/toolUseId":"toolu_1"}}}`, `toolu_1:add:{\"a\":1}`},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add","_meta":{"claudecode/toolUseId":"bad"}}}`, `"isError":true`},
		{`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`, `"resources":[]`},
		{`{"jsonrpc":"2.0","id":5,"method":"sampling/createMessage"}`, `"code":-32601`},
	} {
		if got := string(s.handle(ctx, json.RawMessage(tc.msg))); !strings.Contains(got, tc.want) {
			t.Errorf("%s\n got %s\nwant %s", tc.msg, got, tc.want)
		}
	}
}

func TestCheckRequest(t *testing.T) {
	ok := Request{Messages: []transcript.Entry{user("hi")}}
	if err := checkRequest(ok); err != nil {
		t.Fatal(err)
	}
	forced := ok
	forced.ToolChoice = "required"
	if err := checkRequest(forced); !errors.Is(err, ErrUnsupported) {
		t.Errorf("forced tool choice: %v", err)
	}
	trailing := Request{Messages: []transcript.Entry{user("hi"), {Role: transcript.RoleAssistant}}}
	if checkRequest(trailing) == nil {
		t.Error("a conversation ending with the assistant has nothing to answer")
	}
}

// An approval is the caller's when it names a caller tool, or when the row's
// ApprovalPrefix starts it (droid 1.0.0 titles one "agentbrowser: Probe
// echo"); anything else the agent asks to run is not.
func TestHarnessOurs(t *testing.T) {
	c := &harnessConv{names: map[string]string{"probe_echo": "agentbrowser___probe_echo"}, approvalPrefix: "agentbrowser: "}
	for title, want := range map[string]bool{
		"agentbrowser___probe_echo": true,
		"probe_echo":                true,
		"agentbrowser: Probe echo":  true,
		"Execute":                   false,
		"other: Probe echo":         false,
	} {
		if got := c.ours(title); got != want {
			t.Errorf("ours(%q) = %v, want %v", title, got, want)
		}
	}
	c.approvalPrefix = ""
	if c.ours("agentbrowser: Probe echo") {
		t.Error("a display title matched with no ApprovalPrefix")
	}
}
