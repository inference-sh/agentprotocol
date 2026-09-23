package sqlite

import (
	"context"
	"database/sql"
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

func init() { transcript.Register("goose", Goose) }

// Goose is the goose session store. goose keeps every session of every
// project in one database, ~/.local/share/goose/sessions/sessions.db, with a
// sessions table and a messages table whose content is a goose content
// array.
var Goose transcript.Codec = gooseCodec{}

const goosePath = ".local/share/goose/sessions/sessions.db"

type gooseCodec struct{}

func (gooseCodec) Open(home string) (transcript.Store, error) {
	return &gooseStore{path: filepath.Join(home, goosePath)}, nil
}

type gooseStore struct{ path string }

type gooseContent struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	ToolCall   *gooseToolCall  `json:"toolCall,omitempty"`
	ToolResult *gooseToolReslt `json:"toolResult,omitempty"`
}

type gooseToolCall struct {
	Status string `json:"status"`
	Value  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"value"`
}

type gooseToolReslt struct {
	Status string `json:"status"`
	Value  struct {
		Content []gooseContent `json:"content"`
	} `json:"value"`
}

func (st *gooseStore) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
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
	q := "SELECT id, working_dir, description, updated_at FROM sessions"
	args := []any{}
	if cwd != "" {
		q += " WHERE working_dir = ?"
		args = append(args, cwd)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("goose: list: %w", err)
	}
	defer rows.Close()
	var out []transcript.Info
	for rows.Next() {
		var id, dir, desc, updated string
		if err := rows.Scan(&id, &dir, &desc, &updated); err != nil {
			return nil, err
		}
		out = append(out, transcript.Info{ID: id, CWD: dir, Title: desc, Updated: parseTime(updated), Path: st.path})
	}
	transcript.SortNewest(out)
	return out, rows.Err()
}

func (st *gooseStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	if _, err := os.Stat(st.path); errors.Is(err, os.ErrNotExist) {
		return nil, transcript.ErrNotFound
	}
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	s := &transcript.Session{ID: id, Agent: "goose"}
	var created, updated string
	err = db.QueryRowContext(ctx, "SELECT working_dir, name, created_at, updated_at FROM sessions WHERE id = ?", id).
		Scan(&s.CWD, &s.Title, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("goose: read session: %w", err)
	}
	s.Created, s.Updated = parseTime(created), parseTime(updated)

	rows, err := db.QueryContext(ctx, "SELECT id, message_id, role, content_json, created_timestamp FROM messages WHERE session_id = ? ORDER BY id", id)
	if err != nil {
		return nil, fmt.Errorf("goose: read messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		var msgID sql.NullString
		var role, contentJSON string
		var ts int64
		if err := rows.Scan(&rowID, &msgID, &role, &contentJSON, &ts); err != nil {
			return nil, err
		}
		e, err := gooseEntry(role, contentJSON)
		if err != nil {
			return nil, err
		}
		// The row id identifies the row on a rewrite; message_id may repeat
		// or be empty. Raw marks the entry as read, so its row is kept as is.
		e.ID = gooseRowRef(rowID)
		e.Raw = json.RawMessage(contentJSON)
		if ts > 0 {
			e.Time = time.Unix(ts, 0).UTC()
		}
		s.Entries = append(s.Entries, e)
	}
	return s, rows.Err()
}

func gooseEntry(role, contentJSON string) (transcript.Entry, error) {
	var content []gooseContent
	if err := json.Unmarshal([]byte(contentJSON), &content); err != nil {
		return transcript.Entry{}, fmt.Errorf("goose: content: %w", err)
	}
	e := transcript.Entry{Role: transcript.Role(role)}
	toolResults := 0
	for _, c := range content {
		switch c.Type {
		case "text":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: c.Text})
		case "toolRequest":
			if c.ToolCall != nil {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: c.ID, Name: c.ToolCall.Value.Name, Input: c.ToolCall.Value.Arguments})
			}
		case "toolResponse":
			toolResults++
			var text strings.Builder
			status := transcript.StatusOK
			if c.ToolResult != nil {
				if c.ToolResult.Status != "success" {
					status = transcript.StatusError
				}
				for _, rc := range c.ToolResult.Value.Content {
					text.WriteString(rc.Text)
				}
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: c.ID, Text: text.String(), Status: status})
		}
	}
	if e.Role == transcript.RoleUser && toolResults > 0 && toolResults == len(content) {
		e.Role = transcript.RoleTool
	}
	return e, nil
}

// Write persists a session without disturbing anything it read. Rows of
// entries read from the store (Raw set) are kept exactly; rows of entries the
// session no longer holds are removed; new entries are inserted after them.
// A session goose does not have gets a sessions row, and a new id in goose's
// own scheme, YYYYMMDD_N with N one past the highest for the day, allocated
// inside the transaction so it can never land on a session goose already has.
func (st *gooseStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	now := time.Now()
	if s.Created.IsZero() {
		s.Created = now
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return "", err
	}
	db, err := openRW(st.path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := gooseSchema(ctx, db); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	exists := false
	if s.ID == "" {
		id, err := nextGooseID(ctx, tx, s.Created)
		if err != nil {
			return "", err
		}
		s.ID = id
	} else {
		var one int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", s.ID).Scan(&one); {
		case err == nil:
			exists = true
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}
	if !exists {
		title := s.Title
		if title == "" {
			if msgs := s.Messages(); len(msgs) > 0 {
				title = msgs[0].Text()
			}
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO sessions (id, description, session_type, working_dir) VALUES (?, ?, 'user', ?)",
			s.ID, title, s.CWD); err != nil {
			return "", fmt.Errorf("goose: write session: %w", err)
		}
	}

	keep := map[int64]bool{}
	for _, e := range s.Messages() {
		if e.Raw != nil {
			if id, ok := gooseRowID(e.ID); ok {
				keep[id] = true
			}
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM messages WHERE session_id = ?", s.ID)
	if err != nil {
		return "", err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", id); err != nil {
			return "", fmt.Errorf("goose: remove message: %w", err)
		}
	}

	added := false
	for _, e := range s.Messages() {
		if e.Raw != nil {
			continue
		}
		content, err := gooseContentJSON(e)
		if err != nil {
			return "", err
		}
		ts := now.Unix()
		if !e.Time.IsZero() {
			ts = e.Time.Unix()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (message_id, session_id, role, content_json, created_timestamp, metadata_json) VALUES (?, ?, ?, ?, ?, '{"userVisible":true,"agentVisible":true}')`,
			"msg_"+transcript.NewUUID(), s.ID, string(gooseRole(e.Role)), content, ts); err != nil {
			return "", fmt.Errorf("goose: write message: %w", err)
		}
		added = true
	}
	if added && exists {
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", s.ID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.ID, nil
}

func gooseRowRef(id int64) string { return "goose-row:" + strconv.FormatInt(id, 10) }

func gooseRowID(ref string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(ref, "goose-row:"), 10, 64)
	return n, err == nil && strings.HasPrefix(ref, "goose-row:")
}

// nextGooseID returns the first unused YYYYMMDD_N id for a day.
func nextGooseID(ctx context.Context, tx *sql.Tx, day time.Time) (string, error) {
	prefix := day.UTC().Format("20060102") + "_"
	rows, err := tx.QueryContext(ctx, "SELECT id FROM sessions WHERE id LIKE ?", prefix+"%")
	if err != nil {
		return "", fmt.Errorf("goose: allocate id: %w", err)
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(id, prefix)); err == nil && n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return prefix + strconv.Itoa(max+1), nil
}

// gooseRole maps a tool entry back to the user role goose files it under.
func gooseRole(r transcript.Role) transcript.Role {
	if r == transcript.RoleTool {
		return transcript.RoleUser
	}
	return r
}

func gooseContentJSON(e transcript.Entry) (string, error) {
	var content []gooseContent
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			content = append(content, gooseContent{Type: "text", Text: b.Text})
		case transcript.BlockToolUse:
			c := gooseContent{Type: "toolRequest", ID: b.ToolID, ToolCall: &gooseToolCall{Status: "success"}}
			c.ToolCall.Value.Name = b.Name
			c.ToolCall.Value.Arguments = b.Input
			if len(c.ToolCall.Value.Arguments) == 0 {
				c.ToolCall.Value.Arguments = json.RawMessage(`{}`)
			}
			content = append(content, c)
		case transcript.BlockToolResult:
			status := "success"
			if b.Status == transcript.StatusError {
				status = "error"
			}
			c := gooseContent{Type: "toolResponse", ID: b.ToolID, ToolResult: &gooseToolReslt{Status: status}}
			c.ToolResult.Value.Content = []gooseContent{{Type: "text", Text: b.Text}}
			content = append(content, c)
		}
	}
	if content == nil {
		content = []gooseContent{}
	}
	out, err := json.Marshal(content)
	return string(out), err
}

func gooseSchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
  user_set_name BOOLEAN DEFAULT FALSE, session_type TEXT NOT NULL DEFAULT 'user',
  working_dir TEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, extension_data TEXT DEFAULT '{}');
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT, session_id TEXT NOT NULL REFERENCES sessions(id),
  role TEXT NOT NULL, content_json TEXT NOT NULL, created_timestamp INTEGER NOT NULL,
  timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP, tokens INTEGER, metadata_json TEXT);`
	_, err := db.ExecContext(ctx, ddl)
	return err
}

// parseTime reads the timestamps goose and hermes store, which are SQLite
// datetime strings or unix seconds.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(int64(n), 0).UTC()
	}
	return time.Time{}
}
