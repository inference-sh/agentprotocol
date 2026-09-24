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

func init() { transcript.Register("hermes", Hermes) }

// Hermes is the hermes session store. hermes keeps every session in
// ~/.hermes/state.db, with a sessions table and a messages table whose tool
// calls and reasoning are separate columns.
var Hermes transcript.Codec = hermesCodec{}

const hermesPath = ".hermes/state.db"

type hermesCodec struct{}

func (hermesCodec) Open(home string) (transcript.Store, error) {
	return &hermesStore{path: filepath.Join(home, hermesPath)}, nil
}

type hermesStore struct{ path string }

type hermesToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (st *hermesStore) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
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
	// hermes often leaves the cwd column empty and records the directory in
	// model_config instead.
	cwdExpr, err := hermesCWD(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("hermes: list: %w", err)
	}
	q := "SELECT id, " + cwdExpr + ", COALESCE(title, ''), COALESCE(ended_at, started_at, '') FROM sessions"
	args := []any{}
	if cwd != "" {
		q += " WHERE " + cwdExpr + " = ?"
		args = append(args, cwd)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("hermes: list: %w", err)
	}
	defer rows.Close()
	var out []transcript.Info
	for rows.Next() {
		var id, dir, title, updated string
		if err := rows.Scan(&id, &dir, &title, &updated); err != nil {
			return nil, err
		}
		out = append(out, transcript.Info{ID: id, CWD: dir, Title: title, Updated: parseTime(updated), Path: st.path})
	}
	transcript.SortNewest(out)
	return out, rows.Err()
}

func (st *hermesStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	if _, err := os.Stat(st.path); errors.Is(err, os.ErrNotExist) {
		return nil, transcript.ErrNotFound
	}
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	s := &transcript.Session{ID: id, Agent: "hermes"}
	var cwd, title, start, updated, source, model sql.NullString
	cwdExpr, err := hermesCWD(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("hermes: read session: %w", err)
	}
	err = db.QueryRowContext(ctx, "SELECT "+cwdExpr+", title, started_at, ended_at, source, model FROM sessions WHERE id = ?", id).
		Scan(&cwd, &title, &start, &updated, &source, &model)
	if err == sql.ErrNoRows {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hermes: read session: %w", err)
	}
	s.CWD, s.Title, s.Model = cwd.String, title.String, model.String
	s.Created, s.Updated = parseTime(start.String), parseTime(updated.String)
	if s.Updated.IsZero() {
		s.Updated = s.Created
	}
	s.Vendor = &hermesVendor{Source: source.String}

	rows, err := db.QueryContext(ctx, "SELECT id, role, COALESCE(content, ''), COALESCE(tool_call_id, ''), COALESCE(tool_name, ''), tool_calls, COALESCE(reasoning, ''), timestamp FROM messages WHERE session_id = ? ORDER BY id", id)
	if err != nil {
		return nil, fmt.Errorf("hermes: read messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		var role, content, toolID, toolName, reasoning string
		var toolCalls sql.NullString
		var ts sql.NullFloat64
		if err := rows.Scan(&rowID, &role, &content, &toolID, &toolName, &toolCalls, &reasoning, &ts); err != nil {
			return nil, err
		}
		e, err := hermesEntry(role, content, toolID, toolName, toolCalls.String, reasoning)
		if err != nil {
			return nil, err
		}
		// Raw marks the entry as read; a rewrite keeps its row as is.
		e.ID = "hermes-row:" + strconv.FormatInt(rowID, 10)
		e.Raw = json.RawMessage(strconv.Quote(content))
		if ts.Valid {
			e.Time = time.Unix(int64(ts.Float64), 0).UTC()
		}
		s.Entries = append(s.Entries, e)
	}
	return s, rows.Err()
}

func hermesEntry(role, content, toolID, toolName, toolCalls, reasoning string) (transcript.Entry, error) {
	e := transcript.Entry{Role: transcript.Role(role)}
	switch role {
	case "tool":
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: toolID, Name: toolName, Text: content, Status: transcript.StatusOK}}
		return e, nil
	case "assistant":
		if reasoning != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: reasoning})
		}
		if content != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: content})
		}
		if toolCalls != "" {
			var calls []hermesToolCall
			if err := json.Unmarshal([]byte(toolCalls), &calls); err != nil {
				return transcript.Entry{}, fmt.Errorf("hermes: tool_calls: %w", err)
			}
			for _, c := range calls {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: c.ID, Name: c.Function.Name, Input: arguments(c.Function.Arguments)})
			}
		}
	default:
		e.Role = transcript.RoleUser
		if content != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: content})
		}
	}
	return e, nil
}

func arguments(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

// hermesCWD is the SQL for a session's directory: the cwd column, or the cwd
// hermes records inside model_config when it leaves the column empty. Older
// hermes databases have no cwd column, so the expression names only the
// columns the sessions table has.
func hermesCWD(ctx context.Context, q queryer) (string, error) {
	cols, err := columns(ctx, q, "sessions")
	if err != nil {
		return "", err
	}
	var parts []string
	if cols["cwd"] {
		parts = append(parts, "NULLIF(cwd, '')")
	}
	if cols["model_config"] {
		parts = append(parts, "json_extract(model_config, '$.cwd')")
	}
	return "COALESCE(" + strings.Join(append(parts, "''"), ", ") + ")", nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// columns lists a table's columns.
func columns(ctx context.Context, q queryer, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// hermesVendor carries the source a session was opened from, which hermes
// requires on every session row.
type hermesVendor struct{ Source string }

// Write persists a session without disturbing anything it read. Rows of
// entries read from the store (Raw set) are kept exactly; rows of entries the
// session no longer holds are removed; new entries are inserted after them.
// A session hermes does not have gets a sessions row with the columns hermes
// requires, source and started_at.
func (st *hermesStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.ID == "" {
		s.ID = transcript.NewUUID()
	}
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
	if err := hermesSchema(ctx, db); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var one int
	exists := true
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", s.ID).Scan(&one); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		exists = false
	}
	if !exists {
		// hermes's ACP adapter restores a session only when its source is
		// "acp"; for any other source, session/resume silently starts an
		// empty session instead (acp_adapter/session.py _restore). Its CLI
		// resume does not check the source, so "acp" serves both.
		source := "acp"
		if v, ok := s.Vendor.(*hermesVendor); ok && v.Source != "" {
			source = v.Source
		}
		// The ACP adapter takes the session's directory from model_config.
		modelConfig, err := json.Marshal(map[string]string{"cwd": s.CWD})
		if err != nil {
			return "", err
		}
		title := s.Title
		if title == "" {
			if msgs := s.Messages(); len(msgs) > 0 {
				title = msgs[0].Text()
			}
		}
		// Older hermes databases have no cwd column; model_config carries
		// the directory in every version.
		cols, err := columns(ctx, tx, "sessions")
		if err != nil {
			return "", err
		}
		names := []string{"id", "source", "started_at", "title", "model", "model_config"}
		vals := []any{s.ID, source, epoch(s.Created), title, nullIfEmpty(s.Model), string(modelConfig)}
		if cols["cwd"] {
			names, vals = append(names, "cwd"), append(vals, s.CWD)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO sessions ("+strings.Join(names, ", ")+") VALUES (?"+strings.Repeat(", ?", len(names)-1)+")",
			vals...); err != nil {
			return "", fmt.Errorf("hermes: write session: %w", err)
		}
	}

	keep := map[string]bool{}
	for _, e := range s.Messages() {
		if e.Raw != nil {
			keep[strings.TrimPrefix(e.ID, "hermes-row:")] = true
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
		if !keep[strconv.FormatInt(id, 10)] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", id); err != nil {
			return "", fmt.Errorf("hermes: remove message: %w", err)
		}
	}

	at := now
	added := false
	for _, e := range s.Messages() {
		if e.Raw != nil {
			continue
		}
		role, content, toolID, toolName, calls, reasoning := hermesColumns(e)
		ts := e.Time
		if ts.IsZero() {
			at = at.Add(time.Millisecond)
			ts = at
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, reasoning) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			s.ID, role, content, nullIfEmpty(toolID), calls, nullIfEmpty(toolName), epoch(ts), reasoning); err != nil {
			return "", fmt.Errorf("hermes: write message: %w", err)
		}
		added = true
	}
	if added {
		if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET message_count = (SELECT count(*) FROM messages WHERE session_id = ?) WHERE id = ?", s.ID, s.ID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.ID, nil
}

// epoch is the fractional unix seconds hermes stores its times as.
func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func hermesColumns(e transcript.Entry) (role, content, toolID, toolName string, toolCalls, reasoning any) {
	switch e.Role {
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolResult {
				return "tool", b.Text, b.ToolID, b.Name, nil, nil
			}
		}
		return "tool", "", "", "", nil, nil
	case transcript.RoleAssistant:
		var text, reason string
		var calls []hermesToolCall
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				text += b.Text
			case transcript.BlockReasoning:
				reason += b.Text
			case transcript.BlockToolUse:
				var c hermesToolCall
				c.ID, c.Type = b.ToolID, "function"
				c.Function.Name = b.Name
				c.Function.Arguments = string(b.Input)
				calls = append(calls, c)
			}
		}
		var callsCol any
		if len(calls) > 0 {
			raw, _ := json.Marshal(calls)
			callsCol = string(raw)
		}
		var reasonCol any
		if reason != "" {
			reasonCol = reason
		}
		return "assistant", text, "", "", callsCol, reasonCol
	default:
		return "user", e.Text(), "", "", nil, nil
	}
}

func hermesSchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY, source TEXT NOT NULL, model TEXT, model_config TEXT, system_prompt TEXT,
  started_at REAL NOT NULL, ended_at REAL, message_count INTEGER DEFAULT 0, title TEXT, cwd TEXT);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT,
  timestamp REAL NOT NULL, token_count INTEGER, finish_reason TEXT, reasoning TEXT);`
	_, err := db.ExecContext(ctx, ddl)
	return err
}
