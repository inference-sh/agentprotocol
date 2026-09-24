// Package cursor reads Cursor CLI sessions.
//
// Cursor's store of record is a content-addressed blob database,
// ~/.cursor/chats/<hash>/<id>/store.db, whose layout it does not document.
// From it Cursor writes a readable transcript at
// ~/.cursor/projects/<project>/agent-transcripts/<id>/<id>.jsonl: one
// message per row as {"role", "message": {"content": [...]}}, plus
// {"type":"turn_ended"} rows at turn boundaries. This codec reads that
// transcript.
//
// Cursor writes the transcript only for sessions of its TUI and `agent -p`.
// A session of `cursor-agent acp` lives in ~/.cursor/acp-sessions/<id>/
// with no transcript, so this codec never sees one; the sqlite module's
// Cursor codec reads both.
//
// It is read-only and lossy. Cursor's transcript writer emits text,
// reasoning and tool calls but no row for a tool result, so results exist
// only in the blob store; and Cursor never loads the transcript back, so a
// written one would change nothing. The sqlite module's Cursor codec reads
// the blob store instead, with the results, and can write it; this codec is
// for callers that must stay free of a database driver.
package cursor

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Cursor CLI session store, read-only.
var Codec = transcript.JSONL{
	Agent: "cursor",
	Layout: transcript.Layout{
		Files: files,
		Peek:  peek,
	},
	Decode: decode,
	// No Encode: Write returns transcript.ErrReadOnly.
}

const root = ".cursor/projects"

type row struct {
	Role    string `json:"role"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ProjectDir is the directory name Cursor gives a working directory: every
// run of characters other than ASCII letters and digits turned into one dash,
// and dashes trimmed from both ends, so /a/my_b is a-my-b (workspace-paths.js
// in cursor-agent's bundle). It is not reversible, so the transcript's cwd
// cannot be recovered from it, and listing for a cwd matches the directory
// by name.
func ProjectDir(cwd string) string {
	var b strings.Builder
	dash := false
	for _, r := range cwd {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	return b.String()
}

func files(home, cwd string) ([]string, error) {
	project := "*"
	if cwd != "" {
		project = ProjectDir(cwd)
	}
	return transcript.Glob(filepath.Join(home, root, project, "agent-transcripts", "*", "*.jsonl"))
}

// peek names the session after its file. The transcript carries no cwd.
func peek(path string) (transcript.Info, error) {
	return transcript.Info{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	switch r.Role {
	case "user", "assistant", "system":
	case "":
		// A turn boundary or another non-message row.
		return transcript.Entry{}, false, nil
	default:
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{Role: transcript.Role(r.Role)}
	blocks, results, err := content(r.Message.Content)
	if err != nil {
		return transcript.Entry{}, false, err
	}
	e.Content = blocks
	if e.Role == transcript.RoleUser && results > 0 && results == len(blocks) {
		e.Role = transcript.RoleTool
	}
	return e, true, nil
}

// content decodes a message's content, a string or an array of Anthropic-
// shaped blocks, and reports how many of them are tool results.
func content(raw json.RawMessage) ([]transcript.Block, int, error) {
	if len(raw) == 0 {
		return nil, 0, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []transcript.Block{{Kind: transcript.BlockText, Text: text}}, 0, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, 0, err
	}
	out := make([]transcript.Block, 0, len(bs))
	results := 0
	for _, b := range bs {
		switch b.Type {
		case "text":
			out = append(out, transcript.Block{Kind: transcript.BlockText, Text: b.Text})
		case "tool_use":
			out = append(out, transcript.Block{Kind: transcript.BlockToolUse, ToolID: b.ID, Name: b.Name, Input: b.Input})
		case "tool_result":
			results++
			text, err := resultText(b.Content)
			if err != nil {
				return nil, 0, err
			}
			st := transcript.StatusOK
			if b.IsError {
				st = transcript.StatusError
			}
			out = append(out, transcript.Block{Kind: transcript.BlockToolResult, ToolID: b.ToolUseID, Text: text, Status: st})
		}
	}
	return out, results, nil
}

func resultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, x := range bs {
		b.WriteString(x.Text)
	}
	return b.String(), nil
}
