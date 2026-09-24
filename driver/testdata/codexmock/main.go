// Command codexmock runs harness-test's mock inference server behind a small
// control proxy for driver/codex_integration_test.go: POST /ctl/toolcall
// {"Name","Args"} arms one tool call, POST /ctl/delay {"Ms"} delays every
// model request, and everything else (/v1/responses, GET and DELETE /log) goes
// to the mock. It prints its URL on stdout.
//
// harness-test depends on agentprotocol, so this lives in its own module under
// testdata. Point it at a harness-test checkout before running:
//
//	go mod edit -replace github.com/belt-sh/harness-test=/path/to/harness-test
//	go mod tidy && go run .
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
	mux.HandleFunc("POST /ctl/toolcall", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Args string }
		json.NewDecoder(r.Body).Decode(&req)
		srv.PrepareToolCall(req.Name, req.Args, "")
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /ctl/delay", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Ms int64 }
		json.NewDecoder(r.Body).Decode(&req)
		delay.Store(req.Ms)
		w.WriteHeader(204)
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
