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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	for _, p := range paths {
		c, err := replay(p)
		if err != nil || c.meta.SessionID == "" {
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
	Thoughts  []thought       `json:"thoughts,omitempty"`
	ToolCalls []toolCall      `json:"toolCalls,omitempty"`
	Model     string          `json:"model,omitempty"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type functionResponse struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
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
	switch r.Type {
	case "user":
		var parts []part
		if err := json.Unmarshal(r.Content, &parts); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: content: %w", r.ID, err)
		}
		e.Role = transcript.RoleUser
		responses := 0
		for _, p := range parts {
			if p.FunctionResponse != nil {
				responses++
				e.Content = append(e.Content, transcript.Block{
					Kind: transcript.BlockToolResult, ToolID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name,
					Text: responseText(p.FunctionResponse.Response), Status: transcript.StatusOK,
				})
				continue
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
		if responses > 0 && responses == len(parts) {
			e.Role = transcript.RoleTool
		}
	case "gemini":
		e.Role = transcript.RoleAssistant
		var text string
		if err := json.Unmarshal(r.Content, &text); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: content: %w", r.ID, err)
		}
		for _, th := range r.Thoughts {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: strings.TrimSpace(th.Subject + "\n" + th.Description)})
		}
		if text != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: text})
		}
		for _, tc := range r.ToolCalls {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tc.ID, Name: tc.Name, Input: tc.Args})
		}
	default:
		return transcript.Entry{}, false, nil
	}
	return e, true, nil
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
