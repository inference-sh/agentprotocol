// Package gemini reads and writes Gemini CLI sessions.
//
// Gemini keeps each session as a JSONL file under
// ~/.gemini/tmp/<project>/chats/session-<stamp>-<id prefix>.jsonl. <project>
// is a slug gemini assigns the project root in ~/.gemini/projects.json, the
// root's base name unless another root already holds it, and a
// .project_root marker in that directory names the root.
//
// The file is a log gemini replays into an ordered map of messages keyed by
// id (chatRecordingService.ts, loadConversationRecord): the first record is
// the conversation's metadata; a message record adds a message or replaces
// the one with its id in place; {"$set": {...}} merges metadata, and when it
// carries messages it replaces the whole list; {"$rewindTo": id} drops that
// message and every one after it. A file whose metadata lacks sessionId or
// projectHash is read as a legacy record, fails, and is deleted by gemini's
// startup cleanup, so a written file must carry both.
package gemini

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the Gemini CLI session store.
var Codec transcript.Codec = codec{}

const (
	geminiDir  = ".gemini"
	marker     = ".project_root"
	registryFn = "projects.json"
)

// IDs is the scheme gemini names messages in: UUIDs, or 32 hex digits for
// the context message it writes itself.
var IDs = transcript.IDScheme{New: transcript.NewUUID, Valid: func(id string) bool {
	return transcript.IsUUID(id) || hex32.MatchString(id)
}}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

type codec struct{}

func (codec) Open(home string) (transcript.Store, error) {
	return &store{home: home}, nil
}

type store struct{ home string }

// Vendor is what a gemini session carries in Session.Vendor: the file as
// read and the message ids its replay produced, so a write can leave the
// file as it was and append to it.
type Vendor struct {
	FileName string
	File     []byte
	IDs      []string
	Metadata map[string]json.RawMessage
}

type conversationHeader struct {
	SessionID   string `json:"sessionId"`
	ProjectHash string `json:"projectHash"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
	Kind        string `json:"kind"`
}

// ProjectHash is the hash gemini stamps a conversation with: sha256 of the
// project root, in lowercase hex (utils/paths.ts, getProjectHash).
func ProjectHash(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])
}

func (st *store) tmp() string { return filepath.Join(st.home, geminiDir, "tmp") }

func (st *store) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
	paths, err := transcript.Glob(filepath.Join(st.tmp(), "*", "chats", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []transcript.Info
	seen := map[string]int{}
	for _, p := range paths {
		c, err := replay(p)
		if err != nil || c.meta.SessionID == "" {
			continue
		}
		// gemini lists only a session with something to resume, and never a
		// subagent's (cli utils/sessionUtils.ts, getAllSessionFiles). Loading
		// a session starts a recording under its id before it resolves the
		// file, so a load leaves a second file with the same id and nothing
		// in it but the session context.
		if c.meta.Kind == "subagent" || !c.resumable() {
			continue
		}
		root := projectRoot(filepath.Dir(filepath.Dir(p)))
		if cwd != "" && c.meta.ProjectHash != ProjectHash(cwd) {
			continue
		}
		in := transcript.Info{ID: c.meta.SessionID, CWD: root, Path: p, Root: p}
		if t, err := time.Parse(time.RFC3339Nano, c.meta.LastUpdated); err == nil {
			in.Updated = t
		} else if fi, err := os.Stat(p); err == nil {
			in.Updated = fi.ModTime()
		}
		// Of several files with one id gemini keeps the one updated last, and
		// the first in name order on a tie (getSessionFiles).
		if i, ok := seen[in.ID]; ok {
			if in.Updated.After(out[i].Updated) {
				out[i] = in
			}
			continue
		}
		seen[in.ID] = len(out)
		out = append(out, in)
	}
	transcript.SortNewest(out)
	return out, nil
}

// projectRoot reads the .project_root marker gemini keeps in a project
// directory.
func projectRoot(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, marker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (st *store) Read(ctx context.Context, id string) (*transcript.Session, error) {
	infos, err := st.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, in := range infos {
		if in.ID != id {
			continue
		}
		c, err := replay(in.Path)
		if err != nil {
			return nil, err
		}
		file, err := os.ReadFile(in.Path)
		if err != nil {
			return nil, err
		}
		s := &transcript.Session{ID: id, Agent: "gemini", CWD: in.CWD, Updated: in.Updated}
		if t, err := time.Parse(time.RFC3339Nano, c.meta.StartTime); err == nil {
			s.Created = t
		}
		v := &Vendor{FileName: filepath.Base(in.Path), File: file, Metadata: c.fields}
		for _, m := range c.messages() {
			e, ok, err := decode(m.raw, s)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			e.Raw = m.raw
			s.Entries = append(s.Entries, e)
			v.IDs = append(v.IDs, m.id)
		}
		s.Vendor = v
		return s, nil
	}
	return nil, transcript.ErrNotFound
}

// conversation is a file replayed the way gemini loads it.
type conversation struct {
	meta   conversationHeader
	fields map[string]json.RawMessage
	order  []string
	byID   map[string]json.RawMessage
}

type message struct {
	id  string
	raw json.RawMessage
}

func (c *conversation) messages() []message {
	out := make([]message, 0, len(c.order))
	for _, id := range c.order {
		if raw, ok := c.byID[id]; ok {
			out = append(out, message{id, raw})
		}
	}
	return out
}

// resumable reports whether any message is one gemini would offer to resume
// (core services/chatRecordingService.ts, isResumableMessageRecord).
func (c *conversation) resumable() bool {
	for _, m := range c.messages() {
		var r row
		if json.Unmarshal(m.raw, &r) != nil {
			continue
		}
		content, err := parts(r.Content)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(partsString(content))
		switch r.Type {
		case "user":
			if sentOnResume(text) {
				return true
			}
		case "gemini":
			if text != "" || len(r.ToolCalls) > 0 || len(r.Thoughts) > 0 {
				return true
			}
		}
	}
	return false
}

func (c *conversation) put(id string, raw json.RawMessage) {
	if _, ok := c.byID[id]; !ok {
		c.order = append(c.order, id)
	}
	c.byID[id] = raw
}

func (c *conversation) reset() {
	c.order, c.byID = nil, map[string]json.RawMessage{}
}

func (c *conversation) merge(fields map[string]json.RawMessage) {
	for k, v := range fields {
		if k == "messages" {
			continue
		}
		c.fields[k] = v
	}
}

// replay rebuilds a conversation from its file, record by record, as
// loadConversationRecord does. Lines that do not parse are skipped, as
// gemini skips them.
func replay(path string) (*conversation, error) {
	c := &conversation{fields: map[string]json.RawMessage{}}
	c.reset()
	err := transcript.EachLine(path, func(line json.RawMessage) (bool, error) {
		var rec map[string]json.RawMessage
		if json.Unmarshal(line, &rec) != nil {
			return true, nil
		}
		switch {
		case rec["$rewindTo"] != nil:
			var to string
			if json.Unmarshal(rec["$rewindTo"], &to) != nil {
				return true, nil
			}
			i := indexOf(c.order, to)
			if i < 0 {
				c.reset()
				return true, nil
			}
			for _, id := range c.order[i:] {
				delete(c.byID, id)
			}
			c.order = c.order[:i]
		case isMessage(rec):
			var id string
			_ = json.Unmarshal(rec["id"], &id)
			c.put(id, append(json.RawMessage(nil), line...))
		case rec["$set"] != nil:
			var set map[string]json.RawMessage
			if json.Unmarshal(rec["$set"], &set) != nil {
				return true, nil
			}
			if msgs, ok := set["messages"]; ok {
				var list []json.RawMessage
				if json.Unmarshal(msgs, &list) == nil {
					c.reset()
					for _, m := range list {
						var mr map[string]json.RawMessage
						if json.Unmarshal(m, &mr) == nil && isMessage(mr) {
							var id string
							_ = json.Unmarshal(mr["id"], &id)
							c.put(id, m)
						}
					}
				}
			}
			c.merge(set)
		default:
			// The metadata record, which may itself carry messages.
			c.merge(rec)
			if msgs, ok := rec["messages"]; ok {
				var list []json.RawMessage
				if json.Unmarshal(msgs, &list) == nil {
					for _, m := range list {
						var mr map[string]json.RawMessage
						if json.Unmarshal(m, &mr) == nil && isMessage(mr) {
							var id string
							_ = json.Unmarshal(mr["id"], &id)
							c.put(id, m)
						}
					}
				}
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	meta, err := json.Marshal(c.fields)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(meta, &c.meta); err != nil {
		return nil, err
	}
	return c, nil
}

// isMessage is gemini's test for a message record: an id and a type.
func isMessage(rec map[string]json.RawMessage) bool {
	var id, typ string
	return json.Unmarshal(rec["id"], &id) == nil && id != "" && json.Unmarshal(rec["type"], &typ) == nil && typ != ""
}

func indexOf(ids []string, id string) int {
	for i, x := range ids {
		if x == id {
			return i
		}
	}
	return -1
}

// Write persists a session. A session read from the store keeps its file:
// unchanged, the file is left byte for byte; with entries appended, their
// records are appended to it, which gemini's replay adds after the rest.
// Anything else (a session from elsewhere, or one whose read entries were
// removed or reordered) is written as a new file: the metadata record, with
// the projectHash gemini requires, then one record per message.
//
// Appending races gemini if it has the session open: gemini checkpoints its
// in-memory list with {"$set": {"messages": ...}}, which would drop records
// appended after it loaded. Write while gemini is not running the session.
func (st *store) Write(ctx context.Context, s *transcript.Session) (string, error) {
	// gemini records neither a compaction nor an entry it shows and never
	// sends in a form that lasts: when it compresses, and when it loads a
	// session, it rewrites the message list from the history it sends the
	// model (geminiChat.ts initialize and setHistory,
	// chatRecordingService.ts updateMessagesFromHistory), dropping every
	// record the model is not given. So another agent's compaction is
	// applied, the summary in place of what it retired.
	if s.Agent != "gemini" {
		s = s.Portable().Lower(transcript.Capabilities{})
	}
	if s.CWD == "" {
		return "", errors.New("gemini: a session needs its project root (CWD): gemini files sessions under it")
	}
	transcript.AssignIDs(s, IDs, false)
	if s.ID == "" {
		s.ID = transcript.NewUUID()
	}
	now := time.Now()
	if s.Created.IsZero() {
		s.Created = now
	}
	if s.Updated.IsZero() {
		s.Updated = now
	}
	slug, err := claimProject(st.home, s.CWD)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(st.tmp(), slug, "chats")
	msgs := s.Messages()

	if v, ok := s.Vendor.(*Vendor); ok && v.File != nil && v.FileName != "" && keepsPrefix(msgs, v.IDs) {
		out := append([]byte(nil), v.File...)
		added := false
		for _, e := range msgs[len(v.IDs):] {
			row, err := encode(e, s)
			if err != nil {
				return "", err
			}
			if row == nil {
				continue
			}
			out = appendLine(out, row)
			added = true
		}
		if added {
			set, err := json.Marshal(map[string]map[string]string{"$set": {"lastUpdated": stamp(now)}})
			if err != nil {
				return "", err
			}
			out = appendLine(out, set)
		}
		path := filepath.Join(dir, v.FileName)
		if cur, err := os.ReadFile(path); err == nil && string(cur) == string(out) {
			return s.ID, nil
		}
		return s.ID, writeFile(path, out)
	}

	meta := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Metadata {
			meta[k] = val
		}
	}
	for k, val := range map[string]string{
		"sessionId":   s.ID,
		"projectHash": ProjectHash(s.CWD),
		"startTime":   stamp(s.Created),
		"lastUpdated": stamp(s.Updated),
	} {
		b, err := json.Marshal(val)
		if err != nil {
			return "", err
		}
		meta[k] = b
	}
	if _, ok := meta["kind"]; !ok {
		meta["kind"] = json.RawMessage(`"main"`)
	}
	first, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	out := appendLine(nil, first)
	for _, e := range msgs {
		row := e.Raw
		if row == nil {
			if row, err = encode(e, s); err != nil {
				return "", err
			}
		}
		if row != nil {
			out = appendLine(out, row)
		}
	}
	name := "session-" + fileMinute(s.Created, now).Format("2006-01-02T15-04") + "-" + prefix(s.ID) + ".jsonl"
	if v, ok := s.Vendor.(*Vendor); ok && v.FileName != "" {
		name = v.FileName
	}
	return s.ID, writeFile(filepath.Join(dir, name), out)
}

// fileMinute is the minute a new session file is named for. gemini 0.61's
// session/load starts a new recording for the session before resolving it,
// named for the current minute; if the session's own file carries that
// minute, the recording's first checkpoint replaces the conversation in it
// and the load fails with "Invalid session identifier" (reproduced by the
// harness-test seed probe, deterministic within the minute). Gemini accepts
// a file named for any minute, so a session created in the current minute is
// named for the one before, which no load can collide with.
func fileMinute(created, now time.Time) time.Time {
	m := created.UTC().Truncate(time.Minute)
	if cur := now.UTC().Truncate(time.Minute); !m.Before(cur) {
		m = cur.Add(-time.Minute)
	}
	return m
}

// keepsPrefix reports whether the session's messages begin with the ones read
// from the file, unchanged and in order.
func keepsPrefix(msgs []transcript.Entry, ids []string) bool {
	if len(msgs) < len(ids) {
		return false
	}
	for i, id := range ids {
		if msgs[i].Raw == nil || msgs[i].ID != id {
			return false
		}
	}
	for _, e := range msgs[len(ids):] {
		if e.Raw != nil {
			return false
		}
	}
	return true
}

func appendLine(b []byte, row []byte) []byte {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	b = append(b, row...)
	return append(b, '\n')
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// claimProject returns the directory name gemini uses for a project root,
// claiming one the way gemini's ProjectRegistry does when the root has none:
// the root's base name, lowercased with every other character a dash, then
// -1, -2 and so on past names another root holds in projects.json or by a
// .project_root marker. The claim is recorded in projects.json and marked in
// both of gemini's base directories, under projects.json's lock.
func claimProject(home, root string) (string, error) {
	gdir := filepath.Join(home, geminiDir)
	bases := []string{filepath.Join(gdir, "tmp"), filepath.Join(gdir, "history")}
	registry := filepath.Join(gdir, registryFn)
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		return "", err
	}
	unlock, err := lockRegistry(registry)
	if err != nil {
		return "", err
	}
	defer unlock()

	data := struct {
		Projects map[string]string `json:"projects"`
	}{Projects: map[string]string{}}
	switch raw, err := os.ReadFile(registry); {
	case err == nil:
		if err := json.Unmarshal(raw, &data); err != nil {
			return "", fmt.Errorf("gemini: %s: %w", registry, err)
		}
		if data.Projects == nil {
			data.Projects = map[string]string{}
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	owner := func(slug string) string {
		for _, b := range bases {
			if o := projectRoot(filepath.Join(b, slug)); o != "" {
				return o
			}
		}
		return ""
	}
	slug, ok := data.Projects[root]
	if !ok || (owner(slug) != "" && owner(slug) != root) {
		taken := map[string]bool{}
		for p, s := range data.Projects {
			if p != root {
				taken[s] = true
			}
		}
		base := slugify(filepath.Base(root))
		for n := 0; ; n++ {
			slug = base
			if n > 0 {
				slug = fmt.Sprintf("%s-%d", base, n)
			}
			if o := owner(slug); !taken[slug] && (o == "" || o == root) {
				break
			}
		}
		data.Projects[root] = slug
		out, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return "", err
		}
		if err := writeFile(registry, out); err != nil {
			return "", err
		}
	}
	for _, b := range bases {
		dir := filepath.Join(b, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if projectRoot(dir) == "" {
			if err := os.WriteFile(filepath.Join(dir, marker), []byte(root), 0o644); err != nil {
				return "", err
			}
		}
	}
	return slug, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify is ProjectRegistry.slugify.
func slugify(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		return "project"
	}
	return s
}

// lockRegistry takes projects.json's lock the way gemini's proper-lockfile
// does, by creating <file>.lock as a directory, waiting while another
// process holds it.
func lockRegistry(path string) (func(), error) {
	lock := path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			return func() { os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) || time.Now().After(deadline) {
			return nil, fmt.Errorf("gemini: lock %s: %w", path, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type row struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	// DisplayContent is what gemini's history view shows in place of
	// Content, when the two differ.
	DisplayContent json.RawMessage `json:"displayContent,omitempty"`
	Thoughts       []thought       `json:"thoughts,omitempty"`
	ToolCalls      []toolCall      `json:"toolCalls,omitempty"`
	Model          string          `json:"model,omitempty"`
}

// part is one element of a record's content, a @google/genai Part.
type part struct {
	Text             string            `json:"text,omitempty"`
	Thought          json.RawMessage   `json:"thought,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	// The media and code parts, which only name themselves when a record is
	// rendered as text.
	VideoMetadata       json.RawMessage `json:"videoMetadata,omitempty"`
	CodeExecutionResult json.RawMessage `json:"codeExecutionResult,omitempty"`
	ExecutableCode      json.RawMessage `json:"executableCode,omitempty"`
	FileData            *fileData       `json:"fileData,omitempty"`
	InlineData          *blob           `json:"inlineData,omitempty"`
}

// blob is an inlineData part: an image or file's bytes in base64, as
// gemini records one read into a prompt (an ACP image, an @ reference) or
// returned by a tool.
type blob struct {
	MimeType    string `json:"mimeType"`
	Data        string `json:"data"`
	DisplayName string `json:"displayName,omitempty"`
}

// fileData is a fileData part: a file by reference.
type fileData struct {
	MimeType    string `json:"mimeType,omitempty"`
	FileURI     string `json:"fileUri"`
	DisplayName string `json:"displayName,omitempty"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
	// Parts are images a tool returned, nested for a model that takes
	// multimodal function responses (convertToFunctionResponse in core
	// utils/generateContentResponseUtilities.ts).
	Parts []part `json:"parts,omitempty"`
}

type thought struct {
	Subject     string `json:"subject,omitempty"`
	Description string `json:"description,omitempty"`
}

type toolCall struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	Status string          `json:"status,omitempty"`
}

func prefix(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// parts reads a record's content, a PartListUnion: a string, one part, or a
// list of parts and strings. gemini writes a model turn's content as a
// string, and as parts once its history has been synced back into the file
// (on resume, or after /compress), so both shapes occur in one file.
func parts(raw json.RawMessage) ([]part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		// An empty string is falsy to gemini and so no part at all.
		if text == "" {
			return nil, nil
		}
		return []part{{Text: text}}, nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		list = []json.RawMessage{raw}
	}
	out := make([]part, 0, len(list))
	for _, item := range list {
		var p part
		if json.Unmarshal(item, &text) == nil {
			p.Text = text
		} else if err := json.Unmarshal(item, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// isThought reports whether a part is model reasoning. The field is a
// boolean in the API; gemini tests only that it is present.
func (p part) isThought() bool { return len(p.Thought) > 0 && string(p.Thought) != "false" }

// String is gemini's partListUnionToString for one part (utils/partUtils.ts,
// partToString with verbose set): the text, or a bracketed name for a part
// that has none. Whether a record is sent to the model and shown to the
// person is decided on this string.
func (p part) String() string {
	switch {
	case p.VideoMetadata != nil:
		return "[Video Metadata]"
	case p.Thought != nil:
		return "[Thought: " + string(p.Thought) + "]"
	case p.CodeExecutionResult != nil:
		return "[Code Execution Result]"
	case p.ExecutableCode != nil:
		return "[Executable Code]"
	case p.FileData != nil:
		return "[File Data]"
	case p.FunctionCall != nil:
		return "[Function Call: " + p.FunctionCall.Name + "]"
	case p.FunctionResponse != nil:
		return "[Function Response: " + p.FunctionResponse.Name + "]"
	case p.InlineData != nil:
		return "[Media]"
	}
	return p.Text
}

func partsString(ps []part) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.String())
	}
	return b.String()
}

// Internal context gemini keeps as user records. Neither is shown in its
// history view (cli utils/sessionUtils.ts, convertSessionToHistoryFormats).
const (
	sessionContext = "<session_context>"
	hookContext    = "<hook_context>"
)

// sentOnResume is gemini's test for a user record it leaves out of the
// history a resumed session gives the model (core utils/sessionUtils.ts,
// isIgnoredUserContent): an empty message, a slash or ? command, or its own
// injected context.
func sentOnResume(text string) bool {
	return text != "" && !strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "?") &&
		!strings.HasPrefix(text, sessionContext) && !strings.HasPrefix(text, hookContext)
}

// audience combines whether gemini gives a record to the model on resume and
// whether its history view shows it.
func audience(model, user bool) transcript.Audience {
	switch {
	case model && user:
		return transcript.AudienceAll
	case model:
		return transcript.AudienceModel
	case user:
		return transcript.AudienceUser
	}
	return transcript.AudienceNone
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.ID == "" || r.Type == "" {
		// A patch row or a repeated header.
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.ID}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.ID, err)
		}
		e.Time = t
	}
	if r.Type != "user" && r.Type != "gemini" {
		// info, error and warning records are the CLI's notices. The history
		// view shows them; the model never gets them.
		return transcript.Entry{}, false, nil
	}
	content, err := parts(r.Content)
	if err != nil {
		return transcript.Entry{}, false, fmt.Errorf("row %s: content: %w", r.ID, err)
	}
	text := strings.TrimSpace(partsString(content))
	shown := text
	if display, err := parts(r.DisplayContent); err == nil && partsString(display) != "" {
		shown = strings.TrimSpace(partsString(display))
	}
	switch r.Type {
	case "user":
		e.Role = transcript.RoleUser
		blocks, answered, err := userBlocks(content)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: %w", r.ID, err)
		}
		e.Content = blocks
		if answered > 0 && answered == len(content) {
			e.Role = transcript.RoleTool
		}
		if e.Content, e.ModelContent, err = ownContent(e.Content, r.DisplayContent); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: displayContent: %w", r.ID, err)
		}
		// The session context is the one record gemini leaves out on resume
		// and yet gives the model: it starts every resumed chat with a fresh
		// copy under the same id (core utils/environmentContext.ts,
		// getInitialChatHistory), which the next checkpoint writes back.
		model := sentOnResume(text) || strings.HasPrefix(text, sessionContext)
		user := shown != "" && !strings.HasPrefix(shown, sessionContext) && !strings.HasPrefix(shown, hookContext)
		e.Audience = audience(model, user)
	case "gemini":
		e.Role = transcript.RoleAssistant
		// A record whose content carries calls or thoughts is the model turn
		// as sent; an older one keeps them beside the text, and gemini
		// rebuilds the turn as thoughts, text, then calls (core
		// utils/sessionUtils.ts, convertSessionToClientHistory).
		modern := false
		for _, p := range content {
			if p.FunctionCall != nil || p.isThought() {
				modern = true
			}
		}
		if !modern {
			for _, th := range r.Thoughts {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: strings.TrimSpace(th.Subject + "\n" + th.Description)})
			}
		}
		for _, p := range content {
			switch {
			case p.FunctionCall != nil:
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Input: p.FunctionCall.Args})
			case p.isThought():
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
			case p.InlineData != nil || p.FileData != nil:
				b, err := media(p, "")
				if err != nil {
					return transcript.Entry{}, false, fmt.Errorf("row %s: %w", r.ID, err)
				}
				e.Content = append(e.Content, b)
			case p.Text != "":
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
			}
		}
		if !modern {
			for _, tc := range r.ToolCalls {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tc.ID, Name: tc.Name, Input: tc.Args})
			}
		}
		// The history view shows a turn's thoughts, but gemini strips every
		// thought part from the history it sends (core/geminiChat.ts
		// getHistoryTurns, stripThoughts, and scrubHistory with context
		// management on; only a pre-2 Gemini model kept them), so the model
		// is given the turn without them. A turn with nothing left to send
		// is dropped from the resumed history, and one with nothing at all
		// has nothing to show either.
		e.ModelContent = withoutReasoning(e.Content)
		sent := len(r.ToolCalls) > 0
		for _, p := range content {
			sent = sent || !p.isThought()
		}
		extra := len(r.Thoughts) > 0 || len(r.ToolCalls) > 0
		e.Audience = audience(sent, shown != "" || extra)
	}
	return e, true, nil
}

// userBlocks reads a user record's parts. A tool's images and files come in
// its functionResponse's parts, or, for a model that takes none there, as
// the parts right after it (convertToFunctionResponse returns the response
// and then its media, and a record holds each call's in turn); either way
// they follow the result with its call's id. answered counts the parts
// that are a tool's: its response and what it returned.
func userBlocks(content []part) (blocks []transcript.Block, answered int, err error) {
	toolID := "" // the call whose response came last
	for _, p := range content {
		switch {
		case p.FunctionResponse != nil:
			fr := p.FunctionResponse
			answered++
			toolID = fr.ID
			blocks = append(blocks, transcript.Block{
				Kind: transcript.BlockToolResult, ToolID: fr.ID, Name: fr.Name,
				Text: responseText(fr.Response), Status: transcript.StatusOK,
			})
			for _, np := range fr.Parts {
				if np.InlineData == nil && np.FileData == nil {
					continue
				}
				b, err := media(np, fr.ID)
				if err != nil {
					return nil, 0, err
				}
				blocks = append(blocks, b)
			}
		case p.InlineData != nil || p.FileData != nil:
			b, err := media(p, toolID)
			if err != nil {
				return nil, 0, err
			}
			if toolID != "" {
				answered++
			}
			blocks = append(blocks, b)
		default:
			toolID = ""
			blocks = append(blocks, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
	}
	return blocks, answered, nil
}

// media reads an inlineData or fileData part as an image or file block.
// toolID is set for one a tool returned.
func media(p part, toolID string) (transcript.Block, error) {
	if p.InlineData != nil {
		data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
		if err != nil {
			return transcript.Block{}, fmt.Errorf("inlineData: %w", err)
		}
		return transcript.Block{Kind: mediaKind(p.InlineData.MimeType), ToolID: toolID, MediaType: p.InlineData.MimeType, Data: data, Name: p.InlineData.DisplayName}, nil
	}
	return transcript.Block{Kind: mediaKind(p.FileData.MimeType), ToolID: toolID, MediaType: p.FileData.MimeType, URI: p.FileData.FileURI, Name: p.FileData.DisplayName}, nil
}

// mediaKind is the block an attachment of a media type is: an image, or any
// other file.
func mediaKind(mediaType string) transcript.BlockKind {
	if strings.HasPrefix(mediaType, "image/") {
		return transcript.BlockImage
	}
	return transcript.BlockFile
}

// mediaPart is the part gemini records an image or file as: inlineData for
// bytes, fileData for a reference. A block with neither has no part.
func mediaPart(b transcript.Block) (part, bool) {
	switch {
	case b.Data != nil:
		return part{InlineData: &blob{MimeType: b.MediaType, Data: base64.StdEncoding.EncodeToString(b.Data), DisplayName: b.Name}}, true
	case b.URI != "":
		return part{FileData: &fileData{MimeType: b.MediaType, FileURI: b.URI, DisplayName: b.Name}}, true
	}
	return part{}, false
}

// responseText reads a functionResponse.response, which Gemini tools fill
// with an output field and other tools with anything.
func responseText(raw json.RawMessage) string {
	var out struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Output != "" {
		return out.Output
	}
	return string(raw)
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	if e.ID == "" {
		e.ID = transcript.NewUUID()
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	r := row{ID: e.ID, Timestamp: stamp(t)}
	switch e.Role {
	case transcript.RoleUser, transcript.RoleTool, transcript.RoleSystem:
		r.Type = "user"
		var parts []part
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				parts = append(parts, part{Text: b.Text})
			case transcript.BlockToolResult:
				resp, err := json.Marshal(struct {
					Output string `json:"output"`
				}{b.Text})
				if err != nil {
					return nil, err
				}
				parts = append(parts, part{FunctionResponse: &functionResponse{ID: b.ToolID, Name: b.Name, Response: resp}})
			case transcript.BlockImage, transcript.BlockFile:
				// A tool's media follows its response as parts of their
				// own, the shape gemini gives every model; nested parts
				// are only for models that take them.
				if p, ok := mediaPart(b); ok {
					parts = append(parts, p)
				}
			}
		}
		if len(parts) == 0 {
			return nil, nil
		}
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		r.Content = content
	case transcript.RoleAssistant:
		// A model turn is written in the legacy shape, text with thoughts
		// and calls beside it, which has no place for an image the model
		// made; one is dropped.
		r.Type = "gemini"
		var text strings.Builder
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				text.WriteString(b.Text)
			case transcript.BlockReasoning:
				r.Thoughts = append(r.Thoughts, thought{Description: b.Text})
			case transcript.BlockToolUse:
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				r.ToolCalls = append(r.ToolCalls, toolCall{ID: b.ToolID, Name: b.Name, Args: args, Status: "success"})
			}
		}
		content, err := json.Marshal(text.String())
		if err != nil {
			return nil, err
		}
		r.Content = content
	default:
		return nil, nil
	}
	return json.Marshal(r)
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// withoutReasoning is content without its reasoning blocks, or nil when it
// has none.
func withoutReasoning(content []transcript.Block) []transcript.Block {
	if !slices.ContainsFunc(content, func(b transcript.Block) bool { return b.Kind == transcript.BlockReasoning }) {
		return nil
	}
	out := []transcript.Block{}
	for _, b := range content {
		if b.Kind != transcript.BlockReasoning {
			out = append(out, b)
		}
	}
	return out
}

// ownContent splits a user record's blocks into what the person wrote and
// what the model was given. gemini records the first as displayContent when
// they differ; without it, a hook's additional context is a part of its own
// after the prompt (core/client.ts wraps it in <hook_context>), which the
// model is given and is not the person's. ModelContent is nil when the two
// are the same.
func ownContent(content []transcript.Block, display json.RawMessage) (own, model []transcript.Block, err error) {
	if ps, err := parts(display); err == nil && strings.TrimSpace(partsString(ps)) != "" {
		own, _, err := userBlocks(ps)
		if err != nil {
			return nil, nil, err
		}
		return own, content, nil
	}
	for _, b := range content {
		if b.Kind == transcript.BlockText && strings.HasPrefix(strings.TrimSpace(b.Text), hookContext) {
			continue
		}
		own = append(own, b)
	}
	if len(own) == len(content) || len(own) == 0 {
		return content, nil, nil
	}
	return own, content, nil
}
