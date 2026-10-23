package llm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	claudetx "github.com/inference-sh/agentprotocol/transcript/claude"
)

// A restart resumes a session this package wrote, and Claude Code sends
// what the session holds. For that request to begin as the live one did,
// which is what the prompt cache keys on, the session holds what Claude
// Code itself would have: tool results as an MCP tool's (WriteHosted), the
// working directory the live process had, and the context attachments it
// recorded after the first prompt. The working directory is derived from
// the conversation's key; the attachments are learned from the installed
// claude (probeContext), since what they say and how they are rendered is
// Claude Code's own and changes with its version.

// workDir is the conversation's working directory: the same path for every
// process of one key, since Claude Code names it in the context it sends
// after the first prompt. A call without a key gets a directory of its own.
func workDir(root, key string) (string, error) {
	if key == "" {
		return os.MkdirTemp(root, "llm-claude-")
	}
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(root, "llm-claude-"+hex.EncodeToString(sum[:12]))
	return dir, os.MkdirAll(dir, 0o700)
}

// probeTimeout bounds one probe run.
const probeTimeout = time.Minute

// contextFor is the context attachments a session written for dir records,
// for model, dated date. They come from a probe, once per model; a probe
// that fails is not retried, and the session is written without them.
func (c *Claude) contextFor(ctx context.Context, env []string, model, dir string, date time.Time) []json.RawMessage {
	c.mu.Lock()
	learned, ok := c.contexts[model]
	c.mu.Unlock()
	if !ok {
		var err error
		learned, err = c.probeContext(ctx, env, model)
		if err != nil {
			if c.Stderr != nil {
				fmt.Fprintf(c.Stderr, "llm: claude context probe: %v\n", err)
			}
			learned = probed{}
		}
		c.mu.Lock()
		if c.contexts == nil {
			c.contexts = map[string]probed{}
		}
		c.contexts[model] = learned
		c.mu.Unlock()
	}
	return learned.forDir(dir, date)
}

// probed is what a probe learned: the attachments Claude Code recorded in a
// directory of its own.
type probed struct {
	attachments []json.RawMessage
	dir         string
}

// forDir is the attachments for a session in dir on date: the probe's, with
// its working directory and day replaced.
func (p probed) forDir(dir string, date time.Time) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(p.attachments))
	for _, raw := range p.attachments {
		var a map[string]json.RawMessage
		if json.Unmarshal(raw, &a) != nil {
			return nil
		}
		var typ string
		_ = json.Unmarshal(a["type"], &typ)
		switch typ {
		case "environment":
			var snap map[string]json.RawMessage
			if json.Unmarshal(a["snapshot"], &snap) != nil {
				return nil
			}
			var wd string
			_ = json.Unmarshal(snap["workingDirectory"], &wd)
			if wd != p.dir {
				// Claude Code names the directory as it resolves it.
				if real, err := filepath.EvalSymlinks(dir); err == nil {
					dir = real
				}
			}
			snap["workingDirectory"], _ = json.Marshal(dir)
			a["snapshot"], _ = json.Marshal(snap)
		case "date":
			a["date"], _ = json.Marshal(date.Local().Format(time.DateOnly))
		}
		b, err := json.Marshal(a)
		if err != nil {
			return nil
		}
		out = append(out, b)
	}
	return out
}

// probeContext runs the installed claude once, offline, to learn the
// context it records for model on this machine. It answers a scripted
// Messages endpoint on loopback, in a home and configuration of its own, so
// it uses no login and nothing it does reaches the user's sessions.
func (c *Claude) probeContext(ctx context.Context, env []string, model string) (probed, error) {
	root := c.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	tmp, err := os.MkdirTemp(root, "llm-claude-probe-")
	if err != nil {
		return probed{}, err
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "work")
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return probed{}, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return probed{}, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(probeAnswer), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	var penv []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_OAUTH") || name == "CLAUDE_CONFIG_DIR" || name == "HOME" {
			continue
		}
		penv = append(penv, kv)
	}
	penv = append(penv,
		"HOME="+home,
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"ANTHROPIC_BASE_URL=http://"+ln.Addr().String(),
		"ANTHROPIC_API_KEY=probe",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	)
	command := c.Command
	if command == "" {
		command = "claude"
	}
	args := []string{"-p", "probe", "--tools", "", "--strict-mcp-config", "--setting-sources", "", "--disable-slash-commands"}
	if model != "" {
		args = append(args, "--model", model)
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, command, args...)
	cmd.Dir, cmd.Env = dir, penv
	if out, err := cmd.CombinedOutput(); err != nil {
		return probed{}, fmt.Errorf("%w: %.300s", err, out)
	}
	files, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if len(files) != 1 {
		return probed{}, fmt.Errorf("want one session, found %d", len(files))
	}
	got, err := claudetx.ContextAttachments(files[0])
	if err != nil {
		return probed{}, err
	}
	if len(got) != len(claudetx.ContextTypes) {
		return probed{}, fmt.Errorf("recorded %d of the %d context attachments", len(got), len(claudetx.ContextTypes))
	}
	return probed{attachments: got, dir: dir}, nil
}

// probeAnswer is the probe's Messages endpoint: "ok" to everything.
func probeAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	msg := map[string]any{
		"id": "msg_probe", "type": "message", "role": "assistant", "model": "probe", "content": []any{},
		"stop_reason": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
	}
	if !req.Stream {
		msg["content"] = []any{map[string]string{"type": "text", "text": "ok"}}
		msg["stop_reason"] = "end_turn"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msg)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	bw := bufio.NewWriter(w)
	event := func(name string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", name, b)
	}
	event("message_start", map[string]any{"type": "message_start", "message": msg})
	event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "ok"}})
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 1}})
	event("message_stop", map[string]any{"type": "message_stop"})
	_ = bw.Flush()
}
