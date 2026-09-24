// Command mock runs harness-test's mock inference server behind a small
// control proxy for the real-binary tests in driver/ (claude, codex and pi).
// It prints its URL on stdout and serves on that one address:
//
//	POST   /ctl/toolcall {"Name","Args"}  answer the next model request with this tool call
//	POST   /tool         {"Name","Args"}  the same, as the claude test names it
//	POST   /text         {"Text"}         canned reply
//	POST   /ctl/delay    {"Ms"}           delay every model request
//	POST   /mcp                           an MCP server (streamable HTTP, JSON only) with one tool, echo
//	GET    /mcp/calls                     the MCP methods called
//
// Everything else goes to harness-test's server: /v1/messages (Anthropic),
// /v1/responses (codex), /v1/chat/completions (pi), GET and DELETE /log.
//
// harness-test depends on agentprotocol, so this lives in its own module under
// testdata. Point it at a harness-test checkout, then run one instance per
// agent so armed tool calls and delays do not leak between tests:
//
//	cd driver/testdata/mock
//	go mod edit -replace github.com/belt-sh/harness-test=/path/to/harness-test
//	go mod tidy && go build -o /tmp/apmock .
//	/tmp/apmock &   # prints http://127.0.0.1:PORT
//
//	AGENTPROTOCOL_CLAUDE_MOCK_URL=URL AGENTPROTOCOL_CLAUDE_MOCK_CONTROL=URL go test -race -run TestClaudeRealBinary ./driver/
//	CODEX_MOCK_URL=URL go test -race -run CodexReal ./driver/
//	PI_MOCK_URL=URL go test -race -run PiReal ./driver/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/belt-sh/harness-test/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	flag.Parse()
	srv := server.New()
	base, err := srv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	u, _ := url.Parse(base)
	u.Path = ""
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1
	var delay atomic.Int64
	mux := http.NewServeMux()

	toolcall := func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Args string }
		json.NewDecoder(r.Body).Decode(&req)
		srv.PrepareToolCall(req.Name, req.Args, "")
		w.WriteHeader(204)
	}
	mux.HandleFunc("POST /ctl/toolcall", toolcall)
	mux.HandleFunc("POST /tool", toolcall)
	mux.HandleFunc("POST /text", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Text string }
		json.NewDecoder(r.Body).Decode(&req)
		srv.SetResponse(req.Text)
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /ctl/delay", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Ms int64 }
		json.NewDecoder(r.Body).Decode(&req)
		delay.Store(req.Ms)
		w.WriteHeader(204)
	})

	var mcpMu sync.Mutex
	mcpCalls := []string{}
	mux.HandleFunc("GET /mcp/calls", func(w http.ResponseWriter, r *http.Request) {
		mcpMu.Lock()
		defer mcpMu.Unlock()
		json.NewEncoder(w).Encode(mcpCalls)
	})
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(405) })
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mcpMu.Lock()
		mcpCalls = append(mcpCalls, req.Method+" "+string(req.Params))
		mcpMu.Unlock()
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &p)
			result = map[string]any{"protocolVersion": p.ProtocolVersion,
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo":   map[string]any{"name": "mockmcp", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo text back.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}}
		case "tools/call":
			var p struct {
				Arguments struct{ Text string } `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + p.Arguments.Text}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if d := delay.Load(); d > 0 && r.Method == "POST" {
			select {
			case <-time.After(time.Duration(d) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("http://%s\n", ln.Addr())
	http.Serve(ln, mux)
}
