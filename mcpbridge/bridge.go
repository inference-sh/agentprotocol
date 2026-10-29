// Package mcpbridge carries a stdio MCP connection to an MCP server served
// over streamable HTTP. An agent that launches MCP servers only as
// subprocesses (its initialize declares no mcpCapabilities.http) is given a
// stdio server that runs Run: the agent writes JSON-RPC to the bridge's
// stdin, the bridge POSTs each message to the HTTP server and writes the
// answer to stdout. It reads JSON answers only, which is what this module's
// own tool server sends; an SSE answer is not read.
//
// The bridge is a mode of the host's own binary. Call Main first thing in
// main, and set driver.ACPBackend.StdioBridge to the command that starts the
// binary (os.Executable()). The URL and Authorization header travel in the
// environment (Env), so they never appear in a process listing.
package mcpbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

// The environment variables a bridge reads its server from.
const (
	EnvURL           = "AGENTPROTOCOL_MCP_BRIDGE_URL"
	EnvAuthorization = "AGENTPROTOCOL_MCP_BRIDGE_AUTHORIZATION"
)

// maxMessage bounds one JSON-RPC line, as the HTTP server bounds a body.
const maxMessage = 8 << 20

// Env is the environment that points a bridge at url, sending authorization
// as its Authorization header.
func Env(url, authorization string) map[string]string {
	return map[string]string{EnvURL: url, EnvAuthorization: authorization}
}

// Main runs the bridge and exits when the process was started as one (EnvURL
// is set), and returns at once otherwise. The bridge ends when stdin closes.
func Main() {
	url := os.Getenv(EnvURL)
	if url == "" {
		return
	}
	if err := Run(context.Background(), url, os.Getenv(EnvAuthorization), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mcpbridge:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Run bridges newline-delimited JSON-RPC from in to the server at url and
// writes the server's answers to out, one per line. Messages are sent as they
// arrive, so a long tool call does not hold back the next request; answers are
// written in the order they come back. A request the server fails is answered
// with a JSON-RPC error, so the agent is never left waiting. Run returns when
// in ends and every answer is written.
func Run(ctx context.Context, url, authorization string, in io.Reader, out io.Writer) error {
	b := &bridge{url: url, auth: authorization, out: out}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(nil), line...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.forward(ctx, msg)
		}()
	}
	wg.Wait()
	return sc.Err()
}

type bridge struct {
	url  string
	auth string

	mu  sync.Mutex
	out io.Writer
}

func (b *bridge) forward(ctx context.Context, msg []byte) {
	var head struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(msg, &head)
	res, err := b.post(ctx, msg)
	if err != nil {
		if len(head.ID) == 0 || string(head.ID) == "null" {
			return
		}
		res, _ = json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      head.ID,
			"error":   map[string]any{"code": -32603, "message": "mcpbridge: " + err.Error()},
		})
	}
	if len(res) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.out.Write(append(res, '\n'))
}

// post sends one message and returns the server's answer, nil for a
// notification it accepted.
func (b *bridge) post(ctx context.Context, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if b.auth != "" {
		req.Header.Set("Authorization", b.auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("server answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	case len(body) > maxMessage:
		return nil, errors.New("answer too large")
	}
	return bytes.TrimSpace(body), nil
}
