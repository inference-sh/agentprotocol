package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

func init() {
	transcript.Register("opencode", Opencode)
	transcript.Register("kilo", Kilo)
}

// Opencode is the opencode session store, and Kilo the store of Kilo Code,
// which forked opencode's schema. Both keep every session in one database
// with session, message and part tables whose payloads are JSON in a `data`
// column; a session belongs to a project keyed by its worktree directory.
var (
	Opencode transcript.Codec = openCodec{".local/share/opencode/opencode.db"}
	Kilo     transcript.Codec = openCodec{".local/share/kilo/kilo.db"}
)

type openCodec struct{ rel string }

func (c openCodec) Open(home string) (transcript.Store, error) {
	return &openStore{path: filepath.Join(home, c.rel)}, nil
}

type openStore struct{ path string }

// openMessage is the message `data` payload. Only the fields the codec reads
// or writes are named; Vendor keeps the rest for a same-agent write.
type openMessage struct {
	Role       string   `json:"role"`
	ParentID   string   `json:"parentID,omitempty"`
	Time       openTime `json:"time"`
	Agent      string   `json:"agent,omitempty"`
	ProviderID string   `json:"providerID,omitempty"`
	ModelID    string   `json:"modelID,omitempty"`
}

type openTime struct {
	Created   int64 `json:"created,omitempty"`
	Completed int64 `json:"completed,omitempty"`
}

// openPart is the part `data` payload.
type openPart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	CallID string          `json:"callID,omitempty"`
	State  *openToolState  `json:"state,omitempty"`
	Raw    json.RawMessage `json:"-"`
}

type openToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// openVendor carries what a write needs from a session read from the store:
// the agent, provider and model its messages ran with, which a message
// appended to it must name too.
type openVendor struct {
	Agent      string
	ProviderID string
	ModelID    string
}

func (st *openStore) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
	if _, err := os.Stat(st.path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()
	q := "SELECT id, directory, title, time_updated FROM session"
	args := []any{}
	if cwd != "" {
		q += " WHERE directory = ?"
		args = append(args, cwd)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("opencode: list: %w", err)
	}
	defer rows.Close()
	var out []transcript.Info
	for rows.Next() {
		var id, dir, title string
		var updated int64
		if err := rows.Scan(&id, &dir, &title, &updated); err != nil {
			return nil, err
		}
		out = append(out, transcript.Info{ID: id, CWD: dir, Title: title, Updated: time.UnixMilli(updated).UTC(), Path: st.path})
	}
	transcript.SortNewest(out)
	return out, rows.Err()
}

func (st *openStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	if _, err := os.Stat(st.path); errors.Is(err, os.ErrNotExist) {
		return nil, transcript.ErrNotFound
	}
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	s := &transcript.Session{ID: id}
	var created, updated int64
	err = db.QueryRowContext(ctx, "SELECT directory, title, time_created, time_updated FROM session WHERE id = ?", id).
		Scan(&s.CWD, &s.Title, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("opencode: read session: %w", err)
	}
	s.Created, s.Updated = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	vendor := &openVendor{}
	s.Vendor = vendor

	// Parts, grouped by message.
	partRows, err := db.QueryContext(ctx, "SELECT message_id, data FROM part WHERE session_id = ? ORDER BY time_created, id", id)
	if err != nil {
		return nil, fmt.Errorf("opencode: read parts: %w", err)
	}
	defer partRows.Close()
	partsByMsg := map[string][]openPart{}
	order := map[string]int{}
	for partRows.Next() {
		var msgID, data string
		if err := partRows.Scan(&msgID, &data); err != nil {
			return nil, err
		}
		var p openPart
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, fmt.Errorf("opencode: part: %w", err)
		}
		p.Raw = json.RawMessage(data)
		partsByMsg[msgID] = append(partsByMsg[msgID], p)
		if _, ok := order[msgID]; !ok {
			order[msgID] = len(order)
		}
	}
	if err := partRows.Err(); err != nil {
		return nil, err
	}

	msgRows, err := db.QueryContext(ctx, "SELECT id, data FROM message WHERE session_id = ? ORDER BY time_created, id", id)
	if err != nil {
		return nil, fmt.Errorf("opencode: read messages: %w", err)
	}
	defer msgRows.Close()
	for msgRows.Next() {
		var msgID, data string
		if err := msgRows.Scan(&msgID, &data); err != nil {
			return nil, err
		}
		var m openMessage
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return nil, fmt.Errorf("opencode: message: %w", err)
		}
		if m.Agent != "" {
			vendor.Agent = m.Agent
		}
		if m.Role == "assistant" && m.ProviderID != "" {
			vendor.ProviderID, vendor.ModelID = m.ProviderID, m.ModelID
			s.Model = m.ModelID
		}
		// Raw marks the entry as read from the store: a write leaves its
		// message and part rows exactly as they are.
		e := transcript.Entry{ID: msgID, ParentID: m.ParentID, Role: transcript.Role(m.Role), Raw: json.RawMessage(data)}
		if m.Time.Created > 0 {
			e.Time = time.UnixMilli(m.Time.Created).UTC()
		}
		toolResults := 0
		for _, p := range partsByMsg[msgID] {
			switch p.Type {
			case "text":
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
			case "reasoning":
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
			case "tool":
				b := transcript.Block{Kind: transcript.BlockToolUse, ToolID: p.CallID, Name: p.Tool}
				if p.State != nil {
					b.Input = p.State.Input
					if p.State.Status == "completed" || p.State.Status == "error" {
						// A completed tool part carries both the call and its
						// result; split it so the result reads as a tool entry.
						toolResults++
						status := transcript.StatusOK
						text := p.State.Output
						if p.State.Status == "error" {
							status = transcript.StatusError
							if p.State.Error != "" {
								text = p.State.Error
							}
						}
						e.Content = append(e.Content, b, transcript.Block{Kind: transcript.BlockToolResult, ToolID: p.CallID, Name: p.Tool, Text: text, Status: status})
						continue
					}
				}
				e.Content = append(e.Content, b)
			}
		}
		s.Entries = append(s.Entries, e)
	}
	return s, msgRows.Err()
}

// Write persists a session without disturbing anything it read. Entries read
// from the store (Raw set) keep their message and part rows exactly; rows of
// entries the session no longer holds are removed; new entries are inserted
// after them as messages opencode accepts, with every field its message and
// part schemas require. A session opencode does not have yet gets a session
// row, bound to its directory's project.
func (st *openStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return "", err
	}
	db, err := openRW(st.path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := openSchema(ctx, db); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	ids := openIDs{}
	now := time.Now()
	if s.Created.IsZero() {
		s.Created = now
	}
	exists := false
	if s.ID != "" {
		var one int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM session WHERE id = ?", s.ID).Scan(&one); {
		case err == nil:
			exists = true
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	} else {
		s.ID = ids.next("ses", now, true)
	}
	if !exists {
		if err := insertOpenSession(ctx, tx, s); err != nil {
			return "", err
		}
	}

	// Keep what was read; drop rows for entries the session no longer has.
	keep := map[string]bool{}
	var lastUser string
	for _, e := range s.Messages() {
		if e.Raw != nil {
			keep[e.ID] = true
			if e.Role == transcript.RoleUser {
				lastUser = e.ID
			}
		}
	}
	stale, err := openMessageIDs(ctx, tx, s.ID)
	if err != nil {
		return "", err
	}
	for _, id := range stale {
		if keep[id] {
			continue
		}
		for _, q := range []string{"DELETE FROM part WHERE message_id = ?", "DELETE FROM message WHERE id = ?"} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return "", fmt.Errorf("opencode: remove message: %w", err)
			}
		}
	}

	d, err := openDefaults(ctx, tx, s)
	if err != nil {
		return "", err
	}
	results := toolResults(s)
	at := now
	added := false
	for _, e := range s.Messages() {
		if e.Raw != nil || e.Role == transcript.RoleTool || e.Role == transcript.RoleSystem {
			// Read rows stay; tool results fold into the assistant part that
			// called them; opencode has no system messages.
			continue
		}
		at = at.Add(time.Millisecond)
		ms := at.UnixMilli()
		msgID := ids.next("msg", at, false)
		var data any
		switch e.Role {
		case transcript.RoleUser:
			data = openUser{Role: "user", Time: openTime{Created: ms}, Agent: d.agent, Model: openModelRef{ProviderID: d.provider, ModelID: d.model}}
			lastUser = msgID
		case transcript.RoleAssistant:
			data = openAssistant{
				ParentID: lastUser, Role: "assistant", Mode: d.agent, Agent: d.agent,
				Path: openPath{CWD: s.CWD, Root: s.CWD}, Tokens: openTokens{Cache: openCache{}},
				ModelID: d.model, ProviderID: d.provider, Time: openTime{Created: ms, Completed: ms}, Finish: "stop",
			}
		default:
			continue
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)",
			msgID, s.ID, ms, ms, string(raw)); err != nil {
			return "", fmt.Errorf("opencode: write message: %w", err)
		}
		for _, part := range openParts(e, results, ms) {
			pd, err := json.Marshal(part)
			if err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)",
				ids.next("prt", at, false), msgID, s.ID, ms, ms, string(pd)); err != nil {
				return "", fmt.Errorf("opencode: write part: %w", err)
			}
		}
		added = true
	}
	if added && exists {
		if _, err := tx.ExecContext(ctx, "UPDATE session SET time_updated = ? WHERE id = ?", at.UnixMilli(), s.ID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.ID, nil
}

// The message and part payloads opencode validates on load. The id,
// sessionID and messageID it also requires come from the row's columns.
type openUser struct {
	Role  string       `json:"role"`
	Time  openTime     `json:"time"`
	Agent string       `json:"agent"`
	Model openModelRef `json:"model"`
}

type openModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

type openAssistant struct {
	ParentID   string     `json:"parentID"`
	Role       string     `json:"role"`
	Mode       string     `json:"mode"`
	Agent      string     `json:"agent"`
	Path       openPath   `json:"path"`
	Cost       float64    `json:"cost"`
	Tokens     openTokens `json:"tokens"`
	ModelID    string     `json:"modelID"`
	ProviderID string     `json:"providerID"`
	Time       openTime   `json:"time"`
	Finish     string     `json:"finish,omitempty"`
}

type openPath struct {
	CWD  string `json:"cwd"`
	Root string `json:"root"`
}

type openTokens struct {
	Input     float64   `json:"input"`
	Output    float64   `json:"output"`
	Reasoning float64   `json:"reasoning"`
	Cache     openCache `json:"cache"`
}

type openCache struct {
	Read  float64 `json:"read"`
	Write float64 `json:"write"`
}

type openSpan struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type openPartOut struct {
	Type   string        `json:"type"`
	Text   *string       `json:"text,omitempty"`
	Time   *openSpan     `json:"time,omitempty"`
	CallID string        `json:"callID,omitempty"`
	Tool   string        `json:"tool,omitempty"`
	State  *openStateOut `json:"state,omitempty"`
}

type openStateOut struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   *string         `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
	Title    *string         `json:"title,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Time     openSpan        `json:"time"`
}

// openDefaultsT is who new messages say ran them.
type openDefaultsT struct{ agent, provider, model string }

// openDefaults picks the agent, provider and model new messages name: those
// of the session being appended to, else those of the store's latest
// assistant message, else the session's Model split as provider/model.
func openDefaults(ctx context.Context, tx *sql.Tx, s *transcript.Session) (openDefaultsT, error) {
	d := openDefaultsT{agent: "build"}
	if v, ok := s.Vendor.(*openVendor); ok && v.ProviderID != "" {
		if v.Agent != "" {
			d.agent = v.Agent
		}
		d.provider, d.model = v.ProviderID, v.ModelID
		return d, nil
	}
	var data string
	err := tx.QueryRowContext(ctx, `SELECT data FROM message WHERE json_extract(data, '$.role') = 'assistant' ORDER BY time_created DESC LIMIT 1`).Scan(&data)
	if err == nil {
		var m openMessage
		if json.Unmarshal([]byte(data), &m) == nil && m.ProviderID != "" {
			if m.Agent != "" {
				d.agent = m.Agent
			}
			d.provider, d.model = m.ProviderID, m.ModelID
			return d, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	d.model = s.Model
	if i := strings.Index(s.Model, "/"); i > 0 {
		d.provider = s.Model[:i]
	}
	return d, nil
}

func openMessageIDs(ctx context.Context, tx *sql.Tx, sessionID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM message WHERE session_id = ?", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// insertOpenSession adds a session row for a session opencode does not have,
// bound to the project of its directory the way opencode binds its own.
func insertOpenSession(ctx context.Context, tx *sql.Tx, s *transcript.Session) error {
	created := s.Created.UnixMilli()
	updated := created
	if !s.Updated.IsZero() {
		updated = s.Updated.UnixMilli()
	}
	projectID, err := ensureProject(ctx, tx, s.CWD, created)
	if err != nil {
		return err
	}
	version := "0.0.0"
	if err := tx.QueryRowContext(ctx, "SELECT version FROM session ORDER BY time_updated DESC LIMIT 1").Scan(&version); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, projectID, "imported-session", s.CWD, title, version, created, updated); err != nil {
		return fmt.Errorf("opencode: write session: %w", err)
	}
	return nil
}

// ensureProject returns the project bound to a directory, creating the
// project and the binding when opencode has none. opencode resolves a
// directory to its project through project_directory where that table
// exists, and through the project's worktree otherwise.
func ensureProject(ctx context.Context, tx *sql.Tx, dir string, at int64) (string, error) {
	hasDirs, err := tableExists(ctx, tx, "project_directory")
	if err != nil {
		return "", err
	}
	var id string
	if hasDirs {
		err = tx.QueryRowContext(ctx, "SELECT project_id FROM project_directory WHERE directory = ?", dir).Scan(&id)
	} else {
		err = tx.QueryRowContext(ctx, "SELECT id FROM project WHERE worktree = ?", dir).Scan(&id)
	}
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, "SELECT id FROM project WHERE worktree = ?", dir).Scan(&id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		id = transcript.NewUUID()
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO project (id, worktree, vcs, time_created, time_updated, sandboxes) VALUES (?, ?, 'git', ?, ?, '[]')",
			id, dir, at, at); err != nil {
			return "", fmt.Errorf("opencode: write project: %w", err)
		}
	}
	if hasDirs {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO project_directory (project_id, directory, time_created) VALUES (?, ?, ?)",
			id, dir, at); err != nil {
			return "", fmt.Errorf("opencode: bind directory: %w", err)
		}
	}
	return id, nil
}

func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n)
	return n > 0, err
}

// toolResult is a tool call's outcome, collected across a whole session so a
// call and its result fold into one opencode part even when the normalized
// model keeps them in separate entries.
type toolResult struct {
	text  string
	isErr bool
}

func toolResults(s *transcript.Session) map[string]toolResult {
	out := map[string]toolResult{}
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolResult {
				out[b.ToolID] = toolResult{text: b.Text, isErr: b.Status == transcript.StatusError}
			}
		}
	}
	return out
}

// openParts turns an entry's blocks into the parts opencode stores, each
// with the fields its part schema requires. A tool call and its result fold
// into one tool part.
func openParts(e transcript.Entry, results map[string]toolResult, ms int64) []openPartOut {
	var parts []openPartOut
	span := openSpan{Start: ms, End: ms}
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			text := b.Text
			parts = append(parts, openPartOut{Type: "text", Text: &text})
		case transcript.BlockReasoning:
			text := b.Text
			parts = append(parts, openPartOut{Type: "reasoning", Text: &text, Time: &span})
		case transcript.BlockToolUse:
			input := b.Input
			if !isObject(input) {
				wrapped, _ := json.Marshal(map[string]json.RawMessage{"input": orNull(input)})
				input = wrapped
			}
			st := &openStateOut{Status: "completed", Input: input, Metadata: json.RawMessage(`{}`), Time: span}
			r, ok := results[b.ToolID]
			switch {
			case ok && r.isErr:
				st.Status, st.Error, st.Metadata = "error", r.text, nil
			default:
				out, title := r.text, b.Name
				st.Output, st.Title = &out, &title
			}
			parts = append(parts, openPartOut{Type: "tool", CallID: b.ToolID, Tool: b.Name, State: st})
		}
	}
	return parts
}

func isObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &m) == nil && m != nil
}

func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// openIDs mints ids the way opencode's Identifier does: a prefix, twelve hex
// digits of the millisecond time with a counter in the low bits (inverted for
// ids that sort newest first, as session ids do), and fourteen base62
// characters.
type openIDs struct{ counter uint64 }

func (g *openIDs) next(prefix string, at time.Time, descending bool) string {
	g.counter++
	v := (uint64(at.UnixMilli())<<12 | (g.counter & 0xfff)) & 0xffffffffffff
	if descending {
		v = ^v & 0xffffffffffff
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var rnd [14]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic(err)
	}
	for i := range rnd {
		rnd[i] = alphabet[int(rnd[i])%len(alphabet)]
	}
	return fmt.Sprintf("%s_%012x%s", prefix, v, rnd[:])
}

func openSchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS project (
  id TEXT PRIMARY KEY, worktree TEXT NOT NULL, vcs TEXT, name TEXT,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, sandboxes TEXT NOT NULL DEFAULT '[]');
CREATE TABLE IF NOT EXISTS project_directory (
  project_id TEXT NOT NULL, directory TEXT NOT NULL, type TEXT, strategy TEXT, time_created INTEGER NOT NULL,
  PRIMARY KEY (project_id, directory));
CREATE TABLE IF NOT EXISTS session (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, workspace_id TEXT, parent_id TEXT,
  slug TEXT NOT NULL, directory TEXT NOT NULL, path TEXT, title TEXT NOT NULL, version TEXT NOT NULL,
  cost REAL DEFAULT 0 NOT NULL, tokens_input INTEGER DEFAULT 0 NOT NULL, tokens_output INTEGER DEFAULT 0 NOT NULL,
  tokens_reasoning INTEGER DEFAULT 0 NOT NULL, tokens_cache_read INTEGER DEFAULT 0 NOT NULL,
  tokens_cache_write INTEGER DEFAULT 0 NOT NULL, agent TEXT, model TEXT,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS message (
  id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS part (
  id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);`
	_, err := db.ExecContext(ctx, ddl)
	return err
}
