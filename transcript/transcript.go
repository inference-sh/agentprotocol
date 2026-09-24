// Package transcript reads and writes the conversations coding agents keep on
// disk. Every agent persists its sessions somewhere under $HOME in a format
// of its own; this package gives those files one shape, so a caller can list
// an agent's sessions, read one, or write one the agent will then load.
//
// The shape is message-grained because every store on disk is. A Session is
// a list of Entries, each with a role and content blocks. The vendor's own
// row travels along as Raw so that a session read from an agent and written
// back to the same agent is byte-for-byte what it was, whatever the mapping
// did or did not understand. Only a session that came from somewhere else
// goes through the encoder, and that is where any loss lives.
//
// The package depends only on the standard library. Stores that need a
// database driver live in a nested module.
package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// Role is who produced an entry.
type Role string

const (
	// RoleOpaque marks a vendor row that is not a message: a header, a mode
	// marker, a hook record, a usage line. It is kept in place so a
	// same-agent write loses nothing, and every reader skips it.
	RoleOpaque Role = ""
	// RoleUser is the person, or whatever stands in for them.
	RoleUser Role = "user"
	// RoleAssistant is the model.
	RoleAssistant Role = "assistant"
	// RoleSystem is context the harness injected: instructions, reminders,
	// developer messages.
	RoleSystem Role = "system"
	// RoleTool is a tool answering a call. Codecs normalize to it even when
	// the vendor files tool results under the user role.
	RoleTool Role = "tool"
)

// BlockKind is what a content block holds.
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockReasoning  BlockKind = "reasoning"
	BlockToolUse    BlockKind = "tool_use"
	BlockToolResult BlockKind = "tool_result"
	// BlockImage is an image: its bytes in Data, or where the agent keeps it
	// in URI, with its MediaType.
	BlockImage BlockKind = "image"
	// BlockFile is any other attachment (a PDF, a document), shaped like an
	// image, with its file name in Name when the agent records one.
	BlockFile BlockKind = "file"
)

// Status is how a tool call ended.
type Status string

const (
	StatusOK    Status = "ok"
	StatusError Status = "error"
)

// Block is one piece of an entry's content.
type Block struct {
	Kind BlockKind `json:"kind"`
	// Text carries text, reasoning and tool result output.
	Text string `json:"text,omitempty"`
	// ToolID links a tool_use to its tool_result, and to an image or file
	// the tool returned, which follows the result in the same entry.
	ToolID string `json:"tool_id,omitempty"`
	// Name is the tool name on a tool_use, and on a tool_result when the
	// vendor records it there.
	Name string `json:"name,omitempty"`
	// Input is the tool's arguments on a tool_use, as the agent stored them.
	Input json.RawMessage `json:"input,omitempty"`
	// Status is the outcome on a tool_result.
	Status Status `json:"status,omitempty"`
	// MediaType is an image's or file's type, such as image/png.
	MediaType string `json:"media_type,omitempty"`
	// Data is an image's or file's bytes, when the agent stores them inline.
	Data []byte `json:"data,omitempty"`
	// URI is where an image or file is, when the agent stores a reference
	// instead of the bytes: a path, a file: or https: URL, or the model
	// provider's id for an upload (Codex keeps file ids).
	URI string `json:"uri,omitempty"`
}

// Audience is who an entry is for. Agents keep rows the person never sees
// (context they inject, a compaction summary) and rows the model is no longer
// given (history a compaction retired, a turn the person undid), and say so
// with flags of their own; a codec maps those flags here.
type Audience uint8

const (
	// AudienceAll is shown to the person and given to the model.
	AudienceAll Audience = iota
	// AudienceModel is given to the model and not shown: injected context,
	// reminders, a summary standing in for retired history.
	AudienceModel
	// AudienceUser is shown and not given to the model: a slash command's
	// echo, history a compaction retired that the agent still displays.
	AudienceUser
	// AudienceNone is neither: a turn the person undid or reverted, which the
	// agent keeps in its store and ignores.
	AudienceNone
)

// Model reports whether the model is given entries for this audience.
func (a Audience) Model() bool { return a == AudienceAll || a == AudienceModel }

// User reports whether the person is shown entries for this audience.
func (a Audience) User() bool { return a == AudienceAll || a == AudienceUser }

// Compaction is what a compaction row does to the history before it on the
// active branch: the agent gives the model Summary in place of that history,
// followed by the earlier entries from Keep on.
type Compaction struct {
	// Summary is what stands in for the retired history, in the order the
	// model receives it. It may be empty (a reset that keeps nothing) and may
	// hold several entries (a replacement history). The compaction row itself
	// reaches the model only through it. Every entry here reaches the model;
	// Audience tells the conversation (the summary itself, kept turns:
	// AudienceAll) from context the agent regenerates for itself (a system
	// prompt, an environment block: AudienceModel), which Portable leaves
	// behind.
	Summary []Entry
	// Keep is the ID of the first earlier entry the agent keeps after the
	// summary (pi's firstKeptEntryId, opencode's tail_start_id). Empty keeps
	// nothing.
	Keep string
}

// Entry is one message in a session.
type Entry struct {
	ID string `json:"id"`
	// ParentID is set by stores that keep a tree (claude, pi, copilot). It is
	// empty in list-shaped stores.
	ParentID string    `json:"parent_id,omitempty"`
	Role     Role      `json:"role"`
	Time     time.Time `json:"time,omitempty"`
	Content  []Block   `json:"content"`
	// ModelContent is what the model was given for this entry when the agent
	// sent it something other than what the person saw: a prompt with hook
	// output appended (hermes' api_content), for instance. Nil means Content.
	// Context returns it in Content's place; Portable does not, since what an
	// agent adds to a message is its own.
	ModelContent []Block `json:"model_content,omitempty"`
	// Audience is who the entry is for. The zero value is everyone.
	Audience Audience `json:"audience,omitempty"`
	// Compaction is set on a row that replaces the history before it.
	Compaction *Compaction `json:"compaction,omitempty"`
	// Raw is the vendor row this entry was decoded from. A writer for the
	// same agent emits it unchanged. Nil on an entry built by hand.
	Raw json.RawMessage `json:"-"`
}

// Text returns the entry's text blocks joined.
func (e Entry) Text() string {
	var s string
	for _, b := range e.Content {
		if b.Kind == BlockText {
			s += b.Text
		}
	}
	return s
}

// Session is one conversation.
type Session struct {
	ID string `json:"id"`
	// Agent is the agent whose store the session was read from, and so the
	// agent whose rows the entries' Raw holds. A writer for any other agent
	// takes the session through Portable and never sees those rows.
	Agent string `json:"agent"`
	CWD   string `json:"cwd"`
	Title string `json:"title,omitempty"`
	// Model is the model the session last ran on, as the agent names it, when
	// the store records one. A writer whose format requires a model uses it.
	Model   string    `json:"model,omitempty"`
	Created time.Time `json:"created,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
	Entries []Entry   `json:"entries"`
	// Leaf is the ID of the entry the agent resumes from, set by a tree
	// store whose agent can pin one that is not the last linked row (Claude
	// Code after a rewind). Empty means the last linked row.
	Leaf string `json:"leaf,omitempty"`
	// Restart is set when the agent resumes the session from nothing
	// (Claude Code after a rewind to before the first prompt): the active
	// branch is empty, and the next entry written starts a new root.
	Restart bool `json:"restart,omitempty"`
	// Vendor is state the codec that read the session needs to write it
	// back and no other codec can use: a version stamp, a sidecar, an index
	// row. Each codec documents its own type. Nil on a session built by
	// hand, and ignored by a codec that does not recognize the type.
	Vendor any `json:"-"`
}

// Messages returns every entry that is a message, in store order, whoever it
// is for and whichever branch it is on, leaving out opaque rows. It is what a
// store holds; Linearize is what the person sees and Context what the model
// is given.
func (s *Session) Messages() []Entry {
	out := make([]Entry, 0, len(s.Entries))
	for _, e := range s.Entries {
		if e.Role != RoleOpaque {
			out = append(out, e)
		}
	}
	return out
}

// Info identifies a session without loading it.
type Info struct {
	ID      string    `json:"id"`
	CWD     string    `json:"cwd"`
	Title   string    `json:"title,omitempty"`
	Updated time.Time `json:"updated"`
	// Path is where the store keeps it, when the store is files.
	Path string `json:"path,omitempty"`
	// Root is the file or directory that belongs to this session alone: a
	// process holding it open is using this session. Empty for a store that
	// keeps every session in one database, where an open handle says only
	// that the agent is running.
	Root string `json:"root,omitempty"`
	// Holders are the processes the agent's own in-use markers name for this
	// session (Copilot's inuse.<pid>.hold). A marker can outlive its
	// process; Live checks each pid.
	Holders []int `json:"holders,omitempty"`
}

// LiveState is whether a process is using a session right now.
type LiveState string

const (
	// LiveUnknown: nothing available could tell.
	LiveUnknown LiveState = "unknown"
	// LiveIdle: no process is using the session.
	LiveIdle LiveState = "idle"
	// LiveActive: a process is, or may be, using the session.
	LiveActive LiveState = "active"
)

// Evidence is what a Liveness answer rests on.
type Evidence string

const (
	// EvidenceLockFile: the agent's own in-use marker for this session names
	// a running process. Proof.
	EvidenceLockFile Evidence = "lock-file"
	// EvidenceOpenFile: a process holds the session's own file or directory
	// open. Proof.
	EvidenceOpenFile Evidence = "open-file"
	// EvidenceNoProcess: no process of the agent is running. Proof of idle.
	EvidenceNoProcess Evidence = "no-process"
	// EvidenceProcessInCwd: a process of the agent runs in the session's
	// directory. It may be serving another session there. Heuristic.
	EvidenceProcessInCwd Evidence = "process-in-cwd"
	// EvidenceNoProcessInCwd: the agent runs, but not in the session's
	// directory. Heuristic: agents rarely serve a session from elsewhere.
	EvidenceNoProcessInCwd Evidence = "no-process-in-cwd"
	// EvidenceHeldElsewhere: every agent process in the session's directory
	// holds another session by the agent's own in-use markers. Proof of idle.
	EvidenceHeldElsewhere Evidence = "held-elsewhere"
	// EvidenceRecentWrite: the session was written within the recent window.
	// Heuristic.
	EvidenceRecentWrite Evidence = "recent-write"
	// EvidenceNone: nothing to go on.
	EvidenceNone Evidence = "none"
)

// Liveness is whether a session is in use, and why that is believed.
type Liveness struct {
	State    LiveState `json:"state"`
	Evidence Evidence  `json:"evidence"`
	// Heuristic is true when the answer is inferred rather than proven.
	Heuristic bool `json:"heuristic"`
	// PID is the process the evidence points at, when there is one.
	PID    int    `json:"pid,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ErrReadOnly is returned by Write on a store whose format is derived from
// something the agent does not read back, so a written file would change
// nothing.
var ErrReadOnly = errors.New("transcript: store is read-only")

// ErrNotFound is returned by Read for an id the store does not have.
var ErrNotFound = errors.New("transcript: session not found")

// Store is one agent's sessions on one machine.
type Store interface {
	// List returns the sessions for a working directory, newest first. An
	// empty cwd lists every session the store knows.
	List(ctx context.Context, cwd string) ([]Info, error)
	// Read loads one session. It returns ErrNotFound for an unknown id.
	Read(ctx context.Context, id string) (*Session, error)
	// Write persists a session so the agent can load it, and returns the id
	// the agent will know it by. A session with an empty ID is given one. A
	// store that cannot be written returns ErrReadOnly.
	Write(ctx context.Context, s *Session) (string, error)
}

// ServingLister is implemented by stores whose agent keeps a registry of
// which running process holds which session (Claude Code's
// ~/.claude/sessions, Copilot's inuse.<pid>.hold markers). Serving returns
// that registry as pid to session id, including sessions whose transcript
// is not listed, so a liveness check can tell a process is busy with a
// session it would otherwise not know about.
type ServingLister interface {
	Serving(ctx context.Context) (map[int]string, error)
}

// Codec opens an agent's store under a home directory. The registry holds a
// Codec per agent; a caller binds it to the home it is working in.
type Codec interface {
	Open(home string) (Store, error)
}

// Branch returns the indexes into Entries of the active branch, root first,
// opaque rows included. For a session whose entries form a tree it is the
// chain of parents from the leaf: Leaf when set, else the last entry with an
// ID. Opaque rows take part in the chain, since some agents thread
// attachments, markers and compaction rows through the same links. A session
// without parent links is one branch, every entry in store order.
func (s *Session) Branch() []int {
	if s.Restart {
		return nil
	}
	byID := make(map[string]int, len(s.Entries))
	tree := false
	leaf := -1
	for i, e := range s.Entries {
		if e.ID != "" {
			byID[e.ID] = i
			leaf = i
		}
		if e.ParentID != "" {
			tree = true
		}
	}
	if i, ok := byID[s.Leaf]; ok && s.Leaf != "" {
		leaf = i
	}
	if !tree || leaf < 0 {
		all := make([]int, len(s.Entries))
		for i := range all {
			all[i] = i
		}
		return all
	}
	var chain []int
	seen := make(map[string]bool, len(s.Entries))
	for i := leaf; ; {
		e := s.Entries[i]
		if seen[e.ID] {
			break
		}
		seen[e.ID] = true
		chain = append(chain, i)
		j, ok := byID[e.ParentID]
		if e.ParentID == "" || !ok {
			break
		}
		i = j
	}
	for l, r := 0, len(chain)-1; l < r; l, r = l+1, r-1 {
		chain[l], chain[r] = chain[r], chain[l]
	}
	return chain
}

// Linearize returns the conversation the person sees: the messages on the
// active branch, root first, that are shown to them. History a compaction
// retired stays, since agents keep displaying it; turns that were undone,
// and context only the model is given, do not appear.
func (s *Session) Linearize() []Entry {
	var out []Entry
	for _, i := range s.Branch() {
		e := s.Entries[i]
		if e.Role != RoleOpaque && e.Audience.User() {
			out = append(out, e)
		}
	}
	return out
}

// Context returns what the agent gives the model when it resumes the
// session: the messages on the active branch that are for the model, with
// every compaction applied. At a row carrying a Compaction, everything
// gathered so far is replaced by the compaction's Summary followed by the
// gathered entries from Keep on.
func (s *Session) Context() []Entry {
	out := s.context()
	for i, e := range out {
		if e.ModelContent != nil {
			out[i].Content = e.ModelContent
		}
	}
	return out
}

// context is Context with each entry's Content as the person saw it.
func (s *Session) context() []Entry {
	type placed struct {
		at int // position on the branch
		e  Entry
	}
	branch := s.Branch()
	var ctx []placed
	for at, i := range branch {
		e := s.Entries[i]
		if c := e.Compaction; c != nil {
			keep := len(branch)
			if c.Keep != "" {
				for k := 0; k < at; k++ {
					if s.Entries[branch[k]].ID == c.Keep {
						keep = k
						break
					}
				}
			}
			next := make([]placed, 0, len(c.Summary)+len(ctx))
			for _, m := range c.Summary {
				next = append(next, placed{at, m})
			}
			for _, p := range ctx {
				if p.at >= keep {
					next = append(next, p)
				}
			}
			ctx = next
			continue
		}
		if e.Role != RoleOpaque && e.Audience.Model() {
			ctx = append(ctx, placed{at, e})
		}
	}
	out := make([]Entry, len(ctx))
	for i, p := range ctx {
		out[i] = p.e
	}
	return out
}

// Portable returns the session as a writer for another agent takes it:
// what the model knows of the conversation, without what the agent put there
// for itself. That is Context with only the entries meant for everyone, and
// compaction summaries standing in for the history they retired: injected
// reminders and environment blocks belong to the agent that wrote them, and
// a slash command's echo or a notice the agent showed was never
// conversation. The entries are new, with no vendor rows and no links, so
// the writer encodes every one in its own format and links them in order.
// A session's Raw rows are its own agent's format and meaningless anywhere
// else, and its links may pass through rows that do not survive the move.
func (s *Session) Portable() *Session {
	out := *s
	out.Agent = ""
	out.Leaf = ""
	out.Restart = false
	out.Vendor = nil
	out.Entries = nil
	for _, e := range s.context() {
		if e.Audience != AudienceAll {
			continue
		}
		e.ParentID = ""
		e.Raw = nil
		e.Compaction = nil
		e.ModelContent = nil
		out.Entries = append(out.Entries, e)
	}
	return &out
}

// Register records a codec for an agent id, for codecs that live outside
// this module because they need a database driver. The SQLite module calls
// it from an init function; a program that imports that module can then find
// those codecs through Registered. Pure-Go codecs are referenced directly
// and do not register.
func Register(agent string, c Codec) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if registry == nil {
		registry = map[string]Codec{}
	}
	registry[agent] = c
}

// RegisteredAgents lists the agents with a codec set through Register.
func RegisteredAgents() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]string, 0, len(registry))
	for a := range registry {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Registered returns a codec set through Register.
func Registered(agent string) (Codec, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	c, ok := registry[agent]
	return c, ok
}

var (
	registryMu sync.Mutex
	registry   map[string]Codec
)

// SortNewest orders infos newest first.
func SortNewest(infos []Info) {
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].Updated.After(infos[j].Updated) })
}
