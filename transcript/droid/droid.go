// Package droid reads and writes Factory droid sessions.
//
// droid keeps one JSONL per session under
// ~/.factory/sessions/<mangled cwd>/<id>.jsonl. The first row is
// session_start; message rows link to their parent through id and parentId
// and carry an Anthropic-shaped message. Hook records are message rows with
// no content and a visibility of user_only; they keep their place in the
// chain and stay opaque.
//
// droid loads a session in file order, not by its links: the messages
// after the latest compaction_state row's anchor, behind a summary it
// builds from that row (the droid 0.226 bundle, the session loader and
// compaction's summary builder). A message's visibility says who it is for:
// user_only rows are never sent, llm_only rows (the per-turn
// "context-<turn>" reminders) are never shown. /compress in the TUI
// compacts into a new session whose session_start names the old one as
// parent and whose first row is the compaction_state; the old session is
// left as it was. Over ACP (droid exec) /compress is an ordinary prompt.
package droid

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the droid session store.
var Codec = transcript.JSONL{
	Agent: "droid",
	Layout: transcript.Layout{
		Root:    ".factory/sessions",
		Project: transcript.MangledCwd,
		Ext:     ".jsonl",
		// The session_start header names the working directory, which the
		// mangled directory name cannot give back.
		Peek: func(path string) (transcript.Info, error) {
			return transcript.Info{CWD: transcript.PeekField(path, "cwd", 1)}, nil
		},
	},
	Header:      header,
	Decode:      decode,
	Finish:      finish,
	Encode:      encode,
	WriteHeader: writeHeader,
	Prepare:     joinResults,
	Tree:        true,
	// droid keeps the messages a compaction retired in the file, behind a
	// compaction_state row anchored after them, keeps a message the person
	// sees and the model never gets as user_only, and sends a text file as a
	// text document.
	Caps: transcript.Capabilities{Compaction: true, UserOnly: true, Files: true},
}

// joinResults puts the results of one turn's tool calls back in one
// message. droid answers a message's calls from the single message after
// it, as the Anthropic API does, and reports any call it finds no result
// for there as cancelled; Portable gives each result an entry of its own.
// Only entries this write creates are joined.
func joinResults(home string, s *transcript.Session) error {
	var out []transcript.Entry
	for _, e := range s.Entries {
		if n := len(out); n > 0 && e.Raw == nil && e.Role == transcript.RoleTool && e.Audience == out[n-1].Audience &&
			out[n-1].Raw == nil && out[n-1].Role == transcript.RoleTool {
			prev := &out[n-1]
			prev.Content = append(append([]transcript.Block(nil), prev.Content...), e.Content...)
			continue
		}
		out = append(out, e)
	}
	s.Entries = out
	return nil
}

// Vendor is what a droid session carries in Session.Vendor: the
// session_start row's fields, so a write reproduces the host id and title
// state this codec does not model.
type Vendor struct {
	Header map[string]json.RawMessage
}

type sessionStart struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Owner   string `json:"owner"`
	Version int    `json:"version"`
	CWD     string `json:"cwd"`
	HostID  string `json:"hostId"`
}

type row struct {
	Type      string   `json:"type"`
	ID        string   `json:"id"`
	ParentID  string   `json:"parentId,omitempty"`
	Timestamp string   `json:"timestamp"`
	Message   *message `json:"message,omitempty"`
}

// compactionState is the row a compaction writes (saveCompactionSummary).
type compactionState struct {
	SummaryText string `json:"summaryText"`
	SummaryKind string `json:"summaryKind"`
	// SystemInfoText is the environment reminder droid sends after the
	// summary, since the turns that carried it are gone.
	SystemInfoText string `json:"systemInfoText"`
	// AnchorMessage is the last message the summary replaces. A compaction
	// into a new session has none, and keeps every message in the file.
	AnchorMessage *struct {
		ID string `json:"id"`
	} `json:"anchorMessage"`
}

// Who a message is for. An empty visibility is both.
const (
	visibilityUser = "user_only"
	visibilityLLM  = "llm_only"
)

type message struct {
	Role       string  `json:"role"`
	Content    []block `json:"content"`
	Visibility string  `json:"visibility,omitempty"`
	// HiddenFromUserViews hides a message the model is still given.
	HiddenFromUserViews bool `json:"hiddenFromUserViews,omitempty"`
	// ChatCompletionReasoningContent is the reasoning a chat-completions
	// model streamed for an assistant message, and
	// ChatCompletionReasoningField the request field droid sends it back in
	// (reasoning_content or reasoning).
	ChatCompletionReasoningField   string `json:"chatCompletionReasoningField,omitempty"`
	ChatCompletionReasoningContent string `json:"chatCompletionReasoningContent,omitempty"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Source    *mediaSource    `json:"source,omitempty"`
	// Thinking is a thinking block's text. Its signature and the provider
	// that signed it are left in the row.
	Thinking string `json:"thinking,omitempty"`
}

// mediaSource is an image's or a document's source as droid stores it (the
// 0.226 bundle's message serializer): an image's bytes in base64; a PDF's
// bytes in base64, which droid leaves empty on disk and keeps the file's
// path beside; or a text document's text.
type mediaSource struct {
	Type      string `json:"type"`
	Data      string `json:"data"`
	MediaType string `json:"media_type,omitempty"`
	Name      string `json:"name,omitempty"`
	Path      string `json:"path,omitempty"`
	Mime      string `json:"mime,omitempty"`
}

// The kinds of source a block carries.
const (
	sourceBase64 = "base64"
	sourceText   = "text"
)

const mediaPDF = "application/pdf"

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	var h sessionStart
	if err := json.Unmarshal(raw, &h); err != nil {
		return false, err
	}
	if h.Type != "session_start" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, err
	}
	s.ID = h.ID
	s.CWD = h.CWD
	s.Title = h.Title
	s.Vendor = &Vendor{Header: fields}
	return true, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if (r.Type != "message" && r.Type != "compaction_state") || r.ID == "" {
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.ID, ParentID: r.ParentID, Role: transcript.RoleOpaque}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.ID, err)
		}
		e.Time = t
	}
	if r.Type == "compaction_state" {
		var c compactionState
		if err := json.Unmarshal(raw, &c); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: %w", r.ID, err)
		}
		e.Compaction = &transcript.Compaction{Summary: []transcript.Entry{summary(r.ID, e.Time, c)}}
		return e, true, nil
	}
	if r.Message == nil || len(r.Message.Content) == 0 && r.Message.ChatCompletionReasoningContent == "" {
		return e, true, nil
	}
	e.Role = transcript.Role(r.Message.Role)
	switch {
	case r.Message.Visibility == visibilityUser:
		e.Audience = transcript.AudienceUser
	case r.Message.Visibility == visibilityLLM || r.Message.HiddenFromUserViews:
		e.Audience = transcript.AudienceModel
	}
	// A model's reasoning is its thinking blocks, which droid keeps in the
	// content of every provider's answer (the 0.226 bundle's content-block
	// serializer), signed by the provider that wrote them. On resume droid
	// sends a block the route accepts the signature of as a thinking block,
	// and turns any other into <thinking> text (its history sanitizer), so
	// the model gets the text on every route but Gemini, which drops it. A
	// chat-completions answer also keeps its reasoning as
	// chatCompletionReasoningContent, which is what droid sends back on that
	// route; its thinking blocks are the same text, so they are not read
	// twice. A redacted_thinking block has no text to read.
	cc := r.Message.ChatCompletionReasoningContent
	if cc != "" {
		e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: cc})
	}
	toolResults := 0
	for _, b := range r.Message.Content {
		switch b.Type {
		case "thinking":
			if cc == "" && strings.TrimSpace(b.Thinking) != "" {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: b.Thinking})
			}
		case "text":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: b.Text})
		case "tool_use":
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: b.ID, Name: b.Name, Input: b.Input})
		case "tool_result":
			toolResults++
			text, media, err := result(b.Content, b.ToolUseID)
			if err != nil {
				return transcript.Entry{}, false, fmt.Errorf("row %s: tool_result: %w", r.ID, err)
			}
			st := transcript.StatusOK
			if b.IsError != nil && *b.IsError {
				st = transcript.StatusError
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: b.ToolUseID, Text: text, Status: st})
			e.Content = append(e.Content, media...)
		case "image", "document":
			if m, ok := decodeMedia(b, ""); ok {
				e.Content = append(e.Content, m)
			}
		}
	}
	if e.Role == transcript.RoleUser && toolResults == len(r.Message.Content) {
		e.Role = transcript.RoleTool
	}
	if e.Role == transcript.RoleUser && e.Audience == transcript.AudienceAll {
		e.Content, e.ModelContent = ownContent(e.Content)
	}
	return e, true, nil
}

// ownContent splits a prompt into what the person wrote and what the model
// was given. droid adds its own context to a prompt as text blocks wrapped
// in <system-reminder>, such as the local paths of attached images, and
// strips them from every view of the prompt it shows (the bundle's
// /<system-reminder>[\s\S]*?<\/system-reminder>/g replacements).
// ModelContent is nil when the two are the same.
func ownContent(content []transcript.Block) (own, model []transcript.Block) {
	for _, b := range content {
		if b.Kind == transcript.BlockText && isReminder(b.Text) {
			continue
		}
		own = append(own, b)
	}
	if len(own) == len(content) || len(own) == 0 {
		return content, nil
	}
	return own, content
}

func isReminder(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "<system-reminder>") && strings.HasSuffix(text, "</system-reminder>")
}

// decodeMedia reads an image or document block. toolID is set for one a
// tool returned.
func decodeMedia(b block, toolID string) (transcript.Block, bool) {
	src := b.Source
	if src == nil {
		return transcript.Block{}, false
	}
	out := transcript.Block{Kind: transcript.BlockImage, ToolID: toolID, MediaType: src.MediaType}
	if b.Type == "document" {
		out.Kind, out.Name, out.URI = transcript.BlockFile, src.Name, src.Path
	}
	switch src.Type {
	case sourceBase64:
		if src.Data != "" {
			m, ok := transcript.MediaBlock(out.Kind, out.MediaType, src.Data, toolID)
			if !ok {
				return transcript.Block{}, false
			}
			out.Data = m.Data
		}
	case sourceText:
		if b.Type != "document" {
			return transcript.Block{}, false
		}
		out.Data = []byte(src.Data)
		if src.Mime != "" {
			out.MediaType = src.Mime
		}
	default:
		return transcript.Block{}, false
	}
	if out.Data == nil && out.URI == "" {
		return transcript.Block{}, false
	}
	return out, true
}

// encodeMedia is the block droid stores an image or file as. droid keeps an
// image only as base64 bytes, and a document only as a PDF or as text, so
// an image or file given by reference alone, or a file of another type, has
// no block and is dropped.
func encodeMedia(b transcript.Block) (block, bool) {
	switch {
	case b.Kind == transcript.BlockImage && len(b.Data) > 0:
		return block{Type: "image", Source: &mediaSource{Type: sourceBase64, Data: base64.StdEncoding.EncodeToString(b.Data), MediaType: b.MediaType}}, true
	case b.Kind == transcript.BlockFile && b.MediaType == mediaPDF && len(b.Data) > 0:
		return block{Type: "document", Source: &mediaSource{Type: sourceBase64, Data: base64.StdEncoding.EncodeToString(b.Data), MediaType: mediaPDF, Name: b.Name, Path: b.URI}}, true
	case b.Kind == transcript.BlockFile && strings.HasPrefix(b.MediaType, "text/") && b.Data != nil:
		return block{Type: "document", Source: &mediaSource{Type: sourceText, Data: string(b.Data), MediaType: "text/plain", Name: b.Name, Mime: b.MediaType}}, true
	}
	return block{}, false
}

// summaryPreamble wraps an LLM summary the way droid hands it to the model,
// as captured from its request after resuming a /compress'd session.
const summaryPreamble = "A previous instance of Droid has summarized the conversation thus far as follows:\n\n<summary>\n%s\n</summary>\n\nIMPORTANT: This summary was created by a previous instance of Droid. Files referenced in the summary may not be available until you explicitly view them again."

// summary is the user message droid builds from a compaction_state row: the
// summary, then the environment reminder when the row carries one. Other
// summary kinds (a transcript serialized for a provider switch) are sent
// without the preamble.
func summary(id string, at time.Time, c compactionState) transcript.Entry {
	text := c.SummaryText
	if c.SummaryKind == "" || c.SummaryKind == "llm_summary" {
		text = fmt.Sprintf(summaryPreamble, c.SummaryText)
	}
	// The summary is conversation, the one record of what it retired, so
	// it moves with the session. The preamble and the environment reminder
	// droid sends in the same message are droid's own: the model gets
	// them, and Portable leaves them behind with ModelContent.
	e := transcript.Entry{ID: id, Role: transcript.RoleUser, Time: at,
		Content: []transcript.Block{{Kind: transcript.BlockText, Text: c.SummaryText}}}
	sent := []transcript.Block{{Kind: transcript.BlockText, Text: text}}
	if c.SystemInfoText != "" {
		sent = append(sent, transcript.Block{Kind: transcript.BlockText, Text: c.SystemInfoText})
	}
	if len(sent) > 1 || text != c.SummaryText {
		e.ModelContent = sent
	}
	return e
}

// finish lays the session out the way droid loads it. droid reads rows in
// file order, while their links leave gaps: a turn's context row and the
// hook rows at the start of a run name no parent, and nothing links to a
// compaction_state. So a row without a parent follows the row before it, a
// compaction follows the last message before it, and the row after a
// compaction follows the compaction. Each compaction then keeps the
// messages after its anchor, less the tool results right after it, whose
// call the summary replaced.
func finish(s *transcript.Session) error {
	prev, prevMessage, after := "", "", ""
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.ID == "" {
			continue
		}
		switch {
		case e.Compaction != nil:
			e.ParentID = prevMessage
			after = e.ID
		case after != "":
			e.ParentID = after
			after = ""
		case e.ParentID == "":
			e.ParentID = prev
		}
		prev = e.ID
		if e.Role != transcript.RoleOpaque {
			prevMessage = e.ID
		}
	}
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Compaction == nil {
			continue
		}
		var c compactionState
		if err := json.Unmarshal(e.Raw, &c); err != nil {
			return fmt.Errorf("row %s: %w", e.ID, err)
		}
		from := 0
		if c.AnchorMessage != nil && c.AnchorMessage.ID != "" {
			from = len(s.Entries)
			for j := 0; j < i; j++ {
				if s.Entries[j].ID == c.AnchorMessage.ID {
					from = j + 1
					break
				}
			}
		}
		j := from
		for j < i && s.Entries[j].Role == transcript.RoleTool {
			j++
		}
		// Rows that are not messages give the model nothing, so the kept
		// history starts at the next message.
		for j < i && s.Entries[j].Role == transcript.RoleOpaque {
			j++
		}
		if j < i {
			e.Compaction.Keep = s.Entries[j].ID
		}
	}
	gatherReminders(s)
	return nil
}

// contextRowPrefix starts the id of the llm_only row droid writes with a
// turn's per-turn reminders (Gw in the 0.226 bundle).
const contextRowPrefix = "context-"

// gatherReminders places the per-turn reminders the way droid sends them
// after a compaction. With a summary first, droid pulls every per-turn
// reminder out of the history that follows it and sends them as one message
// right after the summary (the bundle's message builder: Di, called from en
// with the summary Wb attaches). The reminders are the blocks of the
// "context-" rows; the date reminder in the same row is not one of them and
// stays in place. The gathered message joins the latest compaction's
// Summary, and a row left with nothing to send is given to no one.
func gatherReminders(s *transcript.Session) {
	branch := s.Branch()
	last := -1
	for k, i := range branch {
		if s.Entries[i].Compaction != nil {
			last = k
		}
	}
	if last < 0 {
		return
	}
	c := s.Entries[branch[last]].Compaction
	from := last
	for k := 0; k < last; k++ {
		if c.Keep != "" && s.Entries[branch[k]].ID == c.Keep {
			from = k
			break
		}
	}
	var gathered []transcript.Block
	seen := map[string]bool{}
	for _, i := range branch[from:] {
		e := &s.Entries[i]
		if e.Compaction != nil || e.Audience != transcript.AudienceModel || !strings.HasPrefix(e.ID, contextRowPrefix) {
			continue
		}
		var kept []transcript.Block
		for _, b := range e.Content {
			if b.Kind != transcript.BlockText || isDateReminder(b.Text) {
				kept = append(kept, b)
				continue
			}
			if !seen[b.Text] {
				seen[b.Text] = true
				gathered = append(gathered, b)
			}
		}
		switch {
		case len(kept) == 0:
			e.Audience = transcript.AudienceNone
		case len(kept) < len(e.Content):
			e.ModelContent = kept
		}
	}
	if gathered != nil {
		c.Summary = append(c.Summary, transcript.Entry{Role: transcript.RoleUser, Audience: transcript.AudienceModel, Content: gathered})
	}
}

// isDateReminder is the bundle's test for the date reminder (PQ), which
// droid keeps where it is.
func isDateReminder(text string) bool {
	return strings.HasPrefix(text, "<system-reminder>Current date: ") || strings.Contains(text, "IMPORTANT - Current date for web search relevance:")
}

// result reads a tool_result's content: a string, or blocks whose text is
// the result's and whose images and documents the tool returned.
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
	var b strings.Builder
	var media []transcript.Block
	for _, p := range bs {
		switch p.Type {
		case "image", "document":
			if m, ok := decodeMedia(p, toolID); ok {
				media = append(media, m)
			}
		default:
			b.WriteString(p.Text)
		}
	}
	return b.String(), media, nil
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Header {
			fields[k] = val
		}
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	hostID := ""
	if raw, ok := fields["hostId"]; ok {
		if err := json.Unmarshal(raw, &hostID); err != nil {
			return nil, fmt.Errorf("hostId: %w", err)
		}
	}
	if hostID == "" {
		hostID = transcript.NewUUID()
	}
	identity, err := json.Marshal(sessionStart{Type: "session_start", ID: s.ID, Title: title, Owner: "unknown", Version: 2, CWD: s.CWD, HostID: hostID})
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

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	if e.Compaction != nil {
		return encodeCompaction(e, s)
	}
	role := e.Role
	if role == transcript.RoleTool {
		role = transcript.RoleUser
	}
	if role != transcript.RoleUser && role != transcript.RoleAssistant {
		return nil, nil
	}
	content := make([]block, 0, len(e.Content))
	// Reasoning goes where droid keeps what a chat-completions model
	// streamed, which it sends back in that request field when the model
	// takes reasoning: a custom model with enableThinking, or one its
	// registry marks acceptsReasoning (the 0.226 bundle's chat-completions
	// message builder). droid's thinking blocks are left alone: Anthropic
	// replays one only with the model's signature, and the Responses API
	// replays reasoning only as the model's encrypted_content, neither of
	// which another agent's reasoning has.
	var reasoning string
	for _, b := range e.Content {
		if b.Kind == transcript.BlockReasoning && role == transcript.RoleAssistant {
			reasoning += b.Text
		}
	}
	// A tool's images and documents go inside its tool_result, as droid
	// stores them, after the result's text.
	returned := map[string][]block{}
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockImage, transcript.BlockFile:
			if m, ok := encodeMedia(b); ok && b.ToolID != "" {
				returned[b.ToolID] = append(returned[b.ToolID], m)
			}
		}
	}
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			content = append(content, block{Type: "text", Text: b.Text})
		case transcript.BlockToolUse:
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			content = append(content, block{Type: "tool_use", ID: b.ToolID, Name: b.Name, Input: in})
		case transcript.BlockToolResult:
			var out any = b.Text
			if media := returned[b.ToolID]; media != nil {
				var parts []block
				if b.Text != "" {
					parts = append(parts, block{Type: "text", Text: b.Text})
				}
				out = append(parts, media...)
				delete(returned, b.ToolID)
			}
			text, err := json.Marshal(out)
			if err != nil {
				return nil, err
			}
			isErr := b.Status == transcript.StatusError
			content = append(content, block{Type: "tool_result", ToolUseID: b.ToolID, IsError: &isErr, Content: text})
		case transcript.BlockImage, transcript.BlockFile:
			if b.ToolID != "" {
				continue
			}
			if m, ok := encodeMedia(b); ok {
				content = append(content, m)
			}
		}
	}
	if len(content) == 0 && reasoning == "" {
		return nil, nil
	}
	m := &message{Role: string(role), Content: content}
	if e.Audience == transcript.AudienceUser {
		m.Visibility = visibilityUser
	}
	if reasoning != "" {
		m.ChatCompletionReasoningField, m.ChatCompletionReasoningContent = "reasoning_content", reasoning
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	return json.Marshal(row{
		Type:      "message",
		ID:        e.ID,
		ParentID:  messageParent(e.ParentID, s),
		Timestamp: t.UTC().Format(timestamp),
		Message:   m,
	})
}

const timestamp = "2006-01-02T15:04:05.000Z"

// messageParent is the parent a written message names. Nothing links to a
// compaction_state row, so a message after one names the message before it.
func messageParent(id string, s *transcript.Session) string {
	for i, e := range s.Entries {
		if e.ID != id || e.Compaction == nil {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if s.Entries[j].Role != transcript.RoleOpaque {
				return s.Entries[j].ID
			}
		}
		return ""
	}
	return id
}

// compactionRow is the compaction_state row droid writes when it compacts a
// session in place (saveCompactionSummary in the 0.226 bundle).
type compactionRow struct {
	Type          string         `json:"type"`
	ID            string         `json:"id"`
	Timestamp     string         `json:"timestamp"`
	SummaryText   string         `json:"summaryText"`
	SummaryTokens int            `json:"summaryTokens"`
	SummaryKind   string         `json:"summaryKind"`
	AnchorMessage *anchorMessage `json:"anchorMessage,omitempty"`
	RemovedCount  int            `json:"removedCount"`
}

// anchorMessage is the last message a summary replaces: its id, and its
// index among the session's message rows, which droid falls back on when
// the id is not in the loaded history.
type anchorMessage struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
}

// encodeCompaction records a compaction as droid does one in place: the
// summary as an llm_summary, which droid wraps in its preamble when it loads
// it, anchored at the last message before the first one kept. droid gives
// the model the summary and every message after the anchor, less tool
// results whose call it replaced, and keeps the retired messages in the
// file.
func encodeCompaction(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	text := e.Compaction.Text(nil)
	end := -1
	for i := range s.Entries {
		if s.Entries[i].Compaction == e.Compaction {
			end = i
			break
		}
	}
	if keep := e.Compaction.Keep; keep != "" {
		for i := 0; i < end; i++ {
			if s.Entries[i].ID == keep {
				end = i
				break
			}
		}
	}
	var anchor *anchorMessage
	written := 0
	for _, m := range s.Entries[:max(end, 0)] {
		if m.Role == transcript.RoleOpaque {
			continue
		}
		row, err := encode(m, s)
		if err != nil {
			return nil, err
		}
		if len(row) > 0 {
			anchor = &anchorMessage{ID: m.ID, Index: written}
			written++
		}
	}
	id := e.ID
	if !transcript.IsUUID(id) {
		id = transcript.NewUUID()
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	c := compactionRow{Type: "compaction_state", ID: id, Timestamp: t.UTC().Format(timestamp), SummaryText: text,
		// droid's own estimate: a token per four characters.
		SummaryTokens: (len(text) + 3) / 4, SummaryKind: "llm_summary", AnchorMessage: anchor}
	if anchor != nil {
		c.RemovedCount = anchor.Index + 1
	}
	return json.Marshal(c)
}
