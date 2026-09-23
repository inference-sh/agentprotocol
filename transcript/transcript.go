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
	// ToolID links a tool_use to its tool_result.
	ToolID string `json:"tool_id,omitempty"`
	// Name is the tool name on a tool_use, and on a tool_result when the
	// vendor records it there.
	Name string `json:"name,omitempty"`
	// Input is the tool's arguments on a tool_use, as the agent stored them.
	Input json.RawMessage `json:"input,omitempty"`
	// Status is the outcome on a tool_result.
	Status Status `json:"status,omitempty"`
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
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	CWD     string    `json:"cwd"`
	Title   string    `json:"title,omitempty"`
	Created time.Time `json:"created,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
	Entries []Entry   `json:"entries"`
	// Vendor is state the codec that read the session needs to write it
	// back and no other codec can use: a version stamp, a sidecar, an index
	// row. Each codec documents its own type. Nil on a session built by
	// hand, and ignored by a codec that does not recognize the type.
	Vendor any `json:"-"`
}

// Messages returns the entries that are messages, leaving out opaque rows.
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

// Codec opens an agent's store under a home directory. The registry holds a
// Codec per agent; a caller binds it to the home it is working in.
type Codec interface {
	Open(home string) (Store, error)
}

// Linearize returns the messages on the active branch, root first, for a
// session whose entries form a tree. The active branch is the chain of
// parents from the last linked entry. Opaque rows take part in the chain,
// since some agents thread attachments and system rows through the same
// links, and drop out of the result. A session without parent links is
// returned as its messages, in file order.
func (s *Session) Linearize() []Entry {
	byID := make(map[string]int, len(s.Entries))
	tree := false
	last := -1
	for i, e := range s.Entries {
		if e.ID != "" {
			byID[e.ID] = i
			last = i
		}
		if e.ParentID != "" {
			tree = true
		}
	}
	if !tree || last < 0 {
		return s.Messages()
	}
	var chain []Entry
	seen := make(map[string]bool, len(s.Entries))
	for i := last; i >= 0; {
		e := s.Entries[i]
		if seen[e.ID] {
			break
		}
		seen[e.ID] = true
		if e.Role != RoleOpaque {
			chain = append(chain, e)
		}
		if e.ParentID == "" {
			break
		}
		j, ok := byID[e.ParentID]
		if !ok {
			break
		}
		i = j
	}
	for l, r := 0, len(chain)-1; l < r; l, r = l+1, r-1 {
		chain[l], chain[r] = chain[r], chain[l]
	}
	return chain
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
