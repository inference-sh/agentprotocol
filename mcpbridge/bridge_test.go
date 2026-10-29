package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A server that answers requests, accepts notifications, holds "slow" until
// "fast" has been answered, and refuses a wrong token.
func testServer(t *testing.T) *httptest.Server {
	fastDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		if len(m.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch m.Method {
		case "slow":
			select {
			case <-fastDone:
			case <-time.After(5 * time.Second):
			}
		case "fast":
			defer close(fastDone)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"method":"` + m.Method + `"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func run(t *testing.T, url, auth, input string) []string {
	t.Helper()
	var out bytes.Buffer
	if err := Run(context.Background(), url, auth, strings.NewReader(input), &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	return strings.Split(strings.TrimSpace(out.String()), "\n")
}

// Requests are answered one per line, a notification not at all, and a slow
// call does not hold back the next.
func TestRunForwards(t *testing.T) {
	srv := testServer(t)
	start := time.Now()
	lines := run(t, srv.URL, "Bearer good", strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"slow"}`,
		``,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"fast"}`,
	}, "\n")+"\n")
	if len(lines) != 2 || !strings.Contains(strings.Join(lines, " "), `"fast"`) || !strings.Contains(strings.Join(lines, " "), `"slow"`) {
		t.Fatalf("answers %q", lines)
	}
	// slow is answered only once fast has been, or after 5s.
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("slow held back fast: %s", d)
	}
}

// A request the server refuses gets a JSON-RPC error with its id, so the
// agent is not left waiting; a refused notification gets nothing.
func TestRunAnswersFailures(t *testing.T) {
	srv := testServer(t)
	lines := run(t, srv.URL, "Bearer bad", `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`+"\n"+`{"jsonrpc":"2.0","method":"note"}`+"\n")
	var m struct {
		ID    string `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &m) != nil || m.ID != "a" || m.Error.Code != -32603 || !strings.Contains(m.Error.Message, "401") {
		t.Fatalf("answers %q", lines)
	}
}

func TestEnv(t *testing.T) {
	if e := Env("http://x", "Bearer t"); e[EnvURL] != "http://x" || e[EnvAuthorization] != "Bearer t" {
		t.Fatalf("env %v", e)
	}
}
