package transcript

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// JSONL is a store of one file per session, one JSON object per line. It is
// the shape of most agents' stores, and the fields on it are the only places
// they differ. A codec for such an agent sets the fields and nothing else.
//
// Layout answers where files are. Decode and Encode answer what a row is.
// Header, WriteHeader and After cover the few agents that need more.
type JSONL struct {
	// Agent is the agent's id. Read stamps it on every session; Write keeps
	// the Raw rows of a session only when it carries this id, and takes any
	// other session through Session.Portable.
	Agent string

	// Layout locates session files under a home directory.
	Layout Layout

	// Header parses the first row when the format starts with one. It fills
	// the session's id, cwd, times and Meta, and returns true when the row
	// was a header and not a message. Optional.
	Header func(row json.RawMessage, s *Session) (bool, error)

	// Decode turns one row into an entry. It returns ok=false for a row that
	// is not a message; such rows are kept on the session as opaque entries
	// for round trips. A tree-shaped store may instead return ok=true with
	// an empty Role and the row's ID and ParentID, so the row stays opaque
	// but keeps its place in the chain. Decode may also fill session fields
	// it finds on message rows, such as cwd or timestamps on agents that
	// stamp every row.
	Decode func(row json.RawMessage, s *Session) (e Entry, ok bool, err error)

	// Finish runs once every row of a session is decoded, for formats whose
	// rows mean something only together: one message split across rows, a
	// marker that pins the leaf or undoes turns, links the agent repairs in
	// memory when it loads. It sets Audience, Compaction, Leaf and the links
	// on entries the way the agent's loader reads them; it never touches Raw.
	// Optional.
	Finish func(s *Session) error

	// Encode turns an entry into a row for a session that did not come from
	// this agent. Entries with Raw set are written as they were and never
	// reach Encode. Nil means the store is read-only.
	Encode func(e Entry, s *Session) (json.RawMessage, error)

	// WriteHeader emits the header rows for a written session, when the
	// format has any. Optional.
	WriteHeader func(s *Session) ([]json.RawMessage, error)

	// Prepare runs before anything is written, with the store's home, for a
	// format whose write needs values only the agent's own store holds, such
	// as the version stamp its sessions carry. It may fill session fields,
	// Vendor included. Optional.
	Prepare func(home string, s *Session) error

	// After runs once the session file is written, for agents that keep an
	// index or sidecar beside it. Optional.
	After func(ctx context.Context, path string, s *Session) error

	// NewID mints a session id when a written session has none. Optional;
	// the default is a UUID.
	NewID func() string

	// Tree marks a format whose rows link to their parent. Before a write,
	// every entry without Raw that has no ParentID gets the ID of the entry
	// before it, so Encode can emit the link. This holds for a foreign
	// session and for entries appended to one read from the agent.
	Tree bool

	// IDs is the agent's scheme for entry ids. Entries this write creates
	// get ids in it; see AssignIDs. The zero value is UUIDs.
	IDs IDScheme
}

// Layout says where an agent keeps session files. The common case is a
// directory of <id><Ext> files under Root, in a subdirectory named after the
// working directory, and Root, Project and Ext describe it. The three
// functions override discovery, naming and placement for agents that do it
// differently, and Peek reads what the file name does not say.
type Layout struct {
	// Root is the sessions directory, relative to the home directory.
	Root string
	// Project is how the working directory names a subdirectory of Root.
	Project ProjectDir
	// Ext is the file extension of a session file, including the dot.
	Ext string

	// Files lists candidate session files for a cwd, or for every cwd when
	// it is empty. Optional; the default is every <Ext> file in the
	// directory Root, Project and the cwd name.
	Files func(home, cwd string) ([]string, error)
	// PathFor is where a written session goes. Optional; the default is
	// <dir>/<id><Ext> for the session's cwd.
	PathFor func(home string, s *Session) string
	// IDFromName extracts the session id from a file name without its
	// extension. Optional; the default is the whole base name. A Peek that
	// returns an id overrides it.
	IDFromName func(name string) string
	// Peek reads a file's identity without decoding it: id, cwd and title as
	// far as the header says. Required when the id is not in the file name,
	// and used to filter by cwd when the directory does not encode it.
	Peek func(path string) (Info, error)
	// SessionRoot is the file or directory that belongs to one session
	// alone, given its session file; see Info.Root. Optional; the default is
	// the session file itself.
	SessionRoot func(path string) string
	// Holders lists the pids the agent's in-use markers name for the session
	// with this file; see Info.Holders. Optional.
	Holders func(path string) []int
	// Serving reads the agent's whole registry of which process holds which
	// session under home; see ServingLister. Optional.
	Serving func(home string) map[int]string
}

// ProjectDir is how an agent derives a per-project directory name from the
// working directory.
type ProjectDir int

const (
	// NoProjectDir keeps every session directly under Root.
	NoProjectDir ProjectDir = iota
	// MangledCwd turns every path separator into a dash: /a/b is -a-b.
	// claude and droid do this.
	MangledCwd
	// CwdBase uses the last path element: /a/b is b. gemini does this.
	CwdBase
	// EscapedCwd percent-encodes the path: /a/b is %2Fa%2Fb. grok does this.
	EscapedCwd
	// DashWrappedCwd drops the leading separator, turns separators and
	// colons into dashes, and wraps the result in double dashes: /a/b is
	// --a-b--. pi does this.
	DashWrappedCwd
)

// Name returns the directory name for a working directory.
func (p ProjectDir) Name(cwd string) string {
	switch p {
	case MangledCwd:
		return strings.NewReplacer("/", "-", "\\", "-").Replace(cwd)
	case CwdBase:
		return filepath.Base(cwd)
	case EscapedCwd:
		return url.PathEscape(cwd)
	case DashWrappedCwd:
		trimmed := strings.TrimLeft(cwd, "/\\")
		return "--" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(trimmed) + "--"
	default:
		return ""
	}
}

// Dir returns the session directory for a working directory. An empty cwd
// with a per-project layout returns a glob pattern over every project.
func (l Layout) Dir(home, cwd string) string {
	root := filepath.Join(home, l.Root)
	if l.Project == NoProjectDir {
		return root
	}
	if cwd == "" {
		return filepath.Join(root, "*")
	}
	return filepath.Join(root, l.Project.Name(cwd))
}

// Open binds the store to a home directory.
func (j JSONL) Open(home string) (Store, error) {
	if j.Layout.Files == nil && (j.Layout.Root == "" || j.Layout.Ext == "") {
		return nil, errors.New("transcript: JSONL layout needs Root and Ext, or Files")
	}
	if j.Decode == nil {
		return nil, errors.New("transcript: JSONL needs Decode")
	}
	return &jsonlStore{cfg: j, home: home}, nil
}

type jsonlStore struct {
	cfg  JSONL
	home string
}

// files lists the candidate files for a cwd.
func (st *jsonlStore) files(cwd string) ([]string, error) {
	if st.cfg.Layout.Files != nil {
		return st.cfg.Layout.Files(st.home, cwd)
	}
	return Glob(filepath.Join(st.cfg.Layout.Dir(st.home, cwd), "*"+st.cfg.Layout.Ext))
}

func (st *jsonlStore) idOf(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), st.cfg.Layout.Ext)
	if st.cfg.Layout.IDFromName != nil {
		return st.cfg.Layout.IDFromName(base)
	}
	return base
}

func (st *jsonlStore) List(ctx context.Context, cwd string) ([]Info, error) {
	paths, err := st.files(cwd)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		in := Info{ID: st.idOf(p), CWD: cwd, Updated: fi.ModTime(), Path: p, Root: p}
		if st.cfg.Layout.SessionRoot != nil {
			in.Root = st.cfg.Layout.SessionRoot(p)
		}
		if st.cfg.Layout.Holders != nil {
			in.Holders = st.cfg.Layout.Holders(p)
		}
		if st.cfg.Layout.Peek != nil {
			peeked, err := st.cfg.Layout.Peek(p)
			if err != nil {
				continue
			}
			if peeked.ID != "" {
				in.ID = peeked.ID
			}
			if peeked.CWD != "" {
				in.CWD = peeked.CWD
			}
			in.Title = peeked.Title
			if !peeked.Updated.IsZero() {
				in.Updated = peeked.Updated
			}
			if cwd != "" && in.CWD != "" && in.CWD != cwd {
				continue
			}
		}
		out = append(out, in)
	}
	SortNewest(out)
	return out, nil
}

// Serving implements ServingLister when the layout knows the agent's
// registry, and reports nothing otherwise.
func (st *jsonlStore) Serving(ctx context.Context) (map[int]string, error) {
	if st.cfg.Layout.Serving == nil {
		return nil, nil
	}
	return st.cfg.Layout.Serving(st.home), nil
}

func (st *jsonlStore) find(ctx context.Context, id string) (string, error) {
	infos, err := st.List(ctx, "")
	if err != nil {
		return "", err
	}
	for _, in := range infos {
		if in.ID == id {
			return in.Path, nil
		}
	}
	return "", ErrNotFound
}

func (st *jsonlStore) Read(ctx context.Context, id string) (*Session, error) {
	path, err := st.find(ctx, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &Session{ID: id, Agent: st.cfg.Agent}
	if fi, err := f.Stat(); err == nil {
		s.Updated = fi.ModTime()
	}
	if err := st.decodeAll(f, s); err != nil {
		return nil, err
	}
	if st.cfg.Finish != nil {
		if err := st.cfg.Finish(s); err != nil {
			return nil, fmt.Errorf("transcript: %s: %w", id, err)
		}
	}
	return s, nil
}

func (st *jsonlStore) decodeAll(r *os.File, s *Session) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	first := true
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		row := json.RawMessage(append([]byte(nil), line...))
		if first && st.cfg.Header != nil {
			first = false
			isHeader, err := st.cfg.Header(row, s)
			if err != nil {
				return fmt.Errorf("transcript: header: %w", err)
			}
			if isHeader {
				s.Entries = append(s.Entries, Entry{Role: RoleOpaque, Raw: row})
				continue
			}
		}
		first = false
		e, ok, err := st.cfg.Decode(row, s)
		if err != nil {
			return fmt.Errorf("transcript: row: %w", err)
		}
		if !ok {
			e = Entry{Role: RoleOpaque}
		}
		e.Raw = row
		s.Entries = append(s.Entries, e)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if s.Created.IsZero() {
		for _, e := range s.Entries {
			if !e.Time.IsZero() {
				s.Created = e.Time
				break
			}
		}
	}
	return nil
}

func (st *jsonlStore) pathFor(s *Session) string {
	if st.cfg.Layout.PathFor != nil {
		return st.cfg.Layout.PathFor(st.home, s)
	}
	return filepath.Join(st.cfg.Layout.Dir(st.home, s.CWD), s.ID+st.cfg.Layout.Ext)
}

func (st *jsonlStore) Write(ctx context.Context, s *Session) (string, error) {
	// A store without an encoder is read-only for every session, its own
	// included: such a format is derived from something the agent does not
	// read back, so rewriting even its own rows would change nothing.
	if st.cfg.Encode == nil {
		return "", ErrReadOnly
	}
	if s.Agent != st.cfg.Agent {
		s = s.Portable()
	}
	if st.cfg.Prepare != nil {
		if err := st.cfg.Prepare(st.home, s); err != nil {
			return "", err
		}
	}
	foreign := len(s.Entries) == 0 || s.Entries[0].Raw == nil
	if s.ID == "" {
		if st.cfg.NewID != nil {
			s.ID = st.cfg.NewID()
		} else {
			s.ID = NewUUID()
		}
	}
	if s.Created.IsZero() {
		s.Created = time.Now()
	}
	if s.Updated.IsZero() {
		s.Updated = s.Created
	}
	AssignIDs(s, st.cfg.IDs, st.cfg.Tree)

	var buf bytes.Buffer
	if foreign && st.cfg.WriteHeader != nil {
		rows, err := st.cfg.WriteHeader(s)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			buf.Write(row)
			buf.WriteByte('\n')
		}
	}
	// An entry the encoder has no row for is left out, and an entry linked
	// to it is linked to its parent instead, so no written row names a
	// parent the file does not hold.
	dropped := map[string]string{}
	for _, e := range s.Entries {
		for {
			to, ok := dropped[e.ParentID]
			if !ok {
				break
			}
			e.ParentID = to
		}
		row := e.Raw
		if row == nil {
			var err error
			row, err = st.cfg.Encode(e, s)
			if err != nil {
				return "", err
			}
		}
		if len(row) == 0 {
			if e.Raw == nil && e.ID != "" {
				dropped[e.ID] = e.ParentID
			}
			continue
		}
		buf.Write(row)
		buf.WriteByte('\n')
	}

	path := st.pathFor(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if st.cfg.After != nil {
		if err := st.cfg.After(ctx, path, s); err != nil {
			return "", err
		}
	}
	return s.ID, nil
}

// PeekFirstLine reads the first non-empty line of a file and hands it to
// fn. It is what most Layout.Peek implementations need.
func PeekFirstLine(path string, fn func(row json.RawMessage) (Info, error)) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		return fn(json.RawMessage(line))
	}
	if err := sc.Err(); err != nil {
		return Info{}, err
	}
	return Info{}, errors.New("transcript: empty file")
}

// EachLine hands every non-empty line of a file to fn until fn returns
// false or the file ends. Peek implementations use it when the header is
// not the first row.
func EachLine(path string, fn func(row json.RawMessage) (more bool, err error)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		more, err := fn(json.RawMessage(line))
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return sc.Err()
}

// PeekField returns the first non-empty string value of key among the first
// maxRows rows of a JSONL file, for Layout.Peek implementations whose format
// stamps a field such as cwd on its rows rather than in a header.
func PeekField(path, key string, maxRows int) string {
	var found string
	n := 0
	_ = EachLine(path, func(row json.RawMessage) (bool, error) {
		n++
		var fields map[string]json.RawMessage
		if json.Unmarshal(row, &fields) == nil {
			var v string
			if json.Unmarshal(fields[key], &v) == nil && v != "" {
				found = v
				return false, nil
			}
		}
		return n < maxRows, nil
	})
	return found
}

// Glob is filepath.Glob with sorted output, for Layout.Files
// implementations.
func Glob(pattern string) ([]string, error) {
	m, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(m)
	return m, nil
}

// IDScheme is how an agent names its entries: New mints an id, Valid says
// whether an id is one the agent would accept. The zero value is UUIDs.
type IDScheme struct {
	New   func() string
	Valid func(string) bool
}

// UUIDs is the scheme most agents use for entry ids.
var UUIDs = IDScheme{New: NewUUID, Valid: IsUUID}

// AssignIDs gives every entry a write creates (no Raw, not opaque) an id in
// the agent's scheme, and keeps links between entries intact. An entry keeps
// its id only if the scheme accepts it: a session imported from another
// agent carries that agent's ids, which this agent may reject (Copilot
// rejects an event whose id is not a UUID). Each replaced id is remapped in
// every ParentID that named it. With tree set, a new entry with no ParentID
// is linked to the entry before it that has an id: the previous new entry,
// or, for the first, the session's Leaf when it pins one and otherwise the
// last row read back with an id.
//
// Codecs that plan a write across entries before encoding call it
// themselves; the JSONL engine calls it for the rest.
func AssignIDs(s *Session, scheme IDScheme, tree bool) {
	if scheme.New == nil {
		scheme = UUIDs
	}
	remap := map[string]string{}
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Raw != nil || e.Role == RoleOpaque {
			continue
		}
		if e.ID == "" || (scheme.Valid != nil && !scheme.Valid(e.ID)) {
			id := scheme.New()
			if e.ID != "" {
				remap[e.ID] = id
			}
			e.ID = id
		}
	}
	prev, first := "", true
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Raw == nil && e.Role != RoleOpaque {
			if to, ok := remap[e.ParentID]; ok {
				e.ParentID = to
			}
			if first && s.Leaf != "" {
				prev = s.Leaf
			}
			first = false
			if tree && e.ParentID == "" {
				e.ParentID = prev
			}
		}
		if e.ID != "" {
			prev = e.ID
		}
	}
}

// IsUUID reports whether s is a UUID in its canonical 8-4-4-4-12 form.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

// NewUUID returns a random version 4 UUID, which is what most agents use as
// a session id.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
