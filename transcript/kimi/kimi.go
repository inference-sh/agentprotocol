// Package kimi reads and writes Kimi Code CLI sessions.
//
// Kimi keeps a directory per session under
// ~/.kimi-code/sessions/wd_<name>_<hash>/session_<id>, where <name> is the
// last element of the working directory and <hash> the first twelve hex
// digits of its SHA-256. agents/main/wire.jsonl is the conversation as a
// wire log; state.json beside it is the session's identity; and
// ~/.kimi-code/session_index.jsonl lists every session. The wire log mixes
// context and telemetry with messages: agent.message.appended rows carry
// user and assistant messages, tool results arrive as
// context.append_loop_event rows of type tool.result, and everything else
// is opaque.
package kimi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Kimi Code session store.
var Codec transcript.Codec = codec{}

var files = transcript.JSONL{
	Layout: transcript.Layout{
		Files: func(home, cwd string) ([]string, error) {
			workspace := "*"
			if cwd != "" {
				workspace = workspaceDir(cwd)
			}
			return transcript.Glob(filepath.Join(home, root, workspace, "session_*", "agents", "main", "wire.jsonl"))
		},
		PathFor: func(home string, s *transcript.Session) string {
			return filepath.Join(sessionDir(home, s), "agents", "main", "wire.jsonl")
		},
		Peek: peek,
	},
	Header:      header,
	Decode:      decode,
	Encode:      encode,
	WriteHeader: writeHeader,
	After:       writeState,
	NewID:       func() string { return "session_" + transcript.NewUUID() },
}

const root = ".kimi-code/sessions"

// Vendor is what a Kimi session carries in Session.Vendor: state.json as
// read, so a write keeps the agent bindings this codec does not model.
type Vendor struct {
	State map[string]json.RawMessage
}

type state struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	CWD       string `json:"cwd"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Archived  bool   `json:"archived"`
	Title     string `json:"title,omitempty"`
}

type indexRow struct {
	SessionID  string `json:"sessionId"`
	SessionDir string `json:"sessionDir"`
	WorkDir    string `json:"workDir"`
}

type row struct {
	Type    string          `json:"type"`
	Time    int64           `json:"time,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Event   json.RawMessage `json:"event,omitempty"`
	AgentID string          `json:"agentId,omitempty"`
	Kind    string          `json:"kind,omitempty"`
}

// appended is agent.message.appended's message field: the message wrapped
// with its provenance.
type appended struct {
	Message message `json:"message"`
	Meta    meta    `json:"meta"`
}

type message struct {
	Role      string     `json:"role"`
	Content   []part     `json:"content"`
	ToolCalls []toolCall `json:"toolCalls"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCall struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type meta struct {
	Source    string `json:"source"`
	MessageID string `json:"messageId,omitempty"`
	PromptID  string `json:"promptId,omitempty"`
}

type loopEvent struct {
	Type       string `json:"type"`
	ToolCallID string `json:"toolCallId"`
	Name       string `json:"name,omitempty"`
	Result     struct {
		Output string `json:"output"`
		Error  string `json:"error,omitempty"`
	} `json:"result"`
}

func workspaceDir(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return "wd_" + filepath.Base(cwd) + "_" + hex.EncodeToString(sum[:])[:12]
}

func sessionDir(home string, s *transcript.Session) string {
	return filepath.Join(home, root, workspaceDir(s.CWD), s.ID)
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
		if err := readState(filepath.Join(filepath.Dir(in.Path), "..", ".."), s); err != nil {
			return nil, err
		}
		break
	}
	return s, nil
}

func peek(path string) (transcript.Info, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "..", "..", "state.json"))
	if err != nil {
		return transcript.Info{}, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return transcript.Info{}, err
	}
	return transcript.Info{ID: st.ID, CWD: st.CWD, Title: st.Title, Updated: time.UnixMilli(st.UpdatedAt).UTC()}, nil
}

func readState(dir string, s *transcript.Session) error {
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
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
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	s.CWD = st.CWD
	s.Title = st.Title
	s.Created = time.UnixMilli(st.CreatedAt).UTC()
	s.Updated = time.UnixMilli(st.UpdatedAt).UTC()
	s.Vendor = &Vendor{State: fields}
	return nil
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, err
	}
	return r.Type == "metadata", nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	var e transcript.Entry
	if r.Time > 0 {
		e.Time = time.UnixMilli(r.Time).UTC()
	}
	switch r.Type {
	case "agent.message.appended":
		var a appended
		if err := json.Unmarshal(r.Message, &a); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("agent.message.appended: %w", err)
		}
		e.ID = a.Meta.MessageID
		switch a.Message.Role {
		case "user":
			e.Role = transcript.RoleUser
		case "assistant":
			e.Role = transcript.RoleAssistant
		default:
			return transcript.Entry{}, false, nil
		}
		for _, p := range a.Message.Content {
			if p.Type == "text" {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
			}
		}
		for _, tc := range a.Message.ToolCalls {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tc.ID, Name: tc.Name, Input: arguments(tc.Arguments)})
		}
	case "context.append_loop_event":
		var ev loopEvent
		if err := json.Unmarshal(r.Event, &ev); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("context.append_loop_event: %w", err)
		}
		if ev.Type != "tool.result" {
			return transcript.Entry{}, false, nil
		}
		st := transcript.StatusOK
		if ev.Result.Error != "" {
			st = transcript.StatusError
		}
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: ev.ToolCallID, Name: ev.Name, Text: ev.Result.Output, Status: st}}
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

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	h, err := json.Marshal(struct {
		Type            string `json:"type"`
		ProtocolVersion string `json:"protocol_version"`
		CreatedAt       int64  `json:"created_at"`
	}{"metadata", "1.5", s.Created.UnixMilli()})
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
	switch e.Role {
	case transcript.RoleUser, transcript.RoleAssistant:
		m := message{Role: string(e.Role), Content: []part{}, ToolCalls: []toolCall{}}
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				m.Content = append(m.Content, part{Type: "text", Text: b.Text})
			case transcript.BlockToolUse:
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				m.ToolCalls = append(m.ToolCalls, toolCall{Type: "function", ID: b.ToolID, Name: b.Name, Arguments: args})
			}
		}
		source := "input"
		if e.Role == transcript.RoleAssistant {
			source = "llm"
		}
		wrapped, err := json.Marshal(appended{Message: m, Meta: meta{Source: source, MessageID: e.ID}})
		if err != nil {
			return nil, err
		}
		return json.Marshal(row{Type: "agent.message.appended", Time: t.UnixMilli(), Message: wrapped, Kind: "event"})
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			ev := loopEvent{Type: "tool.result", ToolCallID: b.ToolID, Name: b.Name}
			ev.Result.Output = b.Text
			if b.Status == transcript.StatusError {
				ev.Result.Error = b.Text
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				return nil, err
			}
			return json.Marshal(row{Type: "context.append_loop_event", AgentID: "main", Event: raw, Time: t.UnixMilli()})
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// writeState writes state.json and adds the session to the index. path is
// the wire log, three levels below the session directory.
func writeState(ctx context.Context, path string, s *transcript.Session) error {
	dir := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.State {
			fields[k] = val
		}
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	identity, err := json.Marshal(state{ID: s.ID, Version: 2, CWD: s.CWD, CreatedAt: s.Created.UnixMilli(), UpdatedAt: s.Updated.UnixMilli(), Title: title})
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
	if _, ok := fields["agents"]; !ok {
		agents, err := json.Marshal(map[string]struct {
			Homedir string `json:"homedir"`
			Type    string `json:"type"`
		}{"main": {Homedir: filepath.Join(dir, "agents", "main"), Type: "main"}})
		if err != nil {
			return err
		}
		fields["agents"] = agents
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), out, 0o644); err != nil {
		return err
	}
	return addToIndex(filepath.Join(dir, "..", "..", "..", "session_index.jsonl"), indexRow{SessionID: s.ID, SessionDir: dir, WorkDir: s.CWD})
}

// addToIndex appends a row to session_index.jsonl unless the session is
// already listed.
func addToIndex(path string, entry indexRow) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(existing))
	for sc.Scan() {
		var r indexRow
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.SessionID == entry.SessionID {
			return nil
		}
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
