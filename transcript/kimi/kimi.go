// Package kimi reads and writes Kimi Code CLI sessions.
//
// Kimi keeps a directory per session under
// ~/.kimi-code/sessions/wd_<name>_<hash>/session_<id>, where <name> is the
// last element of the working directory and <hash> the first twelve hex
// digits of its SHA-256. agents/main/wire.jsonl is the conversation as a
// wire log; state.json beside it is the session's identity; and
// ~/.kimi-code/session_index.jsonl lists every session.
//
// The wire log mixes the conversation with telemetry and bookkeeping. Kimi
// rebuilds both the model's context and the transcript it shows from the
// context rows alone: context.append_message carries a whole message, and
// context.append_loop_event carries the pieces of an assistant step
// (step.begin, content.part, tool.call, step.end) and the tool results that
// answer it. context.apply_compaction and context.clear replace the history,
// and an undo moves the conversation to a new branch with agent.switched.
// The agent.message.appended rows are the journal of kimi's input state
// machine and are read as opaque. A written session carries both.
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
	Agent: "kimi",
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
		// The session directory, three levels above agents/main/wire.jsonl.
		SessionRoot: func(path string) string { return filepath.Dir(filepath.Dir(filepath.Dir(path))) },
	},
	Header:      header,
	IDs:         transcript.IDScheme{New: messageID, Valid: validMessageID},
	Decode:      decode,
	Finish:      finish,
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
	Role    string `json:"role"`
	Content []part `json:"content"`
	// ToolCalls is written even when empty: kimi's input state machine
	// rejects an assistant row without it (assistantMessageSchema), and the
	// other roles ignore it.
	ToolCalls  []toolCall `json:"toolCalls"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}

type part struct {
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	Think     string  `json:"think,omitempty"`
	Encrypted *string `json:"encrypted,omitempty"`
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
	// Usage is required on an assistant entry: kimi's input state machine
	// rejects an assistant agent.message.appended row without it
	// (human/agent/historySchema.ts assistantMetaSchema).
	Usage *usage `json:"usage,omitempty"`
}

type usage struct {
	InputOther         int `json:"inputOther"`
	Output             int `json:"output"`
	InputCacheRead     int `json:"inputCacheRead"`
	InputCacheCreation int `json:"inputCacheCreation"`
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

// Write plans the session's new entries and writes them as the context rows
// kimi rebuilds the conversation from, with the journal rows of its input
// state machine beside them.
func (st *store) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.Agent != "kimi" {
		s = s.Portable()
	}
	// Ids first: the plan names entries by id. A session read with links
	// (a message kimi delivers out of file order, see link) has its new
	// entries linked on from the last one delivered, so the session stays
	// whole in memory; the rows carry no links either way.
	tree := false
	for _, e := range s.Entries {
		tree = tree || e.ParentID != ""
	}
	transcript.AssignIDs(s, files.IDs, tree)
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

// decode maps a row that is one message on its own: a context message, the
// step.begin an assistant message is folded onto, a tool result. finish
// fills in the rest from the rows around them.
func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r wireRow
	if json.Unmarshal(raw, &r) != nil {
		return transcript.Entry{}, false, nil
	}
	var e transcript.Entry
	switch r.Type {
	case "context.append_message":
		m, ok := appendedMessage(r)
		if !ok {
			return transcript.Entry{}, false, nil
		}
		e = messageEntry(m)
	case "context.append_loop_event":
		var ev loopEvent
		if json.Unmarshal(r.Event, &ev) != nil {
			return transcript.Entry{}, false, nil
		}
		switch ev.Type {
		case "step.begin":
			e = transcript.Entry{ID: ev.UUID, Role: transcript.RoleAssistant}
		case "tool.result":
			e = transcript.Entry{Role: transcript.RoleTool, Content: []transcript.Block{toolResult(ev.ToolCallID, outputText(ev.Result.Output), ev.Result.IsError)}}
		default:
			return transcript.Entry{}, false, nil
		}
	default:
		return transcript.Entry{}, false, nil
	}
	if e.Role == transcript.RoleOpaque {
		return transcript.Entry{}, false, nil
	}
	if ms, ok := number(r.Time); ok && ms > 0 {
		e.Time = time.UnixMilli(int64(ms)).UTC()
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
// the turn and step each entry belongs to, the uuid of each step and tool
// call event, and which entry closes each step.
//
// Kimi records a step's tool results before its step.end (loop/machine
// tools.ts). The order matters on resume: step.end settles the step, and the
// fold answers every call still waiting with "Tool execution was
// interrupted" and ignores a result that arrives later
// (loopEventFold.ts settleOpen). So a step whose calls are answered ends
// after the last entry that answers them.
type plan struct {
	turn  map[string]int // entry id -> turn index
	step  map[string]int // entry id -> step within the turn
	last  map[string]bool
	call  map[string]string   // tool call id -> tool.call event uuid
	uuid  map[string]string   // assistant entry id -> step uuid
	calls map[string]bool     // assistant entry id -> it calls tools
	ends  map[string][]string // entry id -> assistant entries whose step.end follows it
}

// newPlan numbers the session's new entries (those without Raw). Turns
// continue from the turns already in the log; each assistant entry is one
// step of its turn.
func newPlan(s *transcript.Session) plan {
	p := plan{turn: map[string]int{}, step: map[string]int{}, last: map[string]bool{}, call: map[string]string{}, uuid: map[string]string{}, calls: map[string]bool{}, ends: map[string][]string{}}
	turn, step := -1, 0
	for _, e := range s.Entries {
		if e.Raw == nil {
			continue
		}
		var r struct {
			Type   string `json:"type"`
			TurnID *int   `json:"turnId"`
		}
		if json.Unmarshal(e.Raw, &r) == nil && r.TurnID != nil && (r.Type == "turn.prompt" || r.Type == "turn.ended") {
			turn = max(turn, *r.TurnID)
		}
	}
	var prev string
	owner := map[string]string{} // tool call id -> assistant entry id
	closer := map[string]string{}
	var steps []string
	for _, e := range s.Entries {
		if e.Raw != nil || e.Role == transcript.RoleOpaque {
			continue
		}
		if e.Role == transcript.RoleUser {
			if prev != "" {
				p.last[prev] = true
			}
			turn++
			step = 0
		}
		p.turn[e.ID] = max(turn, 0)
		switch e.Role {
		case transcript.RoleAssistant:
			step++
			p.uuid[e.ID] = transcript.NewUUID()
			closer[e.ID] = e.ID
			steps = append(steps, e.ID)
			for _, b := range e.Content {
				if b.Kind == transcript.BlockToolUse {
					p.call[b.ToolID] = transcript.NewUUID()
					p.calls[e.ID] = true
					owner[b.ToolID] = e.ID
				}
			}
		case transcript.RoleTool:
			for _, b := range e.Content {
				if a, ok := owner[b.ToolID]; ok && b.Kind == transcript.BlockToolResult {
					closer[a] = e.ID
				}
			}
		}
		p.step[e.ID] = step
		prev = e.ID
	}
	if prev != "" {
		p.last[prev] = true
	}
	for _, a := range steps {
		p.ends[closer[a]] = append(p.ends[closer[a]], a)
	}
	return p
}

// encoder returns the Encode for one write: it emits, for each new entry, the
// context rows the model's context is rebuilt from and the journal row kimi's
// input state machine keeps, and closes a turn after its last entry.
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
			stepUUID := p.uuid[e.ID]
			step := p.step[e.ID]
			rows = append(rows, loopRow(ms, stepEvent{Type: "step.begin", UUID: stepUUID, TurnID: turnID, Step: step}))
			var calls []toolCall
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
				}
			}
			if calls == nil {
				calls = []toolCall{}
			}
			rows = append(rows, row{Type: "agent.message.appended", Kind: "event", Time: ms, Message: mustJSON(appended{
				Message: message{Role: "assistant", Content: textParts(e), ToolCalls: calls},
				Meta:    meta{Source: "llm", MessageID: e.ID, Usage: &usage{}},
			})})
		case transcript.RoleTool:
			for _, b := range e.Content {
				if b.Kind != transcript.BlockToolResult {
					continue
				}
				ev := toolResultEvent{Type: "tool.result", ParentUUID: p.call[b.ToolID], ToolCallID: b.ToolID}
				ev.Result.Output = b.Text
				ev.Result.IsError = b.Status == transcript.StatusError
				rows = append(rows, loopRow(ms, ev), row{Type: "agent.message.appended", Kind: "event", Time: ms, Message: mustJSON(appended{
					Message: message{Role: "tool", Content: []part{{Type: "text", Text: b.Text}}, ToolCalls: []toolCall{}, ToolCallID: b.ToolID},
					Meta:    meta{Source: "tool"},
				})})
			}
		default:
			return nil, nil
		}
		for _, a := range p.ends[e.ID] {
			reason := "end_turn"
			if p.calls[a] {
				reason = "tool_use"
			}
			rows = append(rows, loopRow(ms, stepEvent{Type: "step.end", UUID: p.uuid[a], TurnID: turnID, Step: p.step[a], FinishReason: reason}))
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

// origin is where a context message came from (PromptOrigin in
// agent/contextMemory/types.ts), as far as the reader needs it.
type origin struct {
	Kind          string `json:"kind"`
	Variant       string `json:"variant,omitempty"`
	OwnerPromptID string `json:"ownerPromptId,omitempty"`
	Trigger       string `json:"trigger,omitempty"`
	InTurn        bool   `json:"inTurn,omitempty"`
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
		Output  string `json:"output"`
		IsError bool   `json:"isError,omitempty"`
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
	if err := addToIndex(filepath.Join(dir, "..", "..", "..", "session_index.jsonl"), indexRow{SessionID: s.ID, SessionDir: dir, WorkDir: s.CWD}); err != nil {
		return err
	}
	return markDirty(filepath.Join(dir, "..", ".."), s.ID)
}

// markDirty tells kimi its cached session list is stale. Kimi serves
// /sessions from a cache it trusts while no mark is present and no workspace
// directory's mtime moved (sessionIndexService.ts manifestFresh), and
// rewriting a session that already exists moves neither. A mark is an empty
// file sessions/.index-dirty/<id>.<ms> (sessionIndexDirtyJournal.ts:11-18);
// kimi clears the marks when it rebuilds the list.
func markDirty(sessions, id string) error {
	dir := filepath.Join(sessions, ".index-dirty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+"."+strconv.FormatInt(time.Now().UnixMilli(), 10)), nil, 0o644)
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
