package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// peer is the CLI's side of a pipe pair.
type peer struct {
	t   *testing.T
	in  *bufio.Scanner // what the client wrote
	out io.WriteCloser // what the client reads
}

func (p *peer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.out, line+"\n"); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

func (p *peer) next() map[string]any {
	p.t.Helper()
	got := make(chan map[string]any, 1)
	go func() {
		if !p.in.Scan() {
			got <- nil
			return
		}
		var m map[string]any
		_ = json.Unmarshal(p.in.Bytes(), &m)
		got <- m
	}()
	select {
	case m := <-got:
		if m == nil {
			p.t.Fatal("client wrote nothing")
		}
		return m
	case <-time.After(3 * time.Second):
		p.t.Fatal("client wrote nothing within 3s")
	}
	return nil
}

func connect(t *testing.T, h Handler) (*Client, *peer) {
	t.Helper()
	cr, pw := io.Pipe() // peer -> client
	pr, cw := io.Pipe() // client -> peer
	c := NewClient(cr, cw, h)
	c.Start()
	t.Cleanup(func() { pw.Close(); cw.Close() })
	return c, &peer{t: t, in: bufio.NewScanner(pr), out: pw}
}

func TestRequestMatchesItsReplyAndSurfacesErrors(t *testing.T) {
	c, p := connect(t, Handler{})

	errc := make(chan error, 1)
	go func() { errc <- c.SetModel(context.Background(), "m") }()
	req := p.next()
	if req["type"] != "control_request" {
		t.Fatalf("wrote %v", req)
	}
	id := req["request_id"].(string)
	body := req["request"].(map[string]any)
	if body["subtype"] != "set_model" || body["model"] != "m" {
		t.Errorf("request body = %v", body)
	}
	// A reply for someone else is ignored; ours is delivered.
	p.send(`{"type":"control_response","response":{"subtype":"success","request_id":"other"}}`)
	p.send(`{"type":"control_response","response":{"subtype":"error","request_id":"` + id + `","error":"nope"}}`)
	var ce *ControlError
	if err := <-errc; !errors.As(err, &ce) || ce.Message != "nope" {
		t.Errorf("err = %v, want the CLI's error", err)
	}
}

func TestRequestCancelledByContextIsWithdrawn(t *testing.T) {
	c, p := connect(t, Handler{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.Interrupt(ctx) }()
	id := p.next()["request_id"]
	cancel()
	// io.Pipe is unbuffered, so read the withdrawal before the request can
	// return.
	m := p.next()
	if m["type"] != "control_cancel_request" || m["request_id"] != id {
		t.Errorf("wrote %v, want a cancel for %v", m, id)
	}
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestPermissionWithdrawnByTheCLICancelsTheHandler(t *testing.T) {
	cancelled := make(chan string, 1)
	withdrawn := make(chan string, 1)
	_, p := connect(t, Handler{
		OnPermission: func(ctx context.Context, id string, req PermissionRequest) (PermissionResult, error) {
			<-ctx.Done()
			cancelled <- req.ToolUseID
			return AllowResult(req.Input), nil
		},
		OnCancel: func(id string) { withdrawn <- id },
	})
	p.send(`{"type":"control_request","request_id":"q1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{},"tool_use_id":"toolu_9"}}`)
	p.send(`{"type":"control_cancel_request","request_id":"q1"}`)
	select {
	case id := <-cancelled:
		if id != "toolu_9" {
			t.Errorf("tool use id = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler context never ended")
	}
	if id := <-withdrawn; id != "q1" {
		t.Errorf("OnCancel got %q", id)
	}
	// A withdrawn request is not answered. The next line the client writes
	// must be the reply to something else.
	p.send(`{"type":"control_request","request_id":"q2","request":{"subtype":"hook_callback","callback_id":"h","input":{}}}`)
	m := p.next()
	if m["response"].(map[string]any)["request_id"] != "q2" {
		t.Errorf("answered a withdrawn request: %v", m)
	}
}

func TestInboundRequestsWithoutHandlers(t *testing.T) {
	var unhandled []string
	_, p := connect(t, Handler{OnUnhandled: func(s string, _ json.RawMessage) { unhandled = append(unhandled, s) }})

	// No permission handler: deny, never allow.
	p.send(`{"type":"control_request","request_id":"a","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{},"tool_use_id":"t"}}`)
	res := p.next()["response"].(map[string]any)
	body := res["response"].(map[string]any)
	if res["subtype"] != "success" || body["behavior"] != "deny" || body["toolUseID"] != "t" {
		t.Errorf("reply = %v", res)
	}

	// No hook handler: an empty object, "no opinion".
	p.send(`{"type":"control_request","request_id":"b","request":{"subtype":"hook_callback","callback_id":"hook_0","input":{"hook_event_name":"PreToolUse"}}}`)
	res = p.next()["response"].(map[string]any)
	if res["subtype"] != "success" || len(res["response"].(map[string]any)) != 0 {
		t.Errorf("hook reply = %v", res)
	}

	// Unknown subtype: an error reply, as the CLI itself gives.
	p.send(`{"type":"control_request","request_id":"c","request":{"subtype":"from_the_future"}}`)
	res = p.next()["response"].(map[string]any)
	if res["subtype"] != "error" || !strings.Contains(res["error"].(string), "from_the_future") {
		t.Errorf("unknown reply = %v", res)
	}
	if !slices.Equal(unhandled, []string{"from_the_future"}) {
		t.Errorf("unhandled = %v", unhandled)
	}
}

func TestMessagesPassThroughWithUnknownTypes(t *testing.T) {
	got := make(chan Message, 8)
	c, p := connect(t, Handler{OnMessage: func(m Message) { got <- m }})
	p.send(`{"type":"keep_alive"}`)
	p.send(`{"type":"brand_new","x":1}`)
	p.send(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"s","result":"hi","novel":true}`)
	p.out.Close()
	<-c.Done()
	close(got)

	var types []string
	for m := range got {
		types = append(types, m.Type)
		if m.Type == TypeResult {
			var r ResultMessage
			if err := m.Decode(&r); err != nil || r.Result != "hi" || r.SessionID != "s" {
				t.Errorf("result = %+v, %v", r, err)
			}
		}
	}
	if !slices.Equal(types, []string{"brand_new", "result"}) {
		t.Errorf("delivered %v; keep_alive is swallowed, the rest pass", types)
	}
	if _, err := c.Request(context.Background(), map[string]any{"subtype": "interrupt"}); !errors.Is(err, ErrClosed) {
		t.Errorf("request after close = %v", err)
	}
}

func TestArgs(t *testing.T) {
	base := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--include-partial-messages", "--permission-prompt-tool", "stdio"}
	if got := Args(Options{}); !slices.Equal(got, base) {
		t.Errorf("Args = %v", got)
	}
	got := Args(Options{Model: "m", SessionID: "new", ResumeSessionID: "old"})
	if !slices.Contains(got, "--resume=old") || slices.Contains(got, "--session-id=new") {
		t.Errorf("resume wins over a fresh ID: %v", got)
	}
	if got := Args(Options{SessionID: "new"}); !slices.Contains(got, "--session-id=new") {
		t.Errorf("Args = %v", got)
	}
}

func TestSpawnRefusesBypass(t *testing.T) {
	if _, err := Spawn(Options{PermissionMode: PermissionModeBypassPermissions}, Handler{}); err == nil {
		t.Error("spawned in bypassPermissions")
	}
	if _, err := Spawn(Options{ExtraArgs: []string{"--allow-dangerously-skip-permissions"}}, Handler{}); err == nil {
		t.Error("spawned with a skip-permissions flag")
	}
}

func TestNewSessionIDIsAUUIDv4(t *testing.T) {
	id := NewSessionID()
	if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Errorf("id = %q", id)
	}
}
