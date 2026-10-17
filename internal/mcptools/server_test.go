package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func echoServer() *Server {
	return &Server{
		Name: "t", Version: "1",
		Tools: []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Run: func(_ context.Context, c Call) (Result, error) {
			if strings.Contains(string(c.Arguments), "boom") {
				return Result{}, errors.New("it broke")
			}
			return Result{Text: "echo " + string(c.Arguments) + " " + c.ToolUseID}, nil
		},
	}
}

func handle(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	raw, err := s.Handle(context.Background(), json.RawMessage(msg))
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

func TestServerAnswersTheToolMethods(t *testing.T) {
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

func TestServerReportsFailures(t *testing.T) {
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

func TestLoopbackNeedsTheTokenAndAnswersOverHTTP(t *testing.T) {
	l, err := Listen(echoServer())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	if !strings.HasPrefix(l.URL, "http://127.0.0.1:") {
		t.Errorf("url = %s", l.URL)
	}

	post := func(auth, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, l.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	if code, _ := post("", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := post("Bearer wrong", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	code, body := post(l.Header(), `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"a":2}}}`)
	if code != http.StatusOK || !strings.Contains(body, `echo {\"a\":2}`) {
		t.Errorf("call: %d %s", code, body)
	}
	if code, _ := post(l.Header(), `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Errorf("notification: %d", code)
	}
	if code, _ := post(l.Header(), `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); code != http.StatusBadRequest {
		t.Errorf("batch: %d", code)
	}
}
