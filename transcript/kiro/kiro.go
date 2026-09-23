// Package kiro reads and writes kiro-cli sessions.
//
// kiro keeps two files per session under ~/.kiro/sessions/cli: <id>.jsonl
// with one message per row, and <id>.json, a sidecar with the session's
// metadata and, per user turn, the ids of the messages that made it up. A
// written session needs both.
package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the kiro-cli session store.
var Codec transcript.Codec = codec{}

var files = transcript.JSONL{
	Layout: transcript.Layout{
		Root: ".kiro/sessions/cli",
		Ext:  ".jsonl",
		Peek: peek,
	},
	Decode: decode,
	Encode: encode,
	After:  writeSidecar,
}

// codec wraps the JSONL store so Read also loads the sidecar, which the
// generic store does not know about.
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
	if err := readSidecar(st.home, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Vendor is what a kiro session carries in Session.Vendor: the sidecar
// document as read, field by field, so a write keeps everything in it that
// this codec does not model.
type Vendor struct {
	Sidecar map[string]json.RawMessage
}

// sidecar is the part of <id>.json the codec reads and writes. Everything
// else in the document passes through Vendor untouched.
type sidecar struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Title     string `json:"title,omitempty"`
}

type row struct {
	Version string `json:"version"`
	Kind    string `json:"kind"`
	Data    data   `json:"data"`
}

type data struct {
	MessageID string          `json:"message_id"`
	Content   []block         `json:"content"`
	Meta      *meta           `json:"meta,omitempty"`
	Results   json.RawMessage `json:"results,omitempty"`
}

type meta struct {
	Timestamp int64 `json:"timestamp"`
}

type block struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type toolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type toolResult struct {
	ToolUseID string  `json:"toolUseId"`
	Content   []block `json:"content"`
	Status    string  `json:"status"`
}

type resultSummary struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
}

func peek(path string) (transcript.Info, error) {
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A transcript without its sidecar is still a transcript; the
			// id is the file name and the cwd is unknown.
			return transcript.Info{}, nil
		}
		return transcript.Info{}, err
	}
	var sc sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return transcript.Info{}, err
	}
	in := transcript.Info{ID: sc.SessionID, CWD: sc.CWD, Title: sc.Title}
	if t, err := time.Parse(time.RFC3339Nano, sc.UpdatedAt); err == nil {
		in.Updated = t
	}
	return in, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	e := transcript.Entry{ID: r.Data.MessageID}
	switch r.Kind {
	case "Prompt":
		e.Role = transcript.RoleUser
	case "AssistantMessage":
		e.Role = transcript.RoleAssistant
	case "ToolResults":
		e.Role = transcript.RoleTool
	default:
		return transcript.Entry{}, false, nil
	}
	if r.Data.Meta != nil && r.Data.Meta.Timestamp > 0 {
		e.Time = time.Unix(r.Data.Meta.Timestamp, 0).UTC()
	}
	for _, b := range r.Data.Content {
		switch b.Kind {
		case "text":
			var t string
			if err := json.Unmarshal(b.Data, &t); err != nil {
				return transcript.Entry{}, false, fmt.Errorf("message %s: text: %w", e.ID, err)
			}
			if t != "" {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: t})
			}
		case "toolUse":
			var tu toolUse
			if err := json.Unmarshal(b.Data, &tu); err != nil {
				return transcript.Entry{}, false, fmt.Errorf("message %s: toolUse: %w", e.ID, err)
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tu.ToolUseID, Name: tu.Name, Input: tu.Input})
		case "toolResult":
			var tr toolResult
			if err := json.Unmarshal(b.Data, &tr); err != nil {
				return transcript.Entry{}, false, fmt.Errorf("message %s: toolResult: %w", e.ID, err)
			}
			var text strings.Builder
			for _, c := range tr.Content {
				if c.Kind != "text" {
					continue
				}
				var t string
				if err := json.Unmarshal(c.Data, &t); err != nil {
					return transcript.Entry{}, false, fmt.Errorf("message %s: toolResult text: %w", e.ID, err)
				}
				text.WriteString(t)
			}
			st := transcript.StatusOK
			if tr.Status != "success" {
				st = transcript.StatusError
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: tr.ToolUseID, Text: text.String(), Status: st})
		}
	}
	return e, true, nil
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	var kind string
	switch e.Role {
	case transcript.RoleUser:
		kind = "Prompt"
	case transcript.RoleAssistant:
		kind = "AssistantMessage"
	case transcript.RoleTool:
		kind = "ToolResults"
	default:
		return nil, nil
	}
	if e.ID == "" {
		e.ID = transcript.NewUUID()
	}
	d := data{MessageID: e.ID, Content: []block{}}
	results := map[string]resultSummary{}
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			bd, err := json.Marshal(b.Text)
			if err != nil {
				return nil, err
			}
			d.Content = append(d.Content, block{Kind: "text", Data: bd})
		case transcript.BlockToolUse:
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			bd, err := json.Marshal(toolUse{ToolUseID: b.ToolID, Name: b.Name, Input: in})
			if err != nil {
				return nil, err
			}
			d.Content = append(d.Content, block{Kind: "toolUse", Data: bd})
		case transcript.BlockToolResult:
			td, err := json.Marshal(b.Text)
			if err != nil {
				return nil, err
			}
			status := "success"
			if b.Status == transcript.StatusError {
				status = "error"
			}
			bd, err := json.Marshal(toolResult{ToolUseID: b.ToolID, Content: []block{{Kind: "text", Data: td}}, Status: status})
			if err != nil {
				return nil, err
			}
			d.Content = append(d.Content, block{Kind: "toolResult", Data: bd})
			results[b.ToolID] = resultSummary{Tool: b.Name, Status: status}
		}
	}
	switch kind {
	case "Prompt":
		t := e.Time
		if t.IsZero() {
			t = s.Updated
		}
		d.Meta = &meta{Timestamp: t.Unix()}
	case "ToolResults":
		rj, err := json.Marshal(results)
		if err != nil {
			return nil, err
		}
		d.Results = rj
	}
	return json.Marshal(row{Version: "v1", Kind: kind, Data: d})
}

// writeSidecar writes <id>.json beside the transcript. A session read from
// kiro carries its sidecar in Vendor and gets it back with the identity
// fields updated; a foreign session gets the smallest document kiro reads.
func writeSidecar(ctx context.Context, path string, s *transcript.Session) error {
	doc := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Sidecar {
			doc[k] = val
		}
	}
	if _, ok := doc["session_state"]; !ok {
		state, err := json.Marshal(sessionState{
			Version:              "v1",
			ConversationMetadata: conversationMetadata{UserTurnMetadatas: turns(s)},
		})
		if err != nil {
			return err
		}
		doc["session_state"] = state
		doc["session_created_reason"] = json.RawMessage(`"user"`)
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	identity, err := json.Marshal(sidecar{
		SessionID: s.ID,
		CWD:       s.CWD,
		CreatedAt: s.Created.UTC().Format(time.RFC3339Nano),
		UpdatedAt: s.Updated.UTC().Format(time.RFC3339Nano),
		Title:     title,
	})
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(identity, &fields); err != nil {
		return err
	}
	for k, val := range fields {
		doc[k] = val
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".json", out, 0o644)
}

// sessionState is the part of the sidecar kiro needs to find a session's
// turns; the real document holds much more, which Vendor carries through.
type sessionState struct {
	Version              string               `json:"version"`
	ConversationMetadata conversationMetadata `json:"conversation_metadata"`
}

type conversationMetadata struct {
	UserTurnMetadatas []userTurn `json:"user_turn_metadatas"`
}

type userTurn struct {
	MessageIDs []string `json:"message_ids"`
	EndReason  string   `json:"end_reason"`
}

// turns groups message ids by user turn, which is what kiro's sidecar
// lists.
func turns(s *transcript.Session) []userTurn {
	out := []userTurn{}
	for _, e := range s.Messages() {
		if e.Role == transcript.RoleUser || len(out) == 0 {
			out = append(out, userTurn{EndReason: "UserTurnEnd"})
		}
		last := &out[len(out)-1]
		last.MessageIDs = append(last.MessageIDs, e.ID)
	}
	return out
}

// readSidecar loads a session's sidecar into Vendor so a Write reproduces
// it.
func readSidecar(home string, s *transcript.Session) error {
	raw, err := os.ReadFile(filepath.Join(home, ".kiro", "sessions", "cli", s.ID+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	var sc sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return err
	}
	if s.CWD == "" {
		s.CWD = sc.CWD
	}
	s.Title = sc.Title
	if t, err := time.Parse(time.RFC3339Nano, sc.CreatedAt); err == nil {
		s.Created = t
	}
	if t, err := time.Parse(time.RFC3339Nano, sc.UpdatedAt); err == nil {
		s.Updated = t
	}
	s.Vendor = &Vendor{Sidecar: doc}
	return nil
}
