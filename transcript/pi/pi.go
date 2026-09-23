// Package pi reads and writes pi coding agent sessions, and those of Oh My
// Pi, which forked the format.
//
// pi keeps one JSONL per session under
// ~/.pi/agent/sessions/<wrapped cwd>/<timestamp>_<id>.jsonl. The file
// starts with a session header (version 3); every later row has an id and a
// parentId, so the file is a tree. Message rows carry a pi-ai message:
// user and assistant with content blocks, and toolResult with the call id.
// Model changes, thinking level changes and custom rows stay opaque in the
// chain.
package pi

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the pi session store.
var Codec = codec(".pi/agent/sessions", transcript.DashWrappedCwd)

// OMP is the Oh My Pi session store. It shares pi's rows. Its directory
// name is a dash and the last path element, as observed on one sample;
// listing does not depend on it because the header carries the cwd.
var OMP = codec(".omp/agent/sessions", ompDir)

// ompDir is the project directory rule Oh My Pi was seen to use.
const ompDir transcript.ProjectDir = -1

func codec(root string, project transcript.ProjectDir) transcript.JSONL {
	return transcript.JSONL{
		Layout: transcript.Layout{
			Files: func(home, cwd string) ([]string, error) {
				return transcript.Glob(filepath.Join(home, root, "*", "*.jsonl"))
			},
			PathFor: func(home string, s *transcript.Session) string {
				dir := project.Name(s.CWD)
				if project == ompDir {
					dir = "-" + filepath.Base(s.CWD)
				}
				return filepath.Join(home, root, dir, fileStamp(s.Created)+"_"+s.ID+".jsonl")
			},
			Peek: peek,
		},
		Header:      header,
		Decode:      decode,
		Encode:      encode,
		WriteHeader: writeHeader,
		Tree:        true,
	}
}

// fileStamp is pi's file-name timestamp: an ISO instant with every colon
// and period turned into a dash.
func fileStamp(t time.Time) string {
	iso := t.UTC().Format("2006-01-02T15:04:05.000Z")
	return strings.NewReplacer(":", "-", ".", "-").Replace(iso)
}

// Vendor is what a pi session carries in Session.Vendor: the header row's
// fields beyond the ones this codec models.
type Vendor struct {
	Header map[string]json.RawMessage
}

type row struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	ParentID  *string         `json:"parentId,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
}

type sessionHeader struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

type message struct {
	Role       string  `json:"role"`
	Content    []block `json:"content"`
	ToolCallID string  `json:"toolCallId,omitempty"`
	ToolName   string  `json:"toolName,omitempty"`
	IsError    bool    `json:"isError,omitempty"`
	Timestamp  int64   `json:"timestamp,omitempty"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// peek reads the header, which is the first or second row: pi writes a
// fixed-width title row ahead of it.
func peek(path string) (transcript.Info, error) {
	var found transcript.Info
	err := transcript.EachLine(path, func(raw json.RawMessage) (bool, error) {
		var h sessionHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return false, err
		}
		if h.Type != "session" {
			return true, nil
		}
		found = transcript.Info{ID: h.ID, CWD: h.CWD}
		if t, err := time.Parse(time.RFC3339Nano, h.Timestamp); err == nil {
			found.Updated = t
		}
		return false, nil
	})
	if err != nil {
		return transcript.Info{}, err
	}
	if found.ID == "" {
		return transcript.Info{}, fmt.Errorf("%s: no session header", path)
	}
	return found, nil
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	// The header is handled in decode because it may not be the first row.
	return false, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.Type == "session" {
		var h sessionHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return transcript.Entry{}, false, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return transcript.Entry{}, false, err
		}
		s.ID = h.ID
		s.CWD = h.CWD
		if t, err := time.Parse(time.RFC3339Nano, h.Timestamp); err == nil {
			s.Created = t
		}
		s.Vendor = &Vendor{Header: fields}
		return transcript.Entry{}, false, nil
	}
	if r.ID == "" {
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.ID, Role: transcript.RoleOpaque}
	if r.ParentID != nil {
		e.ParentID = *r.ParentID
	}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.ID, err)
		}
		e.Time = t
	}
	if r.Type != "message" {
		return e, true, nil
	}
	var m message
	if err := json.Unmarshal(r.Message, &m); err != nil {
		return transcript.Entry{}, false, fmt.Errorf("row %s: message: %w", r.ID, err)
	}
	switch m.Role {
	case "user":
		e.Role = transcript.RoleUser
	case "assistant":
		e.Role = transcript.RoleAssistant
	case "toolResult":
		e.Role = transcript.RoleTool
		st := transcript.StatusOK
		if m.IsError {
			st = transcript.StatusError
		}
		var text strings.Builder
		for _, b := range m.Content {
			text.WriteString(b.Text)
		}
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: m.ToolCallID, Name: m.ToolName, Text: text.String(), Status: st}}
		return e, true, nil
	default:
		return transcript.Entry{}, false, fmt.Errorf("row %s: role %q", r.ID, m.Role)
	}
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: b.Text})
		case "thinking":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: b.Thinking})
		case "toolCall":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: b.ID, Name: b.Name, Input: b.Arguments})
		}
	}
	return e, true, nil
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Header {
			fields[k] = val
		}
	}
	identity, err := json.Marshal(sessionHeader{Type: "session", Version: 3, ID: s.ID, Timestamp: s.Created.UTC().Format(time.RFC3339Nano), CWD: s.CWD})
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
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	var m message
	switch e.Role {
	case transcript.RoleUser, transcript.RoleAssistant:
		m.Role = string(e.Role)
		m.Timestamp = t.UnixMilli()
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				m.Content = append(m.Content, block{Type: "text", Text: b.Text})
			case transcript.BlockReasoning:
				m.Content = append(m.Content, block{Type: "thinking", Thinking: b.Text})
			case transcript.BlockToolUse:
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				m.Content = append(m.Content, block{Type: "toolCall", ID: b.ToolID, Name: b.Name, Arguments: args})
			}
		}
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			m = message{Role: "toolResult", ToolCallID: b.ToolID, ToolName: b.Name, IsError: b.Status == transcript.StatusError,
				Content: []block{{Type: "text", Text: b.Text}}, Timestamp: t.UnixMilli()}
		}
	default:
		return nil, nil
	}
	if m.Role == "" || (len(m.Content) == 0 && m.Role != "toolResult") {
		return nil, nil
	}
	if m.Content == nil {
		m.Content = []block{}
	}
	mj, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	r := row{Type: "message", ID: e.ID, Timestamp: t.UTC().Format("2006-01-02T15:04:05.000Z"), Message: mj}
	parent := e.ParentID
	r.ParentID = &parent
	if parent == "" {
		r.ParentID = nil
	}
	return json.Marshal(r)
}
