package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// scriptedAnthropic is a Messages endpoint that plays calcSystem's
// calculator: a question gets an add call, "multiply" a multiply call, and
// a tool result the number. Every request to the main model (one carrying
// calcSystem) is kept as sent.
type scriptedAnthropic struct {
	url  string
	mu   sync.Mutex
	main []json.RawMessage
}

func newScriptedAnthropic(t *testing.T) *scriptedAnthropic {
	s := &scriptedAnthropic{}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// requests returns the main-model requests so far, and forgets them.
func (s *scriptedAnthropic) requests() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.main
	s.main = nil
	return out
}

func (s *scriptedAnthropic) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(raw, &req)
	// The newest turn, past the system rows Claude Code adds at the end.
	var last string
	for i := len(req.Messages) - 1; i >= 0 && last == ""; i-- {
		if req.Messages[i].Role != "system" {
			last = string(req.Messages[i].Content)
		}
	}
	text, call, stop := "ok", "", "end_turn"
	if bytes.Contains(raw, []byte(calcSystem)) {
		s.mu.Lock()
		s.main = append(s.main, raw)
		s.mu.Unlock()
		switch {
		case strings.Contains(last, `"toolu_mul"`):
			text = "126"
		case strings.Contains(last, `"toolu_add"`):
			text = "42"
		case strings.Contains(last, "1 + 1"):
			text = "2"
		case strings.Contains(last, "multiply"):
			call, stop = "multiply", "tool_use"
		default:
			call, stop = "add", "tool_use"
		}
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_x", "type": "message", "role": "assistant", "model": "claude-test",
			"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn",
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	bw := bufio.NewWriter(w)
	flusher, _ := w.(http.Flusher)
	event := func(name string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", name, b)
		_ = bw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": fmt.Sprintf("msg_%d", time.Now().UnixNano()), "type": "message", "role": "assistant", "model": "claude-test",
		"content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
	}})
	if call != "" {
		id, in := "toolu_add", `{"a":17,"b":25}`
		if call == "multiply" {
			id, in = "toolu_mul", `{"a":42,"b":3}`
		}
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": call, "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": in}})
	} else {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text}})
	}
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop},
		"usage": map[string]int{"output_tokens": 5}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

// cachePrefix is what of a Messages request the prompt cache keys on: its
// system prompt, tools and messages, parsed, since the API caches the
// prompt they render to and not the JSON's key order. The cache_control
// markers are taken out: they mark where a request writes the cache and
// move to its last block, so they differ between any two requests of one
// conversation.
func cachePrefix(t *testing.T, raw json.RawMessage) (system, tools string, messages []string) {
	t.Helper()
	var req struct {
		System   any   `json:"system"`
		Tools    any   `json:"tools"`
		Messages []any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	enc := func(v any) string {
		b, _ := json.Marshal(withoutCacheControl(v))
		return string(b)
	}
	for _, m := range req.Messages {
		messages = append(messages, enc(m))
	}
	return enc(req.System), enc(req.Tools), messages
}

func withoutCacheControl(v any) any {
	switch v := v.(type) {
	case map[string]any:
		delete(v, "cache_control")
		for k, x := range v {
			v[k] = withoutCacheControl(x)
		}
	case []any:
		for i, x := range v {
			v[i] = withoutCacheControl(x)
		}
	}
	return v
}

// TestClaudeRestartKeepsCachePrefix runs one conversation twice through the
// installed claude against a scripted Messages API: once continued on the
// live process, once restarted from a written session at the last call. The
// restarted request must send what the continued one did, byte for byte, up
// to what the restart adds after the tool result: anything else is a prompt
// cache miss on every restart.
func TestClaudeRestartKeepsCachePrefix(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	api := newScriptedAnthropic(t)
	home, work := t.TempDir(), t.TempDir()
	env := append(os.Environ(),
		"HOME="+home,
		"CLAUDE_CONFIG_DIR="+home+"/.claude",
		"ANTHROPIC_BASE_URL="+api.url,
		"ANTHROPIC_API_KEY=test-key",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	)
	newModel := func() *Claude { return &Claude{Env: env, Home: home, WorkRoot: work} }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	gen := func(m *Claude, req Request) *Response {
		t.Helper()
		r, err := m.Generate(ctx, req, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// The conversation on one live process: add, its result, a new turn,
	// multiply, its result.
	live := newModel()
	req := Request{Key: "calc", System: calcSystem, Tools: calcTools, Messages: []transcript.Entry{user("What is 17 + 25?")}}
	r1 := gen(live, req)
	req.Messages = append(append(req.Messages, assistant(r1)), results(r1, calc)...)
	r2 := gen(live, req)
	req.Messages = append(req.Messages, assistant(r2), user("Now multiply that by 3."))
	r3 := gen(live, req)
	req.Messages = append(append(req.Messages, assistant(r3)), results(r3, calc)...)
	r4 := gen(live, req)
	if r2.Restarted || r3.Restarted || r4.Restarted || r4.Text() != "126" {
		t.Fatalf("the live conversation restarted or went wrong: %v %v %v %q", r2.Restarted, r3.Restarted, r4.Restarted, r4.Text())
	}
	sent := api.requests()
	continued := sent[len(sent)-1]
	_ = live.Close()

	// The same last call on a new model: the process is gone, as after a
	// worker restart, and the conversation is written and resumed.
	again := newModel()
	defer again.Close()
	r5 := gen(again, req)
	if !r5.Restarted || r5.Text() != "126" {
		t.Fatalf("want a restarted 126, got restarted=%v %q", r5.Restarted, r5.Text())
	}
	sent = api.requests()
	restarted := sent[len(sent)-1]

	cs, ct, cm := cachePrefix(t, continued)
	rs, rt, rm := cachePrefix(t, restarted)
	if cs != rs {
		t.Errorf("system differs:\ncontinued %s\nrestarted %s", cs, rs)
	}
	if ct != rt {
		t.Errorf("tools differ:\ncontinued %s\nrestarted %s", ct, rt)
	}
	if len(cm) != len(rm) {
		t.Fatalf("continued sent %d messages, restarted %d", len(cm), len(rm))
	}
	for i := range cm[:len(cm)-1] {
		if cm[i] != rm[i] {
			t.Errorf("message %d differs:\ncontinued %s\nrestarted %s", i, cm[i], rm[i])
		}
	}
	// The last message is the tool result, which the restart follows with
	// its "Continue from where you left off." in the same message.
	last := len(cm) - 1
	blocks := func(m string) []string {
		var msg struct{ Content []json.RawMessage }
		_ = json.Unmarshal([]byte(m), &msg)
		var out []string
		for _, b := range msg.Content {
			out = append(out, string(b))
		}
		return out
	}
	cb, rb := blocks(cm[last]), blocks(rm[last])
	if len(rb) < len(cb) || strings.Join(rb[:len(cb)], ",") != strings.Join(cb, ",") {
		t.Errorf("the tool result differs:\ncontinued %s\nrestarted %s", cm[last], rm[last])
	}
}
