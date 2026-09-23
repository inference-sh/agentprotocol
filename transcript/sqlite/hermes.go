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
	q := "SELECT id, COALESCE(cwd, ''), COALESCE(title, ''), COALESCE(ended_at, '') FROM sessions"
	args := []any{}
	if cwd != "" {
		q += " WHERE cwd = ?"
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
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	s := &transcript.Session{ID: id, Agent: "hermes"}
	var cwd, title, start, updated sql.NullString
	err = db.QueryRowContext(ctx, "SELECT cwd, title, started_at, ended_at FROM sessions WHERE id = ?", id).
		Scan(&cwd, &title, &start, &updated)
	if err == sql.ErrNoRows {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hermes: read session: %w", err)
	}
	s.CWD, s.Title = cwd.String, title.String
	s.Created, s.Updated = parseTime(start.String), parseTime(updated.String)

	rows, err := db.QueryContext(ctx, "SELECT role, COALESCE(content, ''), COALESCE(tool_call_id, ''), COALESCE(tool_name, ''), tool_calls, COALESCE(reasoning, ''), timestamp FROM messages WHERE session_id = ? ORDER BY id", id)
	if err != nil {
		return nil, fmt.Errorf("hermes: read messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var role, content, toolID, toolName, reasoning string
		var toolCalls sql.NullString
		var ts sql.NullFloat64
		if err := rows.Scan(&role, &content, &toolID, &toolName, &toolCalls, &reasoning, &ts); err != nil {
			return nil, err
		}
		e, err := hermesEntry(role, content, toolID, toolName, toolCalls.String, reasoning)
		if err != nil {
			return nil, err
		}
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

func (st *hermesStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.ID == "" {
		s.ID = transcript.NewUUID()
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
	if err := hermesSchema(ctx, db); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO sessions (id, cwd, title, started_at, ended_at) VALUES (?, ?, ?, ?, ?)",
		s.ID, s.CWD, title, created.UTC().Format(time.RFC3339Nano), updated.UTC().Format(time.RFC3339Nano)); err != nil {
		return "", fmt.Errorf("hermes: write session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", s.ID); err != nil {
		return "", err
	}
	for _, e := range s.Messages() {
		role, content, toolID, toolName, calls, reasoning := hermesColumns(e)
		ts := float64(updated.Unix())
		if !e.Time.IsZero() {
			ts = float64(e.Time.Unix())
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, reasoning) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			s.ID, role, content, toolID, calls, toolName, ts, reasoning); err != nil {
			return "", fmt.Errorf("hermes: write message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.ID, nil
}

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
  id TEXT PRIMARY KEY, source TEXT, model TEXT, system_prompt TEXT,
  started_at TEXT, ended_at TEXT, title TEXT, cwd TEXT);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT,
  timestamp REAL NOT NULL, token_count INTEGER, finish_reason TEXT, reasoning TEXT);`
	_, err := db.ExecContext(ctx, ddl)
	return err
}
