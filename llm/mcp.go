package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
)

// mcpVersions are the MCP revisions the server speaks, newest first. It
// answers initialize with the client's own when it is one of these, and the
// newest a client of any age takes otherwise: clients refuse a revision
// newer than theirs.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func mcpVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	if slices.Contains(mcpVersions, p.ProtocolVersion) {
		return p.ProtocolVersion
	}
	return "2025-06-18"
}

// toolServer is the MCP server that offers the caller's tools. It lists
// them and hands each tools/call to call, which blocks until the caller has
// the result. It speaks JSON-RPC messages, whatever carries them.
type toolServer struct {
	name  string
	tools []Tool
	// call answers tools/call. id is the agent's id for the call, when the
	// agent sends one (claude's claudecode/toolUseId).
	call func(ctx context.Context, id, name string, args json.RawMessage) (toolResult, error)
}

// toolResult is what a tool returned, as MCP carries it.
type toolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// handle answers one JSON-RPC message. A notification gets nil.
func (s *toolServer) handle(ctx context.Context, raw json.RawMessage) json.RawMessage {
	var m rpcMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return rpcError(nil, -32700, "parse error")
	}
	if len(m.ID) == 0 {
		return nil
	}
	switch m.Method {
	case "initialize":
		return rpcResult(m.ID, map[string]any{
			"protocolVersion": mcpVersion(m.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": "1"},
		})
	case "ping":
		return rpcResult(m.ID, map[string]any{})
	case "prompts/list":
		// Not advertised, but asked for anyway by some clients (qwen),
		// which take an error as a broken server.
		return rpcResult(m.ID, map[string]any{"prompts": []any{}})
	case "resources/list":
		return rpcResult(m.ID, map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return rpcResult(m.ID, map[string]any{"resourceTemplates": []any{}})
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
		}
		return rpcResult(m.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      map[string]any  `json:"_meta"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return rpcError(m.ID, -32602, "invalid params")
		}
		id, _ := p.Meta["claudecode/toolUseId"].(string)
		res, err := s.call(ctx, id, p.Name, p.Arguments)
		if err != nil {
			return rpcResult(m.ID, toolResult{Content: []mcpContent{{Type: "text", Text: err.Error()}}, IsError: true})
		}
		return rpcResult(m.ID, res)
	}
	return rpcError(m.ID, -32601, fmt.Sprintf("method not found: %s", m.Method))
}

func rpcResult(id json.RawMessage, result any) json.RawMessage {
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return out
}

func rpcError(id json.RawMessage, code int, msg string) json.RawMessage {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	return out
}

// httpToolServer serves a toolServer as a streamable-HTTP MCP endpoint on
// loopback, for an agent that takes MCP servers by URL. Each message is one
// POST answered with JSON; a tools/call is answered when its result is
// there, so the request stays open while the caller runs the tool. A bearer
// token keeps other local processes out.
type httpToolServer struct {
	srv   *http.Server
	url   string
	token string
}

// debug logs every tool-server exchange and direct launch to stderr.
var debug = os.Getenv("AGENTPROTOCOL_LLM_DEBUG") != ""

func serveHTTP(s *toolServer) (*httpToolServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("llm: mcp listen: %w", err)
	}
	tok := make([]byte, 24)
	_, _ = rand.Read(tok)
	h := &httpToolServer{url: "http://" + ln.Addr().String() + "/mcp", token: hex.EncodeToString(tok)}
	h.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if debug && r.Method != http.MethodPost {
			fmt.Fprintf(os.Stderr, "llm mcp: %s %s accept=%q\n", r.Method, r.URL.Path, r.Header.Get("Accept"))
		}
		if r.Header.Get("Authorization") != "Bearer "+h.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "streamable HTTP over POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply := s.handle(r.Context(), body)
		if debug {
			fmt.Fprintf(os.Stderr, "llm mcp: %s %s accept=%q body=%.300s reply=%.300s\n", r.Method, r.URL.Path, r.Header.Get("Accept"), body, reply)
		}
		if reply == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	})}
	go func() { _ = h.srv.Serve(ln) }()
	return h, nil
}

func (h *httpToolServer) close() { _ = h.srv.Close() }
