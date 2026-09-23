package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Role     string   `json:"role"`
	ParentID string   `json:"parentID,omitempty"`
	Time     openTime `json:"time"`
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

// openVendor keeps the raw message and part payloads of a session read from
// the store, so a same-agent write reproduces the fields this codec does not
// model.
type openVendor struct {
	Messages map[string]json.RawMessage // message id -> data
	Version  string
	Slug     string
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
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	s := &transcript.Session{ID: id}
	var created, updated int64
	var version, slug sql.NullString
	err = db.QueryRowContext(ctx, "SELECT directory, title, version, slug, time_created, time_updated FROM session WHERE id = ?", id).
		Scan(&s.CWD, &s.Title, &version, &slug, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("opencode: read session: %w", err)
	}
	s.Created, s.Updated = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	vendor := &openVendor{Messages: map[string]json.RawMessage{}, Version: version.String, Slug: slug.String}
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
		vendor.Messages[msgID] = json.RawMessage(data)
		e := transcript.Entry{ID: msgID, ParentID: m.ParentID, Role: transcript.Role(m.Role)}
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

func (st *openStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.ID == "" {
		s.ID = "ses_" + transcript.NewUUID()
	}
	now := time.Now()
	created := now
	if !s.Created.IsZero() {
		created = s.Created
	}
	updated := now
	if !s.Updated.IsZero() {
		updated = s.Updated
	}
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

	projectID, err := ensureProject(ctx, tx, s.CWD, created, updated)
	if err != nil {
		return "", err
	}
	vendor, _ := s.Vendor.(*openVendor)
	version, slug := "0.0.0", "session"
	if vendor != nil {
		if vendor.Version != "" {
			version = vendor.Version
		}
		if vendor.Slug != "" {
			slug = vendor.Slug
		}
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO session (id, project_id, slug, directory, title, version, cost, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0, 0, 0, ?, ?)",
		s.ID, projectID, slug, s.CWD, title, version, created.UnixMilli(), updated.UnixMilli()); err != nil {
		return "", fmt.Errorf("opencode: write session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM part WHERE session_id = ?", s.ID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM message WHERE session_id = ?", s.ID); err != nil {
		return "", err
	}

	results := toolResults(s)
	for i, e := range s.Messages() {
		if e.Role == transcript.RoleTool {
			// A standalone tool-result entry is folded into the assistant
			// tool part it answers, so it writes no message of its own.
			continue
		}
		msgID := e.ID
		if msgID == "" {
			msgID = fmt.Sprintf("msg_%s%03d", s.ID, i)
		}
		mt := created
		if !e.Time.IsZero() {
			mt = e.Time
		}
		data, err := openMessageData(e, msgID, vendor)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)",
			msgID, s.ID, mt.UnixMilli(), mt.UnixMilli(), string(data)); err != nil {
			return "", fmt.Errorf("opencode: write message: %w", err)
		}
		for j, part := range openParts(e, results) {
			partID := fmt.Sprintf("prt_%s%03d%03d", s.ID, i, j)
			pd, err := json.Marshal(part)
			if err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)",
				partID, msgID, s.ID, mt.UnixMilli(), mt.UnixMilli(), string(pd)); err != nil {
				return "", fmt.Errorf("opencode: write part: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.ID, nil
}

// ensureProject returns the id of the project for a worktree, creating one
// when the directory is not yet known. opencode identifies a project by its
// worktree, so a fresh id with the right worktree is enough for it to bind
// the session to the directory.
func ensureProject(ctx context.Context, tx *sql.Tx, dir string, created, updated time.Time) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM project WHERE worktree = ?", dir).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = transcript.NewUUID()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO project (id, worktree, vcs, time_created, time_updated, sandboxes) VALUES (?, ?, 'git', ?, ?, '[]')",
		id, dir, created.UnixMilli(), updated.UnixMilli()); err != nil {
		return "", fmt.Errorf("opencode: write project: %w", err)
	}
	return id, nil
}

// openMessageData reproduces the original message payload for a same-agent
// session, or builds a minimal one for an imported entry.
func openMessageData(e transcript.Entry, msgID string, vendor *openVendor) (json.RawMessage, error) {
	if vendor != nil {
		if raw, ok := vendor.Messages[msgID]; ok {
			return raw, nil
		}
	}
	m := map[string]any{"role": openRole(e.Role), "time": map[string]int64{"created": timeMillis(e.Time)}}
	if e.ParentID != "" {
		m["parentID"] = e.ParentID
	}
	return json.Marshal(m)
}

// openRole maps a tool entry to the assistant role opencode files tool parts
// under.
func openRole(r transcript.Role) string {
	if r == transcript.RoleTool {
		return string(transcript.RoleAssistant)
	}
	return string(r)
}

// toolResult is a tool call's outcome, collected across a whole session so a
// call and its result can be folded into one opencode part even when the
// normalized model keeps them in separate entries.
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

// openParts turns an entry's blocks into opencode parts, folding each tool
// call together with its result into the single completed tool part opencode
// stores.
func openParts(e transcript.Entry, results map[string]toolResult) []openPart {
	var parts []openPart
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			parts = append(parts, openPart{Type: "text", Text: b.Text})
		case transcript.BlockReasoning:
			parts = append(parts, openPart{Type: "reasoning", Text: b.Text})
		case transcript.BlockToolUse:
			state := &openToolState{Status: "completed", Input: b.Input}
			if r, ok := results[b.ToolID]; ok {
				state.Output = r.text
				if r.isErr {
					state.Status, state.Error = "error", r.text
				}
			}
			parts = append(parts, openPart{Type: "tool", Tool: b.Name, CallID: b.ToolID, State: state})
		}
	}
	return parts
}

func openSchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS project (
  id TEXT PRIMARY KEY, worktree TEXT NOT NULL, vcs TEXT, name TEXT,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, sandboxes TEXT NOT NULL DEFAULT '[]');
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

func timeMillis(t time.Time) int64 {
	if t.IsZero() {
		return time.Now().UnixMilli()
	}
	return t.UnixMilli()
}
