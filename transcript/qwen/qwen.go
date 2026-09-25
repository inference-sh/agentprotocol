// Package qwen reads and writes Qwen Code sessions.
//
// Qwen keeps one JSONL per session under
// ~/.qwen/projects/<sanitized cwd>/chats/<session id>.jsonl, where the
// project directory is the working directory with every character other
// than a letter or digit turned into a dash. Rows form a tree through uuid
// and parentUuid. A row's type is user, assistant, tool_result or system;
// the first three carry a message whose parts are Gemini-shaped (text,
// functionCall, functionResponse). System rows (telemetry, checkpoints,
// slash command results) stay opaque in the chain, except that a
// chat_compression row carries the history the model is given from then on.
//
// Qwen forked Gemini CLI but not its session store: nothing here matches
// ~/.gemini/tmp.
package qwen

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Version is the Qwen Code version whose row shape this codec writes for a
// session that did not come from Qwen.
const Version = "0.24.4"

const (
	agent = "qwen"
	root  = ".qwen/projects"
)

// sessionFile is the file name Qwen lists a session under
// (SESSION_FILE_PATTERN in its services/sessionService.ts). A file that does
// not match is not a session to Qwen.
var sessionFile = regexp.MustCompile(`^[0-9a-fA-F-]{32,36}\.jsonl$`)

// Codec is the Qwen Code session store.
var Codec = transcript.JSONL{
	Agent: agent,
	Layout: transcript.Layout{
		Ext: ".jsonl",
		Files: func(home, cwd string) ([]string, error) {
			project := "*"
			if cwd != "" {
				project = ProjectDir(cwd)
			}
			paths, err := transcript.Glob(filepath.Join(home, root, project, "chats", "*.jsonl"))
			if err != nil {
				return nil, err
			}
			out := paths[:0]
			for _, p := range paths {
				if sessionFile.MatchString(filepath.Base(p)) {
					out = append(out, p)
				}
			}
			return out, nil
		},
		PathFor: func(home string, s *transcript.Session) string {
			return filepath.Join(home, root, ProjectDir(s.CWD), "chats", s.ID+".jsonl")
		},
		Peek: peek,
	},
	Decode:  decode,
	Finish:  finish,
	Encode:  encode,
	Prepare: prepare,
	Tree:    true,
	// Qwen records a compaction as a chat_compression row whose history the
	// model is given from then on, and keeps the rows before it for the
	// person; a realtime_message row is shown and never sent.
	Caps: transcript.Capabilities{Compaction: true, UserOnly: true},
}

// Vendor is what a Qwen session carries in Session.Vendor: the stamps Qwen
// puts on every row.
type Vendor struct {
	Version   string
	GitBranch string
}

type row struct {
	UUID           string          `json:"uuid"`
	ParentUUID     *string         `json:"parentUuid"`
	SessionID      string          `json:"sessionId"`
	Timestamp      string          `json:"timestamp"`
	Type           string          `json:"type"`
	Subtype        subtype         `json:"subtype,omitempty"`
	Provenance     string          `json:"provenance,omitempty"`
	CWD            string          `json:"cwd"`
	Version        string          `json:"version,omitempty"`
	GitBranch      string          `json:"gitBranch,omitempty"`
	Message        *message        `json:"message,omitempty"`
	Model          string          `json:"model,omitempty"`
	ToolCallResult *toolCallResult `json:"toolCallResult,omitempty"`
	SystemPayload  json.RawMessage `json:"systemPayload,omitempty"`
}

// subtype refines a row's type. Only the subtypes that change what the
// person sees or the model is given are named.
type subtype string

const (
	subtypeCompression  subtype = "chat_compression"
	subtypeSlashCommand subtype = "slash_command"
	subtypeRealtime     subtype = "realtime_message"
	subtypeGoalRuntime  subtype = "goal_runtime"
	subtypeNotification subtype = "notification"
)

// sideRecords are the system rows Qwen's loader keeps out of the
// conversation: they are never the leaf and no row finds its parent among
// them (isTranscriptConversationRecord in utils/transcript-records.ts).
var sideRecords = map[subtype]bool{
	"session_artifact_event":    true,
	"session_artifact_snapshot": true,
	"session_sources_snapshot":  true,
	"managed_session_header_v1": true,
	"managed_session_event_v1":  true,
	"managed_session_commit_v1": true,
}

type message struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	InlineData       *blob             `json:"inlineData,omitempty"`
	FileData         *fileData         `json:"fileData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

// blob is a Gemini inlineData part: bytes in base64, as Qwen records an
// image or a file read into a prompt or returned by a tool.
type blob struct {
	MimeType    string `json:"mimeType"`
	Data        string `json:"data"`
	DisplayName string `json:"displayName,omitempty"`
}

// fileData is a Gemini fileData part: a file by reference.
type fileData struct {
	MimeType    string `json:"mimeType,omitempty"`
	FileURI     string `json:"fileUri"`
	DisplayName string `json:"displayName,omitempty"`
}

type functionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type functionResponse struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
	// Parts are the images and files the tool returned. Qwen nests them in
	// the response for every model (convertToFunctionResponse in
	// core/coreToolScheduler.ts).
	Parts []part `json:"parts,omitempty"`
}

type toolCallResult struct {
	CallID string `json:"callId"`
	Status string `json:"status"`
}

// compressionPayload is a chat_compression row's systemPayload. When it
// carries compressedHistory, Qwen's resume replaces the whole model history
// with it (SessionApiHistoryAccumulator in services/session-api-history.ts);
// the history the person sees is untouched.
type compressionPayload struct {
	CompressedHistory []message `json:"compressedHistory"`
}

// promptPayload is a user row's systemPayload. Over ACP, Qwen records the
// prompt's text as the message and the resource links the prompt carried
// here: its transcript replay shows them after the text
// (projectUserAttachmentReferences in acp-bridge/src/transcript-replay.ts),
// and the resumed model history, built from the message, never has them.
type promptPayload struct {
	ResourceLinks []resourceLink `json:"resourceLinks"`
}

// resourceLink is an ACP resource_link content block.
type resourceLink struct {
	Type     string `json:"type"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
}

// slashCommandPayload is a slash_command row's systemPayload.
type slashCommandPayload struct {
	Phase              string `json:"phase"`
	RawCommand         string `json:"rawCommand"`
	SentToModel        *bool  `json:"sentToModel"`
	OutputHistoryItems []struct {
		Type string `json:"type"`
	} `json:"outputHistoryItems"`
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.UUID == "" {
		return transcript.Entry{}, false, nil
	}
	if r.Type == "system" && sideRecords[r.Subtype] {
		// Without an id the row stays out of the chain, as it does for Qwen,
		// and a row appended after it links to the conversation instead.
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.UUID, Role: transcript.RoleOpaque}
	if r.ParentUUID != nil {
		e.ParentID = *r.ParentUUID
	}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.UUID, err)
		}
		e.Time = t
	}
	if s.ID == "" {
		s.ID = r.SessionID
	}
	if s.CWD == "" {
		s.CWD = r.CWD
	}
	if r.Version != "" || r.GitBranch != "" {
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
	}
	if r.Type == "system" && r.Subtype == subtypeCompression {
		var p compressionPayload
		if err := json.Unmarshal(r.SystemPayload, &p); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: compression: %w", r.UUID, err)
		}
		if p.CompressedHistory != nil {
			// composePostCompactHistory lays it out as the summary, a
			// synthetic acknowledgement, the files and images Qwen embeds
			// again, and a function call still waiting for its response.
			// The summary and that call are conversation; the rest is
			// Qwen's own context.
			c := &transcript.Compaction{}
			for i, m := range p.CompressedHistory {
				e, err := content(m, transcript.StatusOK)
				if err != nil {
					return transcript.Entry{}, false, fmt.Errorf("row %s: compression: %w", r.UUID, err)
				}
				if i == 0 || hasCall(e) {
					e.Audience = transcript.AudienceAll
				}
				c.Summary = append(c.Summary, e)
			}
			e.Compaction = c
		}
		return e, true, nil
	}
	if r.Message == nil {
		return e, true, nil
	}
	switch r.Type {
	case "user":
		e.Role = transcript.RoleUser
	case "assistant":
		e.Role = transcript.RoleAssistant
		if r.Model != "" {
			s.Model = r.Model
		}
	case "tool_result":
		e.Role = transcript.RoleTool
	default:
		return e, true, nil
	}
	switch r.Subtype {
	case subtypeRealtime:
		// A voice exchange: shown, never replayed to the model.
		e.Audience = transcript.AudienceUser
	case subtypeGoalRuntime:
		// The goal runtime's own prompt: the model is given it, the TUI's
		// resume leaves it out.
		e.Role, e.Audience = transcript.RoleSystem, transcript.AudienceModel
	case subtypeNotification:
		// A background task reporting back. Qwen shows it as a notice, not a
		// turn; only the recorder's system provenance marks it as injected.
		if r.Provenance == "system" {
			e.Role = transcript.RoleSystem
		}
	}
	status := transcript.StatusOK
	if r.ToolCallResult != nil && r.ToolCallResult.Status != "" && r.ToolCallResult.Status != "success" {
		status = transcript.StatusError
	}
	c, err := content(*r.Message, status)
	if err != nil {
		return transcript.Entry{}, false, fmt.Errorf("row %s: %w", r.UUID, err)
	}
	e.Content = c.Content
	if r.Type == "user" && len(r.SystemPayload) > 0 {
		var p promptPayload
		if json.Unmarshal(r.SystemPayload, &p) == nil {
			var links []transcript.Block
			for _, l := range p.ResourceLinks {
				if l.Type == "resource_link" && l.URI != "" {
					links = append(links, transcript.Block{Kind: mediaKind(l.MimeType), MediaType: l.MimeType, URI: l.URI, Name: l.Name})
				}
			}
			if links != nil {
				e.ModelContent = e.Content
				e.Content = append(append([]transcript.Block(nil), e.Content...), links...)
			}
		}
	}
	return e, true, nil
}

// content maps a Gemini-shaped message to an entry, for the history a
// compression keeps as much as for a row's own message. A user message that
// only answers function calls is a tool entry.
func content(m message, status transcript.Status) (transcript.Entry, error) {
	e := transcript.Entry{Role: transcript.RoleUser, Audience: transcript.AudienceModel}
	if m.Role == "model" {
		e.Role = transcript.RoleAssistant
	}
	results := 0
	for _, p := range m.Parts {
		switch {
		case p.FunctionCall != nil:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Input: p.FunctionCall.Args})
		case p.FunctionResponse != nil:
			results++
			fr := p.FunctionResponse
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: fr.ID, Name: fr.Name, Text: responseText(fr.Response), Status: status})
			for _, np := range fr.Parts {
				b, ok, err := media(np, fr.ID)
				if err != nil {
					return transcript.Entry{}, err
				}
				if ok {
					e.Content = append(e.Content, b)
				}
			}
		case p.InlineData != nil || p.FileData != nil:
			b, _, err := media(p, "")
			if err != nil {
				return transcript.Entry{}, err
			}
			e.Content = append(e.Content, b)
		case p.Thought:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
		default:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
	}
	if e.Role == transcript.RoleUser && results > 0 && results == len(m.Parts) {
		e.Role = transcript.RoleTool
	}
	return e, nil
}

// media reads an inlineData or fileData part as an image or file block.
// toolID is set for one a tool returned.
func media(p part, toolID string) (transcript.Block, bool, error) {
	switch {
	case p.InlineData != nil:
		data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
		if err != nil {
			return transcript.Block{}, false, fmt.Errorf("inlineData: %w", err)
		}
		return transcript.Block{Kind: mediaKind(p.InlineData.MimeType), ToolID: toolID, MediaType: p.InlineData.MimeType, Data: data, Name: p.InlineData.DisplayName}, true, nil
	case p.FileData != nil:
		return transcript.Block{Kind: mediaKind(p.FileData.MimeType), ToolID: toolID, MediaType: p.FileData.MimeType, URI: p.FileData.FileURI, Name: p.FileData.DisplayName}, true, nil
	}
	return transcript.Block{}, false, nil
}

// mediaKind is the block an attachment of a media type is: an image, or any
// other file.
func mediaKind(mediaType string) transcript.BlockKind {
	if strings.HasPrefix(mediaType, "image/") {
		return transcript.BlockImage
	}
	return transcript.BlockFile
}

// mediaPart is the part Qwen records an image or file as: inlineData for
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

// finish reads the rows the way Qwen's loader and resume do
// (prepareTranscriptRecords in utils/transcript-records.ts, then
// SessionApiHistoryAccumulator). A mid_turn_user_message stays its own
// entry: the accumulator joins its parts onto the user-role content before
// it, which gives the model the same text in one Gemini message.
func finish(s *transcript.Session) error {
	// Rows repeated under one uuid are fragments of one record: the first
	// carries the links, and the parts of every copy are joined onto it. The
	// leaf is the last row's uuid, which may name an earlier copy.
	first := map[string]int{}
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.ID == "" {
			continue
		}
		s.Leaf = e.ID
		j, ok := first[e.ID]
		if !ok {
			first[e.ID] = i
			continue
		}
		base := &s.Entries[j]
		base.Content = append(base.Content, e.Content...)
		if e.Time.After(base.Time) {
			base.Time = e.Time
		}
		e.ID, e.ParentID = "", ""
	}

	// An ACP slash command records the typed command as a user row and its
	// output as a slash_command result. The accumulator drops the command
	// from the model's history when the result follows it directly and is
	// only local output; the person still sees it.
	last, lastUser := -1, false // the last row the accumulator took into history
	for _, i := range s.Branch() {
		e := &s.Entries[i]
		var r row
		if json.Unmarshal(e.Raw, &r) != nil {
			continue
		}
		switch {
		case e.Role != transcript.RoleOpaque:
			if r.Subtype != subtypeRealtime {
				last, lastUser = i, r.Type == "user"
			}
		case e.Compaction != nil:
			last = -1
		case r.Type == "system" && r.Subtype == subtypeSlashCommand:
			if last >= 0 && localCommand(r.SystemPayload, s.Entries[last].Raw) {
				s.Entries[last].Audience = transcript.AudienceUser
			}
			// A command fences off the input before it from the next one.
			if lastUser {
				last = -1
			}
		}
	}
	keptVerbatim(s)
	return nil
}

// keptVerbatim marks the turns a compressed history repeats from before the
// compression as conversation. Qwen's own compression keeps none (the
// summary, an acknowledgement, re-embedded files); a session written from
// another agent's compaction keeps that agent's recent turns there, and
// they must travel on with it rather than read as Qwen's context.
func keptVerbatim(s *transcript.Session) {
	seen := map[string]bool{}
	for _, i := range s.Branch() {
		e := s.Entries[i]
		if c := e.Compaction; c != nil {
			for k := range c.Summary {
				m := &c.Summary[k]
				if m.Audience == transcript.AudienceModel && seen[turnKey(*m)] {
					m.Audience = transcript.AudienceAll
				}
			}
			continue
		}
		if e.Role != transcript.RoleOpaque {
			seen[turnKey(e)] = true
		}
	}
}

// turnKey is what makes two entries the same turn: role and text, tool
// calls and results by id.
func turnKey(e transcript.Entry) string {
	var b strings.Builder
	b.WriteString(string(e.Role))
	for _, x := range e.Content {
		b.WriteString("\x00" + string(x.Kind) + "\x00" + x.ToolID + "\x00" + x.Text)
	}
	return b.String()
}

// prepare gives a session from another agent a UUID when its id is not one
// Qwen lists: Qwen only lists a file whose name matches sessionFile and whose
// first row's sessionId is that name. It also lays out the compactions the
// session carries.
func prepare(home string, s *transcript.Session) error {
	if s.Agent != agent && !sessionFile.MatchString(s.ID+".jsonl") {
		s.ID = transcript.NewUUID()
	}
	restoreRetired(s)
	compactions(s)
	return nil
}

// restoreRetired makes an entry a new compaction marker retires, which the
// source agent showed and no longer sent, an ordinary row again when a
// shown-only realtime row, which holds a prompt's or an answer's text
// alone, would lose part of it: a tool call, its result, an image. Qwen
// keeps the rows before a chat_compression for the person and gives the
// model the compressed history instead, so the row stays out of the
// model's view all the same.
func restoreRetired(s *transcript.Session) {
	for i, e := range s.Entries {
		c := e.Compaction
		if c == nil || e.Raw != nil {
			continue
		}
		upto := i
		if k := slices.IndexFunc(s.Entries[:i], func(m transcript.Entry) bool { return c.Keep != "" && m.ID == c.Keep }); k >= 0 {
			upto = k
		}
		for k := range s.Entries[:upto] {
			if m := &s.Entries[k]; m.Raw == nil && m.Compaction == nil && m.Audience == transcript.AudienceUser && !realtimeHolds(*m) {
				m.Audience = transcript.AudienceAll
			}
		}
	}
}

// realtimeHolds reports whether a realtime_message row holds all of an
// entry: a prompt's or an answer's text and nothing else.
func realtimeHolds(e transcript.Entry) bool {
	if e.Role != transcript.RoleUser && e.Role != transcript.RoleAssistant {
		return false
	}
	return !slices.ContainsFunc(e.Content, func(b transcript.Block) bool { return b.Kind != transcript.BlockText })
}

// resumeTrailer and acknowledgement are what composePostCompactHistory
// (services/postCompactAttachments.ts) puts around a summary: the trailer
// after it in the user message, and the model's reply after that.
const (
	resumeTrailer   = "Resume the prior task using the summary above. Continue from the last in-flight step; do not acknowledge the summary, do not re-introduce, do not greet the user again."
	acknowledgement = "Got it. Thanks for the additional context!"
)

// compactions turns each compaction marker this write creates into the
// history a chat_compression row gives the model outright: the summary as
// Qwen wraps it and the model's acknowledgement, then what the model had
// gathered by then from the marker's Keep on, the way Session.Context
// applies a compaction. The acknowledgement is left out when a kept
// answer follows the summary, so the roles still alternate. A marker's
// row is linked to the row before it, as Qwen records one, which the id
// assignment does not do for a row that is not a message.
func compactions(s *transcript.Session) {
	type placed struct {
		at int
		e  transcript.Entry
	}
	var ctx []placed
	var markers []int
	first := map[string]int{}
	for i := range s.Entries {
		e := &s.Entries[i]
		c := e.Compaction
		if c == nil {
			if _, seen := first[e.ID]; !seen && e.ID != "" {
				first[e.ID] = i
			}
			if e.Role != transcript.RoleOpaque && e.Audience.Model() {
				ctx = append(ctx, placed{i, *e})
			}
			continue
		}
		keep := len(s.Entries)
		if at, ok := first[c.Keep]; ok && c.Keep != "" {
			keep = at
		}
		next := make([]placed, 0, len(c.Summary)+len(ctx))
		for _, m := range c.Summary {
			next = append(next, placed{i, m})
		}
		var kept []transcript.Entry
		for _, k := range ctx {
			if k.at >= keep {
				next = append(next, k)
				kept = append(kept, k.e)
			}
		}
		ctx = next
		if e.Raw != nil {
			continue
		}
		var text []string
		for _, m := range c.Summary {
			if t := m.Text(); t != "" {
				text = append(text, t)
			}
		}
		summary := strings.Join(text, "\n\n")
		if !strings.HasSuffix(summary, resumeTrailer) {
			summary += "\n\n" + resumeTrailer
		}
		history := []transcript.Entry{{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: summary}}}}
		if len(kept) == 0 || kept[0].Role != transcript.RoleAssistant {
			history = append(history, transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: acknowledgement}}})
		}
		e.Compaction = &transcript.Compaction{Summary: append(history, kept...)}
		if !transcript.IsUUID(e.ID) {
			e.ID = transcript.NewUUID()
		}
		markers = append(markers, i)
	}
	if len(markers) == 0 {
		return
	}
	transcript.AssignIDs(s, transcript.UUIDs, true)
	for _, i := range markers {
		if i > 0 {
			s.Entries[i].ParentID = s.Entries[i-1].ID
		}
	}
}

// localCommand reports whether a slash_command payload is the local result
// of the command typed in the user row before it: phase result, not sent to
// the model, only assistant output, and a user row holding nothing but the
// command's text.
func localCommand(payload, prev json.RawMessage) bool {
	var p slashCommandPayload
	if json.Unmarshal(payload, &p) != nil || p.Phase != "result" || (p.SentToModel != nil && *p.SentToModel) || len(p.OutputHistoryItems) == 0 {
		return false
	}
	for _, it := range p.OutputHistoryItems {
		if it.Type != "assistant" {
			return false
		}
	}
	var u struct {
		Type    string  `json:"type"`
		Subtype *string `json:"subtype"`
		Message *struct {
			Role  string                       `json:"role"`
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"message"`
	}
	if json.Unmarshal(prev, &u) != nil || u.Type != "user" || u.Subtype != nil || u.Message == nil || u.Message.Role != "user" || len(u.Message.Parts) != 1 || len(u.Message.Parts[0]) != 1 {
		return false
	}
	var text string
	return json.Unmarshal(u.Message.Parts[0]["text"], &text) == nil && text == p.RawCommand
}

// responseText reads a functionResponse.response, whose output field holds
// the tool's text for Qwen's own tools.
func responseText(raw json.RawMessage) string {
	var out struct {
		Output *string `json:"output"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Output != nil {
		return *out.Output
	}
	return string(raw)
}

// compressionRow is a chat_compression row's payload as
// recordChatCompression writes it (services/chatRecordingService.ts).
type compressionRow struct {
	Info              compressionInfo `json:"info"`
	CompressedHistory []message       `json:"compressedHistory"`
}

type compressionInfo struct {
	OriginalTokenCount int    `json:"originalTokenCount"`
	NewTokenCount      int    `json:"newTokenCount"`
	CompressionStatus  int    `json:"compressionStatus"`
	TriggerReason      string `json:"triggerReason"`
}

// compressed is Qwen's COMPRESSED CompressionStatus.
const compressed = 1

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	v, _ := s.Vendor.(*Vendor)
	if v == nil {
		v = &Vendor{}
	}
	version := v.Version
	if version == "" {
		version = Version
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	r := row{
		UUID:      e.ID,
		SessionID: s.ID,
		Timestamp: t.UTC().Format("2006-01-02T15:04:05.000Z"),
		CWD:       s.CWD,
		Version:   version,
		GitBranch: v.GitBranch,
		Message:   &message{},
	}
	if e.ParentID != "" {
		r.ParentUUID = &e.ParentID
	}
	if e.Role == transcript.RoleOpaque && e.Compaction != nil {
		// prepare laid out the history the model is given from here on.
		p := compressionRow{Info: compressionInfo{CompressionStatus: compressed, TriggerReason: "manual"}, CompressedHistory: []message{}}
		for _, m := range e.Compaction.Summary {
			if msg, _, err := qwenMessage(m); err != nil {
				return nil, err
			} else if len(msg.Parts) > 0 {
				p.CompressedHistory = append(p.CompressedHistory, msg)
			}
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		r.Type, r.Subtype, r.Provenance, r.Message, r.SystemPayload = "system", subtypeCompression, "system", nil, payload
		return json.Marshal(r)
	}
	switch e.Role {
	case transcript.RoleUser, transcript.RoleSystem:
		r.Type, r.Provenance = "user", "real_user"
	case transcript.RoleAssistant:
		r.Type, r.Provenance, r.Model = "assistant", "assistant_output", s.Model
	case transcript.RoleTool:
		r.Type, r.Provenance = "tool_result", "tool_result"
	default:
		return nil, nil
	}
	if !e.Audience.Model() {
		// Shown and never sent: a realtime_message row, which Qwen's replay
		// shows and its model history skips (session-api-history.ts). Such a
		// row holds a user's or an assistant's text and nothing else.
		if e.Role != transcript.RoleUser && e.Role != transcript.RoleAssistant || e.Text() == "" {
			return nil, nil
		}
		e.Content = []transcript.Block{{Kind: transcript.BlockText, Text: e.Text()}}
		r.Subtype = subtypeRealtime
	}
	msg, result, err := qwenMessage(e)
	if err != nil || len(msg.Parts) == 0 {
		return nil, err
	}
	r.Message, r.ToolCallResult = &msg, result
	return json.Marshal(r)
}

// qwenMessage is an entry as a Gemini-shaped message, with the result of
// the last tool call it answers.
func qwenMessage(e transcript.Entry) (message, *toolCallResult, error) {
	m := message{Role: "user"}
	if e.Role == transcript.RoleAssistant {
		m.Role = "model"
	}
	var result *toolCallResult
	responses := map[string]int{} // tool call id -> index of its functionResponse part
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockImage, transcript.BlockFile:
			p, ok := mediaPart(b)
			if !ok {
				continue
			}
			// What a tool returned goes in its response, as Qwen nests it.
			if i, found := responses[b.ToolID]; found && b.ToolID != "" {
				fr := m.Parts[i].FunctionResponse
				fr.Parts = append(fr.Parts, p)
				continue
			}
			m.Parts = append(m.Parts, p)
		case transcript.BlockText:
			m.Parts = append(m.Parts, part{Text: b.Text})
		case transcript.BlockReasoning:
			m.Parts = append(m.Parts, part{Text: b.Text, Thought: true})
		case transcript.BlockToolUse:
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			m.Parts = append(m.Parts, part{FunctionCall: &functionCall{ID: b.ToolID, Name: b.Name, Args: args}})
		case transcript.BlockToolResult:
			resp, err := json.Marshal(struct {
				Output string `json:"output"`
			}{b.Text})
			if err != nil {
				return message{}, nil, err
			}
			responses[b.ToolID] = len(m.Parts)
			m.Parts = append(m.Parts, part{FunctionResponse: &functionResponse{ID: b.ToolID, Name: b.Name, Response: resp}})
			status := "success"
			if b.Status == transcript.StatusError {
				status = "error"
			}
			result = &toolCallResult{CallID: b.ToolID, Status: status}
		}
	}
	return m, result, nil
}

// ProjectDir is the directory Qwen keeps a working directory's sessions in,
// under ~/.qwen/projects.
func ProjectDir(cwd string) string { return transcript.SanitizedCwd.Name(cwd) }

// peek names the session's working directory, which the directory name
// holds only in a form that cannot be reversed, from the cwd its rows carry.
func peek(path string) (transcript.Info, error) {
	return transcript.Info{CWD: transcript.PeekField(path, "cwd", 64)}, nil
}

// hasCall reports whether an entry makes a tool call.
func hasCall(e transcript.Entry) bool {
	for _, b := range e.Content {
		if b.Kind == transcript.BlockToolUse {
			return true
		}
	}
	return false
}
