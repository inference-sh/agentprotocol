// Package copilot reads and writes GitHub Copilot CLI sessions.
//
// Copilot keeps a directory per session under ~/.copilot/session-state/<id>
// with events.jsonl, the conversation as an event log, and workspace.yaml,
// the session's identity. Events link to their parent through id and
// parentId. user.message and assistant.message rows are the conversation;
// hook, model change and turn boundary rows stay opaque in the chain.
//
// Copilot finds sessions through an index, ~/.copilot/session-store.db, so a
// session it will load needs a row there too. Codec here is read-only;
// Writer writes the files, and the sqlite module adds the index.
package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec reads Copilot CLI sessions. It is read-only: Copilot finds sessions
// through its index, ~/.copilot/session-store.db, and a session with no row
// there is "not found or could not be loaded" however well-formed its files
// are. Writing the index needs a database driver, so the codec that writes
// lives in the sqlite module, built on Writer.
var Codec = func() transcript.JSONL {
	c := Writer
	c.Encode = nil
	c.WriteHeader = nil
	c.After = nil
	return c
}()

// Writer reads Copilot sessions and writes their files, events.jsonl and
// workspace.yaml, but not the index. Alone it produces sessions Copilot will
// not load; the sqlite module's Copilot codec wraps it and adds the index.
var Writer = transcript.JSONL{
	Layout: transcript.Layout{
		Files:       files,
		PathFor:     pathFor,
		Peek:        peek,
		SessionRoot: filepath.Dir,
		Holders:     holders,
		Serving:     serving,
	},
	Header:      header,
	Decode:      decode,
	Encode:      encode,
	WriteHeader: writeHeader,
	Prepare:     prepareStart,
	After:       writeWorkspace,
	Tree:        true,
}

// Vendor is what a Copilot session carries in Session.Vendor: the
// session.start event's data, so a write reproduces the producer and
// version fields this codec does not model.
type Vendor struct {
	Start map[string]json.RawMessage
}

const root = ".copilot/session-state"

// event is Copilot's event envelope. Copilot rejects an envelope that lacks
// any of its fields ("Session file is corrupted: invalid session event
// envelope"), so every field is always written; the root event's parentId is
// null.
type event struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	ParentID  *string         `json:"parentId"`
}

func parent(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

type sessionStart struct {
	SessionID string   `json:"sessionId"`
	Version   int      `json:"version,omitempty"`
	Producer  string   `json:"producer,omitempty"`
	StartTime string   `json:"startTime"`
	Context   context_ `json:"context"`
}

// startIdentity is the part of session.start that names this session. The
// rest of the event's data is the same for every session a given Copilot
// writes, and comes from Vendor.
type startIdentity struct {
	SessionID string   `json:"sessionId"`
	StartTime string   `json:"startTime"`
	Context   context_ `json:"context"`
}

// defaultStart is session.start's fixed data as Copilot CLI 1.0.88 writes it,
// used when the store has no session of Copilot's own to copy from. Copilot
// rejects a session.start without copilotVersion ("Session file is corrupted
// (line 1: missing field copilotVersion)").
var defaultStart = map[string]json.RawMessage{
	"version":        json.RawMessage(`1`),
	"producer":       json.RawMessage(`"copilot-agent"`),
	"copilotVersion": json.RawMessage(`"1.0.88"`),
	"contextTier":    json.RawMessage(`null`),
	"alreadyInUse":   json.RawMessage(`false`),
}

// prepareStart gives a session with no session.start of its own the fixed
// fields of the newest session Copilot wrote in the same store, so the
// version stamp matches the Copilot that will load it.
func prepareStart(home string, s *transcript.Session) error {
	if v, ok := s.Vendor.(*Vendor); ok && len(v.Start) > 0 {
		return nil
	}
	start := map[string]json.RawMessage{}
	for k, val := range defaultStart {
		start[k] = val
	}
	paths, err := files(home, "")
	if err != nil {
		return err
	}
	var newest string
	var newestTime time.Time
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(newestTime) {
			newest, newestTime = p, fi.ModTime()
		}
	}
	if newest != "" {
		_, _ = transcript.PeekFirstLine(newest, func(row json.RawMessage) (transcript.Info, error) {
			var ev event
			if json.Unmarshal(row, &ev) != nil || ev.Type != "session.start" {
				return transcript.Info{}, nil
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(ev.Data, &fields) == nil {
				for k, val := range fields {
					switch k {
					case "sessionId", "startTime", "context":
					default:
						start[k] = val
					}
				}
			}
			return transcript.Info{}, nil
		})
	}
	s.Vendor = &Vendor{Start: start}
	return nil
}

type context_ struct {
	CWD string `json:"cwd"`
}

type userMessage struct {
	Content   string `json:"content"`
	MessageID string `json:"messageId"`
}

type assistantMessage struct {
	MessageID    string        `json:"messageId"`
	Content      string        `json:"content"`
	Model        string        `json:"model,omitempty"`
	ToolRequests []toolRequest `json:"toolRequests"`
}

type toolRequest struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

type toolResult struct {
	ToolCallID string `json:"toolCallId"`
	Success    bool   `json:"success"`
	Result     struct {
		Content string `json:"content"`
	} `json:"result"`
}

func files(home, cwd string) ([]string, error) {
	return transcript.Glob(filepath.Join(home, root, "*", "events.jsonl"))
}

// holders reads the in-use markers Copilot keeps in a session's directory
// while a process has the session open: inuse.<pid>.hold, one per process.
// Measured on Copilot CLI 1.0.88, which holds the file open for as long as
// it serves the session.
func holders(path string) []int {
	marks, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "inuse.*.hold"))
	var pids []int
	for _, m := range marks {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "inuse."), ".hold")
		if pid, err := strconv.Atoi(name); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// serving reads every session's in-use markers as pid to session id.
func serving(home string) map[int]string {
	out := map[int]string{}
	marks, _ := filepath.Glob(filepath.Join(home, root, "*", "inuse.*.hold"))
	for _, m := range marks {
		for _, pid := range holders(filepath.Join(filepath.Dir(m), "events.jsonl")) {
			out[pid] = filepath.Base(filepath.Dir(m))
		}
	}
	return out
}

func pathFor(home string, s *transcript.Session) string {
	return filepath.Join(home, root, s.ID, "events.jsonl")
}

func peek(path string) (transcript.Info, error) {
	return transcript.PeekFirstLine(path, func(raw json.RawMessage) (transcript.Info, error) {
		var ev event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return transcript.Info{}, err
		}
		if ev.Type != "session.start" {
			return transcript.Info{}, fmt.Errorf("%s: first event is %q", path, ev.Type)
		}
		var st sessionStart
		if err := json.Unmarshal(ev.Data, &st); err != nil {
			return transcript.Info{}, err
		}
		in := transcript.Info{ID: st.SessionID, CWD: st.Context.CWD}
		if t, err := time.Parse(time.RFC3339Nano, st.StartTime); err == nil {
			in.Updated = t
		}
		return in, nil
	})
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return false, err
	}
	if ev.Type != "session.start" {
		return false, nil
	}
	var st sessionStart
	if err := json.Unmarshal(ev.Data, &st); err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &fields); err != nil {
		return false, err
	}
	s.ID = st.SessionID
	s.CWD = st.Context.CWD
	if t, err := time.Parse(time.RFC3339Nano, st.StartTime); err == nil {
		s.Created = t
	}
	s.Vendor = &Vendor{Start: fields}
	// The header is a node in the chain too, so it is returned through
	// decode rather than swallowed here.
	return false, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return transcript.Entry{}, false, err
	}
	if ev.ID == "" {
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: ev.ID, Role: transcript.RoleOpaque}
	if ev.ParentID != nil {
		e.ParentID = *ev.ParentID
	}
	if ev.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, ev.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: timestamp: %w", ev.ID, err)
		}
		e.Time = t
	}
	switch ev.Type {
	case "user.message":
		var m userMessage
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: %w", ev.ID, err)
		}
		e.Role = transcript.RoleUser
		e.Content = []transcript.Block{{Kind: transcript.BlockText, Text: m.Content}}
	case "assistant.message":
		var m assistantMessage
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: %w", ev.ID, err)
		}
		e.Role = transcript.RoleAssistant
		if m.Content != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: m.Content})
		}
		for _, tr := range m.ToolRequests {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tr.ToolCallID, Name: tr.Name, Input: tr.Arguments})
		}
	case "tool.execution_complete":
		var tr toolResult
		if err := json.Unmarshal(ev.Data, &tr); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: %w", ev.ID, err)
		}
		st := transcript.StatusOK
		if !tr.Success {
			st = transcript.StatusError
		}
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: tr.ToolCallID, Text: tr.Result.Content, Status: st}}
	}
	return e, true, nil
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Start {
			fields[k] = val
		}
	}
	identity, err := json.Marshal(startIdentity{SessionID: s.ID, StartTime: stamp(s.Created), Context: context_{CWD: s.CWD}})
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
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	// The header is the root of the chain: the first message's parent.
	headID := transcript.NewUUID()
	for i := range s.Entries {
		if s.Entries[i].Role != transcript.RoleOpaque {
			if s.Entries[i].ParentID == "" {
				s.Entries[i].ParentID = headID
			}
			break
		}
	}
	h, err := json.Marshal(event{Type: "session.start", Data: data, ID: headID, Timestamp: stamp(s.Created)})
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
	ev := event{ID: e.ID, ParentID: parent(e.ParentID), Timestamp: stamp(t)}
	var data any
	switch e.Role {
	case transcript.RoleUser, transcript.RoleSystem:
		ev.Type = "user.message"
		data = userMessage{Content: e.Text(), MessageID: e.ID}
	case transcript.RoleAssistant:
		ev.Type = "assistant.message"
		m := assistantMessage{MessageID: e.ID, Content: e.Text(), ToolRequests: []toolRequest{}}
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolUse {
				continue
			}
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			m.ToolRequests = append(m.ToolRequests, toolRequest{ToolCallID: b.ToolID, Name: b.Name, Arguments: args})
		}
		data = m
	case transcript.RoleTool:
		ev.Type = "tool.execution_complete"
		var tr toolResult
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			tr.ToolCallID = b.ToolID
			tr.Success = b.Status != transcript.StatusError
			tr.Result.Content = b.Text
		}
		data = tr
	default:
		return nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	ev.Data = raw
	return json.Marshal(ev)
}

// writeWorkspace writes workspace.yaml beside events.jsonl. The file is a
// flat key: value document, so it is emitted directly rather than through
// a YAML dependency.
func writeWorkspace(ctx context.Context, path string, s *transcript.Session) error {
	name := s.Title
	if name == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			name = msgs[0].Text()
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", s.ID)
	fmt.Fprintf(&b, "cwd: %s\n", s.CWD)
	fmt.Fprintf(&b, "client_name: agentprotocol\n")
	fmt.Fprintf(&b, "name: %s\n", yamlString(name))
	fmt.Fprintf(&b, "user_named: false\n")
	fmt.Fprintf(&b, "summary_count: 0\n")
	fmt.Fprintf(&b, "fork_count: 0\n")
	fmt.Fprintf(&b, "created_at: %s\n", stamp(s.Created))
	fmt.Fprintf(&b, "updated_at: %s\n", stamp(s.Updated))
	return os.WriteFile(filepath.Join(filepath.Dir(path), "workspace.yaml"), []byte(b.String()), 0o644)
}

// yamlString quotes a scalar when YAML would otherwise misread it.
func yamlString(s string) string {
	if s == "" || strings.ContainsAny(s, ":#\n\"'{}[]&*!|>%@`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		q, _ := json.Marshal(s)
		return string(q)
	}
	return s
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
