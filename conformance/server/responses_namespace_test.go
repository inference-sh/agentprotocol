package server

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A tool declared inside a Responses namespace is called in it, and the
// namespace is not counted as a tool.
func TestResponsesNamespacedToolCall(t *testing.T) {
	s := New()
	url, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.PrepareToolCall("probe_echo", `{"text":"x"}`, "")
	body := `{"model":"test","stream":true,"input":"hi","tools":[{"type":"function","name":"flat"},{"type":"namespace","name":"mcp__tools","description":"","tools":[{"type":"function","name":"probe_echo","parameters":{"type":"object"}}]}]}`
	resp, err := http.Post(url+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `"namespace":"mcp__tools"`) || !strings.Contains(string(data), `"name":"probe_echo"`) {
		t.Fatalf("want a probe_echo call in mcp__tools, got %s", data)
	}
	if got := s.ToolDeclarationsIn([]byte(body)); !slices.Equal(got, []string{"flat", "probe_echo"}) && !slices.Equal(got, []string{"probe_echo", "flat"}) {
		t.Fatalf("declarations %v, want flat and probe_echo", got)
	}
}
