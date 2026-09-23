// Package gemini reads and writes Gemini CLI sessions.
//
// Gemini keeps one JSONL per session under
// ~/.gemini/tmp/<project>/chats/session-<stamp>-<id prefix>.jsonl, where
// <project> is the last element of the working directory. The file is a
// patch log: the first row is the conversation header, message rows are
// appended as they happen, and {"$set": ...} rows patch header fields such
// as lastUpdated. A message row has type user or gemini; user content is a
// list of parts, text or functionResponse; gemini content is a string with
// toolCalls and thoughts beside it.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Gemini CLI session store.
var Codec transcript.Codec = wrap{".gemini/tmp"}

// wrap adds cwd recovery to the JSONL store. Gemini writes the working
// directory nowhere in the file, only as the <project> directory (the
// working directory's last element), so Read restores that much from the
// path and a written session lands under the same project directory.
type wrap struct{ root string }

func (w wrap) Open(home string) (transcript.Store, error) {
	st, err := jsonl(w.root).Open(home)
	if err != nil {
		return nil, err
	}
	return &store{Store: st, root: filepath.Join(home, w.root)}, nil
}

type store struct {
	transcript.Store
	root string
}

func (st *store) Read(ctx context.Context, id string) (*transcript.Session, error) {
	s, err := st.Store.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.CWD == "" {
		if infos, err := st.Store.List(ctx, ""); err == nil {
			for _, in := range infos {
				if in.ID == id {
					// <root>/<project>/chats/<file>: the project is two up.
					s.CWD = filepath.Base(filepath.Dir(filepath.Dir(in.Path)))
					break
				}
			}
		}
	}
	return s, nil
}

func jsonl(root string) transcript.JSONL {
	return transcript.JSONL{
		Layout: transcript.Layout{
			Files: func(home, cwd string) ([]string, error) {
				project := "*"
				if cwd != "" {
					project = filepath.Base(cwd)
				}
				return transcript.Glob(filepath.Join(home, root, project, "chats", "*.jsonl"))
			},
			PathFor: func(home string, s *transcript.Session) string {
				return filepath.Join(home, root, filepath.Base(s.CWD), "chats",
					"session-"+s.Created.UTC().Format("2006-01-02T15-04")+"-"+prefix(s.ID)+".jsonl")
			},
			Peek: peek,
		},
		Header:      header,
		Decode:      decode,
		Encode:      encode,
		WriteHeader: writeHeader,
	}
}

// Vendor is what a Gemini session carries in Session.Vendor: the header's
// fields, so a write reproduces the project hash and kind.
type Vendor struct {
	Header map[string]json.RawMessage
}

type conversationHeader struct {
	SessionID   string `json:"sessionId"`
	ProjectHash string `json:"projectHash"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
	Kind        string `json:"kind"`
}

type row struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	Thoughts  []thought       `json:"thoughts,omitempty"`
	ToolCalls []toolCall      `json:"toolCalls,omitempty"`
	Model     string          `json:"model,omitempty"`
	Set       json.RawMessage `json:"$set,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type functionResponse struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type thought struct {
	Subject     string `json:"subject,omitempty"`
	Description string `json:"description,omitempty"`
}

type toolCall struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	Status string          `json:"status,omitempty"`
}

func prefix(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func peek(path string) (transcript.Info, error) {
	return transcript.PeekFirstLine(path, func(raw json.RawMessage) (transcript.Info, error) {
		var h conversationHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return transcript.Info{}, err
		}
		if h.SessionID == "" {
			return transcript.Info{}, fmt.Errorf("%s: first row is not a conversation header", path)
		}
		in := transcript.Info{ID: h.SessionID}
		if t, err := time.Parse(time.RFC3339Nano, h.LastUpdated); err == nil {
			in.Updated = t
		}
		return in, nil
	})
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	var h conversationHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return false, err
	}
	if h.SessionID == "" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, err
	}
	s.ID = h.SessionID
	if t, err := time.Parse(time.RFC3339Nano, h.StartTime); err == nil {
		s.Created = t
	}
	s.Vendor = &Vendor{Header: fields}
	return true, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.ID == "" || r.Type == "" {
		// A patch row or a repeated header.
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.ID}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.ID, err)
		}
		e.Time = t
	}
	switch r.Type {
	case "user":
		var parts []part
		if err := json.Unmarshal(r.Content, &parts); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: content: %w", r.ID, err)
		}
		e.Role = transcript.RoleUser
		responses := 0
		for _, p := range parts {
			if p.FunctionResponse != nil {
				responses++
				e.Content = append(e.Content, transcript.Block{
					Kind: transcript.BlockToolResult, ToolID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name,
					Text: responseText(p.FunctionResponse.Response), Status: transcript.StatusOK,
				})
				continue
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
		if responses > 0 && responses == len(parts) {
			e.Role = transcript.RoleTool
		}
	case "gemini":
		e.Role = transcript.RoleAssistant
		var text string
		if err := json.Unmarshal(r.Content, &text); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: content: %w", r.ID, err)
		}
		for _, th := range r.Thoughts {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: strings.TrimSpace(th.Subject + "\n" + th.Description)})
		}
		if text != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: text})
		}
		for _, tc := range r.ToolCalls {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tc.ID, Name: tc.Name, Input: tc.Args})
		}
	default:
		return transcript.Entry{}, false, nil
	}
	return e, true, nil
}

// responseText reads a functionResponse.response, which Gemini tools fill
// with an output field and other tools with anything.
func responseText(raw json.RawMessage) string {
	var out struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Output != "" {
		return out.Output
	}
	return string(raw)
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Header {
			fields[k] = val
		}
	}
	identity, err := json.Marshal(conversationHeader{
		SessionID:   s.ID,
		StartTime:   stamp(s.Created),
		LastUpdated: stamp(s.Updated),
		Kind:        "main",
	})
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
	if _, ok := fields["projectHash"]; !ok {
		fields["projectHash"] = json.RawMessage(`""`)
	}
	h, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{h}, nil
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	if e.ID == "" {
		e.ID = transcript.NewUUID()
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	r := row{ID: e.ID, Timestamp: stamp(t)}
	switch e.Role {
	case transcript.RoleUser, transcript.RoleTool, transcript.RoleSystem:
		r.Type = "user"
		var parts []part
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				parts = append(parts, part{Text: b.Text})
			case transcript.BlockToolResult:
				resp, err := json.Marshal(struct {
					Output string `json:"output"`
				}{b.Text})
				if err != nil {
					return nil, err
				}
				parts = append(parts, part{FunctionResponse: &functionResponse{ID: b.ToolID, Name: b.Name, Response: resp}})
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
		r.Type = "gemini"
		var text strings.Builder
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				text.WriteString(b.Text)
			case transcript.BlockReasoning:
				r.Thoughts = append(r.Thoughts, thought{Description: b.Text})
			case transcript.BlockToolUse:
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				r.ToolCalls = append(r.ToolCalls, toolCall{ID: b.ToolID, Name: b.Name, Args: args, Status: "success"})
			}
		}
		content, err := json.Marshal(text.String())
		if err != nil {
			return nil, err
		}
		r.Content = content
	default:
		return nil, nil
	}
	return json.Marshal(r)
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
