package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// sseData is the data lines of an SSE response, in order.
func sseData(t *testing.T, url, body string) []string {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok && d != "[DONE]" {
			out = append(out, d)
		}
	}
	return out
}

var parallelCalls = []ParallelCall{
	{ID: "call_0", Args: `{"file_path":"README.md"}`},
	{ID: "call_1", Args: `{"file_path":"NOTES.md"}`},
}

const offersRead = `"tools":[{"type":"function","function":{"name":"Read","parameters":{}}}]`

// A client that assembles deltas by index gets both calls whole, and one that
// appends to the last call it saw does not: the stream is interleaved.
func TestChatParallelInterleaved(t *testing.T) {
	s := New()
	url, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.PrepareParallelToolCalls("Read", parallelCalls, "")

	type call struct{ id, name, args string }
	byIndex := map[int]*call{}
	var last *call
	naive := map[*call]string{}
	for _, d := range sseData(t, url+"/v1/chat/completions", `{"model":"m","stream":true,`+offersRead+`,"messages":[]}`) {
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      *string `json:"name"`
							Arguments string  `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		json.Unmarshal([]byte(d), &chunk)
		if len(chunk.Choices) == 0 {
			continue
		}
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			c := byIndex[tc.Index]
			if c == nil {
				c = &call{id: tc.ID}
				byIndex[tc.Index] = c
				last = c
			} else if tc.Function.Name != nil {
				t.Errorf("call %d: name sent again in a delta", tc.Index)
			}
			if tc.Function.Name != nil {
				c.name = *tc.Function.Name
			}
			c.args += tc.Function.Arguments
			naive[last] += tc.Function.Arguments
		}
	}
	if len(byIndex) != 2 {
		t.Fatalf("got %d calls, want 2", len(byIndex))
	}
	for i, want := range parallelCalls {
		c := byIndex[i]
		if c.id != want.ID || c.name != "Read" || c.args != want.Args {
			t.Errorf("call %d = %+v, want id %s args %s", i, *c, want.ID, want.Args)
		}
	}
	if naive[byIndex[1]] == parallelCalls[1].Args {
		t.Error("the stream is not interleaved: appending to the last call opened gives the right answer")
	}
	if s.ParallelServed() != 1 {
		t.Errorf("ParallelServed = %d, want 1", s.ParallelServed())
	}
}

func TestResponsesAndAnthropicParallel(t *testing.T) {
	s := New()
	url, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	s.PrepareParallelToolCalls("Read", parallelCalls, "")
	args := map[int]string{}
	var done []string
	for _, d := range sseData(t, url+"/v1/responses", `{"model":"m","stream":true,"tools":[{"type":"function","name":"Read"}],"input":[]}`) {
		var ev struct {
			Type        string `json:"type"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
			Response    struct {
				Output []struct {
					CallID    string `json:"call_id"`
					Arguments string `json:"arguments"`
				} `json:"output"`
			} `json:"response"`
		}
		json.Unmarshal([]byte(d), &ev)
		switch ev.Type {
		case "response.function_call_arguments.delta":
			args[ev.OutputIndex] += ev.Delta
		case "response.completed":
			for _, o := range ev.Response.Output {
				done = append(done, o.CallID+" "+o.Arguments)
			}
		}
	}
	for i, c := range parallelCalls {
		if args[i] != c.Args {
			t.Errorf("responses call %d args %q, want %q", i, args[i], c.Args)
		}
	}
	if strings.Join(done, "|") != "call_0 "+parallelCalls[0].Args+"|call_1 "+parallelCalls[1].Args {
		t.Errorf("responses completed output = %v", done)
	}

	s.PrepareParallelToolCalls("Read", []ParallelCall{{ID: "toolu_0", Args: parallelCalls[0].Args}, {ID: "toolu_1", Args: parallelCalls[1].Args}}, "")
	blocks := map[int]string{}
	ids := map[int]string{}
	for _, d := range sseData(t, url+"/v1/messages", `{"model":"m","stream":true,"tools":[{"name":"Read"}],"messages":[]}`) {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				ID string `json:"id"`
			} `json:"content_block"`
			Delta struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		json.Unmarshal([]byte(d), &ev)
		switch ev.Type {
		case "content_block_start":
			ids[ev.Index] = ev.ContentBlock.ID
		case "content_block_delta":
			blocks[ev.Index] += ev.Delta.PartialJSON
		}
	}
	for i, c := range parallelCalls {
		if blocks[i] != c.Args || ids[i] != []string{"toolu_0", "toolu_1"}[i] {
			t.Errorf("anthropic block %d = %s %q", i, ids[i], blocks[i])
		}
	}
}
