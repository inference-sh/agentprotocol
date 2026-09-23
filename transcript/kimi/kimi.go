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
//
// Kimi keeps two views of a conversation in the wire log. The context rows,
// context.append_message and context.append_loop_event, are what it rebuilds
// the model's context from on resume. The agent.message.appended rows are the
// transcript it shows. A written session needs both: with only the transcript
// rows, kimi resumes it and the model sees none of it.
package kimi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	IDs:         transcript.IDScheme{New: messageID, Valid: validMessageID},
	Decode:      decode,
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
	AgentID string          `json:"agentId,omitempty"`
	TurnID  *int            `json:"turnId,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Event   json.RawMessage `json:"event,omitempty"`
	Time    int64           `json:"time,omitempty"`
	Kind    string          `json:"kind,omitempty"`
}

// appended is agent.message.appended's message field: the message wrapped
// with its provenance.
type appended struct {
	Message message `json:"message"`
	Meta    meta    `json:"meta"`
}

type message struct {
	Role       string     `json:"role"`
	Content    []part     `json:"content"`
	ToolCalls  []toolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
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

// Write plans the session's new entries and writes them with the rows kimi
// rebuilds the model's context from as well as its transcript rows.
func (st *store) Write(ctx context.Context, s *transcript.Session) (string, error) {
	// Ids first: the plan names entries by id.
	transcript.AssignIDs(s, files.IDs, false)
	w := files
	w.Encode = newPlan(s).encoder()
	ws, err := w.Open(st.home)
	if err != nil {
		return "", err
	}
	return ws.Write(ctx, s)
}

// messageID mints an id in kimi's form: msg_ and a 26-character ULID-style
// string.
func messageID() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [26]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "msg_" + string(b[:])
}

// validMessageID reports whether id is in kimi's form.
func validMessageID(id string) bool {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	rest, ok := strings.CutPrefix(id, "msg_")
	if !ok || len(rest) != 26 {
		return false
	}
	for _, c := range rest {
		if !strings.ContainsRune(alphabet, c) {
			return false
		}
	}
	return true
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

// decode reads the transcript rows, which hold the whole conversation in
// order: user and assistant messages, and each tool result as a tool message.
// The context rows describe the same conversation for the model and are kept
// opaque.
func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.Type != "agent.message.appended" {
		return transcript.Entry{}, false, nil
	}
	var a appended
	if err := json.Unmarshal(r.Message, &a); err != nil {
		return transcript.Entry{}, false, fmt.Errorf("agent.message.appended: %w", err)
	}
	e := transcript.Entry{ID: a.Meta.MessageID}
	if r.Time > 0 {
		e.Time = time.UnixMilli(r.Time).UTC()
	}
	switch a.Message.Role {
	case "user":
		e.Role = transcript.RoleUser
	case "assistant":
		e.Role = transcript.RoleAssistant
	case "tool":
		e.Role = transcript.RoleTool
		var text strings.Builder
		for _, p := range a.Message.Content {
			text.WriteString(p.Text)
		}
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: a.Message.ToolCallID, Text: text.String(), Status: transcript.StatusOK}}
		return e, true, nil
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

// plan is what writing a session's new entries needs to know across them:
// the turn and step each entry belongs to, and the uuid of each tool call
// event, which the result that answers it points at.
type plan struct {
	turn map[string]int // entry id -> turn index
	step map[string]int // entry id -> step within the turn
	last map[string]bool
	call map[string]string // tool call id -> tool.call event uuid
}

// newPlan numbers the session's new entries (those without Raw). Turns
// continue from the user turns already in the log; each assistant entry is
// one step of its turn.
func newPlan(s *transcript.Session) plan {
	p := plan{turn: map[string]int{}, step: map[string]int{}, last: map[string]bool{}, call: map[string]string{}}
	turn, step := -1, 0
	var prev string
	for _, e := range s.Messages() {
		if e.Role == transcript.RoleUser {
			if prev != "" {
				p.last[prev] = true
			}
			turn++
			step = 0
		}
		if e.Raw != nil {
			continue
		}
		p.turn[e.ID] = max(turn, 0)
		if e.Role == transcript.RoleAssistant {
			step++
			for _, b := range e.Content {
				if b.Kind == transcript.BlockToolUse {
					p.call[b.ToolID] = transcript.NewUUID()
				}
			}
		}
		p.step[e.ID] = step
		prev = e.ID
	}
	if prev != "" {
		p.last[prev] = true
	}
	return p
}

// encoder returns the Encode for one write: it emits, for each new entry, the
// context rows the model's context is rebuilt from and the transcript row
// kimi shows, and closes a turn after its last entry.
func (p plan) encoder() func(transcript.Entry, *transcript.Session) (json.RawMessage, error) {
	return func(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
		t := e.Time
		if t.IsZero() {
			t = s.Updated
		}
		ms := t.UnixMilli()
		turn := p.turn[e.ID]
		turnID := strconv.Itoa(turn)
		var rows []any
		switch e.Role {
		case transcript.RoleUser, transcript.RoleSystem:
			content := textParts(e)
			rows = append(rows,
				row{Type: "context.append_message", AgentID: "main", Time: ms, Message: mustJSON(contextMessage{
					Role: "user", Content: content, ID: e.ID, ToolCalls: []toolCall{}, Origin: origin{Kind: "user"},
				})},
				row{Type: "agent.message.appended", Kind: "event", Time: ms, Message: mustJSON(appended{
					Message: message{Role: "user", Content: content, ToolCalls: []toolCall{}},
					Meta:    meta{Source: "input", MessageID: e.ID},
				})})
		case transcript.RoleAssistant:
			stepUUID := transcript.NewUUID()
			step := p.step[e.ID]
			rows = append(rows, loopRow(ms, stepEvent{Type: "step.begin", UUID: stepUUID, TurnID: turnID, Step: step}))
			var calls []toolCall
			finish := "end_turn"
			for _, b := range e.Content {
				switch b.Kind {
				case transcript.BlockText:
					rows = append(rows, loopRow(ms, contentPart{Type: "content.part", UUID: transcript.NewUUID(), TurnID: turnID, Step: step, StepUUID: stepUUID, Part: part{Type: "text", Text: b.Text}}))
				case transcript.BlockToolUse:
					args := b.Input
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					rows = append(rows, loopRow(ms, toolCallEvent{Type: "tool.call", UUID: p.call[b.ToolID], TurnID: turnID, Step: step, StepUUID: stepUUID, ToolCallID: b.ToolID, Name: b.Name, Args: args}))
					calls = append(calls, toolCall{Type: "function", ID: b.ToolID, Name: b.Name, Arguments: string(args)})
					finish = "tool_use"
				}
			}
			rows = append(rows, loopRow(ms, stepEvent{Type: "step.end", UUID: stepUUID, TurnID: turnID, Step: step, FinishReason: finish}))
			if calls == nil {
				calls = []toolCall{}
			}
			rows = append(rows, row{Type: "agent.message.appended", Kind: "event", Time: ms, Message: mustJSON(appended{
				Message: message{Role: "assistant", Content: textParts(e), ToolCalls: calls},
				Meta:    meta{Source: "llm", MessageID: e.ID},
			})})
		case transcript.RoleTool:
			for _, b := range e.Content {
				if b.Kind != transcript.BlockToolResult {
					continue
				}
				ev := toolResultEvent{Type: "tool.result", ParentUUID: p.call[b.ToolID], ToolCallID: b.ToolID}
				ev.Result.Output = b.Text
				if b.Status == transcript.StatusError {
					ev.Result.Error = b.Text
				}
				rows = append(rows, loopRow(ms, ev), row{Type: "agent.message.appended", Kind: "event", Time: ms, Message: mustJSON(appended{
					Message: message{Role: "tool", Content: []part{{Type: "text", Text: b.Text}}, ToolCallID: b.ToolID},
					Meta:    meta{Source: "tool"},
				})})
			}
		default:
			return nil, nil
		}
		if p.last[e.ID] {
			rows = append(rows, row{Type: "turn.ended", AgentID: "main", TurnID: &turn, Reason: "completed", Time: ms})
		}
		var b strings.Builder
		for i, r := range rows {
			line, err := json.Marshal(r)
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
}

// The context-row payloads, as kimi writes them.
type contextMessage struct {
	Role      string     `json:"role"`
	Content   []part     `json:"content"`
	ID        string     `json:"id,omitempty"`
	ToolCalls []toolCall `json:"toolCalls"`
	Origin    origin     `json:"origin"`
}

type origin struct {
	Kind string `json:"kind"`
}

type stepEvent struct {
	Type         string `json:"type"`
	UUID         string `json:"uuid"`
	TurnID       string `json:"turnId"`
	Step         int    `json:"step"`
	FinishReason string `json:"finishReason,omitempty"`
}

type contentPart struct {
	Type     string `json:"type"`
	UUID     string `json:"uuid"`
	TurnID   string `json:"turnId"`
	Step     int    `json:"step"`
	StepUUID string `json:"stepUuid"`
	Part     part   `json:"part"`
}

type toolCallEvent struct {
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	TurnID     string          `json:"turnId"`
	Step       int             `json:"step"`
	StepUUID   string          `json:"stepUuid"`
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
}

type toolResultEvent struct {
	Type       string `json:"type"`
	ParentUUID string `json:"parentUuid"`
	ToolCallID string `json:"toolCallId"`
	Result     struct {
		Output string `json:"output"`
		Error  string `json:"error,omitempty"`
	} `json:"result"`
}

func loopRow(ms int64, event any) row {
	return row{Type: "context.append_loop_event", AgentID: "main", Event: mustJSON(event), Time: ms}
}

func textParts(e transcript.Entry) []part {
	out := []part{}
	for _, b := range e.Content {
		if b.Kind == transcript.BlockText {
			out = append(out, part{Type: "text", Text: b.Text})
		}
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("kimi: marshal " + err.Error())
	}
	return b
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
