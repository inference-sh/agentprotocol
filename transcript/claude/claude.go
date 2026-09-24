// Package claude reads and writes Claude Code sessions.
//
// Claude keeps one JSONL per session under
// ~/.claude/projects/<mangled cwd>/<id>.jsonl. Rows form a tree through
// uuid and parentUuid. Message rows have type user or assistant and carry an
// Anthropic-shaped message. Every other row (system, attachment, mode,
// last-prompt, summary) is opaque but keeps its place in the chain.
//
// The file alone does not say what Claude resumes with: its loader picks the
// leaf, splices back what a compaction kept, and repairs the tree a parallel
// tool call leaves behind. finish in resume.go does the same.
package claude

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Version is the Claude Code version whose row shape this codec writes for
// a session that did not come from Claude.
const Version = "2.1.278"

// Codec is the Claude Code session store.
var Codec = transcript.JSONL{
	Agent: "claude",
	Layout: transcript.Layout{
		Root:    ".claude/projects",
		Project: transcript.MangledCwd,
		Ext:     ".jsonl",
		Peek:    peek,
		Holders: holders,
		Serving: func(home string) map[int]string {
			return running.serving(filepath.Join(home, ".claude", "sessions"))
		},
	},
	Decode:  decode,
	Finish:  finish,
	Prepare: prepare,
	Encode:  encode,
	Tree:    true,
	// Claude records a compaction as a boundary and a summary and keeps the
	// history before them in the file, which is what another agent's
	// compaction becomes. It has no row that is shown and never sent other
	// than an API error it made up itself.
	Caps: transcript.Capabilities{Compaction: true},
}

// Vendor is what a Claude session carries in Session.Vendor: the stamps
// Claude puts on every row, so a write reproduces them.
type Vendor struct {
	Version   string
	GitBranch string
	Model     string

	// summaryOf names, for each compact boundary this write creates, the
	// summary row prepare put after it.
	summaryOf map[string]string
}

// row is a Claude transcript line. Only the fields the codec reads or
// writes are named; a same-agent write emits the original bytes.
type row struct {
	ParentUUID  *string  `json:"parentUuid"`
	IsSidechain bool     `json:"isSidechain"`
	UserType    string   `json:"userType,omitempty"`
	CWD         string   `json:"cwd,omitempty"`
	SessionID   string   `json:"sessionId,omitempty"`
	Version     string   `json:"version,omitempty"`
	GitBranch   string   `json:"gitBranch,omitempty"`
	Type        string   `json:"type"`
	Message     *message `json:"message,omitempty"`
	UUID        string   `json:"uuid,omitempty"`
	Timestamp   string   `json:"timestamp,omitempty"`

	// A compaction's summary row is sent and shown only in the transcript
	// view.
	IsVisibleInTranscriptOnly bool `json:"isVisibleInTranscriptOnly,omitempty"`
	IsCompactSummary          bool `json:"isCompactSummary,omitempty"`

	// Read only: Claude sets these on rows it writes itself, never on a row
	// this codec encodes.
	IsMeta            bool `json:"isMeta,omitempty"`
	IsAPIErrorMessage bool `json:"isApiErrorMessage,omitempty"`
}

// boundaryRow is a compact_boundary system row. Its fields stay off row,
// whose decode would otherwise take every row's content field for a string.
type boundaryRow struct {
	row
	LogicalParentUUID string    `json:"logicalParentUuid,omitempty"`
	Subtype           string    `json:"subtype"`
	Content           string    `json:"content"`
	Level             string    `json:"level"`
	CompactMetadata   *compacts `json:"compactMetadata"`
}

// compacts is a boundary's compactMetadata as createCompactBoundaryMessage
// writes it (utils/messages.ts), with the preservedSegment compact.ts adds
// when the compaction keeps recent messages.
type compacts struct {
	Trigger          string     `json:"trigger"`
	PreTokens        int        `json:"preTokens"`
	PreservedSegment *preserved `json:"preservedSegment,omitempty"`
}

type preserved struct {
	HeadUUID   string `json:"headUuid"`
	AnchorUUID string `json:"anchorUuid"`
	TailUUID   string `json:"tailUuid"`
}

// timeLayout is JavaScript's Date.toISOString, which Claude stamps rows
// with. Its loader compares timestamps as strings, which orders them only at
// this fixed width.
const timeLayout = "2006-01-02T15:04:05.000Z"

// syntheticModel is the model Claude names on an assistant row it made up
// itself, such as an API error it shows in place of a reply.
const syntheticModel = "<synthetic>"

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ID           string          `json:"id,omitempty"`
	Type         string          `json:"type,omitempty"`
	Model        string          `json:"model,omitempty"`
	StopReason   *string         `json:"stop_reason,omitempty"`
	StopSequence *string         `json:"stop_sequence,omitempty"`
	Usage        *usage          `json:"usage,omitempty"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// block is an Anthropic content block. Content is a string or an array of
// these; tool_result content is the same again, one level down.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Source is an image's or document's bytes or location, Title a
	// document's name.
	Source *source `json:"source,omitempty"`
	Title  string  `json:"title,omitempty"`
}

// source is an Anthropic image or document source. Claude Code writes
// base64 for what the person pastes (processUserInput.ts), what Read returns
// (FileReadTool.ts: images in the tool_result, a PDF in a meta row or the
// result) and what MCP tools return (services/mcp/client.ts); the API also
// takes a URL, and plain text for a document.
type source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.UUID == "" {
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.UUID, Role: transcript.RoleOpaque}
	if r.ParentUUID != nil {
		e.ParentID = *r.ParentUUID
	}
	// Every row with a uuid is stamped, opaque ones included: a writer needs
	// the file's latest time to keep appended rows after it.
	var terr error
	if r.Timestamp != "" {
		e.Time, terr = time.Parse(time.RFC3339Nano, r.Timestamp)
	}
	if r.Type != "user" && r.Type != "assistant" || r.IsSidechain || r.Message == nil {
		// Attachments and system rows sit in the parent chain between
		// messages, so they keep their links and stay opaque.
		return e, true, nil
	}
	if terr != nil {
		return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.UUID, terr)
	}
	if s.CWD == "" {
		s.CWD = r.CWD
	}
	if s.ID == "" {
		s.ID = r.SessionID
	}
	v, _ := s.Vendor.(*Vendor)
	if v == nil {
		v = &Vendor{}
		s.Vendor = v
	}
	if r.Version != "" {
		v.Version = r.Version
	}
	if r.GitBranch != "" {
		v.GitBranch = r.GitBranch
	}
	if r.Message.Model != "" {
		v.Model = r.Message.Model
		s.Model = r.Message.Model
	}
	content, err := blocks(r.Message.Content)
	if err != nil {
		return transcript.Entry{}, false, fmt.Errorf("row %s: %w", r.UUID, err)
	}
	e.Content = content
	e.Role = transcript.Role(r.Message.Role)
	if e.Role == transcript.RoleUser && onlyToolResults(content) {
		e.Role = transcript.RoleTool
	}
	e.Audience = audience(r)
	return e, true, nil
}

// audience maps the flags Claude's UI filters on (shouldShowUserMessage in
// its utils/messages.ts) and the rows normalizeMessagesForAPI leaves out.
func audience(r row) transcript.Audience {
	switch {
	case r.Type == "user" && (r.IsMeta || r.IsVisibleInTranscriptOnly):
		// Injected context (a caveat, a skill's body) is sent and never
		// shown. The summary a full compaction leaves is sent and shown only
		// in the ctrl+o transcript view. A partial compaction's summary lacks
		// the transcript-only flag and is shown collapsed, so it stays for
		// everyone.
		return transcript.AudienceModel
	case r.Type == "assistant" && r.IsAPIErrorMessage && r.Message.Model == syntheticModel:
		// An API error Claude shows in place of a reply and never sends.
		return transcript.AudienceUser
	}
	return transcript.AudienceAll
}

// onlyToolResults reports whether a user row answers tool calls and says
// nothing else: its blocks are tool results and what those results returned.
func onlyToolResults(content []transcript.Block) bool {
	if len(content) == 0 {
		return false
	}
	for _, b := range content {
		if b.Kind != transcript.BlockToolResult && b.ToolID == "" {
			return false
		}
	}
	return true
}

// blocks decodes an Anthropic content field.
func blocks(raw json.RawMessage) ([]transcript.Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []transcript.Block{{Kind: transcript.BlockText, Text: text}}, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	out := make([]transcript.Block, 0, len(bs))
	for _, b := range bs {
		switch b.Type {
		case "text":
			out = append(out, transcript.Block{Kind: transcript.BlockText, Text: b.Text})
		case "thinking":
			out = append(out, transcript.Block{Kind: transcript.BlockReasoning, Text: b.Thinking})
		case "tool_use":
			out = append(out, transcript.Block{Kind: transcript.BlockToolUse, ToolID: b.ID, Name: b.Name, Input: b.Input})
		case "image", "document":
			if m, ok := media(b, ""); ok {
				out = append(out, m)
			}
		case "tool_result":
			text, returned, err := result(b.Content, b.ToolUseID)
			if err != nil {
				return nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			st := transcript.StatusOK
			if b.IsError {
				st = transcript.StatusError
			}
			out = append(out, transcript.Block{Kind: transcript.BlockToolResult, ToolID: b.ToolUseID, Text: text, Status: st})
			out = append(out, returned...)
		}
	}
	return out, nil
}

// result reads tool_result content, which is a string or an array of text,
// image and document blocks. The text is flattened; images and documents
// come back as blocks carrying the call's id.
func result(raw json.RawMessage, toolID string) (string, []transcript.Block, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return "", nil, err
	}
	var returned []transcript.Block
	for _, b := range bs {
		switch b.Type {
		case "image", "document":
			if m, ok := media(b, toolID); ok {
				returned = append(returned, m)
			}
		default:
			text += b.Text
		}
	}
	return text, returned, nil
}

// media reads an image or document block. A document's bytes are its text
// when its source is plain text. A source this codec cannot read (a file
// uploaded to the API, bytes that are not base64) carries nothing another
// agent could send, and is left out.
func media(b block, toolID string) (transcript.Block, bool) {
	out := transcript.Block{Kind: transcript.BlockImage, ToolID: toolID}
	if b.Type == "document" {
		out.Kind, out.Name = transcript.BlockFile, b.Title
	}
	if b.Source == nil {
		return transcript.Block{}, false
	}
	out.MediaType = b.Source.MediaType
	switch b.Source.Type {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(b.Source.Data)
		if err != nil {
			return transcript.Block{}, false
		}
		out.Data = data
	case "text":
		out.Data = []byte(b.Source.Data)
	case "url":
		out.URI = b.Source.URL
	default:
		return transcript.Block{}, false
	}
	return out, true
}

// encode builds a Claude row for an entry from another agent, with the
// fields Claude writes itself on the version named above.
func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	v, _ := s.Vendor.(*Vendor)
	if v == nil {
		v = &Vendor{}
	}
	if e.Role == transcript.RoleOpaque && e.Compaction != nil {
		return encodeBoundary(e, s, v)
	}
	if sum, ok := v.summaryOf[e.ParentID]; ok && sum == e.ID {
		return encodeSummary(e, s, v)
	}
	role := e.Role
	if role == transcript.RoleTool {
		role = transcript.RoleUser
	}
	if role != transcript.RoleUser && role != transcript.RoleAssistant {
		return nil, nil
	}
	// What a tool returned goes inside its tool_result, as Claude Code
	// writes it.
	returned := map[string][]block{}
	for _, b := range e.Content {
		if (b.Kind == transcript.BlockImage || b.Kind == transcript.BlockFile) && b.ToolID != "" {
			if m, ok := encodeMedia(b); ok {
				returned[b.ToolID] = append(returned[b.ToolID], m)
			}
		}
	}
	content := make([]block, 0, len(e.Content))
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			content = append(content, block{Type: "text", Text: b.Text})
		case transcript.BlockReasoning:
			// A thinking block carries a signature Claude will not accept
			// from anyone else, so foreign reasoning is left out rather than
			// forged.
		case transcript.BlockToolUse:
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			content = append(content, block{Type: "tool_use", ID: b.ToolID, Name: b.Name, Input: in})
		case transcript.BlockToolResult:
			var out any = b.Text
			if media := returned[b.ToolID]; len(media) > 0 {
				var parts []block
				if b.Text != "" {
					parts = append(parts, block{Type: "text", Text: b.Text})
				}
				out = append(parts, media...)
			}
			text, err := json.Marshal(out)
			if err != nil {
				return nil, err
			}
			content = append(content, block{Type: "tool_result", ToolUseID: b.ToolID, Content: text, IsError: b.Status == transcript.StatusError})
		case transcript.BlockImage, transcript.BlockFile:
			if b.ToolID != "" {
				continue
			}
			if m, ok := encodeMedia(b); ok {
				content = append(content, m)
			}
		}
	}
	if len(content) == 0 {
		return nil, nil
	}
	cj, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	m := &message{Role: string(role), Content: cj}
	if role == transcript.RoleAssistant {
		m.ID = "msg_" + e.ID
		m.Type = "message"
		m.Model = v.Model
		if m.Model == "" {
			m.Model = s.Model
		}
		m.Usage = &usage{}
	}
	r := stamped(e, s, v, string(role))
	r.Message = m
	return json.Marshal(r)
}

// stamped is a row of type typ for an entry, with the stamps every row
// carries and its parent link.
func stamped(e transcript.Entry, s *transcript.Session, v *Vendor, typ string) row {
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	version := v.Version
	if version == "" {
		version = Version
	}
	r := row{
		IsSidechain: false,
		UserType:    "external",
		CWD:         s.CWD,
		SessionID:   s.ID,
		Version:     version,
		GitBranch:   v.GitBranch,
		Type:        typ,
		UUID:        e.ID,
		Timestamp:   t.UTC().Format(timeLayout),
	}
	if e.ParentID != "" {
		r.ParentUUID = &e.ParentID
	}
	return r
}

// encodeBoundary writes a compaction marker as createCompactBoundaryMessage
// builds one: no parent, the last row before it as logical parent, and,
// when the compaction kept earlier messages, the segment from Keep to that
// last row, which the loader splices in behind the summary
// (applyPreservedSegmentRelinks, sessionStorage.ts).
func encodeBoundary(e transcript.Entry, s *transcript.Session, v *Vendor) (json.RawMessage, error) {
	r := boundaryRow{
		row:               stamped(e, s, v, "system"),
		LogicalParentUUID: e.ParentID,
		Subtype:           "compact_boundary",
		Content:           "Conversation compacted",
		Level:             "info",
		CompactMetadata:   &compacts{Trigger: "manual"},
	}
	r.ParentUUID = nil
	if keep := e.Compaction.Keep; keep != "" && e.ParentID != "" {
		anchor := e.ID
		if sum, ok := v.summaryOf[e.ID]; ok {
			anchor = sum
		}
		r.CompactMetadata.PreservedSegment = &preserved{HeadUUID: keep, AnchorUUID: anchor, TailUUID: e.ParentID}
	}
	return json.Marshal(r)
}

// encodeSummary writes the summary prepare put after a boundary as the user
// row compact.ts writes: its text as a string, sent, and shown only in the
// transcript view.
func encodeSummary(e transcript.Entry, s *transcript.Session, v *Vendor) (json.RawMessage, error) {
	text, err := json.Marshal(e.Text())
	if err != nil {
		return nil, err
	}
	r := stamped(e, s, v, "user")
	r.Message = &message{Role: "user", Content: text}
	r.IsVisibleInTranscriptOnly = true
	r.IsCompactSummary = true
	return json.Marshal(r)
}

// summaryIntro and keptNote are how getCompactUserSummaryMessage
// (services/compact/prompt.ts) wraps a summary for the model; Claude stores
// the wrapped text in the summary row.
const (
	summaryIntro = "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n"
	keptNote     = "\n\nRecent messages are preserved verbatim."
)

// prepare readies a session for writing: compactions from another agent
// take Claude's form, and new rows get their stamps.
func prepare(home string, s *transcript.Session) error {
	compactions(s)
	return stamp(home, s)
}

// compactions lays out each compaction marker this write creates the way
// compact.ts records a compaction: the boundary, then a user row holding the
// summary, with the conversation continuing from the summary. The boundary
// names the row before it, which ends the retired history, and the entry
// the compaction kept, which starts the segment the loader splices in behind
// the summary. Entries get their ids here, so those names are final.
func compactions(s *transcript.Session) {
	type marker struct{ at, keep int }
	var markers []marker
	var out []transcript.Entry
	index := map[string]int{} // entry id to its place in out
	for _, e := range s.Entries {
		if e.Raw != nil || e.Compaction == nil {
			if e.ID != "" {
				index[e.ID] = len(out)
			}
			out = append(out, e)
			continue
		}
		c := *e.Compaction
		e.Compaction = &c
		// The kept segment must start on a row that is written: the loader
		// walks it from its end and loads the whole file when it breaks.
		keep := -1
		if at, ok := index[c.Keep]; ok && c.Keep != "" {
			for k := at; k < len(out) && keep < 0; k++ {
				if written(out[k], s) {
					keep = k
				}
			}
		}
		if !transcript.IsUUID(e.ID) {
			e.ID = transcript.NewUUID()
		}
		markers = append(markers, marker{len(out), keep})
		out = append(out, e)
		var text []string
		for _, m := range e.Compaction.Summary {
			if t := m.Text(); t != "" {
				text = append(text, t)
			}
		}
		if len(text) == 0 {
			continue
		}
		body := summaryIntro + strings.Join(text, "\n\n")
		if keep >= 0 {
			body += keptNote
		}
		v, _ := s.Vendor.(*Vendor)
		if v == nil {
			v = &Vendor{}
			s.Vendor = v
		}
		if v.summaryOf == nil {
			v.summaryOf = map[string]string{}
		}
		sum := transcript.Entry{ID: transcript.NewUUID(), ParentID: e.ID, Role: transcript.RoleUser, Time: e.Time,
			Content: []transcript.Block{{Kind: transcript.BlockText, Text: body}}}
		v.summaryOf[e.ID] = sum.ID
		out = append(out, sum)
	}
	if len(markers) == 0 {
		return
	}
	s.Entries = out
	transcript.AssignIDs(s, transcript.UUIDs, true)
	for _, m := range markers {
		e := &s.Entries[m.at]
		if m.at > 0 {
			e.ParentID = s.Entries[m.at-1].ID
		}
		e.Compaction.Keep = ""
		if m.keep >= 0 {
			e.Compaction.Keep = s.Entries[m.keep].ID
		}
	}
}

// written reports whether an entry becomes a message row of its own.
func written(e transcript.Entry, s *transcript.Session) bool {
	if e.Compaction != nil || e.Role == transcript.RoleOpaque {
		return false
	}
	row, err := encode(e, s)
	return err == nil && len(row) > 0
}

// imageTypes are the image types the Messages API accepts. Claude sends
// its history as it is on resume, and the API rejects the whole request
// over any other.
var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// encodeMedia builds an image or document block. A document is a PDF (as
// bytes or a URL) or text; an image is one of imageTypes. A block the API
// would refuse, or a local path Claude has no block for, is left out.
func encodeMedia(b transcript.Block) (block, bool) {
	web := strings.HasPrefix(b.URI, "https://") || strings.HasPrefix(b.URI, "http://")
	var src *source
	switch {
	case b.Kind == transcript.BlockImage && imageTypes[b.MediaType] && len(b.Data) > 0:
		src = &source{Type: "base64", MediaType: b.MediaType, Data: base64.StdEncoding.EncodeToString(b.Data)}
	case b.Kind == transcript.BlockImage && len(b.Data) == 0 && web:
		src = &source{Type: "url", URL: b.URI}
	case b.Kind == transcript.BlockFile && b.MediaType == "application/pdf" && len(b.Data) > 0:
		src = &source{Type: "base64", MediaType: b.MediaType, Data: base64.StdEncoding.EncodeToString(b.Data)}
	case b.Kind == transcript.BlockFile && b.MediaType == "application/pdf" && len(b.Data) == 0 && web:
		src = &source{Type: "url", URL: b.URI}
	case b.Kind == transcript.BlockFile && strings.HasPrefix(b.MediaType, "text/") && len(b.Data) > 0 && utf8.Valid(b.Data):
		// The API takes text documents as text/plain only.
		src = &source{Type: "text", MediaType: "text/plain", Data: string(b.Data)}
	default:
		return block{}, false
	}
	if b.Kind == transcript.BlockFile {
		return block{Type: "document", Source: src, Title: b.Name}, true
	}
	return block{Type: "image", Source: src}, true
}

// peek names the session's working directory, which the directory name
// holds only in a form that cannot be reversed, from the cwd its rows carry.
func peek(path string) (transcript.Info, error) {
	return transcript.Info{CWD: transcript.PeekField(path, "cwd", 64)}, nil
}

// holders reads Claude Code's registry of running sessions,
// ~/.claude/sessions/<pid>.json, each naming the session a process holds
// ({"pid", "sessionId", "cwd", ...}), and returns the pids that name the
// session in this file. A file can outlive its process; the caller checks
// each pid. path is <home>/.claude/projects/<dir>/<id>.jsonl.
func holders(path string) []int {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	dir := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "sessions")
	return running.pids(dir)[id]
}

// running caches the registry per directory, keyed on its modification
// time, so listing hundreds of sessions reads it once.
var running = &registryCache{byDir: map[string]registryEntry{}}

type registryCache struct {
	mu    sync.Mutex
	byDir map[string]registryEntry
}

type registryEntry struct {
	mod     time.Time
	pids    map[string][]int
	serving map[int]string
}

// serving is the whole registry as pid to session id.
func (c *registryCache) serving(dir string) map[int]string {
	c.pids(dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byDir[dir].serving
}

func (c *registryCache) pids(dir string) map[string][]int {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byDir[dir]; ok && e.mod.Equal(fi.ModTime()) {
		return e.pids
	}
	out := map[string][]int{}
	serving := map[int]string{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rec struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &rec) == nil && rec.PID > 0 && rec.SessionID != "" {
			out[rec.SessionID] = append(out[rec.SessionID], rec.PID)
			serving[rec.PID] = rec.SessionID
		}
	}
	c.byDir[dir] = registryEntry{mod: fi.ModTime(), pids: out, serving: serving}
	return out
}
