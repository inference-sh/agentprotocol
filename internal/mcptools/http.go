package mcptools

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxBody bounds one JSON-RPC message read from HTTP. Tool arguments are model
// output, so this is generous, but a peer cannot make us buffer without limit.
const maxBody = 8 << 20

// ServeHTTP answers MCP's streamable HTTP transport in its JSON-only form: one
// JSON-RPC message per POST, a request answered with application/json, a
// notification with 202. There is no SSE stream (GET is 405), and no session
// id, because nothing here keeps state between messages.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		// Batches left the spec in 2025-06-18; no agent we drive sends them.
		http.Error(w, "batches are not supported", http.StatusBadRequest)
		return
	}
	res, err := s.Handle(r.Context(), json.RawMessage(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if res == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(res)
}

// Loopback serves a Server on 127.0.0.1 for an agent that dials MCP servers
// itself. Every request must carry the bearer token: the port is reachable by
// any local process, and the tools act with the caller's authority.
type Loopback struct {
	// URL is the endpoint to hand the agent.
	URL string
	// Token is the bearer token the agent must send.
	Token string

	srv *http.Server
	ln  net.Listener
}

// Listen starts serving s on a free loopback port.
func Listen(s *Server) (*Loopback, error) {
	if s == nil {
		return nil, errors.New("mcptools: no server")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		ln.Close()
		return nil, err
	}
	l := &Loopback{
		URL:   "http://" + ln.Addr().String() + "/mcp",
		Token: hex.EncodeToString(b[:]),
		ln:    ln,
	}
	want := []byte("Bearer " + l.Token)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.ServeHTTP(w, r)
	})
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// Header is the Authorization header value the agent must send.
func (l *Loopback) Header() string { return "Bearer " + l.Token }

// Close stops serving and ends calls still running.
func (l *Loopback) Close() error {
	if l == nil || l.srv == nil {
		return nil
	}
	return l.srv.Close()
}
