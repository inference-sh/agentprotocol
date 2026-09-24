package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// pipeServer connects a Client to a scripted peer over in-memory pipes.
type pipeServer struct {
	in  *bufio.Scanner // what the client wrote
	out io.WriteCloser // what the client reads
}

func connect(t *testing.T, h Handler) (*Client, *pipeServer) {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	c := NewClient(cr, cw, h)
	c.Start()
	t.Cleanup(func() { sw.Close(); sr.Close() })
	return c, &pipeServer{in: bufio.NewScanner(sr), out: sw}
}

func (p *pipeServer) read(t *testing.T) Message {
	t.Helper()
	if !p.in.Scan() {
		t.Fatal("client wrote nothing")
	}
	var m Message
	if err := json.Unmarshal(p.in.Bytes(), &m); err != nil {
		t.Fatalf("client wrote %q: %v", p.in.Text(), err)
	}
	return m
}

func (p *pipeServer) send(s string) { _, _ = io.WriteString(p.out, s+"\n") }

func TestCallDecodesResultAndIgnoresUnknownFields(t *testing.T) {
	c, srv := connect(t, Handler{})
	done := make(chan error, 1)
	var res TurnStartResponse
	go func() {
		done <- c.Call(context.Background(), MethodTurnStart, TurnStartParams{ThreadID: "t", Input: []UserInput{TextInput("hi")}}, &res)
	}()
	req := srv.read(t)
	if req.Method != MethodTurnStart || !strings.Contains(string(req.Params), `"type":"text"`) {
		t.Fatalf("request = %s %s", req.Method, req.Params)
	}
	srv.send(`{"id":` + string(req.ID) + `,"result":{"turn":{"id":"turn_1","items":[],"status":"inProgress","futureField":{"a":1}}}}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if res.Turn.ID != "turn_1" || res.Turn.Status != TurnStatusInProgress {
		t.Errorf("result = %+v", res.Turn)
	}
}

func TestCallReturnsTheServersError(t *testing.T) {
	c, srv := connect(t, Handler{})
	done := make(chan error, 1)
	go func() {
		done <- c.Call(context.Background(), MethodThreadResume, ThreadResumeParams{ThreadID: "x"}, nil)
	}()
	req := srv.read(t)
	srv.send(`{"id":` + string(req.ID) + `,"error":{"code":-32600,"message":"no rollout found"}}`)
	var rpcErr *Error
	if err := <-done; !errors.As(err, &rpcErr) || rpcErr.Message != "no rollout found" {
		t.Errorf("err = %v", err)
	}
}

func TestServerRequestIsAnsweredWithItsOwnID(t *testing.T) {
	c, srv := connect(t, Handler{
		OnRequest: func(_ context.Context, _ json.RawMessage, method string, _ json.RawMessage) (any, *Error) {
			if method != MethodItemCommandExecutionRequestApproval {
				return nil, &Error{Code: CodeMethodNotFound, Message: method}
			}
			return CommandExecutionRequestApprovalResponse{Decision: CommandExecutionApprovalDecisionAccept}, nil
		},
	})
	_ = c
	srv.send(`{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1}}`)
	m := srv.read(t)
	if string(m.ID) != "0" || string(m.Result) != `{"decision":"accept"}` {
		t.Errorf("answer = id %s result %s", m.ID, m.Result)
	}
	srv.send(`{"id":"s-1","method":"some/future/request","params":{}}`)
	m = srv.read(t)
	if string(m.ID) != `"s-1"` || m.Error == nil || m.Error.Code != CodeMethodNotFound {
		t.Errorf("unknown request answered with %+v", m)
	}
}

func TestNotificationsArriveInOrderAndGarbageIsSkipped(t *testing.T) {
	got := make(chan string, 4)
	_, srv := connect(t, Handler{OnNotification: func(method string, _ json.RawMessage) { got <- method }})
	srv.send(`not json`)
	srv.send(`{"method":"a/b","params":{}}`)
	srv.send(`{"method":"never/heard/of","params":{"x":1}}`)
	for _, want := range []string{"a/b", "never/heard/of"} {
		select {
		case m := <-got:
			if m != want {
				t.Errorf("got %s, want %s", m, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %s", want)
		}
	}
}

func TestPendingCallsFailWhenTheStreamEnds(t *testing.T) {
	c, srv := connect(t, Handler{})
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), MethodThreadStart, ThreadStartParams{}, nil) }()
	srv.read(t)
	srv.out.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	<-c.Done()
	if err := c.Call(context.Background(), MethodThreadStart, ThreadStartParams{}, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call after close = %v", err)
	}
}

func TestRawUnionsRoundTrip(t *testing.T) {
	var d CommandExecutionApprovalDecision
	if err := json.Unmarshal([]byte(`{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["ls"]}}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.String() != "" {
		t.Errorf("object variant read as string %q", d.String())
	}
	if CommandExecutionApprovalDecisionAcceptForSession.String() != "acceptForSession" {
		t.Error("string variant lost")
	}
	raw, _ := json.Marshal(ThreadStartParams{ApprovalPolicy: AskForApprovalUntrusted})
	if string(raw) != `{"approvalPolicy":"untrusted"}` {
		t.Errorf("params = %s", raw)
	}
}
