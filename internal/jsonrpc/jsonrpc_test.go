package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLines(t *testing.T) {
	long := strings.Repeat("x", 200<<10)
	in := "a\r\n\n  \nb c\n" + long + "\nlast"
	var got []string
	if err := ReadLines(strings.NewReader(in), func(l []byte) { got = append(got, string(l)) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b c", long, "last"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %.20q, want %.20q", i, got[i], want[i])
		}
	}
}

func TestReadLinesFailsOverTheCap(t *testing.T) {
	r := io.MultiReader(strings.NewReader(strings.Repeat("x", MaxLine+1)), strings.NewReader("\n"))
	if err := ReadLines(r, func([]byte) {}); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestConn(t *testing.T) {
	connReads, peerWrites := io.Pipe()
	peerReads, connWrites := io.Pipe()
	peerErr := make(chan *Error, 1)
	var c *Conn
	c = NewConn(connReads, connWrites, "2.0", Handler{
		OnRequest:   func(m Message) { go func() { _ = c.Reply(m.ID, map[string]string{"echo": m.Method}) }() },
		OnPeerError: func(e *Error) { peerErr <- e },
	})
	c.Start()

	in := bufio.NewScanner(peerReads)
	out := NewLineWriter(peerWrites)
	next := func() Message {
		t.Helper()
		if !in.Scan() {
			t.Fatal("conn sent nothing")
		}
		var m Message
		if err := json.Unmarshal(in.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	res := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "ping", nil)
		res <- err
	}()
	call := next()
	if call.JSONRPC != "2.0" || call.Method != "ping" || !call.HasID() {
		t.Fatalf("call = %+v", call)
	}
	_ = out.WriteJSON(Message{ID: call.ID, Error: &Error{Code: CodeInternal, Message: "Internal error", Data: json.RawMessage(`"no session"`)}})
	var e *Error
	if err := <-res; !errors.As(err, &e) || err.Error() != `Internal error: "no session"` {
		t.Fatalf("err = %v", err)
	}

	_ = out.WriteJSON(Message{ID: json.RawMessage(`"s-1"`), Method: "ask"})
	if reply := next(); string(reply.ID) != `"s-1"` || string(reply.Result) != `{"echo":"ask"}` {
		t.Fatalf("reply = %+v", reply)
	}

	_ = out.WriteJSON(Message{ID: json.RawMessage("null"), Error: &Error{Code: -1, Message: "stray"}})
	if e := <-peerErr; e.Message != "stray" {
		t.Fatalf("peer error = %v", e)
	}

	go func() {
		_, err := c.Call(context.Background(), "hang", nil)
		res <- err
	}()
	next()
	_ = peerWrites.Close()
	if err := <-res; !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if _, err := c.Call(context.Background(), "late", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
