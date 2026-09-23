package sqlite

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

func init() { transcript.Register("cursor", Cursor) }

// Cursor is the Cursor CLI session store of record. Each session is a
// directory ~/.cursor/chats/<md5 of cwd>/<agent id>/ holding meta.json (cwd
// and times) and store.db, a content-addressed blob store: every message is a
// JSON blob keyed by the sha256 of its bytes, a root blob lists the message
// ids in order, and meta row "0" names the latest root.
//
// It supersedes the transcript/cursor codec when this module is imported.
// That codec reads the readable transcript Cursor derives from this store,
// which drops tool results; this one reads them.
var Cursor transcript.Codec = cursorCodec{}

const cursorChats = ".cursor/chats"

type cursorCodec struct{}

func (cursorCodec) Open(home string) (transcript.Store, error) {
	return &cursorStore{root: filepath.Join(home, cursorChats)}, nil
}

type cursorStore struct{ root string }

// CursorVendor is what a Cursor session carries in Session.Vendor, so a
// same-agent write reproduces what this codec does not model: the store's
// meta record and meta.json field by field, and the root blob as read.
type CursorVendor struct {
	Meta     map[string]json.RawMessage
	MetaJSON map[string]json.RawMessage
	Root     []byte
	RootIDs  []string
}

// cursorMeta is the meta.json beside store.db.
type cursorMeta struct {
	SchemaVersion   int    `json:"schemaVersion"`
	CreatedAtMs     int64  `json:"createdAtMs"`
	HasConversation bool   `json:"hasConversation"`
	UpdatedAtMs     int64  `json:"updatedAtMs"`
	CWD             string `json:"cwd"`
}

// cursorStoreMeta is store.db's meta row "0", stored hex-encoded.
type cursorStoreMeta struct {
	AgentID           string `json:"agentId"`
	LatestRootBlobID  string `json:"latestRootBlobId"`
	Name              string `json:"name"`
	Mode              string `json:"mode"`
	CreatedAt         int64  `json:"createdAt"`
	BlobEncryptionKey string `json:"blobEncryptionKey"`
}

type cursorMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type cursorPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
}

// cursorDir is the project directory Cursor keys sessions under: the md5 of
// the working directory.
func cursorDir(cwd string) string {
	sum := md5.Sum([]byte(cwd))
	return hex.EncodeToString(sum[:])
}

func (st *cursorStore) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
	project := "*"
	if cwd != "" {
		project = cursorDir(cwd)
	}
	dirs, err := transcript.Glob(filepath.Join(st.root, project, "*"))
	if err != nil {
		return nil, err
	}
	var out []transcript.Info
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "store.db")); err != nil {
			continue
		}
		m, err := readCursorMeta(dir)
		if err != nil {
			return nil, err
		}
		if !m.HasConversation {
			continue
		}
		out = append(out, transcript.Info{ID: filepath.Base(dir), CWD: m.CWD, Updated: time.UnixMilli(m.UpdatedAtMs).UTC(), Path: filepath.Join(dir, "store.db")})
	}
	transcript.SortNewest(out)
	return out, nil
}

func readCursorMeta(dir string) (cursorMeta, error) {
	var m cursorMeta
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, fmt.Errorf("cursor: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("cursor: meta.json: %w", err)
	}
	return m, nil
}

func (st *cursorStore) sessionDir(id string) (string, error) {
	dirs, err := transcript.Glob(filepath.Join(st.root, "*", id))
	if err != nil {
		return "", err
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "store.db")); err == nil {
			return dir, nil
		}
	}
	return "", transcript.ErrNotFound
}

func (st *cursorStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	dir, err := st.sessionDir(id)
	if err != nil {
		return nil, err
	}
	m, err := readCursorMeta(dir)
	if err != nil {
		return nil, err
	}
	metaJSON, err := rawFields(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	db, done, err := openRO(filepath.Join(dir, "store.db"))
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	var metaHex string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = '0'`).Scan(&metaHex); err != nil {
		return nil, fmt.Errorf("cursor: meta row: %w", err)
	}
	metaBytes, err := hex.DecodeString(metaHex)
	if err != nil {
		return nil, fmt.Errorf("cursor: meta row: %w", err)
	}
	var sm cursorStoreMeta
	if err := json.Unmarshal(metaBytes, &sm); err != nil {
		return nil, fmt.Errorf("cursor: meta row: %w", err)
	}
	var metaFields map[string]json.RawMessage
	if err := json.Unmarshal(metaBytes, &metaFields); err != nil {
		return nil, fmt.Errorf("cursor: meta row: %w", err)
	}

	root, err := blob(ctx, db, sm.LatestRootBlobID)
	if err != nil {
		return nil, fmt.Errorf("cursor: root blob: %w", err)
	}
	ids, err := rootMessageIDs(root)
	if err != nil {
		return nil, fmt.Errorf("cursor: root blob: %w", err)
	}

	s := &transcript.Session{
		ID:      id,
		Agent:   "cursor",
		CWD:     m.CWD,
		Title:   sm.Name,
		Created: time.UnixMilli(m.CreatedAtMs).UTC(),
		Updated: time.UnixMilli(m.UpdatedAtMs).UTC(),
		Vendor:  &CursorVendor{Meta: metaFields, MetaJSON: metaJSON, Root: root, RootIDs: ids},
	}
	for _, bid := range ids {
		data, err := blob(ctx, db, bid)
		if err != nil {
			return nil, fmt.Errorf("cursor: message %s: %w", bid, err)
		}
		e, err := cursorEntry(data)
		if err != nil {
			return nil, fmt.Errorf("cursor: message %s: %w", bid, err)
		}
		e.ID = bid
		e.Raw = data
		s.Entries = append(s.Entries, e)
	}
	return s, nil
}

func blob(ctx context.Context, db *sql.DB, id string) ([]byte, error) {
	var data []byte
	err := db.QueryRowContext(ctx, `SELECT data FROM blobs WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("blob %s missing", id)
	}
	return data, err
}

// rootMessageIDs reads field 1 of the root blob, a ConversationStateStructure
// whose repeated bytes field root_prompt_messages_json holds the message blob
// ids, raw sha256, in order. Every other field is skipped by wire type.
func rootMessageIDs(b []byte) ([]string, error) {
	var ids []string
	for len(b) > 0 {
		key, n := uvarint(b)
		if n <= 0 {
			return nil, errors.New("bad field key")
		}
		b = b[n:]
		field, wire := key>>3, key&7
		switch wire {
		case 0:
			_, n := uvarint(b)
			if n <= 0 {
				return nil, errors.New("bad varint")
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errors.New("short fixed64")
			}
			b = b[8:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("bad length")
			}
			val := b[n : n+int(l)]
			b = b[n+int(l):]
			if field == 1 {
				ids = append(ids, hex.EncodeToString(val))
			}
		case 5:
			if len(b) < 4 {
				return nil, errors.New("short fixed32")
			}
			b = b[4:]
		default:
			return nil, fmt.Errorf("wire type %d", wire)
		}
	}
	return ids, nil
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		x |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return x, i + 1
		}
	}
	return 0, 0
}

func appendRoot(b []byte, id []byte) []byte {
	b = append(b, 0x0a)
	b = appendUvarint(b, uint64(len(id)))
	return append(b, id...)
}

func appendUvarint(b []byte, x uint64) []byte {
	for x >= 0x80 {
		b = append(b, byte(x)|0x80)
		x >>= 7
	}
	return append(b, byte(x))
}

func cursorEntry(data []byte) (transcript.Entry, error) {
	var m cursorMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return transcript.Entry{}, err
	}
	e := transcript.Entry{}
	switch m.Role {
	case "user":
		e.Role = transcript.RoleUser
	case "assistant":
		e.Role = transcript.RoleAssistant
	case "system":
		e.Role = transcript.RoleSystem
	case "tool":
		e.Role = transcript.RoleTool
	default:
		return transcript.Entry{}, fmt.Errorf("role %q", m.Role)
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		e.Content = []transcript.Block{{Kind: transcript.BlockText, Text: text}}
		return e, nil
	}
	var parts []cursorPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return transcript.Entry{}, fmt.Errorf("content: %w", err)
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		case "reasoning":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
		case "tool-call":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: p.ToolCallID, Name: p.ToolName, Input: p.Args})
		case "tool-result":
			st := transcript.StatusOK
			if p.IsError {
				st = transcript.StatusError
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: p.ToolCallID, Name: p.ToolName, Text: resultString(p.Result), Status: st})
		}
	}
	return e, nil
}

// resultString reads a tool result, which Cursor stores as a string or as
// structured JSON; structured results are kept as their JSON text.
func resultString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func (st *cursorStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.ID == "" {
		s.ID = transcript.NewUUID()
	}
	now := time.Now()
	if s.Created.IsZero() {
		s.Created = now
	}
	if s.Updated.IsZero() {
		s.Updated = s.Created
	}
	v, _ := s.Vendor.(*CursorVendor)

	type stored struct {
		id   string
		data []byte
	}
	var blobs []stored
	var ids []string
	for _, e := range s.Messages() {
		data := []byte(e.Raw)
		if data == nil {
			var err error
			if data, err = cursorBlob(e); err != nil {
				return "", err
			}
		}
		sum := sha256.Sum256(data)
		id := hex.EncodeToString(sum[:])
		blobs = append(blobs, stored{id, data})
		ids = append(ids, id)
	}

	// The root is reused as read when the conversation is unchanged, which
	// keeps any fields this codec does not model; otherwise it is rebuilt with
	// only the message list.
	var root []byte
	if v != nil && equalIDs(v.RootIDs, ids) {
		root = v.Root
	} else {
		for _, id := range ids {
			raw, _ := hex.DecodeString(id)
			root = appendRoot(root, raw)
		}
	}
	rootSum := sha256.Sum256(root)
	rootID := hex.EncodeToString(rootSum[:])

	meta := map[string]json.RawMessage{}
	if v != nil {
		for k, val := range v.Meta {
			meta[k] = val
		}
	}
	key := ""
	if raw, ok := meta["blobEncryptionKey"]; ok {
		_ = json.Unmarshal(raw, &key)
	}
	if key == "" {
		var k [32]byte
		if _, err := rand.Read(k[:]); err != nil {
			return "", err
		}
		key = hex.EncodeToString(k[:])
	}
	name := s.Title
	if name == "" {
		name = "New Agent"
	}
	if err := mergeFields(meta, cursorStoreMeta{AgentID: s.ID, LatestRootBlobID: rootID, Name: name, Mode: "default", CreatedAt: s.Created.UnixMilli(), BlobEncryptionKey: key}); err != nil {
		return "", err
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}

	dir := filepath.Join(st.root, cursorDir(s.CWD), s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "store.db")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	db, err := openRW(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB); CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);`); err != nil {
		return "", fmt.Errorf("cursor: schema: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for _, b := range append(blobs, stored{rootID, root}) {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO blobs (id, data) VALUES (?, ?)`, b.id, b.data); err != nil {
			return "", fmt.Errorf("cursor: blob: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('0', ?)`, hex.EncodeToString(metaBytes)); err != nil {
		return "", fmt.Errorf("cursor: meta row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}

	metaJSON := map[string]json.RawMessage{}
	if v != nil {
		for k, val := range v.MetaJSON {
			metaJSON[k] = val
		}
	}
	if err := mergeFields(metaJSON, cursorMeta{SchemaVersion: 1, CreatedAtMs: s.Created.UnixMilli(), HasConversation: len(ids) > 0, UpdatedAtMs: s.Updated.UnixMilli(), CWD: s.CWD}); err != nil {
		return "", err
	}
	out, err := json.Marshal(metaJSON)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), out, 0o644); err != nil {
		return "", err
	}
	return s.ID, nil
}

// cursorBlob encodes an entry from another agent as the message JSON Cursor
// stores. Keys marshal in sorted order, which is the order Cursor writes.
func cursorBlob(e transcript.Entry) ([]byte, error) {
	role := map[transcript.Role]string{
		transcript.RoleUser: "user", transcript.RoleAssistant: "assistant",
		transcript.RoleSystem: "system", transcript.RoleTool: "tool",
	}[e.Role]
	if role == "" {
		return nil, fmt.Errorf("cursor: role %q", e.Role)
	}
	parts := make([]map[string]any, 0, len(e.Content))
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case transcript.BlockReasoning:
			parts = append(parts, map[string]any{"type": "reasoning", "text": b.Text})
		case transcript.BlockToolUse:
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			parts = append(parts, map[string]any{"type": "tool-call", "toolCallId": b.ToolID, "toolName": b.Name, "args": args})
		case transcript.BlockToolResult:
			p := map[string]any{"type": "tool-result", "toolCallId": b.ToolID, "toolName": b.Name, "result": b.Text}
			if b.Status == transcript.StatusError {
				p["isError"] = true
			}
			parts = append(parts, p)
		}
	}
	if role == "user" && len(parts) == 1 && parts[0]["type"] == "text" {
		return json.Marshal(map[string]any{"role": role, "content": parts[0]["text"]})
	}
	return json.Marshal(map[string]any{"role": role, "content": parts})
}

// rawFields reads a JSON object file field by field.
func rawFields(path string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return out, nil
}

// mergeFields overlays the fields of v onto dst, keeping dst's other keys.
func mergeFields(dst map[string]json.RawMessage, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for k, val := range fields {
		dst[k] = val
	}
	return nil
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
