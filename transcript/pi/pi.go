// Package pi reads and writes pi coding agent sessions, and those of Oh My
// Pi, which forked the format.
//
// pi keeps one JSONL per session under
// ~/.pi/agent/sessions/<wrapped cwd>/<timestamp>_<id>.jsonl. The file
// starts with a session header (version 3); every later row has an id and a
// parentId, so the file is a tree. Message rows carry a pi-ai message:
// user and assistant with content blocks, toolResult with the call id, and
// the agents' own roles (a user's bash run, an extension's message, omp's
// developer notes and @file mentions), which reach the model as the user or
// developer messages the agents turn them into. Compaction, branch summary
// and extension message rows are messages too; model changes, thinking level
// changes and custom rows stay opaque in the chain.
package pi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the pi session store.
var Codec = codec("pi", ".pi/agent/sessions", transcript.DashWrappedCwd, piVariant)

// OMP is the Oh My Pi session store. It shares pi's rows. Its directory
// name is a dash and the last path element, as observed on one sample;
// listing does not depend on it because the header carries the cwd.
var OMP = codec("omp", ".omp/agent/sessions", ompDir, ompVariant)

// ompDir is the project directory rule Oh My Pi was seen to use.
const ompDir transcript.ProjectDir = -1

// variant is which of the two agents a codec reads for. They write the same
// rows and differ in how some of them reach the model.
type variant uint8

const (
	piVariant variant = iota
	ompVariant
)

func codec(agent, root string, project transcript.ProjectDir, v variant) transcript.JSONL {
	return transcript.JSONL{
		Agent: agent,
		Layout: transcript.Layout{
			Files: func(home, cwd string) ([]string, error) {
				return transcript.Glob(filepath.Join(home, root, "*", "*.jsonl"))
			},
			PathFor: func(home string, s *transcript.Session) string {
				dir := project.Name(s.CWD)
				if project == ompDir {
					dir = "-" + filepath.Base(s.CWD)
				}
				return filepath.Join(home, root, dir, fileStamp(s.Created)+"_"+s.ID+".jsonl")
			},
			Peek: peek,
		},
		Header:      header,
		Decode:      v.decode,
		Finish:      v.finish,
		Encode:      encode,
		WriteHeader: writeHeader,
		Tree:        true,
		IDs:         transcript.IDScheme{New: newID, Valid: validID},
	}
}

// newID mints an entry id the way pi does: eight lowercase hex digits.
func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func validID(id string) bool {
	if len(id) != 8 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// fileStamp is pi's file-name timestamp: an ISO instant with every colon
// and period turned into a dash.
func fileStamp(t time.Time) string {
	iso := t.UTC().Format("2006-01-02T15:04:05.000Z")
	return strings.NewReplacer(":", "-", ".", "-").Replace(iso)
}

// Vendor is what a pi session carries in Session.Vendor: the header row's
// fields beyond the ones this codec models.
type Vendor struct {
	Header map[string]json.RawMessage
}

// row is any session row, with the fields of every row type this codec
// reads.
type row struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	ParentID  *string         `json:"parentId,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`

	// model_change: pi names a provider and a modelId, omp one
	// "provider/modelId" model and the role it is for.
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
	Model    string `json:"model,omitempty"`
	Role     string `json:"role,omitempty"`

	// compaction and branch_summary.
	Summary                      string                     `json:"summary,omitempty"`
	FirstKeptEntryID             string                     `json:"firstKeptEntryId,omitempty"`
	FirstKeptEntryIndex          *int                       `json:"firstKeptEntryIndex,omitempty"`
	SystemMessage                *stored                    `json:"systemMessage,omitempty"`
	Method                       string                     `json:"method,omitempty"`
	ProviderReplayThroughEntryID string                     `json:"providerReplayThroughEntryId,omitempty"`
	PreserveData                 map[string]json.RawMessage `json:"preserveData,omitempty"`

	// custom_message.
	CustomType  string  `json:"customType,omitempty"`
	Content     content `json:"content,omitempty"`
	Display     bool    `json:"display,omitempty"`
	Attribution string  `json:"attribution,omitempty"`

	// context_edit.
	TargetID    string          `json:"targetId,omitempty"`
	Replacement json.RawMessage `json:"replacement,omitempty"`
}

type sessionHeader struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

// stored is a message as the agents store it, with the fields of every
// role they write.
type stored struct {
	Role       string  `json:"role"`
	Content    content `json:"content"`
	ToolCallID string  `json:"toolCallId"`
	ToolName   string  `json:"toolName"`
	IsError    bool    `json:"isError"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`

	// system: pi's prompt as named sections, applied in order.
	Sections sections `json:"sections"`

	// bashExecution and omp's pythonExecution: a command the person ran.
	Command            string `json:"command"`
	Code               string `json:"code"`
	Output             string `json:"output"`
	ExitCode           *int   `json:"exitCode"`
	Cancelled          bool   `json:"cancelled"`
	Truncated          bool   `json:"truncated"`
	FullOutputPath     string `json:"fullOutputPath"`
	ExcludeFromContext bool   `json:"excludeFromContext"`

	// custom and hookMessage: an extension's message.
	CustomType  string `json:"customType"`
	Display     bool   `json:"display"`
	Attribution string `json:"attribution"`

	// fileMention: the files an @path in omp's prompt read in.
	Files []mentionedFile `json:"files"`

	// branchSummary and compactionSummary.
	Summary string `json:"summary"`
	Method  string `json:"method"`
}

type mentionedFile struct {
	Path    string          `json:"path"`
	Content string          `json:"content"`
	Image   json.RawMessage `json:"image"`
}

// message is what this codec writes for an entry from another agent.
type message struct {
	Role       string  `json:"role"`
	Content    content `json:"content"`
	ToolCallID string  `json:"toolCallId,omitempty"`
	ToolName   string  `json:"toolName,omitempty"`
	IsError    bool    `json:"isError,omitempty"`
	Timestamp  int64   `json:"timestamp,omitempty"`
}

// content is a message's content: pi writes a plain string for some rows and
// an array of typed blocks for others. A string decodes as one text block.
type content []block

func (c *content) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*c = nil
		return nil
	}
	var text string
	if err := json.Unmarshal(b, &text); err == nil {
		*c = content{{Type: "text", Text: text}}
		return nil
	}
	var bs []block
	if err := json.Unmarshal(b, &bs); err != nil {
		return err
	}
	*c = bs
	return nil
}

// text joins the content's text blocks the way pi-ai's contentText does.
func (c content) text() string {
	var parts []string
	for _, b := range c {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// blocks maps content to entry blocks. Images have no block kind and are
// left out.
func (c content) blocks() []transcript.Block {
	var out []transcript.Block
	for _, b := range c {
		switch b.Type {
		case "text":
			out = append(out, transcript.Block{Kind: transcript.BlockText, Text: b.Text})
		case "thinking":
			out = append(out, transcript.Block{Kind: transcript.BlockReasoning, Text: b.Thinking})
		case "toolCall":
			out = append(out, transcript.Block{Kind: transcript.BlockToolUse, ToolID: b.ID, Name: b.Name, Input: b.Arguments})
		}
	}
	return out
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// sections is a system message's named prompt sections in file order, which
// is the order pi joins them in. A null section removes one an earlier
// system message set.
type sections []section

type section struct {
	name string
	text *string
}

func (s *sections) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("sections: %v", tok)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name, _ := tok.(string)
		var text *string
		if err := dec.Decode(&text); err != nil {
			return err
		}
		*s = append(*s, section{name, text})
	}
	return nil
}

// systemText is a system message as the model reads it: its content, then
// each section, blank-line separated (getSystemMessageText in pi-ai).
func systemText(m stored) string {
	var parts []string
	if t := m.Content.text(); t != "" {
		parts = append(parts, t)
	}
	for _, sec := range m.Sections {
		if sec.text != nil && *sec.text != "" {
			parts = append(parts, *sec.text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// peek reads the header, which is the first or second row: pi writes a
// fixed-width title row ahead of it.
func peek(path string) (transcript.Info, error) {
	var found transcript.Info
	err := transcript.EachLine(path, func(raw json.RawMessage) (bool, error) {
		var h sessionHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return false, err
		}
		if h.Type != "session" {
			return true, nil
		}
		found = transcript.Info{ID: h.ID, CWD: h.CWD}
		if t, err := time.Parse(time.RFC3339Nano, h.Timestamp); err == nil {
			found.Updated = t
		}
		return false, nil
	})
	if err != nil {
		return transcript.Info{}, err
	}
	if found.ID == "" {
		return transcript.Info{}, fmt.Errorf("%s: no session header", path)
	}
	return found, nil
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	// The header is handled in decode because it may not be the first row.
	return false, nil
}

func text(s string) []transcript.Block {
	return []transcript.Block{{Kind: transcript.BlockText, Text: s}}
}

func (v variant) decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.Type == "session" {
		var h sessionHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return transcript.Entry{}, false, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return transcript.Entry{}, false, err
		}
		s.ID = h.ID
		s.CWD = h.CWD
		if t, err := time.Parse(time.RFC3339Nano, h.Timestamp); err == nil {
			s.Created = t
		}
		s.Vendor = &Vendor{Header: fields}
		return transcript.Entry{}, false, nil
	}
	// Rows of a version 1 session have no id; Finish links them.
	e := transcript.Entry{ID: r.ID, Role: transcript.RoleOpaque}
	if r.ParentID != nil {
		e.ParentID = *r.ParentID
	}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.ID, err)
		}
		e.Time = t
	}
	switch r.Type {
	case "message":
		var m stored
		if err := json.Unmarshal(r.Message, &m); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: message: %w", r.ID, err)
		}
		v.message(&e, m)
	case "custom_message":
		// omp leaves two of its own planning notes out of the model's
		// context (buildSessionContext in session-context.ts).
		held := v == ompVariant && (r.CustomType == "prewalk-plan" || r.CustomType == "vibe-mode-context")
		v.custom(&e, r.CustomType, r.Attribution, r.Content, r.Display, held)
	case "branch_summary":
		if r.Summary != "" {
			e.Role, e.Content = transcript.RoleUser, text(v.branchSummary(r.Summary))
		}
	case "compaction":
		// Shown where it happened; it reaches the model through the
		// Compaction Finish sets on the newest one.
		e.Role, e.Content, e.Audience = transcript.RoleUser, text(v.compactionSummary(r.Summary, r.Method, r.PreserveData)), transcript.AudienceUser
	}
	return e, true, nil
}

// message fills an entry from a message row the way the agent's
// convertToLlm hands it to the model and its TUI shows it. A role neither
// agent knows stays opaque: both drop it.
func (v variant) message(e *transcript.Entry, m stored) {
	switch m.Role {
	case "system":
		// Never shown (addMessageToChat skips it).
		e.Role, e.Content, e.Audience = transcript.RoleSystem, text(systemText(m)), transcript.AudienceModel
	case "developer":
		// omp's reminders and notes. Its chat renders only user text.
		e.Role, e.Content, e.Audience = transcript.RoleSystem, m.Content.blocks(), transcript.AudienceModel
	case "user":
		e.Role, e.Content = transcript.RoleUser, m.Content.blocks()
	case "assistant":
		e.Role, e.Content = transcript.RoleAssistant, m.Content.blocks()
	case "toolResult":
		st := transcript.StatusOK
		if m.IsError {
			st = transcript.StatusError
		}
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: m.ToolCallID, Name: m.ToolName, Text: joined(m.Content), Status: st}}
	case "bashExecution":
		e.Role, e.Content = transcript.RoleUser, text(bashText(m))
		if m.ExcludeFromContext {
			e.Audience = transcript.AudienceUser
		}
	case "pythonExecution":
		e.Role, e.Content = transcript.RoleUser, text(pythonText(m))
		if m.ExcludeFromContext {
			e.Audience = transcript.AudienceUser
		}
	case "custom", "hookMessage":
		// hookMessage is the version 2 name of custom; pi renames it on
		// load and omp handles both alike.
		v.custom(e, m.CustomType, m.Attribution, m.Content, m.Display, false)
	case "fileMention":
		v.fileMention(e, m.Files)
	case "branchSummary":
		e.Role, e.Content = transcript.RoleUser, text(v.branchSummary(m.Summary))
	case "compactionSummary":
		e.Role, e.Content = transcript.RoleUser, text(v.compactionSummary(m.Summary, m.Method, nil))
	}
}

// joined is a tool result's text blocks run together.
func joined(c content) string {
	var b strings.Builder
	for _, x := range c {
		b.WriteString(x.Text)
	}
	return b.String()
}

// custom fills an extension's message. pi sends it as the user's; omp as a
// developer message, except a /skill: or collaborator prompt the person
// sent. display decides whether the TUI shows it; held keeps it from the
// model as well.
func (v variant) custom(e *transcript.Entry, customType, attribution string, c content, display, held bool) {
	e.Role, e.Content = transcript.RoleUser, c.blocks()
	if v == ompVariant && !(attribution == "user" && (customType == "skill-prompt" || customType == "collab-prompt")) {
		e.Role = transcript.RoleSystem
	}
	switch {
	case held && display:
		e.Audience = transcript.AudienceUser
	case held:
		e.Audience = transcript.AudienceNone
	case !display:
		e.Audience = transcript.AudienceModel
	}
}

// fileMention fills omp's @path read: text files go to the model as one
// developer message of <file> elements. A mention of images only goes as
// the user's; a mixed one is split by omp in two, and the entry keeps the
// text part.
func (v variant) fileMention(e *transcript.Entry, files []mentionedFile) {
	wrap := func(f mentionedFile) string {
		inner := "\n"
		if f.Content != "" {
			inner = "\n" + f.Content + "\n"
		}
		return `<file path="` + f.Path + `">` + inner + "</file>"
	}
	var texts, images []string
	for _, f := range files {
		if len(f.Image) > 0 && string(f.Image) != "null" {
			images = append(images, wrap(f))
		} else {
			texts = append(texts, wrap(f))
		}
	}
	e.Role, e.Content = transcript.RoleSystem, text(strings.Join(texts, "\n"))
	if len(texts) == 0 {
		e.Role, e.Content = transcript.RoleUser, text(strings.Join(images, "\n"))
	}
}

// bashText is a person's bash run as the model reads it
// (bashExecutionToText in both agents' messages.ts). omp also appends a
// notice from the run's output metadata, which this leaves out.
func bashText(m stored) string {
	t := "Ran `" + m.Command + "`\n"
	if m.Output != "" {
		t += "```\n" + m.Output + "\n```"
	} else {
		t += "(no output)"
	}
	if m.Cancelled {
		t += "\n\n(command cancelled)"
	} else if m.ExitCode != nil && *m.ExitCode != 0 {
		t += fmt.Sprintf("\n\nCommand exited with code %d", *m.ExitCode)
	}
	if m.Truncated && m.FullOutputPath != "" {
		t += "\n\n[Output truncated. Full output: " + m.FullOutputPath + "]"
	}
	return t
}

// pythonText is an omp $ run as the model reads it (pythonExecutionToText).
func pythonText(m stored) string {
	t := "Ran Python:\n```python\n" + m.Code + "\n```\n"
	if m.Output != "" {
		t += "Output:\n```\n" + m.Output + "\n```"
	} else {
		t += "(no output)"
	}
	if m.Cancelled {
		t += "\n\n(execution cancelled)"
	} else if m.ExitCode != nil && *m.ExitCode != 0 {
		t += fmt.Sprintf("\n\nExecution failed with code %d", *m.ExitCode)
	}
	return t
}

// The wrappers each agent puts around a summary it sends: pi's constants in
// core/messages.ts, omp's prompt templates in
// packages/agent/src/compaction/prompts.
const (
	piCompactionPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	piCompactionSuffix = "\n</summary>"
	piBranchPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	piBranchSuffix     = "</summary>"

	ompCompactionTemplate = "Prior model work/tool state available.\nMUST build on prior work; NEVER duplicate prior work.\n\n<summary>\n{{summary}}\n</summary>\n"
	ompHandoffTemplate    = "Context replaced. The <handoff> below is a handoff document a prior instance of you wrote from the full conversation. It is your own working memory, not user input.\n- First person inside it refers to you (the prior instance).\n- \"Next Steps\" is your own resumed plan; re-check it against the latest user message before acting.\n- The handoff already exists and is complete: NEVER write another handoff document unless the user explicitly asks.\nMUST build on prior work; NEVER duplicate prior work.\n\n<handoff>\n{{summary}}\n</handoff>\n"
	ompBranchTemplate     = "Branch-return summary:\n\n<summary>\n{{summary}}\n</summary>\n"
)

func (v variant) branchSummary(summary string) string {
	if v == ompVariant {
		return renderPrompt(ompBranchTemplate, summary)
	}
	return piBranchPrefix + summary + piBranchSuffix
}

// compactionSummary is a compaction's summary as the model reads it. omp
// sends a snapcompact summary as is, since its archive follows it, and a
// handoff document under a template of its own.
func (v variant) compactionSummary(summary, method string, preserve map[string]json.RawMessage) string {
	if v == piVariant {
		return piCompactionPrefix + summary + piCompactionSuffix
	}
	switch {
	case isObject(preserve["snapcompact"]):
		return summary
	case method == "handoff":
		return renderPrompt(ompHandoffTemplate, summary)
	}
	return renderPrompt(ompCompactionTemplate, summary)
}

// renderPrompt fills an omp template the way prompt.render does, for what
// a summary can hold: lines lose trailing whitespace outside code fences
// and trailing blank lines go. Its other clean-ups (blank-line runs, table
// padding) are not reproduced.
func renderPrompt(template, summary string) string {
	lines := strings.Split(strings.ReplaceAll(template, "{{summary}}", summary), "\n")
	fenced := false
	for i, l := range lines {
		if t := strings.TrimLeft(l, " \t"); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		if !fenced {
			lines[i] = strings.TrimRight(l, " \t\r")
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func isObject(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '{'
}

// finish reads the rows together the way the agents' loaders do: it links a
// version 1 session, finds the model the session runs on, and applies the
// newest compaction (or omp /clear) and pi's context edits on the active
// branch.
func (v variant) finish(s *transcript.Session) error {
	rows := make([]row, len(s.Entries))
	msgs := make([]stored, len(s.Entries))
	for i, e := range s.Entries {
		if json.Unmarshal(e.Raw, &rows[i]) != nil {
			rows[i] = row{}
			continue
		}
		if rows[i].Type == "message" {
			_ = json.Unmarshal(rows[i].Message, &msgs[i])
		}
	}
	if version(s) < 2 {
		relinkV1(s, rows)
	}
	branch := s.Branch()
	if m := v.model(rows, msgs, branch); m != "" {
		s.Model = m
	}

	// The context starts from the newest compaction on the branch, or for
	// omp from a /clear boundary if that came later. Older ones do nothing.
	cut := -1
	for at, i := range branch {
		if t := rows[i].Type; t == "compaction" || (v == ompVariant && t == "reset_boundary") {
			cut = at
		}
	}
	keep := -1 // position on the branch of the first kept entry
	if cut >= 0 && rows[branch[cut]].Type == "compaction" {
		keep = v.kept(rows, branch, cut)
	}

	omitted := map[string]bool{}
	if v == piVariant {
		omitted = applyEdits(s, rows, msgs, branch, cut, keep)
	}
	if cut < 0 {
		return nil
	}
	i := branch[cut]
	c := &transcript.Compaction{}
	if keep >= 0 {
		c.Keep = s.Entries[branch[keep]].ID
	}
	r := rows[i]
	if r.Type == "compaction" && v == piVariant {
		// pi drops every system message before the compaction, kept range
		// included (buildContextEntries), and sends the prompt state the
		// compaction recorded instead.
		for _, j := range branch[:cut] {
			if msgs[j].Role == "system" && rows[j].Type == "message" {
				s.Entries[j].Audience = transcript.AudienceNone
			}
		}
	}
	if r.Type == "compaction" && !omitted[r.ID] {
		if v == piVariant {
			if r.SystemMessage != nil {
				c.Summary = append(c.Summary, transcript.Entry{Role: transcript.RoleSystem, Time: s.Entries[i].Time,
					Content: text(systemText(*r.SystemMessage)), Audience: transcript.AudienceModel})
			}
		}
		self := s.Entries[i]
		self.Compaction, self.Raw, self.Audience = nil, nil, transcript.AudienceModel
		c.Summary = append(c.Summary, self)
	}
	s.Entries[i].Compaction = c
	return nil
}

// version is the session's format version; a header without one is
// version 1.
func version(s *transcript.Session) int {
	v, ok := s.Vendor.(*Vendor)
	if !ok {
		return 1
	}
	var n int
	if json.Unmarshal(v.Header["version"], &n) != nil || n == 0 {
		return 1
	}
	return n
}

// relinkV1 gives the rows of a version 1 session the links pi gives them on
// load (migrateV1ToV2): each row is the child of the one before it in the
// file, and a compaction's firstKeptEntryIndex, an index into the file's
// rows, becomes the id of that row. The ids are made up for this read;
// pi makes up its own, so none is ever written.
func relinkV1(s *transcript.Session, rows []row) {
	ids := map[int]string{}
	prev, n := "", 0
	for i, e := range s.Entries {
		if !json.Valid(e.Raw) {
			// pi skips a line it cannot parse, and it takes no index.
			continue
		}
		at := n
		n++
		if rows[i].Type == "session" {
			continue
		}
		id := fmt.Sprintf("v1-%d", at)
		ids[at] = id
		s.Entries[i].ID, s.Entries[i].ParentID = id, prev
		rows[i].ID = id
		prev = id
	}
	for i := range rows {
		if r := &rows[i]; r.Type == "compaction" && r.FirstKeptEntryIndex != nil {
			if id, ok := ids[*r.FirstKeptEntryIndex]; ok {
				r.FirstKeptEntryID = id
			}
		}
	}
}

// model is the model the session runs on at the leaf: the newest model
// change on the branch, or the model of a later assistant message. omp
// names the model "provider/modelId", takes only changes for its default
// role, and once one is recorded no longer lets assistant messages override
// it (buildSessionContext in session-context.ts).
func (v variant) model(rows []row, msgs []stored, branch []int) string {
	var model string
	explicit := false
	for _, i := range branch {
		r, m := rows[i], msgs[i]
		switch {
		case r.Type == "model_change" && v == piVariant:
			model = r.ModelID
		case r.Type == "model_change" && r.Model != "" && (r.Role == "" || r.Role == "default"):
			model, explicit = r.Model, true
		case r.Type == "message" && m.Role == "assistant" && v == piVariant:
			model = m.Model
		case r.Type == "message" && m.Role == "assistant" && !explicit:
			model = m.Provider + "/" + m.Model
		}
	}
	return model
}

// kept is the position on the branch of the first entry a compaction at cut
// keeps, or -1. pi keeps from firstKeptEntryId. omp does too, and when a
// provider replays history natively it keeps what follows the last entry
// that replay covers: always for an OpenAI remote compaction, whose own
// replacement history stands in for the rest and is not modeled here, and
// for an Anthropic one without a firstKeptEntryId.
func (v variant) kept(rows []row, branch []int, cut int) int {
	r := rows[branch[cut]]
	find := func(id string) int {
		for at := 0; at < cut && id != ""; at++ {
			if rows[branch[at]].ID == id {
				return at
			}
		}
		return -1
	}
	after := func(id string) int {
		if at := find(id); at >= 0 && at+1 < cut {
			return at + 1
		}
		return -1
	}
	if v == piVariant {
		return find(r.FirstKeptEntryID)
	}
	if isRemoteCompaction(r.PreserveData["openaiRemoteCompaction"]) {
		return after(r.ProviderReplayThroughEntryID)
	}
	if at := find(r.FirstKeptEntryID); at >= 0 {
		return at
	}
	if r.FirstKeptEntryID == "" && isAnthropicCompaction(r.PreserveData["anthropicCompaction"]) {
		return after(r.ProviderReplayThroughEntryID)
	}
	return -1
}

// isRemoteCompaction is getOpenAiRemoteCompactionPayload's check.
func isRemoteCompaction(raw json.RawMessage) bool {
	var p struct {
		Provider           string            `json:"provider"`
		ReplacementHistory []json.RawMessage `json:"replacementHistory"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Provider == "" || p.ReplacementHistory == nil {
		return false
	}
	for _, item := range p.ReplacementHistory {
		if !isObject(item) {
			return false
		}
	}
	return true
}

// isAnthropicCompaction is getPreservedAnthropicCompactionData's check.
func isAnthropicCompaction(raw json.RawMessage) bool {
	var p struct {
		Provider string `json:"provider"`
		Content  string `json:"content"`
	}
	return json.Unmarshal(raw, &p) == nil && p.Provider != "" && p.Content != ""
}

// applyEdits applies pi's context edits (buildSessionProjection): among the
// entries the model is given, the newest edit of each wins. A null edit
// leaves its target out of the model's context, and the TUI, which renders
// entries unedited, still shows it. A replacement swaps the content of a
// user, assistant, tool result or extension message; the entry then shows
// the replacement too, as an entry has one content. It returns the ids of
// the entries left out.
func applyEdits(s *transcript.Session, rows []row, msgs []stored, branch []int, cut, keep int) map[string]bool {
	// The entries pi builds context from: the compaction, its kept range,
	// and what follows it.
	path := branch
	if cut >= 0 {
		path = []int{branch[cut]}
		if keep >= 0 {
			path = append(path, branch[keep:cut]...)
		}
		path = append(path, branch[cut+1:]...)
	}
	on := map[string]int{}
	edits := map[string]row{}
	for _, i := range path {
		on[rows[i].ID] = i
		if rows[i].Type == "context_edit" {
			edits[rows[i].TargetID] = rows[i]
		}
	}
	omitted := map[string]bool{}
	for target, edit := range edits {
		i, ok := on[target]
		if !ok || target == "" {
			continue
		}
		e := &s.Entries[i]
		var rep *struct {
			Content content `json:"content"`
		}
		if json.Unmarshal(edit.Replacement, &rep) != nil {
			continue
		}
		if rep == nil {
			omitted[target] = true
			switch e.Audience {
			case transcript.AudienceAll:
				e.Audience = transcript.AudienceUser
			case transcript.AudienceModel:
				e.Audience = transcript.AudienceNone
			}
			continue
		}
		role := msgs[i].Role
		if rows[i].Type == "custom_message" {
			role = "custom"
		} else if rows[i].Type != "message" {
			continue
		}
		switch role {
		case "user", "assistant", "custom", "hookMessage":
			e.Content = rep.Content.blocks()
		case "toolResult":
			for k := range e.Content {
				if e.Content[k].Kind == transcript.BlockToolResult {
					e.Content[k].Text = joined(rep.Content)
				}
			}
		}
	}
	return omitted
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Header {
			fields[k] = val
		}
	}
	identity, err := json.Marshal(sessionHeader{Type: "session", Version: 3, ID: s.ID, Timestamp: s.Created.UTC().Format(time.RFC3339Nano), CWD: s.CWD})
	if err != nil {
		return nil, err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(identity, &known); err != nil {
		return nil, err
	}
	for k, val := range known {
		fields[k] = val
	}
	h, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{h}, nil
}

// messageRow is a message row as this codec writes it. The first row of a
// session has a null parentId, as pi writes it.
type messageRow struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	var m message
	switch e.Role {
	case transcript.RoleUser, transcript.RoleAssistant:
		m.Role = string(e.Role)
		m.Timestamp = t.UnixMilli()
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				m.Content = append(m.Content, block{Type: "text", Text: b.Text})
			case transcript.BlockReasoning:
				m.Content = append(m.Content, block{Type: "thinking", Thinking: b.Text})
			case transcript.BlockToolUse:
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				m.Content = append(m.Content, block{Type: "toolCall", ID: b.ToolID, Name: b.Name, Arguments: args})
			}
		}
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			m = message{Role: "toolResult", ToolCallID: b.ToolID, ToolName: b.Name, IsError: b.Status == transcript.StatusError,
				Content: []block{{Type: "text", Text: b.Text}}, Timestamp: t.UnixMilli()}
		}
	default:
		return nil, nil
	}
	if m.Role == "" || (len(m.Content) == 0 && m.Role != "toolResult") {
		return nil, nil
	}
	if m.Content == nil {
		m.Content = []block{}
	}
	mj, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	r := messageRow{Type: "message", ID: e.ID, Timestamp: t.UTC().Format("2006-01-02T15:04:05.000Z"), Message: mj}
	if e.ParentID != "" {
		parent := e.ParentID
		r.ParentID = &parent
	}
	return json.Marshal(r)
}
