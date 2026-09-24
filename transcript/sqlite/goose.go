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
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	ID          string            `json:"id,omitempty"`
	ToolCall    *gooseToolCall    `json:"toolCall,omitempty"`
	ToolResult  *gooseToolReslt   `json:"toolResult,omitempty"`
	Annotations *gooseAnnotations `json:"annotations,omitempty"`
	// Thinking is a thinking block's text; Msg a system notification's;
	// Message an error's.
	Thinking string `json:"thinking,omitempty"`
	Msg      string `json:"msg,omitempty"`
	Message  string `json:"message,omitempty"`
}

// gooseAnnotations are the MCP annotations goose keeps on text, images and
// tool output. Audience limits who a block is for: "user", "assistant", or
// both when absent.
type gooseAnnotations struct {
	Audience []string `json:"audience,omitempty"`
}

// includes reports whether a block annotated so is for the given audience.
func (a *gooseAnnotations) includes(role string) bool {
	if a == nil || a.Audience == nil {
		return true
	}
	for _, r := range a.Audience {
		if r == role {
			return true
		}
	}
	return false
}

type gooseToolCall struct {
	Status string `json:"status"`
	Value  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"value"`
}

// gooseToolReslt is a tool call's outcome: status "success" with the MCP
// result as value, or "error" with the failure as error.
type gooseToolReslt struct {
	Status string `json:"status"`
	Value  struct {
		Content []gooseContent `json:"content"`
		IsError bool           `json:"isError,omitempty"`
	} `json:"value"`
	Error string `json:"error,omitempty"`
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

// Read loads a session in the order goose does, by created_timestamp and
// then row id (session_manager.rs get_conversation).
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

	cols, err := columns(ctx, db, "messages")
	if err != nil {
		return nil, fmt.Errorf("goose: read messages: %w", err)
	}
	metadata := "NULL"
	if cols["metadata_json"] {
		metadata = "metadata_json"
	}
	rows, err := db.QueryContext(ctx, "SELECT id, role, content_json, created_timestamp, "+metadata+" FROM messages WHERE session_id = ? ORDER BY created_timestamp, id", id)
	if err != nil {
		return nil, fmt.Errorf("goose: read messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		var role, contentJSON string
		var ts int64
		var meta sql.NullString
		if err := rows.Scan(&rowID, &role, &contentJSON, &ts, &meta); err != nil {
			return nil, err
		}
		e, err := gooseEntry(role, contentJSON, gooseVisibility(meta.String))
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	gooseCompactions(s)
	return s, nil
}

// gooseMeta is the part of a row's metadata_json that says who it is for.
type gooseMeta struct {
	UserVisible  *bool `json:"userVisible"`
	AgentVisible *bool `json:"agentVisible"`
}

// gooseVisibility reads a row's metadata. goose falls back to visible to
// both when the metadata is missing or does not parse, and both flags are
// required fields, so a row missing either gets that fallback too.
func gooseVisibility(meta string) gooseMeta {
	yes := true
	both := gooseMeta{UserVisible: &yes, AgentVisible: &yes}
	var m gooseMeta
	if meta == "" || json.Unmarshal([]byte(meta), &m) != nil || m.UserVisible == nil || m.AgentVisible == nil {
		return both
	}
	return m
}

// gooseEntry decodes a row. goose keeps a row's visibility in its metadata
// (userVisible, agentVisible), and a block's in its MCP audience annotation;
// the provider formats then drop what the model is never sent (system
// notifications, errors, confirmation requests). The
// entry's Content is what the model was given when it was given the row,
// else what the person was shown. Images and documents have no block here.
func gooseEntry(role, contentJSON string, meta gooseMeta) (transcript.Entry, error) {
	var content []gooseContent
	if err := json.Unmarshal([]byte(contentJSON), &content); err != nil {
		return transcript.Entry{}, fmt.Errorf("goose: content: %w", err)
	}
	e := transcript.Entry{Role: transcript.Role(role)}
	if role != "user" && role != "assistant" {
		// goose skips any other role on load.
		e.Role = transcript.RoleOpaque
		return e, nil
	}
	var model, user []transcript.Block
	var toModel, toUser bool
	toolResults := 0
	for _, c := range content {
		switch c.Type {
		case "text":
			b := transcript.Block{Kind: transcript.BlockText, Text: c.Text}
			if c.Annotations.includes("assistant") {
				model, toModel = append(model, b), true
			}
			if c.Annotations.includes("user") {
				user, toUser = append(user, b), true
			}
		case "image", "document":
			toModel = toModel || c.Annotations.includes("assistant")
			toUser = toUser || c.Annotations.includes("user")
		case "thinking":
			b := transcript.Block{Kind: transcript.BlockReasoning, Text: c.Thinking}
			model, user, toModel, toUser = append(model, b), append(user, b), true, true
		case "redactedThinking":
			toModel = true
		case "systemNotification":
			user, toUser = append(user, transcript.Block{Kind: transcript.BlockText, Text: c.Msg}), true
		case "error":
			user, toUser = append(user, transcript.Block{Kind: transcript.BlockText, Text: c.Message}), true
		case "toolConfirmationRequest", "actionRequired":
			toUser = true
		case "toolRequest":
			if c.ToolCall != nil {
				b := transcript.Block{Kind: transcript.BlockToolUse, ToolID: c.ID, Name: c.ToolCall.Value.Name, Input: c.ToolCall.Value.Arguments}
				model, user, toModel, toUser = append(model, b), append(user, b), true, true
			}
		case "toolResponse":
			toolResults++
			m, u := gooseToolResult(c)
			model, user, toModel, toUser = append(model, m), append(user, u), true, true
		}
	}
	if e.Role == transcript.RoleUser && toolResults > 0 && toolResults == len(content) {
		e.Role = transcript.RoleTool
	}
	toModel = toModel && *meta.AgentVisible
	toUser = toUser && *meta.UserVisible
	e.Content = user
	if toModel {
		e.Content = model
	}
	e.Audience = audienceOf(toUser, toModel)
	return e, nil
}

// gooseToolResult is a tool result as the model and as the person get it:
// each keeps the output items annotated for it. A call that failed, or whose
// result says isError, is an error.
func gooseToolResult(c gooseContent) (model, user transcript.Block) {
	var m, u strings.Builder
	status := transcript.StatusOK
	if r := c.ToolResult; r != nil {
		switch {
		case r.Status != "success":
			status = transcript.StatusError
			m.WriteString(r.Error)
			u.WriteString(r.Error)
		case r.Value.IsError:
			status = transcript.StatusError
		}
		for _, rc := range r.Value.Content {
			if rc.Annotations.includes("assistant") {
				m.WriteString(rc.Text)
			}
			if rc.Annotations.includes("user") {
				u.WriteString(rc.Text)
			}
		}
	}
	model = transcript.Block{Kind: transcript.BlockToolResult, ToolID: c.ID, Text: m.String(), Status: status}
	user = model
	user.Text = u.String()
	return model, user
}

// The texts goose's compaction appends after its summary, telling the model
// the message before is a summary (context_mgmt/mod.rs).
var gooseContinuations = []string{
	"Your context was compacted. The previous message contains a summary of the conversation so far.",
	"Your context was compacted at the user's request. The previous message contains a summary of the conversation so far.",
}

// gooseCompactions finds the summaries goose's compaction wrote. A
// compaction keeps every earlier row for the person only, then adds the
// summary and a continuation message for the model only
// (context_mgmt/mod.rs compact_messages). The summary is the conversation
// the model carries on from, so its row gets a Compaction whose Summary is
// what the model has at that point, the summary last and meant for everyone;
// the continuation stays the agent's own instruction.
func gooseCompactions(s *transcript.Session) {
	for i := 1; i < len(s.Entries); i++ {
		c, sum := s.Entries[i], &s.Entries[i-1]
		if c.Role != transcript.RoleAssistant || c.Audience != transcript.AudienceModel || sum.Audience != transcript.AudienceModel || sum.Compaction != nil {
			continue
		}
		continuation := false
		for _, prefix := range gooseContinuations {
			continuation = continuation || strings.HasPrefix(c.Text(), prefix)
		}
		if !continuation {
			continue
		}
		full := *sum
		full.Audience, full.Raw = transcript.AudienceAll, nil
		before := (&transcript.Session{Entries: s.Entries[:i-1]}).Context()
		sum.Compaction = &transcript.Compaction{Summary: append(before, full)}
	}
}

// Write persists a session without disturbing anything it read. Rows of
// entries read from the store (Raw set) are kept exactly; rows of entries the
// session no longer holds are removed; new entries are inserted after them.
// A session goose does not have gets a sessions row, and a new id in goose's
// own scheme, YYYYMMDD_N with N one past the highest for the day, allocated
// inside the transaction so it can never land on a session goose already has.
func (st *gooseStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.Agent != "goose" {
		s = s.Portable()
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

	// Every row read is kept, rows goose no longer shows or sends included.
	keep := map[int64]bool{}
	for _, e := range s.Entries {
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

	// goose reads messages back by created_timestamp, so a new row never
	// takes a time before the latest one stored, as goose's own append does
	// (session_manager.rs add_message).
	var latest sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT MAX(created_timestamp) FROM messages WHERE session_id = ?", s.ID).Scan(&latest); err != nil {
		return "", err
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
		if latest.Valid && ts < latest.Int64 {
			ts = latest.Int64
		}
		latest = sql.NullInt64{Int64: ts, Valid: true}
		meta, err := json.Marshal(map[string]bool{"userVisible": e.Audience.User(), "agentVisible": e.Audience.Model()})
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (message_id, session_id, role, content_json, created_timestamp, metadata_json) VALUES (?, ?, ?, ?, ?, ?)`,
			"msg_"+s.ID+"_"+transcript.NewUUID(), s.ID, string(gooseRole(e.Role)), content, ts, string(meta)); err != nil {
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
