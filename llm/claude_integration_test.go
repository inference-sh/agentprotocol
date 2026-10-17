package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// liveEnv runs these tests against the user's installed claude and login.
// They spend a few cents of the subscription.
const liveEnv = "AGENTPROTOCOL_LLM_CLAUDE_LIVE"

// apiLog is a reverse proxy to the Anthropic API that keeps every Messages
// request body, so a test can check what the model was sent.
type apiLog struct {
	mu   sync.Mutex
	reqs []json.RawMessage
}

func (l *apiLog) start(t *testing.T) string {
	target, _ := url.Parse("https://api.anthropic.com")
	proxy := httputil.NewSingleHostReverseProxy(target)
	base := proxy.Director
	proxy.Director = func(r *http.Request) { base(r); r.Host = target.Host }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/messages") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			l.mu.Lock()
			l.reqs = append(l.reqs, body)
			l.mu.Unlock()
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// last is the newest request to the main model (one with our tools or our
// system prompt), decoded.
func (l *apiLog) last(t *testing.T, system string) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.reqs) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal(l.reqs[i], &m) == nil && strings.Contains(string(l.reqs[i]), system) {
			return m
		}
	}
	t.Fatalf("no request carried the system prompt")
	return nil
}

var calcTools = []Tool{
	{Name: "add", Description: "Add two integers.", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`)},
	{Name: "multiply", Description: "Multiply two integers.", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`)},
}

const calcSystem = "You are a terse calculator. Always use the tools for arithmetic, one call per operation. Reply with the number only."

func user(text string) transcript.Entry {
	return transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: text}}}
}

func assistant(r *Response) transcript.Entry {
	return transcript.Entry{Role: transcript.RoleAssistant, Content: r.Content}
}

func results(r *Response, out func(transcript.Block) string) []transcript.Entry {
	var es []transcript.Entry
	for _, c := range r.ToolCalls() {
		es = append(es, transcript.Entry{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: c.ToolID, Name: c.Name, Text: out(c), Status: transcript.StatusOK}}})
	}
	return es
}

func calc(b transcript.Block) string {
	var in struct{ A, B int }
	_ = json.Unmarshal(b.Input, &in)
	if b.Name == "multiply" {
		return itoa(in.A * in.B)
	}
	return itoa(in.A + in.B)
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestClaudeLive(t *testing.T) {
	if os.Getenv(liveEnv) == "" {
		t.Skip("set " + liveEnv + "=1 to run against the installed claude")
	}
	var log apiLog
	env := append(os.Environ(), "ANTHROPIC_BASE_URL="+log.start(t))
	m := &Claude{Env: env, Stderr: os.Stderr}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	req := Request{Key: "calc", System: calcSystem, Tools: calcTools, Messages: []transcript.Entry{user("What is 17 + 25?")}}
	var deltas int
	r1, err := m.Generate(ctx, req, func(Delta) { deltas++ })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("1: stop=%s content=%+v usage=%+v", r1.StopReason, r1.Content, r1.Usage)
	if r1.StopReason != "tool_use" || len(r1.ToolCalls()) != 1 || r1.ToolCalls()[0].Name != "add" {
		t.Fatalf("1: want one add call, got %+v", r1)
	}
	if deltas == 0 {
		t.Error("1: nothing streamed")
	}
	sent := log.last(t, calcSystem)
	logSent(t, "1", sent)

	// The result goes back on the live process.
	req.Messages = append(append(req.Messages, assistant(r1)), results(r1, calc)...)
	r2, err := m.Generate(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("2: stop=%s text=%q restarted=%v", r2.StopReason, r2.Text(), r2.Restarted)
	if r2.Restarted || !strings.Contains(r2.Text(), "42") {
		t.Fatalf("2: want a continued answer of 42, got %+v", r2)
	}
	sent = log.last(t, calcSystem)
	msgs := sent["messages"].([]any)
	lastMsg, _ := json.Marshal(msgs[len(msgs)-1])
	t.Logf("2: last message sent: %s", lastMsg)
	if !strings.Contains(string(lastMsg), `"tool_result"`) || strings.Contains(string(lastMsg), "Continue from") {
		t.Errorf("2: the last message should be the tool result alone: %s", lastMsg)
	}

	// A new user turn on the live process.
	req.Messages = append(req.Messages, assistant(r2), user("Now multiply that by 3."))
	r3, err := m.Generate(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("3: stop=%s calls=%+v restarted=%v", r3.StopReason, r3.ToolCalls(), r3.Restarted)
	if r3.Restarted || len(r3.ToolCalls()) != 1 || r3.ToolCalls()[0].Name != "multiply" {
		t.Fatalf("3: want a continued multiply call, got %+v", r3)
	}

	// The same conversation on another key restarts from a written session
	// that ends in the tool result.
	req.Key = "calc-restart"
	req.Messages = append(append(req.Messages, assistant(r3)), results(r3, calc)...)
	r4, err := m.Generate(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("4: stop=%s text=%q restarted=%v", r4.StopReason, r4.Text(), r4.Restarted)
	if !r4.Restarted || !strings.Contains(r4.Text(), "126") {
		t.Fatalf("4: want a restarted answer of 126, got %+v", r4)
	}
	sent = log.last(t, calcSystem)
	logSent(t, "4", sent)
	b, _ := json.MarshalIndent(sent["messages"], "", " ")
	t.Logf("4: messages sent after restart:\n%s", b)
	if dump := os.Getenv("LLM_DUMP"); dump != "" {
		log.mu.Lock()
		all, _ := json.Marshal(log.reqs)
		log.mu.Unlock()
		_ = os.WriteFile(dump, all, 0o644)
	}
}

// logSent reports what Claude Code sent besides the caller's own.
func logSent(t *testing.T, step string, sent map[string]any) {
	t.Helper()
	sys, _ := json.Marshal(sent["system"])
	t.Logf("%s: system sent: %s", step, sys)
	var names []string
	for _, tl := range sent["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	t.Logf("%s: tools sent: %v, model %v", step, names, sent["model"])
	if strings.Join(names, ",") != "add,multiply" {
		t.Errorf("%s: tools sent %v, want add,multiply", step, names)
	}
	first, _ := json.Marshal(sent["messages"].([]any)[0])
	t.Logf("%s: first message sent: %.600s", step, first)
}
