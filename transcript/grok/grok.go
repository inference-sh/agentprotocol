// Package grok reads and writes Grok CLI sessions.
//
// Grok keeps a directory per session under
// ~/.grok/sessions/<percent-encoded cwd>/<id>, and two views of the
// conversation in it. chat_history.jsonl is what the model is given: grok
// rewrites it whenever the context changes, on compaction, rewind and
// resume. updates.jsonl is what the person sees: the append-only log of the
// updates grok sent its client, which it replays when a session is loaded
// and reads its prompt list from. summary.json is the session's identity
// and counts; events.jsonl is telemetry and is not read.
//
// A session holds both files' rows as entries, the updates rows first. The
// conversation is the chat_history rows: prompts and replies for everyone,
// injected context for the model. The updates rows repeat it, and stand for
// nothing more, except before a compaction: the history it retired is gone
// from chat_history and lives on only in updates.jsonl, where the person
// still sees it. Each file is written back from its own rows, and a new
// entry is written to both, as grok writes a turn.
package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Grok CLI session store.
var Codec transcript.Codec = codec{}

const (
	agent       = "grok"
	root        = ".grok/sessions"
	chatFile    = "chat_history.jsonl"
	updatesFile = "updates.jsonl"
)

// sessionFiles is the engine for one of a session's two JSONL files.
func sessionFiles(name string) transcript.Layout {
	return transcript.Layout{
		Files: func(home, cwd string) ([]string, error) {
			project := "*"
			if cwd != "" {
				project = transcript.EscapedCwd.Name(cwd)
			}
			return transcript.Glob(filepath.Join(home, root, project, "*", name))
		},
		PathFor: func(home string, s *transcript.Session) string {
			return filepath.Join(home, root, transcript.EscapedCwd.Name(s.CWD), s.ID, name)
		},
		Peek: peek,
		// grok holds its session directory and events.jsonl open while live.
		SessionRoot: filepath.Dir,
	}
}

// chat is chat_history.jsonl. Its Encode is set per write; see plan.
var chat = transcript.JSONL{
	Agent:  agent,
	Layout: sessionFiles(chatFile),
	Decode: decodeChat,
	Finish: finishChat,
}

// updates is updates.jsonl, which the codec reads through the engine and
// writes itself, since a new entry becomes several of its rows.
var updates = transcript.JSONL{
	Agent:  agent,
	Layout: sessionFiles(updatesFile),
	Decode: decodeUpdate,
	Finish: finishUpdates,
}

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

type codec struct{}

func (codec) Open(home string) (transcript.Store, error) {
	cs, err := chat.Open(home)
	if err != nil {
		return nil, err
	}
	us, err := updates.Open(home)
	if err != nil {
		return nil, err
	}
	return &store{Store: cs, updates: us, home: home}, nil
}

// store lists sessions by their chat_history.jsonl, the file grok requires
// to open one.
type store struct {
	transcript.Store
	updates transcript.Store
	home    string
}

// Read loads chat_history.jsonl, then updates.jsonl and the summary beside
// it, which the generic store does not know about. A session with no
// updates.jsonl has nothing grok would show: session/load replays nothing
// for it (load_updates_for_replay in storage/replay.rs).
func (st *store) Read(ctx context.Context, id string) (*transcript.Session, error) {
	s, err := st.Store.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	shown, err := st.updates.Read(ctx, id)
	switch {
	case err == nil:
		s.Entries = append(shown.Entries, s.Entries...)
	case !errors.Is(err, transcript.ErrNotFound):
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

// plan is what one write needs to continue the session the way grok would:
// the prompt index of each new prompt, the last event id in the log, and the
// compactions it records.
type plan struct {
	prompt map[string]int
	seq    int
	// meta marks the summary entries a compaction puts in chat_history.jsonl,
	// written as grok writes its own summary.
	meta map[string]bool
	// compactions holds, by the compaction entry's id, what the compaction
	// records.
	compactions map[string]*compaction
}

// compaction is a compaction another agent recorded, as grok records one
// (persist_compaction_checkpoint in session/compaction.rs): a checkpoint
// file holding the compacted history, and a compaction_checkpoint update
// naming it and the prompt index the next prompt takes.
type compaction struct {
	id      string
	prompt  int
	history []transcript.Entry
}

// newPlan numbers the new prompts on from the prompts grok already lists
// for the session. A new entry both sent and shown is a prompt when it is
// the person's; one only sent or only shown is context or an echo, which
// grok does not count.
func newPlan(s *transcript.Session, shown []json.RawMessage) *plan {
	p := &plan{prompt: map[string]int{}, meta: map[string]bool{}, compactions: map[string]*compaction{}}
	for _, raw := range shown {
		p.seq = max(p.seq, eventSeq(raw))
	}
	n := len(prompts(shown))
	for _, e := range s.Entries {
		switch {
		case e.Raw != nil:
		case e.Compaction != nil:
			p.compactions[e.ID] = &compaction{id: transcript.NewUUID(), prompt: n}
		case e.Role == transcript.RoleUser && e.Audience == transcript.AudienceAll:
			p.prompt[e.ID] = n
			n++
		}
	}
	return p
}

// caps is what a write records of another agent's session. A compaction is
// recorded as grok records its own: updates.jsonl keeps the history it
// retired, then the checkpoint, and chat_history.jsonl holds the compacted
// history in its place. An entry the model is not given goes to
// updates.jsonl alone, which grok replays to the person and never sends, as
// it does the echo of a command.
var caps = transcript.Capabilities{Compaction: true, UserOnly: true}

// Write writes chat_history.jsonl from the session's chat rows and the new
// entries the model is given, and updates.jsonl from its update rows and
// the new entries the person is shown, then the summary counting both.
func (st *store) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.Agent != agent {
		s = s.Portable().Lower(caps)
		s.Agent = agent
	}
	// The plan keys new entries by id; grok's rows carry none of them.
	assignIDs(s)
	if s.Updated.IsZero() {
		s.Updated = time.Now()
	}

	var shownRaw []json.RawMessage
	// A torn line is attributed to the file of the row before it: a crash
	// cuts a file's last line, and updates rows come first.
	inUpdates := true
	updateRow := make([]bool, len(s.Entries))
	for i, e := range s.Entries {
		if e.Raw == nil {
			continue
		}
		if json.Valid(e.Raw) {
			inUpdates = isUpdateRow(e.Raw)
		}
		if updateRow[i] = inUpdates; inUpdates {
			shownRaw = append(shownRaw, e.Raw)
		}
	}
	p := newPlan(s, shownRaw)

	model := *s
	model.Entries = nil
	var shown []transcript.Entry
	for i, e := range s.Entries {
		switch {
		case e.Raw != nil && updateRow[i]:
			shown = append(shown, e)
		case e.Raw != nil:
			model.Entries = append(model.Entries, e)
		case e.Compaction != nil:
			// grok compacts to the messages it keeps and then the summary
			// (build_compacted_history in xai-chat-state compaction_utils.rs).
			c := p.compactions[e.ID]
			for k, m := range model.Entries {
				if e.Compaction.Keep != "" && m.ID == e.Compaction.Keep {
					c.history = append(c.history, model.Entries[k:]...)
					break
				}
			}
			for _, m := range e.Compaction.Summary {
				m.ID = transcript.NewUUID()
				p.meta[m.ID] = m.Role == transcript.RoleUser
				c.history = append(c.history, m)
			}
			model.Entries = append([]transcript.Entry(nil), c.history...)
			shown = append(shown, e)
		default:
			if e.Audience.Model() {
				model.Entries = append(model.Entries, e)
			}
			if e.Audience.User() {
				shown = append(shown, e)
			}
		}
	}

	w := chat
	w.Encode = p.encodeChat
	ws, err := w.Open(st.home)
	if err != nil {
		return "", err
	}
	id, err := ws.Write(ctx, &model)
	if err != nil {
		return "", err
	}
	s.ID, s.Created, s.Updated = model.ID, model.Created, model.Updated
	dir := filepath.Join(st.home, root, transcript.EscapedCwd.Name(s.CWD), s.ID)

	var buf bytes.Buffer
	var numUpdates int
	for _, e := range shown {
		rows := []json.RawMessage{e.Raw}
		if e.Raw == nil {
			if rows, err = p.encodeUpdates(e, s); err != nil {
				return "", err
			}
		}
		for _, row := range rows {
			buf.Write(row)
			buf.WriteByte('\n')
			numUpdates++
		}
	}
	if numUpdates > 0 {
		if err := writeFile(filepath.Join(dir, updatesFile), buf.Bytes()); err != nil {
			return "", err
		}
	}
	for _, c := range p.compactions {
		if err := p.writeCheckpoint(dir, c, s); err != nil {
			return "", err
		}
	}
	chatRows, err := countRows(filepath.Join(dir, chatFile))
	if err != nil {
		return "", err
	}
	return id, writeSummary(dir, s, numUpdates, chatRows)
}

// assignIDs gives the session's new entries ids, and a compaction one if it
// has none: the plan keys both by id.
func assignIDs(s *transcript.Session) {
	transcript.AssignIDs(s, transcript.UUIDs, false)
	for i := range s.Entries {
		if e := &s.Entries[i]; e.Compaction != nil && e.Raw == nil && e.ID == "" {
			e.ID = transcript.NewUUID()
		}
	}
}

// checkpointFile is a compaction checkpoint file
// (CompactionCheckpointFile in extensions/notification.rs), which grok
// reads back when a rewind crosses the compaction.
type checkpointFile struct {
	CheckpointID     string            `json:"checkpoint_id"`
	PromptIndex      int               `json:"prompt_index_at_compaction"`
	History          []json.RawMessage `json:"compacted_history"`
	SchemaVersion    int               `json:"schema_version"`
	CreatedAt        string            `json:"created_at"`
	OriginalUserInfo *string           `json:"original_user_info"`
	RereadFilePaths  []string          `json:"reread_file_paths"`
}

func (c *compaction) file() string {
	return "compaction_checkpoints/" + c.id + ".json"
}

// writeCheckpoint writes a compaction's checkpoint file: the chat items of
// the history it compacted to.
func (p *plan) writeCheckpoint(dir string, c *compaction, s *transcript.Session) error {
	items := []json.RawMessage{}
	for _, e := range c.history {
		rows, err := p.encodeChat(e, s)
		if err != nil {
			return err
		}
		for _, line := range bytes.Split(rows, []byte("\n")) {
			if len(line) > 0 {
				items = append(items, line)
			}
		}
	}
	out, err := json.MarshalIndent(checkpointFile{CheckpointID: c.id, PromptIndex: c.prompt, History: items, SchemaVersion: 1,
		CreatedAt: s.Updated.UTC().Format(time.RFC3339Nano), RereadFilePaths: []string{}}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, c.file())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFile(path, out)
}

// countRows counts the items in a written chat_history.jsonl: its lines that
// parse, as grok counts num_chat_messages when it replaces the file.
func countRows(path string) (int, error) {
	n := 0
	err := transcript.EachLine(path, func(row json.RawMessage) (bool, error) {
		if json.Valid(row) {
			n++
		}
		return true, nil
	})
	return n, err
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

// writeSummary writes summary.json with the session's identity and the two
// files' counts: num_messages is the rows of updates.jsonl, and
// num_chat_messages the items of chat_history.jsonl (copy.rs and
// summary_write.rs in grok's storage).
func writeSummary(dir string, s *transcript.Session, numUpdates, numChat int) error {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Summary {
			fields[k] = val
		}
	}
	title := s.Title
	if title == "" {
		for _, e := range s.Linearize() {
			if e.Role == transcript.RoleUser && e.Text() != "" {
				title = e.Text()
				break
			}
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
		NumMessages:    numUpdates,
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
	fields["num_chat_messages"] = mustJSON(numChat)
	if _, ok := fields["chat_format_version"]; !ok {
		fields["chat_format_version"] = json.RawMessage("1")
	}
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "summary.json"), out, 0o644)
}
