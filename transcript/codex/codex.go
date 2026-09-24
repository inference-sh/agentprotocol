// Package codex reads and writes Codex CLI sessions.
//
// Codex keeps a rollout file per thread under
// ~/.codex/sessions/YYYY/MM/DD/rollout-<timestamp>-<id>.jsonl. The first row
// is session_meta; the conversation is the response_item rows, whose
// payloads are OpenAI Responses items: messages, reasoning, and tool calls
// and their outputs. A compacted row replaces the history before it, and a
// thread_rolled_back event drops the last turns; history.go reads both.
// Other event_msg rows, turn_context, world_state and token accounting are
// opaque.
//
// A thread in paginated history mode can span several files. thread/revert
// leaves the old rollout untouched and starts a new one for the same thread,
// named rollout-<timestamp>-<id>_<rollout id>.jsonl, whose session_meta
// history_base names the old file's rollout id and the point its history
// stops at; a paginated fork starts a file of its own that points at its
// parent the same way. Each file holds only the rows it added, and every row
// carries an ordinal that continues the sequence of the file it extends
// (rollout/src/ordinal.rs). Read follows history_base from the thread's
// newest file back to the first and joins the inherited prefixes in order;
// Write writes only that newest file.
//
// Codex also indexes rollouts in ~/.codex/state_5.sqlite. This codec does
// not write that index: resume by id falls back to the file name, and the
// resume picker and --last need the index row.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Codex session store.
var Codec transcript.Codec = codec{}

// files is the JSONL engine over single rollout files. The store reads a
// thread's files through it one at a time, joins them, and runs finish over
// the whole, since a compaction or rollback in one file acts on rows from
// the files before it.
var files = transcript.JSONL{
	Agent: "codex",
	Layout: transcript.Layout{
		Files:   rollouts,
		PathFor: pathFor,
		Peek:    peek,
	},
	Header:      header,
	Decode:      decode,
	Encode:      (&plan{}).encode,
	WriteHeader: (&plan{}).header,
}

// Vendor is what a Codex session carries in Session.Vendor.
type Vendor struct {
	// Meta is the newest file's session_meta payload as read, so a write
	// reproduces the originator, version, history mode and history base
	// this codec does not model.
	Meta map[string]json.RawMessage
	// File is the newest file's path relative to the home directory. A
	// reverted thread's file name carries a rollout id of its own, which a
	// write keeps.
	File string
	// Inherited are the earlier files whose prefixes the thread's history
	// starts with, oldest first. The session's first entries are their
	// rows, in this order, and a write leaves those files as they are.
	Inherited []Segment
}

// Segment is the part of an earlier rollout file a thread inherits.
type Segment struct {
	// File is the rollout's path relative to the home directory.
	File string
	// Rows is how many of the session's entries come from it: the file's
	// session_meta and its rows up to the recorded history position.
	Rows int
}

const (
	codexDir  = ".codex"
	root      = ".codex/sessions"
	archived  = ".codex/archived_sessions"
	fileStamp = "2006-01-02T15-04-05"

	historyPaginated = "paginated"
)

type row struct {
	Timestamp string          `json:"timestamp"`
	Ordinal   *uint64         `json:"ordinal,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID          string           `json:"id"`
	SessionID   string           `json:"session_id,omitempty"`
	Timestamp   string           `json:"timestamp"`
	CWD         string           `json:"cwd"`
	HistoryMode string           `json:"history_mode,omitempty"`
	HistoryBase *historyPosition `json:"history_base,omitempty"`
}

// historyPosition is SessionMeta.history_base: where in another rollout
// the history this file continues stops. ThreadID is that rollout's id,
// which is not the thread id after a revert (protocol.rs, HistoryPosition).
type historyPosition struct {
	ThreadID            string `json:"thread_id"`
	EndOrdinalExclusive uint64 `json:"end_ordinal_exclusive"`
	EndByteOffset       int64  `json:"end_byte_offset"`
}

type codec struct{}

func (codec) Open(home string) (transcript.Store, error) {
	st, err := files.Open(home)
	if err != nil {
		return nil, err
	}
	return &store{files: st, home: home}, nil
}

type store struct {
	files transcript.Store
	home  string
}

// List returns each thread once, by its newest file.
func (st *store) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
	infos, err := st.files.List(ctx, cwd)
	if err != nil {
		return nil, err
	}
	head := map[string]int{}
	var out []transcript.Info
	for _, in := range infos {
		i, ok := head[in.ID]
		if !ok {
			head[in.ID] = len(out)
			out = append(out, in)
			continue
		}
		if newer(in.Path, out[i].Path) {
			out[i] = in
		}
	}
	return out, nil
}

func (st *store) Read(ctx context.Context, id string) (*transcript.Session, error) {
	infos, err := st.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, in := range infos {
		if in.ID == id {
			s, err := st.readThread(ctx, in.Path)
			if err != nil {
				return nil, err
			}
			if err := finish(s); err != nil {
				return nil, fmt.Errorf("transcript: %s: %w", id, err)
			}
			return s, nil
		}
	}
	return nil, transcript.ErrNotFound
}

// readThread reads the file at path and the prefixes of the files its
// history_base chain names, the way rollout_lineage.rs resolves a thread's
// lineage: each earlier file contributes its rows up to the byte offset
// the later one recorded.
func (st *store) readThread(ctx context.Context, path string) (*transcript.Session, error) {
	s, err := st.readFile(ctx, path)
	if err != nil {
		return nil, err
	}
	v := vendorOf(s)
	v.File = st.rel(path)
	seen := map[string]bool{rolloutID(path): true}
	base := v.base()
	for base != nil {
		if seen[base.ThreadID] {
			return nil, fmt.Errorf("transcript: codex: history_base cycle through rollout %s", base.ThreadID)
		}
		seen[base.ThreadID] = true
		p, err := st.findRollout(base.ThreadID)
		if err != nil {
			return nil, err
		}
		prev, err := st.readFile(ctx, p)
		if err != nil {
			return nil, err
		}
		n, err := rowsBefore(p, base.EndByteOffset)
		if err != nil {
			return nil, err
		}
		if n > len(prev.Entries) {
			return nil, fmt.Errorf("transcript: codex: history_base of %s is past the end of %s", path, p)
		}
		s.Entries = append(prev.Entries[:n:n], s.Entries...)
		v.Inherited = append([]Segment{{File: st.rel(p), Rows: n}}, v.Inherited...)
		s.Created = prev.Created
		base = vendorOf(prev).base()
	}
	s.Vendor = v
	return s, nil
}

// readFile decodes one rollout file.
func (st *store) readFile(ctx context.Context, path string) (*transcript.Session, error) {
	in, err := peek(path)
	if err != nil {
		return nil, err
	}
	one := files
	one.Layout.Files = func(string, string) ([]string, error) { return []string{path}, nil }
	fs, err := one.Open(st.home)
	if err != nil {
		return nil, err
	}
	return fs.Read(ctx, in.ID)
}

// findRollout locates a rollout by its rollout id, in live or archived
// sessions (list.rs, find_rollout_path_by_rollout_id).
func (st *store) findRollout(id string) (string, error) {
	var found string
	for _, dir := range []string{root, archived} {
		err := filepath.WalkDir(filepath.Join(st.home, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				return err
			}
			if !d.IsDir() && rolloutID(p) == id {
				found = p
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("transcript: codex: rollout %s, which a thread's history starts from, is missing: %w", id, transcript.ErrNotFound)
}

func (st *store) rel(path string) string {
	r, err := filepath.Rel(st.home, path)
	if err != nil {
		return path
	}
	return r
}

// Write writes the thread's newest file. The files it inherits from are
// immutable once another rollout points into them, so they are written
// only where the destination lacks them, as when a thread is copied to
// another home, and then exactly as read.
func (st *store) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.Agent != files.Agent {
		s = s.Portable().Lower(transcript.Capabilities{})
	}
	v, _ := s.Vendor.(*Vendor)
	at := 0
	if v != nil {
		for _, seg := range v.Inherited {
			if at+seg.Rows > len(s.Entries) {
				return "", fmt.Errorf("transcript: codex: session has %d entries, fewer than the %s rows it inherits", len(s.Entries), seg.File)
			}
			if err := st.writeInherited(filepath.Join(st.home, seg.File), s.Entries[at:at+seg.Rows]); err != nil {
				return "", err
			}
			at += seg.Rows
		}
	}
	head := *s
	head.Entries = s.Entries[at:]
	p := newPlan(&head)
	w := files
	w.Encode = p.encode
	w.WriteHeader = p.header
	ws, err := w.Open(st.home)
	if err != nil {
		return "", err
	}
	return ws.Write(ctx, &head)
}

func (st *store) writeInherited(path string, rows []transcript.Entry) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var buf bytes.Buffer
	for _, e := range rows {
		if e.Raw == nil {
			return fmt.Errorf("transcript: codex: an entry inherited from %s has no row", path)
		}
		buf.Write(e.Raw)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rowsBefore counts the rows the engine decodes from the first off bytes
// of a file: its non-blank lines.
func rowsBefore(path string, off int64) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if off < 0 || off > int64(len(data)) {
		return 0, fmt.Errorf("transcript: codex: history_base offset %d is past the end of %s", off, path)
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data[:off]))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			n++
		}
	}
	return n, sc.Err()
}

func vendorOf(s *transcript.Session) *Vendor {
	if v, ok := s.Vendor.(*Vendor); ok {
		return v
	}
	return &Vendor{}
}

func (v *Vendor) meta() sessionMeta {
	var m sessionMeta
	if v == nil {
		return m
	}
	raw, err := json.Marshal(v.Meta)
	if err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

func (v *Vendor) base() *historyPosition { return v.meta().HistoryBase }

// rolloutName splits a canonical rollout file name,
// rollout-<timestamp>-<thread id>[_<rollout id>].jsonl
// (rollout_file_name.rs). A file without the suffix is its thread's
// original rollout, whose rollout id is the thread id.
func rolloutName(path string) (stamp, thread, rollout string, ok bool) {
	core, ok := strings.CutPrefix(filepath.Base(path), "rollout-")
	if !ok {
		return "", "", "", false
	}
	core, ok = strings.CutSuffix(core, ".jsonl")
	if !ok || len(core) < 21 || core[19] != '-' {
		return "", "", "", false
	}
	stamp, ids := core[:19], core[20:]
	thread, rollout, found := strings.Cut(ids, "_")
	if !found {
		rollout = thread
	}
	return stamp, thread, rollout, true
}

func rolloutID(path string) string {
	_, _, id, _ := rolloutName(path)
	return id
}

// newer reports whether rollout a is newer than rollout b the way Codex
// picks a thread's current file without its index: by the timestamp in the
// name, then by the UUIDv7 rollout id (list.rs,
// find_thread_path_by_id_from_filenames).
func newer(a, b string) bool {
	sa, _, ra, _ := rolloutName(a)
	sb, _, rb, _ := rolloutName(b)
	if sa != sb {
		return sa > sb
	}
	return strings.ToLower(ra) > strings.ToLower(rb)
}

func rollouts(home, cwd string) ([]string, error) {
	return transcript.Glob(filepath.Join(home, root, "*", "*", "*", "rollout-*.jsonl"))
}

func pathFor(home string, s *transcript.Session) string {
	if v, ok := s.Vendor.(*Vendor); ok && v.File != "" {
		if _, thread, _, ok := rolloutName(v.File); ok && thread == s.ID {
			return filepath.Join(home, v.File)
		}
	}
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
	var e transcript.Entry
	var ok bool
	var err error
	switch r.Type {
	case "response_item":
		e, _, ok, err = decodeItem(r.Payload)
	case "inter_agent_communication":
		e, ok, err = decodeCommunication(r.Payload)
	}
	if err != nil || !ok {
		return transcript.Entry{}, false, err
	}
	if t, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
		e.Time = t
	}
	return e, true, nil
}

// plan numbers the rows one write adds. Every row of a paginated rollout
// carries an ordinal one past the row before it; Codex refuses to append to
// a rollout whose last row has none, and its history projection skips such
// rows (rollout/src/ordinal.rs, ordinal_state_for_rollout;
// thread_history_materialization.rs). A legacy rollout has no ordinals.
type plan struct {
	paginated bool
	next      uint64
}

func newPlan(s *transcript.Session) *plan {
	p := &plan{}
	v, ok := s.Vendor.(*Vendor)
	if !ok {
		return p
	}
	m := v.meta()
	if m.HistoryMode != historyPaginated {
		return p
	}
	p.paginated = true
	// A rollout that continues another starts numbering where that one's
	// history stops (ordinal.rs, for_new_rollout).
	if m.HistoryBase != nil {
		p.next = m.HistoryBase.EndOrdinalExclusive
	}
	for _, e := range s.Entries {
		var r row
		if e.Raw != nil && json.Unmarshal(e.Raw, &r) == nil && r.Ordinal != nil && *r.Ordinal >= p.next {
			p.next = *r.Ordinal + 1
		}
	}
	return p
}

func (p *plan) ordinal() *uint64 {
	if !p.paginated {
		return nil
	}
	o := p.next
	p.next++
	return &o
}

func (p *plan) header(s *transcript.Session) ([]json.RawMessage, error) {
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
	// Codex rejects a session_meta without either as not being one.
	if _, ok := meta["originator"]; !ok {
		meta["originator"] = json.RawMessage(`"agentprotocol"`)
	}
	if _, ok := meta["cli_version"]; !ok {
		meta["cli_version"] = json.RawMessage(`""`)
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	h, err := json.Marshal(row{Timestamp: stamp(s.Created), Ordinal: p.ordinal(), Type: "session_meta", Payload: payload})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{h}, nil
}

func (p *plan) encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	if e.ID == "" {
		e.ID = transcript.NewUUID()
	}
	var items []any
	switch e.Role {
	case transcript.RoleUser, transcript.RoleSystem, transcript.RoleAssistant:
		var content []any
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
				content = append(content, part{Type: partType, Text: b.Text})
			case transcript.BlockImage:
				// Only a user message takes an input_image: Codex puts
				// what the person attaches there and nothing else
				// (models.rs, from_user_input). Files have no content
				// item at all.
				if u, ok := imageURL(b); ok && role == "user" {
					content = append(content, imagePart{Type: "input_image", ImageURL: u})
				}
			case transcript.BlockReasoning:
				items = append(items, reasoningItem{Type: "reasoning", ID: "rs_" + e.ID, Summary: []part{{Type: "summary_text", Text: b.Text}}})
			case transcript.BlockToolUse:
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				items = append(items, functionCall{Type: "function_call", ID: "fc_" + b.ToolID, Name: b.Name, Arguments: args, CallID: b.ToolID})
			}
		}
		if len(content) > 0 {
			items = append([]any{message{Type: "message", ID: "msg_" + e.ID, Role: role, Content: content}}, items...)
		}
	case transcript.RoleTool:
		// An image a tool returned goes into its output as an input_image
		// content item, as view_image's does (models.rs,
		// FunctionCallOutputContentItem).
		images := map[string][]any{}
		for _, b := range e.Content {
			if b.Kind == transcript.BlockImage && b.ToolID != "" {
				if u, ok := imageURL(b); ok {
					images[b.ToolID] = append(images[b.ToolID], imagePart{Type: "input_image", ImageURL: u})
				}
			}
		}
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			var body any = b.Text
			if imgs := images[b.ToolID]; len(imgs) > 0 {
				var content []any
				if b.Text != "" {
					content = append(content, part{Type: "input_text", Text: b.Text})
				}
				body = append(content, imgs...)
			}
			out, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			items = append(items, functionCallOutput{Type: "function_call_output", ID: "fco_" + b.ToolID, CallID: b.ToolID, Output: out})
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
		line, err := json.Marshal(row{Timestamp: stamp(t), Ordinal: p.ordinal(), Type: "response_item", Payload: payload})
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
