// Package grok reads and writes Grok CLI sessions.
//
// Grok keeps a directory per session under
// ~/.grok/sessions/<percent-encoded cwd>/<id>. chat_history.jsonl is the
// conversation, one message per row with an OpenAI-shaped role; summary.json
// is the session's identity and counts; events.jsonl is telemetry and is
// not read.
package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Grok CLI session store.
var Codec transcript.Codec = codec{}

var files = transcript.JSONL{
	Agent: "grok",
	Layout: transcript.Layout{
		Files: func(home, cwd string) ([]string, error) {
			project := "*"
			if cwd != "" {
				project = transcript.EscapedCwd.Name(cwd)
			}
			return transcript.Glob(filepath.Join(home, root, project, "*", "chat_history.jsonl"))
		},
		PathFor: func(home string, s *transcript.Session) string {
			return filepath.Join(home, root, transcript.EscapedCwd.Name(s.CWD), s.ID, "chat_history.jsonl")
		},
		Peek: peek,
		// grok holds its session directory and events.jsonl open while live.
		SessionRoot: filepath.Dir,
	},
	Decode: decode,
	Encode: encode,
	After:  writeSummary,
}

const root = ".grok/sessions"

// Vendor is what a Grok session carries in Session.Vendor: summary.json as
// read, so a write keeps the agent and attempt ids this codec does not
// model.
type Vendor struct {
	Summary map[string]json.RawMessage
}

// summary is the part of summary.json the codec reads and writes. It holds
// every field grok requires when it parses the file: those without a serde
// default in grok's Summary type (xai-grok-shell session/persistence.rs).
// Everything else passes through Vendor.
type summary struct {
	Info           summaryInfo `json:"info"`
	Title          string      `json:"session_summary"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	NumMessages    int         `json:"num_messages"`
	CurrentModelID string      `json:"current_model_id"`
}

type summaryInfo struct {
	ID  string `json:"id"`
	CWD string `json:"cwd"`
}

type row struct {
	Type       string          `json:"type"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ModelID    string          `json:"model_id,omitempty"`
}

type toolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codec struct{}

func (codec) Open(home string) (transcript.Store, error) {
	st, err := files.Open(home)
	if err != nil {
		return nil, err
	}
	return &store{Store: st, home: home}, nil
}

type store struct {
	transcript.Store
	home string
}

// Read loads the summary as well, which the generic store does not know
// about.
func (st *store) Read(ctx context.Context, id string) (*transcript.Session, error) {
	s, err := st.Store.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	infos, err := st.Store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, in := range infos {
		if in.ID != id {
			continue
		}
		s.CWD = in.CWD
		s.Title = in.Title
		s.Updated = in.Updated
		if err := readSummary(filepath.Dir(in.Path), s); err != nil {
			return nil, err
		}
		break
	}
	return s, nil
}

func peek(path string) (transcript.Info, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "summary.json"))
	if err != nil {
		return transcript.Info{}, err
	}
	var sm summary
	if err := json.Unmarshal(raw, &sm); err != nil {
		return transcript.Info{}, err
	}
	in := transcript.Info{ID: sm.Info.ID, CWD: sm.Info.CWD, Title: sm.Title}
	if t, err := time.Parse(time.RFC3339Nano, sm.UpdatedAt); err == nil {
		in.Updated = t
	}
	return in, nil
}

func readSummary(dir string, s *transcript.Session) error {
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var sm summary
	if err := json.Unmarshal(raw, &sm); err != nil {
		return err
	}
	if t, err := time.Parse(time.RFC3339Nano, sm.CreatedAt); err == nil {
		s.Created = t
	}
	s.Model = sm.CurrentModelID
	s.Vendor = &Vendor{Summary: fields}
	return nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	var e transcript.Entry
	switch r.Type {
	case "system":
		e.Role = transcript.RoleSystem
		var text string
		if err := json.Unmarshal(r.Content, &text); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("system row: content: %w", err)
		}
		e.Content = []transcript.Block{{Kind: transcript.BlockText, Text: text}}
	case "user":
		e.Role = transcript.RoleUser
		var parts []part
		if err := json.Unmarshal(r.Content, &parts); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("user row: content: %w", err)
		}
		for _, p := range parts {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
	case "assistant":
		e.Role = transcript.RoleAssistant
		var text string
		if err := json.Unmarshal(r.Content, &text); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("assistant row: content: %w", err)
		}
		if text != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: text})
		}
		for _, tc := range r.ToolCalls {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tc.ID, Name: tc.Name, Input: arguments(tc.Arguments)})
		}
	case "tool_result":
		e.Role = transcript.RoleTool
		var text string
		if err := json.Unmarshal(r.Content, &text); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("tool_result row: content: %w", err)
		}
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: r.ToolCallID, Text: text, Status: transcript.StatusOK}}
	default:
		return transcript.Entry{}, false, nil
	}
	return e, true, nil
}

func arguments(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	var r row
	switch e.Role {
	case transcript.RoleSystem:
		r.Type = "system"
		content, err := json.Marshal(e.Text())
		if err != nil {
			return nil, err
		}
		r.Content = content
	case transcript.RoleUser:
		r.Type = "user"
		var parts []part
		for _, b := range e.Content {
			if b.Kind == transcript.BlockText {
				parts = append(parts, part{Type: "text", Text: b.Text})
			}
		}
		if len(parts) == 0 {
			return nil, nil
		}
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		r.Content = content
	case transcript.RoleAssistant:
		r.Type = "assistant"
		content, err := json.Marshal(e.Text())
		if err != nil {
			return nil, err
		}
		r.Content = content
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolUse {
				continue
			}
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			r.ToolCalls = append(r.ToolCalls, toolCall{ID: b.ToolID, Name: b.Name, Arguments: args})
		}
	case transcript.RoleTool:
		r.Type = "tool_result"
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			r.ToolCallID = b.ToolID
			content, err := json.Marshal(b.Text)
			if err != nil {
				return nil, err
			}
			r.Content = content
		}
		if r.ToolCallID == "" {
			return nil, nil
		}
	default:
		return nil, nil
	}
	return json.Marshal(r)
}

func writeSummary(ctx context.Context, path string, s *transcript.Session) error {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Summary {
			fields[k] = val
		}
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	// grok overwrites current_model_id with the model it runs on when it
	// opens the session, so a session with no recorded model can carry an
	// empty one; the field only has to be present.
	identity, err := json.Marshal(summary{
		Info:           summaryInfo{ID: s.ID, CWD: s.CWD},
		Title:          title,
		CreatedAt:      s.Created.UTC().Format(time.RFC3339Nano),
		UpdatedAt:      s.Updated.UTC().Format(time.RFC3339Nano),
		NumMessages:    len(s.Messages()),
		CurrentModelID: s.Model,
	})
	if err != nil {
		return err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(identity, &known); err != nil {
		return err
	}
	for k, val := range known {
		fields[k] = val
	}
	if _, ok := fields["num_chat_messages"]; !ok {
		fields["num_chat_messages"] = known["num_messages"]
	}
	if _, ok := fields["chat_format_version"]; !ok {
		fields["chat_format_version"] = json.RawMessage("1")
	}
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(path), "summary.json"), out, 0o644)
}
