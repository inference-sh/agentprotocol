// Package droid reads and writes Factory droid sessions.
//
// droid keeps one JSONL per session under
// ~/.factory/sessions/<mangled cwd>/<id>.jsonl. The first row is
// session_start; message rows link to their parent through id and parentId
// and carry an Anthropic-shaped message. Hook records are message rows with
// no content and a visibility of user_only; they keep their place in the
// chain and stay opaque.
package droid

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the droid session store.
var Codec = transcript.JSONL{
	Layout: transcript.Layout{
		Root:    ".factory/sessions",
		Project: transcript.MangledCwd,
		Ext:     ".jsonl",
	},
	Header:      header,
	Decode:      decode,
	Encode:      encode,
	WriteHeader: writeHeader,
	Tree:        true,
}

// Vendor is what a droid session carries in Session.Vendor: the
// session_start row's fields, so a write reproduces the host id and title
// state this codec does not model.
type Vendor struct {
	Header map[string]json.RawMessage
}

type sessionStart struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Owner   string `json:"owner"`
	Version int    `json:"version"`
	CWD     string `json:"cwd"`
	HostID  string `json:"hostId"`
}

type row struct {
	Type      string   `json:"type"`
	ID        string   `json:"id"`
	ParentID  string   `json:"parentId,omitempty"`
	Timestamp string   `json:"timestamp"`
	Message   *message `json:"message,omitempty"`
}

type message struct {
	Role       string  `json:"role"`
	Content    []block `json:"content"`
	Visibility string  `json:"visibility,omitempty"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	var h sessionStart
	if err := json.Unmarshal(raw, &h); err != nil {
		return false, err
	}
	if h.Type != "session_start" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, err
	}
	s.ID = h.ID
	s.CWD = h.CWD
	s.Title = h.Title
	s.Vendor = &Vendor{Header: fields}
	return true, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.Type != "message" || r.ID == "" {
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.ID, ParentID: r.ParentID, Role: transcript.RoleOpaque}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.ID, err)
		}
		e.Time = t
	}
	if r.Message == nil || len(r.Message.Content) == 0 {
		return e, true, nil
	}
	e.Role = transcript.Role(r.Message.Role)
	toolResults := 0
	for _, b := range r.Message.Content {
		switch b.Type {
		case "text":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: b.Text})
		case "tool_use":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: b.ID, Name: b.Name, Input: b.Input})
		case "tool_result":
			toolResults++
			text, err := resultText(b.Content)
			if err != nil {
				return transcript.Entry{}, false, fmt.Errorf("row %s: tool_result: %w", r.ID, err)
			}
			st := transcript.StatusOK
			if b.IsError != nil && *b.IsError {
				st = transcript.StatusError
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: b.ToolUseID, Text: text, Status: st})
		}
	}
	if e.Role == transcript.RoleUser && toolResults == len(r.Message.Content) {
		e.Role = transcript.RoleTool
	}
	return e, true, nil
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
	for _, p := range bs {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Header {
			fields[k] = val
		}
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	hostID := ""
	if raw, ok := fields["hostId"]; ok {
		if err := json.Unmarshal(raw, &hostID); err != nil {
			return nil, fmt.Errorf("hostId: %w", err)
		}
	}
	if hostID == "" {
		hostID = transcript.NewUUID()
	}
	identity, err := json.Marshal(sessionStart{Type: "session_start", ID: s.ID, Title: title, Owner: "unknown", Version: 2, CWD: s.CWD, HostID: hostID})
	if err != nil {
		return nil, err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(identity, &known); err != nil {
		return nil, err
	}
	for k, val := range known {
		fields[k] = val
	}
	h, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{h}, nil
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	role := e.Role
	if role == transcript.RoleTool {
		role = transcript.RoleUser
	}
	if role != transcript.RoleUser && role != transcript.RoleAssistant {
		return nil, nil
	}
	content := make([]block, 0, len(e.Content))
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			content = append(content, block{Type: "text", Text: b.Text})
		case transcript.BlockToolUse:
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			content = append(content, block{Type: "tool_use", ID: b.ToolID, Name: b.Name, Input: in})
		case transcript.BlockToolResult:
			text, err := json.Marshal(b.Text)
			if err != nil {
				return nil, err
			}
			isErr := b.Status == transcript.StatusError
			content = append(content, block{Type: "tool_result", ToolUseID: b.ToolID, IsError: &isErr, Content: text})
		}
	}
	if len(content) == 0 {
		return nil, nil
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	return json.Marshal(row{
		Type:      "message",
		ID:        e.ID,
		ParentID:  e.ParentID,
		Timestamp: t.UTC().Format("2006-01-02T15:04:05.000Z"),
		Message:   &message{Role: string(role), Content: content},
	})
}
