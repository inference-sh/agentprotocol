package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/kiro"
)

func init() { transcript.Register("kiro", Kiro) }

// Kiro is the kiro-cli session store across both of its engines. The v2
// engine, which ACP and the TUI run, keeps each session as a transcript and
// sidecar under ~/.kiro/sessions/cli (transcript/kiro). The v1 engine, which
// `kiro-cli chat --no-interactive --agent-engine v1` runs, keeps each
// conversation as one JSON document in ~/.local/share/kiro-cli/data.sqlite3,
// table conversations_v2, keyed by working directory. This codec lists and
// reads both, and writes v2 sessions.
//
// v1 conversations are read-only. Their document carries the engine's tool
// specifications, context manager, hooks and request metadata, and nothing
// yet checks that a written one loads; a caller that wants a v1 conversation
// in kiro again can copy its entries into a new Session, which is written as
// a v2 session.
var Kiro transcript.Codec = kiroCodec{}

const kiroV1Path = ".local/share/kiro-cli/data.sqlite3"

type kiroCodec struct{}

func (kiroCodec) Open(home string) (transcript.Store, error) {
	v2, err := kiro.Codec.Open(home)
	if err != nil {
		return nil, err
	}
	return &kiroStore{v2: v2, v1: filepath.Join(home, kiroV1Path)}, nil
}

type kiroStore struct {
	v2 transcript.Store
	v1 string
}

// kiroV1Vendor marks a session read from the v1 store.
type kiroV1Vendor struct{}

func (st *kiroStore) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
	out, err := st.v2.List(ctx, cwd)
	if err != nil {
		return nil, err
	}
	v1, err := st.listV1(ctx, cwd)
	if err != nil {
		return nil, err
	}
	out = append(out, v1...)
	transcript.SortNewest(out)
	return out, nil
}

func (st *kiroStore) listV1(ctx context.Context, cwd string) ([]transcript.Info, error) {
	if _, err := os.Stat(st.v1); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	db, done, err := openRO(st.v1)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'conversations_v2'`).Scan(&exists); err != nil || exists == 0 {
		return nil, err
	}
	q := `SELECT key, conversation_id, updated_at FROM conversations_v2`
	var args []any
	if cwd != "" {
		q += ` WHERE key = ?`
		args = append(args, cwd)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("kiro v1: list: %w", err)
	}
	defer rows.Close()
	var out []transcript.Info
	for rows.Next() {
		var key, id string
		var updated int64
		if err := rows.Scan(&key, &id, &updated); err != nil {
			return nil, err
		}
		out = append(out, transcript.Info{ID: id, CWD: key, Updated: time.UnixMilli(updated).UTC(), Path: st.v1})
	}
	return out, rows.Err()
}

func (st *kiroStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	s, err := st.v2.Read(ctx, id)
	if !errors.Is(err, transcript.ErrNotFound) {
		return s, err
	}
	return st.readV1(ctx, id)
}

// Write writes a v2 session. A session read from the v1 store is refused:
// see Kiro.
func (st *kiroStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if _, ok := s.Vendor.(kiroV1Vendor); ok {
		return "", fmt.Errorf("kiro: %w: v1 conversation %s", transcript.ErrReadOnly, s.ID)
	}
	return st.v2.Write(ctx, s)
}

// The v1 conversation document, as far as the codec reads it.
type kiroV1Doc struct {
	ConversationID string `json:"conversation_id"`
	History        []struct {
		User struct {
			Content   json.RawMessage `json:"content"`
			Timestamp *string         `json:"timestamp"`
		} `json:"user"`
		Assistant       json.RawMessage `json:"assistant"`
		RequestMetadata *struct {
			MessageID string `json:"message_id"`
			StartMs   int64  `json:"request_start_timestamp_ms"`
			ModelID   string `json:"model_id"`
		} `json:"request_metadata"`
	} `json:"history"`
	ModelInfo *struct {
		ModelID string `json:"model_id"`
	} `json:"model_info"`
}

type kiroV1ToolUse struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type kiroV1Assistant struct {
	MessageID string          `json:"message_id"`
	Content   string          `json:"content"`
	ToolUses  []kiroV1ToolUse `json:"tool_uses"`
}

type kiroV1Result struct {
	ToolUseID string `json:"tool_use_id"`
	Content   []struct {
		Text *string         `json:"Text"`
		JSON json.RawMessage `json:"Json"`
	} `json:"content"`
	Status string `json:"status"`
}

func (st *kiroStore) readV1(ctx context.Context, id string) (*transcript.Session, error) {
	if _, err := os.Stat(st.v1); errors.Is(err, os.ErrNotExist) {
		return nil, transcript.ErrNotFound
	}
	db, done, err := openRO(st.v1)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()
	var key, value string
	var created, updated int64
	err = db.QueryRowContext(ctx, `SELECT key, value, created_at, updated_at FROM conversations_v2 WHERE conversation_id = ?`, id).
		Scan(&key, &value, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) || (err != nil && strings.Contains(err.Error(), "no such table")) {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("kiro v1: read: %w", err)
	}
	var doc kiroV1Doc
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		return nil, fmt.Errorf("kiro v1: conversation %s: %w", id, err)
	}
	s := &transcript.Session{
		ID: id, Agent: "kiro", CWD: key,
		Created: time.UnixMilli(created).UTC(), Updated: time.UnixMilli(updated).UTC(),
		Vendor: kiroV1Vendor{},
	}
	if doc.ModelInfo != nil {
		s.Model = doc.ModelInfo.ModelID
	}
	for i, h := range doc.History {
		user, err := kiroV1User(h.User.Content)
		if err != nil {
			return nil, fmt.Errorf("kiro v1: conversation %s, turn %d: %w", id, i, err)
		}
		user.ID = fmt.Sprintf("%s:%d:user", id, i)
		if h.User.Timestamp != nil {
			if t, err := time.Parse(time.RFC3339Nano, *h.User.Timestamp); err == nil {
				user.Time = t
			}
		}
		s.Entries = append(s.Entries, user)
		if len(h.Assistant) == 0 {
			continue
		}
		asst, err := kiroV1Answer(h.Assistant)
		if err != nil {
			return nil, fmt.Errorf("kiro v1: conversation %s, turn %d: %w", id, i, err)
		}
		if h.RequestMetadata != nil && h.RequestMetadata.StartMs > 0 {
			asst.Time = time.UnixMilli(h.RequestMetadata.StartMs).UTC()
		}
		s.Entries = append(s.Entries, asst)
	}
	return s, nil
}

// kiroV1User reads a user item's content: a prompt, or tool results.
func kiroV1User(raw json.RawMessage) (transcript.Entry, error) {
	var c struct {
		Prompt *struct {
			Prompt string `json:"prompt"`
		} `json:"Prompt"`
		ToolUseResults *struct {
			Results []kiroV1Result `json:"tool_use_results"`
		} `json:"ToolUseResults"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return transcript.Entry{}, fmt.Errorf("user content: %w", err)
	}
	switch {
	case c.Prompt != nil:
		return transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: c.Prompt.Prompt}}}, nil
	case c.ToolUseResults != nil:
		e := transcript.Entry{Role: transcript.RoleTool}
		for _, r := range c.ToolUseResults.Results {
			var text strings.Builder
			for _, part := range r.Content {
				switch {
				case part.Text != nil:
					text.WriteString(*part.Text)
				case len(part.JSON) > 0:
					text.Write(part.JSON)
				}
			}
			st := transcript.StatusOK
			if r.Status != "Success" {
				st = transcript.StatusError
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: r.ToolUseID, Text: text.String(), Status: st})
		}
		return e, nil
	default:
		return transcript.Entry{}, fmt.Errorf("user content %s", truncate(raw))
	}
}

// kiroV1Answer reads an assistant item: a tool use or a response.
func kiroV1Answer(raw json.RawMessage) (transcript.Entry, error) {
	var a struct {
		ToolUse  *kiroV1Assistant `json:"ToolUse"`
		Response *kiroV1Assistant `json:"Response"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return transcript.Entry{}, fmt.Errorf("assistant: %w", err)
	}
	m := a.Response
	if m == nil {
		m = a.ToolUse
	}
	if m == nil {
		return transcript.Entry{}, fmt.Errorf("assistant %s", truncate(raw))
	}
	e := transcript.Entry{ID: m.MessageID, Role: transcript.RoleAssistant}
	if m.Content != "" {
		e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: m.Content})
	}
	for _, tu := range m.ToolUses {
		e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tu.ID, Name: tu.Name, Input: tu.Args})
	}
	return e, nil
}

func truncate(raw json.RawMessage) string {
	if len(raw) > 120 {
		return string(raw[:120]) + "…"
	}
	return string(raw)
}
