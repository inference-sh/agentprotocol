package pirpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func connect(t *testing.T, h Handler) (*Client, *bufio.Scanner, io.WriteCloser) {
	t.Helper()
	cr, pw := io.Pipe() // pi -> client
	pr, cw := io.Pipe() // client -> pi
	c := NewClient(cr, cw, h)
	c.Start()
	t.Cleanup(func() { pw.Close(); cw.Close() })
	return c, bufio.NewScanner(pr), pw
}

func next(t *testing.T, sc *bufio.Scanner) map[string]any {
	t.Helper()
	got := make(chan map[string]any, 1)
	go func() {
		var m map[string]any
		if sc.Scan() {
			_ = json.Unmarshal(sc.Bytes(), &m)
		}
		got <- m
	}()
	select {
	case m := <-got:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("client wrote nothing")
	}
	return nil
}

func TestRequestMatchesItsResponseByID(t *testing.T) {
	c, in, out := connect(t, Handler{})
	type result struct {
		st  State
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, err := c.GetState(context.Background())
		done <- result{st, err}
	}()
	req := next(t, in)
	if req["type"] != "get_state" || req["id"] == "" {
		t.Fatalf("wrote %v", req)
	}
	io.WriteString(out, `{"id":"someone-else","type":"response","command":"get_state","success":true,"data":{"sessionId":"x"}}`+"\n")
	io.WriteString(out, `{"id":"`+req["id"].(string)+`","type":"response","command":"get_state","success":true,"data":{"sessionId":"s1","isStreaming":true}}`+"\n")
	r := <-done
	if r.err != nil || r.st.SessionID != "s1" || !r.st.IsStreaming {
		t.Errorf("got %+v, %v", r.st, r.err)
	}
}

func TestFailedCommandIsACommandError(t *testing.T) {
	c, in, out := connect(t, Handler{})
	errc := make(chan error, 1)
	go func() { errc <- c.Abort(context.Background()) }()
	req := next(t, in)
	io.WriteString(out, `{"id":"`+req["id"].(string)+`","type":"response","command":"abort","success":false,"error":"nope"}`+"\n")
	var ce *CommandError
	if err := <-errc; !errors.As(err, &ce) || ce.Message != "nope" {
		t.Errorf("err = %v", err)
	}
}

// pi's strings may hold U+2028 and U+2029; only LF ends a record.
func TestRecordsSplitOnLFOnly(t *testing.T) {
	got := make(chan Record, 4)
	_, _, out := connect(t, Handler{OnRecord: func(r Record) { got <- r }})
	io.WriteString(out, "{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"a b c\"}}\r\n")
	r := <-got
	var u MessageUpdate
	if err := r.Decode(&u); err != nil || u.AssistantMessageEvent.Delta != "a b c" {
		t.Errorf("decoded %+v, %v", u, err)
	}
}

func TestUnknownRecordsReachTheHandlerAndBadLinesAreReported(t *testing.T) {
	recs := make(chan Record, 4)
	bad := make(chan string, 4)
	_, _, out := connect(t, Handler{
		OnRecord:    func(r Record) { recs <- r },
		OnMalformed: func(line []byte, _ error) { bad <- string(line) },
	})
	io.WriteString(out, "not json\n{\"type\":\"something_new\",\"x\":1}\n")
	if l := <-bad; l != "not json" {
		t.Errorf("malformed = %q", l)
	}
	if r := <-recs; r.Type != "something_new" {
		t.Errorf("record = %+v", r)
	}
}

func TestPendingRequestsFailWhenTheStreamEnds(t *testing.T) {
	c, in, out := connect(t, Handler{})
	errc := make(chan error, 1)
	go func() { _, err := c.GetState(context.Background()); errc <- err }()
	next(t, in)
	out.Close()
	if err := <-errc; !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v", err)
	}
	if _, err := c.GetState(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("after close: %v", err)
	}
}

func TestUIResponsesHaveOneForm(t *testing.T) {
	for _, tc := range []struct {
		r    UIResponse
		want string
	}{
		{UIValue("a", "Yes"), `{"type":"extension_ui_response","id":"a","value":"Yes"}`},
		{UIValue("a", ""), `{"type":"extension_ui_response","id":"a","value":""}`},
		{UIConfirmed("b", false), `{"type":"extension_ui_response","id":"b","confirmed":false}`},
		{UICancelled("c"), `{"type":"extension_ui_response","id":"c","cancelled":true}`},
	} {
		got, _ := json.Marshal(tc.r)
		if string(got) != tc.want {
			t.Errorf("got %s, want %s", got, tc.want)
		}
	}
}

func TestArgs(t *testing.T) {
	got := Args(Options{Model: "m", SessionID: "s", AppendSystemPrompt: "p", ExtraArgs: []string{"--provider", "x"}})
	want := []string{"--mode", "rpc", "--model", "m", "--session-id", "s", "--append-system-prompt", "p", "--provider", "x"}
	if len(got) != len(want) {
		t.Fatalf("args = %q", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("args = %q, want %q", got, want)
		}
	}
}
