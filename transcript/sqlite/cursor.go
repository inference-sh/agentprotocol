package sqlite

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

func init() { transcript.Register("cursor", Cursor) }

// Cursor is the Cursor CLI session store of record, both of its locations
// (see CursorLocation). Each session is a directory holding meta.json and
// store.db, a content-addressed blob store: every message is a JSON blob
// keyed by the sha256 of its bytes, a root blob lists the message ids in
// order, and meta row "0" names the latest root.
//
// It writes a session it did not read under acp-sessions. This module's
// driver runs Cursor as `cursor-agent acp`, so the agent that loads a written
// session is ACP session/load, which opens nothing else. Open CursorCodec
// with New set to CursorCLI to write for `agent --resume` instead.
//
// It supersedes the transcript/cursor codec when this module is imported.
// That codec reads the readable transcript Cursor derives from the chats
// store, which drops tool results and which Cursor never writes for an ACP
// session; this one reads both stores, with the results.
var Cursor transcript.Codec = CursorCodec{New: CursorACP}

// CursorLocation is which of cursor-agent's two session stores a session is
// kept in. The two hold the same blob store and neither side reads the
// other's: `cursor-agent acp` creates and loads sessions only under
// acp-sessions (src/acp/agent-store.ts, which answers session/load with
// "Session not found" when <id>/store.db is missing there), and the TUI and
// `agent -p` only under chats.
type CursorLocation uint8

const (
	// CursorACP is ~/.cursor/acp-sessions/<id>/, whose meta.json holds the
	// cwd and a title (src/acp/acp-storage.ts).
	CursorACP CursorLocation = iota
	// CursorCLI is ~/.cursor/chats/<md5 of cwd>/<id>/, whose meta.json holds
	// the cwd and the session's times.
	CursorCLI
)

const (
	cursorChats = ".cursor/chats"
	cursorACP   = ".cursor/acp-sessions"
)

// CursorCodec opens Cursor's session stores. A session read from one is
// written back to the same one; New is where a session the store did not
// read goes.
type CursorCodec struct{ New CursorLocation }

func (c CursorCodec) Open(home string) (transcript.Store, error) {
	return &cursorStore{chats: filepath.Join(home, cursorChats), acp: filepath.Join(home, cursorACP), new: c.New}, nil
}

type cursorStore struct {
	chats, acp string
	new        CursorLocation
}

// CursorVendor is what a Cursor session carries in Session.Vendor, so a
// same-agent write reproduces what this codec does not model: the store's
// meta record and meta.json field by field, and the root blob as read.
type CursorVendor struct {
	Meta     map[string]json.RawMessage
	MetaJSON map[string]json.RawMessage
	Root     []byte
	RootIDs  []string
	// MetaHex and MetaFile are the meta row and meta.json exactly as read,
	// written back unchanged when the conversation is unchanged. MetaFile is
	// nil for a session kept without meta.json, and none is added.
	MetaHex  string
	MetaFile []byte
	// ProjectDir is the directory a CLI session was read from, md5 of its
	// cwd, so a session whose cwd is unknown writes back where it was.
	ProjectDir string
	// Location is the store the session was read from, and so the one a
	// write puts it back in.
	Location CursorLocation
	// Blobs are the store's blobs other than the messages, by id: the turn
	// and step records the root links, which cursor-agent replays on
	// session/load, and the roots of earlier checkpoints. A write adds them,
	// so a session copied to another home keeps what its root refers to.
	Blobs map[string][]byte
}

// cursorMeta is the meta.json beside store.db.
type cursorMeta struct {
	SchemaVersion   int    `json:"schemaVersion"`
	CreatedAtMs     int64  `json:"createdAtMs"`
	HasConversation bool   `json:"hasConversation"`
	UpdatedAtMs     int64  `json:"updatedAtMs"`
	CWD             string `json:"cwd"`
}

// cursorACPMeta is the meta.json beside an ACP session's store.db, fields in
// the order cursor-agent writes them (acp-storage.ts). Its reader keeps no
// other field.
type cursorACPMeta struct {
	SchemaVersion int    `json:"schemaVersion"`
	CWD           string `json:"cwd"`
	Title         string `json:"title,omitempty"`
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
	// An image part carries Image and a file part Data, each base64, a
	// data: URL or a URL, with the type in MimeType (MediaType in later
	// versions of the AI SDK these messages follow) and a file's name in
	// Filename.
	Image     string `json:"image,omitempty"`
	Data      string `json:"data,omitempty"`
	MimeType  string `json:"mimeType,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

// media reads an image or file part. Cursor's transcript writer names the
// two part types and a file's filename (the "[Image]" and "[File: name]"
// lines TranscriptStore writes for them); the rest of the shape is the AI
// SDK's message parts, whose layout the other parts follow.
func (p cursorPart) media() (transcript.Block, bool) {
	b := transcript.Block{Kind: transcript.BlockImage, MediaType: p.MimeType}
	src := p.Image
	if p.Type == "file" {
		b.Kind, b.Name, src = transcript.BlockFile, p.Filename, p.Data
	}
	if b.MediaType == "" {
		b.MediaType = p.MediaType
	}
	switch {
	case strings.HasPrefix(src, "data:"):
		mt, data, ok := transcript.ParseDataURL(src)
		if !ok {
			return transcript.Block{}, false
		}
		b.Data = data
		if b.MediaType == "" {
			b.MediaType = mt
		}
	case strings.Contains(src, "://"):
		b.URI = src
	case src != "":
		data, err := base64.StdEncoding.DecodeString(src)
		if err != nil {
			return transcript.Block{}, false
		}
		b.Data = data
	default:
		return transcript.Block{}, false
	}
	return b, true
}

// createdMs is a session's creation time: meta.json's, or the store's own
// record of it when meta.json is missing.
func createdMs(m cursorMeta, sm cursorStoreMeta) int64 {
	if m.CreatedAtMs > 0 {
		return m.CreatedAtMs
	}
	return sm.CreatedAt
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
	dirs, err := transcript.Glob(filepath.Join(st.chats, project, "*"))
	if err != nil {
		return nil, err
	}
	out, err := st.listACP(cwd)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "store.db")); err != nil {
			continue
		}
		m, ok, err := readCursorMeta(dir)
		if err != nil {
			return nil, err
		}
		if ok && !m.HasConversation {
			continue
		}
		if !ok {
			// No meta.json: the directory is md5(cwd), so a filtered listing
			// knows the cwd and an unfiltered one cannot.
			m.CWD = cwd
			if fi, err := os.Stat(filepath.Join(dir, "store.db")); err == nil {
				m.UpdatedAtMs = fi.ModTime().UnixMilli()
			}
		}
		// Cursor holds the session's store.db open while it serves the
		// session, so the directory is this session's alone.
		out = append(out, transcript.Info{ID: filepath.Base(dir), CWD: m.CWD, Updated: time.UnixMilli(m.UpdatedAtMs).UTC(), Path: filepath.Join(dir, "store.db"), Root: dir})
	}
	transcript.SortNewest(out)
	return out, nil
}

// listACP lists ACP sessions as cursor-agent's session/list does
// (src/acp/session-list.ts): a directory with a store.db and a meta.json
// naming a cwd, updated when store.db was last written. One whose meta.json
// is missing or unreadable is skipped, not an error; session/load still
// opens it by id, and so does Read.
func (st *cursorStore) listACP(cwd string) ([]transcript.Info, error) {
	dirs, err := transcript.Glob(filepath.Join(st.acp, "*"))
	if err != nil {
		return nil, err
	}
	var out []transcript.Info
	for _, dir := range dirs {
		db := filepath.Join(dir, "store.db")
		fi, err := os.Stat(db)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		var m cursorACPMeta
		if json.Unmarshal(raw, &m) != nil || m.CWD == "" || (cwd != "" && m.CWD != cwd) {
			continue
		}
		out = append(out, transcript.Info{ID: filepath.Base(dir), CWD: m.CWD, Title: m.Title, Updated: fi.ModTime().UTC(), Path: db, Root: dir})
	}
	return out, nil
}

// readCursorMeta reads meta.json. Older Cursor versions kept CLI sessions
// without one; for those it reports ok false and the store's own meta row is
// all there is. An ACP session's meta.json has the cwd and no times.
func readCursorMeta(dir string) (m cursorMeta, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("cursor: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, false, fmt.Errorf("cursor: meta.json: %w", err)
	}
	return m, true, nil
}

// sessionDir finds a session in either store. Cursor names sessions by
// UUID in both, so an id is in one of them.
func (st *cursorStore) sessionDir(id string) (string, CursorLocation, error) {
	if id == "" || filepath.Base(id) != id {
		return "", 0, transcript.ErrNotFound
	}
	dir := filepath.Join(st.acp, id)
	if _, err := os.Stat(filepath.Join(dir, "store.db")); err == nil {
		return dir, CursorACP, nil
	}
	dirs, err := transcript.Glob(filepath.Join(st.chats, "*", id))
	if err != nil {
		return "", 0, err
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "store.db")); err == nil {
			return dir, CursorCLI, nil
		}
	}
	return "", 0, transcript.ErrNotFound
}

func (st *cursorStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	dir, loc, err := st.sessionDir(id)
	if err != nil {
		return nil, err
	}
	m, hasMeta, err := readCursorMeta(dir)
	if err != nil {
		return nil, err
	}
	var metaFile []byte
	metaJSON := map[string]json.RawMessage{}
	if hasMeta {
		if metaFile, err = os.ReadFile(filepath.Join(dir, "meta.json")); err != nil {
			return nil, fmt.Errorf("cursor: %w", err)
		}
		if metaJSON, err = rawFields(filepath.Join(dir, "meta.json")); err != nil {
			return nil, err
		}
	}
	if m.UpdatedAtMs == 0 {
		// No times kept beside the store: an older CLI session, or any ACP
		// one, which cursor-agent dates by the store's own mtime.
		if fi, err := os.Stat(filepath.Join(dir, "store.db")); err == nil {
			m.UpdatedAtMs = fi.ModTime().UnixMilli()
		}
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
		Created: time.UnixMilli(createdMs(m, sm)).UTC(),
		Updated: time.UnixMilli(m.UpdatedAtMs).UTC(),
		Vendor: &CursorVendor{Meta: metaFields, MetaJSON: metaJSON, Root: root, RootIDs: ids, MetaHex: metaHex, MetaFile: metaFile,
			Location: loc},
	}
	if loc == CursorCLI {
		s.Vendor.(*CursorVendor).ProjectDir = filepath.Base(filepath.Dir(dir))
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
	if s.Vendor.(*CursorVendor).Blobs, err = otherBlobs(ctx, db, ids); err != nil {
		return nil, fmt.Errorf("cursor: %w", err)
	}
	return s, nil
}

// otherBlobs reads every blob but the messages.
func otherBlobs(ctx context.Context, db *sql.DB, messages []string) (map[string][]byte, error) {
	skip := make(map[string]bool, len(messages))
	for _, id := range messages {
		skip[id] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT id, data FROM blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		if !skip[id] {
			out[id] = data
		}
	}
	return out, rows.Err()
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
		case "image", "file":
			if b, ok := p.media(); ok {
				e.Content = append(e.Content, b)
			}
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
	if s.Agent != "cursor" {
		s = s.Portable()
	}
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
	unchanged := v != nil && equalIDs(v.RootIDs, ids)
	var root []byte
	if unchanged {
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

	loc := st.new
	if v != nil {
		loc = v.Location
	}
	var dir string
	switch loc {
	case CursorACP:
		// session/list finds an ACP session by the cwd in its meta.json, and
		// cursor-agent refuses to write one that is not absolute. Only a
		// session read without meta.json is written back without one.
		if s.CWD == "" && (v == nil || v.MetaFile != nil) || s.CWD != "" && !filepath.IsAbs(s.CWD) {
			return "", fmt.Errorf("cursor: an ACP session needs an absolute working directory, got %q", s.CWD)
		}
		dir = filepath.Join(st.acp, s.ID)
	case CursorCLI:
		project := cursorDir(s.CWD)
		if s.CWD == "" {
			if v == nil || v.ProjectDir == "" {
				return "", errors.New("cursor: a session needs its working directory: Cursor files sessions under md5(cwd)")
			}
			project = v.ProjectDir
		}
		dir = filepath.Join(st.chats, project, s.ID)
	default:
		return "", fmt.Errorf("cursor: location %d", loc)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// The store is append-only and content-addressed: every checkpoint adds
	// blobs and a new root, and earlier roots stay for rewind. A write adds
	// what is missing and moves the meta row to the new root; it never
	// removes a blob.
	path := filepath.Join(dir, "store.db")
	db, err := openRW(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS blobs (id TEXT PRIMARY KEY, data BLOB); CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);`); err != nil {
		return "", fmt.Errorf("cursor: schema: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if v != nil {
		for id, data := range v.Blobs {
			blobs = append(blobs, stored{id, data})
		}
	}
	for _, b := range append(blobs, stored{rootID, root}) {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO blobs (id, data) VALUES (?, ?)`, b.id, b.data); err != nil {
			return "", fmt.Errorf("cursor: blob: %w", err)
		}
	}
	metaValue := hex.EncodeToString(metaBytes)
	if unchanged && v.MetaHex != "" {
		metaValue = v.MetaHex
	}
	var current string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = '0'`).Scan(&current); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('0', ?)`, metaValue); err != nil {
			return "", fmt.Errorf("cursor: meta row: %w", err)
		}
	case err != nil:
		return "", err
	case current != metaValue:
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = '0'`, metaValue); err != nil {
			return "", fmt.Errorf("cursor: meta row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}

	var out []byte
	switch {
	case unchanged && v.MetaFile == nil, loc == CursorACP && s.CWD == "":
		// Kept without meta.json, and nothing to put in one: add none.
		return s.ID, nil
	case unchanged:
		out = v.MetaFile
	case loc == CursorACP:
		// cursor-agent writes the title as the store's name, trimmed, and
		// rewrites the whole file on every load, keeping no other field.
		if out, err = json.Marshal(cursorACPMeta{SchemaVersion: 1, CWD: s.CWD, Title: strings.TrimSpace(s.Title)}); err != nil {
			return "", err
		}
	default:
		metaJSON := map[string]json.RawMessage{}
		if v != nil {
			for k, val := range v.MetaJSON {
				metaJSON[k] = val
			}
		}
		if err := mergeFields(metaJSON, cursorMeta{SchemaVersion: 1, CreatedAtMs: s.Created.UnixMilli(), HasConversation: len(ids) > 0, UpdatedAtMs: s.Updated.UnixMilli(), CWD: s.CWD}); err != nil {
			return "", err
		}
		if out, err = json.Marshal(metaJSON); err != nil {
			return "", err
		}
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
		// Images and files are left out: Cursor's own code shows only the
		// part types and a file's filename, not how the bytes are laid out
		// in a part its backend reads back.
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
