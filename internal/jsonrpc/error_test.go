package jsonrpc

import (
	"encoding/json"
	"testing"
)

func TestErrorIncludesData(t *testing.T) {
	cases := []struct {
		data json.RawMessage
		want string
	}{
		{nil, "Internal error"},
		{json.RawMessage("null"), "Internal error"},
		{json.RawMessage(`"session not found"`), `Internal error: "session not found"`},
	}
	for _, c := range cases {
		e := &Error{Code: -32603, Message: "Internal error", Data: c.data}
		if got := e.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}
