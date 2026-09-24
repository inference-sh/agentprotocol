// Package codex reads and writes Codex CLI sessions.
//
// Codex keeps one rollout per thread under
// ~/.codex/sessions/YYYY/MM/DD/rollout-<timestamp>-<id>.jsonl. The first row
// is session_meta; the conversation is the response_item rows, whose
// payloads are OpenAI Responses items: message, function_call,
// function_call_output, reasoning. event_msg, turn_context and token
// accounting rows are opaque.
//
// Codex also indexes rollouts in ~/.codex/state_5.sqlite. This codec does
// not write that index; whether resume finds an unindexed rollout is the
// harness-test seed probe's question.
package codex

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Codex session store.
var Codec = transcript.JSONL{
	Agent: "codex",
	Layout: transcript.Layout{
		Files:   files,
		PathFor: pathFor,
		Peek:    peek,
	},
	Header:      header,
	Decode:      decode,
	Encode:      encode,
	WriteHeader: writeHeader,
}

// Vendor is what a Codex session carries in Session.Vendor: the session_meta
// payload as read, so a write reproduces the originator, version and model
// provider fields this codec does not model.
type Vendor struct {
	Meta map[string]json.RawMessage
}

const (
	root      = ".codex/sessions"
	fileStamp = "2006-01-02T15-04-05"
)

type row struct {
	Timestamp string          `json:"timestamp"`
	Ordinal   *int            `json:"ordinal,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id,omitempty"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

type item struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   []part          `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Summary   []part          `json:"summary,omitempty"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func files(home, cwd string) ([]string, error) {
	return transcript.Glob(filepath.Join(home, root, "*", "*", "*", "rollout-*.jsonl"))
}

func pathFor(home string, s *transcript.Session) string {
	t := s.Created.UTC()
	return filepath.Join(home, root, t.Format("2006"), t.Format("01"), t.Format("02"),
		"rollout-"+t.Format(fileStamp)+"-"+s.ID+".jsonl")
}

func peek(path string) (transcript.Info, error) {
	return transcript.PeekFirstLine(path, func(raw json.RawMessage) (transcript.Info, error) {
		var r row
		if err := json.Unmarshal(raw, &r); err != nil {
			return transcript.Info{}, err
		}
		if r.Type != "session_meta" {
			return transcript.Info{}, fmt.Errorf("first row is %q, want session_meta", r.Type)
		}
		var m sessionMeta
		if err := json.Unmarshal(r.Payload, &m); err != nil {
			return transcript.Info{}, err
		}
		in := transcript.Info{ID: m.ID, CWD: m.CWD}
		if t, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
			in.Updated = t
		}
		return in, nil
	})
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, err
	}
	if r.Type != "session_meta" {
		return false, nil
	}
	var m sessionMeta
	if err := json.Unmarshal(r.Payload, &m); err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Payload, &fields); err != nil {
		return false, err
	}
	s.ID = m.ID
	s.CWD = m.CWD
	if t, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
		s.Created = t
	}
	s.Vendor = &Vendor{Meta: fields}
	return true, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.Type != "response_item" {
		return transcript.Entry{}, false, nil
	}
	var it item
	if err := json.Unmarshal(r.Payload, &it); err != nil {
		return transcript.Entry{}, false, err
	}
	e := transcript.Entry{ID: it.ID}
	if t, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
		e.Time = t
	}
	switch it.Type {
	case "message":
		switch it.Role {
		case "user":
			e.Role = transcript.RoleUser
		case "assistant":
			e.Role = transcript.RoleAssistant
		case "developer", "system":
			e.Role = transcript.RoleSystem
		default:
			return transcript.Entry{}, false, fmt.Errorf("item %s: role %q", it.ID, it.Role)
		}
		for _, p := range it.Content {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
	case "reasoning":
		e.Role = transcript.RoleAssistant
		for _, p := range it.Summary {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
		}
	case "function_call":
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: it.CallID, Name: it.Name, Input: arguments(it.Arguments)}}
	case "function_call_output":
		e.Role = transcript.RoleTool
		text, err := outputText(it.Output)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("item %s: output: %w", it.ID, err)
		}
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: it.CallID, Text: text, Status: transcript.StatusOK}}
	default:
		return transcript.Entry{}, false, nil
	}
	return e, true, nil
}

// arguments turns the JSON-encoded string Codex stores into the object it
// encodes, or keeps the string when it is not valid JSON.
func arguments(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

// outputText reads function_call_output.output, a string on this version
// and an array of content parts on newer ones.
func outputText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	meta := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Meta {
			meta[k] = val
		}
	}
	identity, err := json.Marshal(sessionMeta{ID: s.ID, SessionID: s.ID, Timestamp: stamp(s.Created), CWD: s.CWD})
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(identity, &fields); err != nil {
		return nil, err
	}
	for k, val := range fields {
		meta[k] = val
	}
	if _, ok := meta["originator"]; !ok {
		meta["originator"] = json.RawMessage(`"agentprotocol"`)
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	zero := 0
	h, err := json.Marshal(row{Timestamp: stamp(s.Created), Ordinal: &zero, Type: "session_meta", Payload: payload})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{h}, nil
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	if e.ID == "" {
		e.ID = transcript.NewUUID()
	}
	var items []item
	switch e.Role {
	case transcript.RoleUser, transcript.RoleSystem, transcript.RoleAssistant:
		var text []part
		partType := "input_text"
		role := "user"
		switch e.Role {
		case transcript.RoleSystem:
			role = "developer"
		case transcript.RoleAssistant:
			role = "assistant"
			partType = "output_text"
		}
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				text = append(text, part{Type: partType, Text: b.Text})
			case transcript.BlockReasoning:
				items = append(items, item{Type: "reasoning", ID: "rs_" + e.ID, Summary: []part{{Type: "summary_text", Text: b.Text}}})
			case transcript.BlockToolUse:
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				items = append(items, item{Type: "function_call", ID: "fc_" + b.ToolID, Name: b.Name, Arguments: args, CallID: b.ToolID})
			}
		}
		if len(text) > 0 {
			items = append([]item{{Type: "message", ID: "msg_" + e.ID, Role: role, Content: text}}, items...)
		}
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			out, err := json.Marshal(b.Text)
			if err != nil {
				return nil, err
			}
			items = append(items, item{Type: "function_call_output", ID: "fco_" + b.ToolID, CallID: b.ToolID, Output: out})
		}
	default:
		return nil, nil
	}
	if len(items) == 0 {
		return nil, nil
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	// One entry can need several rows (a message plus its tool calls); the
	// store writes whatever bytes Encode returns, so they are joined with
	// newlines here.
	var b strings.Builder
	for i, it := range items {
		payload, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		line, err := json.Marshal(row{Timestamp: stamp(t), Type: "response_item", Payload: payload})
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(line)
	}
	return json.RawMessage(b.String()), nil
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
