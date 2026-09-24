package grok

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// updates.jsonl is grok's append-only log of the ACP session/update
// notifications it sent, beside its own _x.ai/session/update ones (hooks,
// turn ends, compaction checkpoints, rewind markers). It is what the person
// sees: session/load replays it to the client (storage/replay.rs), and grok
// takes the prompt list, and with it the next prompt index and the rewind
// points, from its user_message_chunk rows (load_user_prompts_from_updates
// in acp_session_impl/rewind.rs).

const (
	acpMethod = "session/update"
	xaiMethod = "_x.ai/session/update"
)

// updateLine is a row of updates.jsonl. Rows written before the envelope
// existed are the params object alone, with no method.
type updateLine struct {
	Timestamp int64           `json:"timestamp"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
}

type updateParams struct {
	SessionID string       `json:"sessionId"`
	Update    update       `json:"update"`
	Meta      *updateTrace `json:"_meta,omitempty"`
}

// updateTrace is the envelope's _meta: the event id a reconnecting client
// resumes from, and when the agent sent the update.
type updateTrace struct {
	EventID          string `json:"eventId,omitempty"`
	AgentTimestampMs int64  `json:"agentTimestampMs,omitempty"`
}

// update is the fields of an ACP or xAI session update this codec reads and
// writes.
type update struct {
	SessionUpdate string `json:"sessionUpdate"`
	// Content is a chunk's content block, or a tool call update's list of
	// tool call contents.
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      string          `json:"title,omitempty"`
	Status     string          `json:"status,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage `json:"rawOutput,omitempty"`
	Meta       *updateMeta     `json:"_meta,omitempty"`
	// TargetPromptIndex is set on a rewind marker.
	TargetPromptIndex *int `json:"target_prompt_index,omitempty"`
}

type updateMeta struct {
	PromptIndex *int `json:"promptIndex,omitempty"`
	// HostTurn marks the echo of a command the host ran, such as /compact,
	// which is not a prompt.
	HostTurn bool `json:"hostTurn,omitempty"`
	Tool     *struct {
		Name string `json:"name"`
	} `json:"x.ai/tool,omitempty"`
}

// contentBlock is an ACP content block, as a chunk carries it: text, an
// image with its bytes in base64, or a resource link or embedded resource a
// prompt carried.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Meta *struct {
		BashCommand json.RawMessage `json:"bashCommand"`
	} `json:"_meta,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	// Resource is an embedded resource's contents: text or a base64 blob.
	Resource *struct {
		URI      string  `json:"uri"`
		MimeType string  `json:"mimeType"`
		Text     *string `json:"text"`
		Blob     string  `json:"blob"`
	} `json:"resource,omitempty"`
}

// block is the block for a content block's attachment, one that is not
// text: an image, a linked or embedded file. toolID is set for one a tool
// returned.
func (c contentBlock) block(toolID string) (transcript.Block, bool) {
	kind := transcript.BlockFile
	if strings.HasPrefix(c.MimeType, "image/") {
		kind = transcript.BlockImage
	}
	switch c.Type {
	case "image":
		b := transcript.Block{Kind: transcript.BlockImage, ToolID: toolID, MediaType: c.MimeType, URI: c.URI}
		if c.Data != "" {
			data, err := base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				return transcript.Block{}, false
			}
			b.Data = data
		}
		return b, b.Data != nil || b.URI != ""
	case "resource_link":
		return transcript.Block{Kind: kind, ToolID: toolID, MediaType: c.MimeType, URI: c.URI, Name: c.Name}, c.URI != ""
	case "resource":
		r := c.Resource
		if r == nil {
			return transcript.Block{}, false
		}
		b := transcript.Block{Kind: transcript.BlockFile, ToolID: toolID, MediaType: r.MimeType, URI: r.URI}
		if strings.HasPrefix(r.MimeType, "image/") {
			b.Kind = transcript.BlockImage
		}
		switch {
		case r.Text != nil:
			b.Data = []byte(*r.Text)
		case r.Blob != "":
			data, err := base64.StdEncoding.DecodeString(r.Blob)
			if err != nil {
				return transcript.Block{}, false
			}
			b.Data = data
		}
		return b, true
	}
	return transcript.Block{}, false
}

// toolImage is an image entry of a tool call update's content.
type toolImage struct {
	Type    string   `json:"type"`
	Content acpImage `json:"content"`
}

// toolContent is one entry of a tool call update's content.
type toolContent struct {
	Type    string       `json:"type"`
	Content contentBlock `json:"content"`
}

// parsedUpdate is a row of updates.jsonl as far as the codec reads it.
type parsedUpdate struct {
	xai    bool
	update update
	trace  updateTrace
	time   time.Time
}

func parseUpdate(raw json.RawMessage) (parsedUpdate, bool) {
	var line updateLine
	if json.Unmarshal(raw, &line) != nil {
		return parsedUpdate{}, false
	}
	params := line.Params
	if line.Method == "" {
		params = raw
	}
	var p updateParams
	if json.Unmarshal(params, &p) != nil || p.Update.SessionUpdate == "" {
		return parsedUpdate{}, false
	}
	out := parsedUpdate{xai: line.Method == xaiMethod, update: p.Update}
	if p.Meta != nil {
		out.trace = *p.Meta
	}
	switch {
	case out.trace.AgentTimestampMs > 0:
		out.time = time.UnixMilli(out.trace.AgentTimestampMs).UTC()
	case line.Timestamp > 0:
		out.time = time.Unix(line.Timestamp, 0).UTC()
	}
	return out, true
}

// isUpdateRow reports whether a valid row is an updates.jsonl row rather
// than a chat_history one. Update rows are enveloped notifications, or bare
// params in the oldest sessions; chat rows are conversation items.
func isUpdateRow(raw json.RawMessage) bool {
	var probe struct {
		Method *string          `json:"method"`
		Update *json.RawMessage `json:"update"`
	}
	return json.Unmarshal(raw, &probe) == nil && (probe.Method != nil || probe.Update != nil)
}

// decodeUpdate reads an updates.jsonl row. The ACP rows that carry the
// conversation become entries for the person; every other row, grok's own
// notifications included, is opaque. A message split across chunk rows is
// joined in finishUpdates.
func decodeUpdate(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	p, ok := parseUpdate(raw)
	if !ok || p.xai {
		return transcript.Entry{}, false, nil
	}
	u := p.update
	e := transcript.Entry{Time: p.time, Audience: transcript.AudienceUser}
	switch u.SessionUpdate {
	case "user_message_chunk":
		// An image the person attached is a chunk of its own, which
		// finishUpdates joins onto the prompt's text.
		e.Role = transcript.RoleUser
		e.Content = chunkBlocks(u.Content)
	case "agent_message_chunk":
		e.Role = transcript.RoleAssistant
		e.Content = chunkBlocks(u.Content)
	case "agent_thought_chunk":
		e.Role = transcript.RoleAssistant
		e.Content = textBlocks(transcript.BlockReasoning, chunkText(u.Content))
	case "tool_call":
		name := u.Title
		if u.Meta != nil && u.Meta.Tool != nil && u.Meta.Tool.Name != "" {
			name = u.Meta.Tool.Name
		}
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: u.ToolCallID, Name: name, Input: u.RawInput}}
	case "tool_call_update":
		// Only the update that ends a call is its result; the others refine
		// its title, input or progress.
		var status transcript.Status
		switch u.Status {
		case "completed":
			status = transcript.StatusOK
		case "failed":
			status = transcript.StatusError
		default:
			return transcript.Entry{}, false, nil
		}
		e.Role = transcript.RoleTool
		text, media := toolOutput(u)
		e.Content = append([]transcript.Block{{Kind: transcript.BlockToolResult, ToolID: u.ToolCallID, Text: text, Status: status}}, media...)
	default:
		return transcript.Entry{}, false, nil
	}
	return e, true, nil
}

func chunkText(raw json.RawMessage) string {
	var c contentBlock
	if json.Unmarshal(raw, &c) != nil || c.Type != "text" {
		return ""
	}
	return c.Text
}

// chunkBlocks is a message chunk's block: its text, or what it attaches.
func chunkBlocks(raw json.RawMessage) []transcript.Block {
	var c contentBlock
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	if c.Type == "text" {
		return textBlocks(transcript.BlockText, c.Text)
	}
	if b, ok := c.block(""); ok {
		return []transcript.Block{b}
	}
	return nil
}

// toolOutput is what a finished call shows: its text content and the
// images it returned, or, for a call the server ran, which reports only raw
// output, that output.
func toolOutput(u update) (string, []transcript.Block) {
	var contents []toolContent
	_ = json.Unmarshal(u.Content, &contents)
	var b strings.Builder
	var media []transcript.Block
	for _, c := range contents {
		if c.Type != "content" {
			continue
		}
		if c.Content.Type == "text" {
			b.WriteString(c.Content.Text)
		} else if m, ok := c.Content.block(u.ToolCallID); ok {
			media = append(media, m)
		}
	}
	if b.Len() == 0 && media == nil && len(u.RawOutput) > 0 && string(u.RawOutput) != "null" {
		var text string
		if json.Unmarshal(u.RawOutput, &text) == nil {
			return text, nil
		}
		return string(u.RawOutput), nil
	}
	return b.String(), media
}

// step is what a row means to grok's prompt and rewind bookkeeping
// (storage/mod.rs): a user chunk that may open a prompt, a rewind marker, or
// anything else, which ends a run of user chunks.
type step struct {
	user bool
	// prompt is set on a user chunk the prompt list takes: text that is not
	// a direct bash command (parse_prompt_extract_event).
	prompt      bool
	text        string
	promptIndex *int
	rewind      bool
	target      int
}

func stepOf(raw json.RawMessage) step {
	p, ok := parseUpdate(raw)
	if !ok {
		return step{}
	}
	u := p.update
	if p.xai {
		if u.SessionUpdate == "rewind_marker" && u.TargetPromptIndex != nil {
			return step{rewind: true, target: *u.TargetPromptIndex}
		}
		return step{}
	}
	if u.SessionUpdate != "user_message_chunk" || (u.Meta != nil && u.Meta.HostTurn) {
		return step{}
	}
	st := step{user: true}
	if u.Meta != nil {
		st.promptIndex = u.Meta.PromptIndex
	}
	var c contentBlock
	if json.Unmarshal(u.Content, &c) == nil && c.Type == "text" && (c.Meta == nil || c.Meta.BashCommand == nil) {
		st.prompt, st.text = true, c.Text
	}
	return st
}

// runs is grok's UserRunTurnTracker: user chunks form runs, and a run counts
// as a prompt only once any chunk carries a prompt index if it carries one
// too, since mid-turn echoes leave it off.
type runs struct {
	seenMarker bool
	inUser     bool
	current    *int
}

// user takes a user chunk and reports whether it opens a new run and
// whether that run counts as a prompt.
func (r *runs) user(promptIndex *int) (open, counts bool) {
	if promptIndex != nil {
		r.seenMarker = true
	}
	counts = !r.seenMarker || promptIndex != nil
	switch {
	case !r.inUser:
		open = true
	case r.seenMarker || promptIndex != nil:
		open = !sameIndex(promptIndex, r.current)
	}
	r.inUser = true
	if open {
		r.current = promptIndex
		return true, counts
	}
	return false, false
}

func (r *runs) other() {
	r.inUser = false
	r.current = nil
}

func sameIndex(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// finishUpdates keeps, of the updates rows, the history the last
// compaction retired, which the person still sees and chat_history.jsonl no
// longer holds; every message after it is in chat_history.jsonl too, and
// its rows here are left opaque, except the echo of a command such as
// /compact, which grok shows and never sends and which has no chat row. The
// store places such an echo among the chat rows (see place). A rewind is
// applied first: a rewind to prompt N drops everything from prompt N's
// first chunk up to the marker, as filter_rewind_by does before replaying
// the file to the client, and a compaction it drops no longer counts. A
// message grok wrote as several chunk rows is joined into its first row's
// entry.
func finishUpdates(s *transcript.Session) error {
	joinChunks(s)
	retired := 0
	for i, e := range s.Entries {
		if e.Audience != transcript.AudienceNone && stepKind(e.Raw) == "compaction_checkpoint" {
			retired = i
		}
	}
	for i := retired; i < len(s.Entries); i++ {
		if e := &s.Entries[i]; e.Audience != transcript.AudienceNone && !isHostTurn(e.Raw) {
			e.Role, e.Content = transcript.RoleOpaque, nil
		}
	}
	return nil
}

// isHostTurn reports whether an update row is the echo of a command the
// host ran: a user chunk marked hostTurn, which is no prompt.
func isHostTurn(raw json.RawMessage) bool {
	p, ok := parseUpdate(raw)
	return ok && !p.xai && p.update.SessionUpdate == "user_message_chunk" && p.update.Meta != nil && p.update.Meta.HostTurn
}

// lastCheckpoint is the index of the last compaction_checkpoint row a rewind
// left standing among the updates entries, or -1.
func lastCheckpoint(shown []transcript.Entry) int {
	last := -1
	for i, e := range shown {
		if e.Audience != transcript.AudienceNone && stepKind(e.Raw) == "compaction_checkpoint" {
			last = i
		}
	}
	return last
}

// place lays out a session's entries as grok shows them: the updates rows
// the last compaction retired, then the chat rows with the updates rows
// written since merged in where they happened. An updates row goes just
// before the chat prompt of the next prompt index updates.jsonl records at
// or after it, or after every chat row when none follows; so a command's
// echo is shown ahead of the prompt that came after it. Each file's rows
// keep their order, and a torn line stays behind the row before it, so each
// file is written back as it was.
func place(shown, chat []transcript.Entry) []transcript.Entry {
	cut := lastCheckpoint(shown) + 1
	out := append([]transcript.Entry(nil), shown[:cut]...)
	tail := shown[cut:]
	// Where each chat prompt sits, by its prompt index.
	at := map[int]int{}
	for k, e := range chat {
		var r struct {
			PromptIndex *int `json:"prompt_index"`
		}
		if json.Unmarshal(e.Raw, &r) == nil && r.PromptIndex != nil {
			if _, ok := at[*r.PromptIndex]; !ok {
				at[*r.PromptIndex] = k
			}
		}
	}
	// The chat position each tail row goes before, never behind the row
	// ahead of it.
	pos := make([]int, len(tail))
	next := len(chat)
	for k := len(tail) - 1; k >= 0; k-- {
		if st := stepOf(tail[k].Raw); st.user && st.prompt && st.promptIndex != nil {
			next = len(chat)
			if c, ok := at[*st.promptIndex]; ok {
				next = c
			}
		}
		pos[k] = next
		if !json.Valid(tail[k].Raw) && k > 0 {
			pos[k] = -1 // settled with the row before it below
		}
	}
	for k := range pos {
		switch {
		case pos[k] < 0:
			pos[k] = pos[k-1]
		case k > 0 && pos[k] < pos[k-1]:
			pos[k] = pos[k-1]
		}
	}
	k := 0
	for c := 0; c <= len(chat); c++ {
		for k < len(tail) && pos[k] <= c {
			out = append(out, tail[k])
			k++
		}
		if c < len(chat) {
			out = append(out, chat[c])
		}
	}
	return out
}

// joinChunks applies rewinds and joins chunked messages; see finishUpdates.
func joinChunks(s *transcript.Session) {
	var tracker runs
	var live, starts []int // entry indexes that survive rewinds, and where prompts start among them
	userRun, agentRun := -1, -1
	for i := range s.Entries {
		e := &s.Entries[i]
		st := stepOf(e.Raw)
		switch {
		case st.rewind:
			cut := len(live)
			if st.target < len(starts) {
				cut = starts[st.target]
				starts = starts[:st.target]
			}
			for _, j := range live[cut:] {
				s.Entries[j].Audience = transcript.AudienceNone
			}
			live = live[:cut]
			tracker.other()
			userRun = -1
			continue
		case st.user:
			open, counts := tracker.user(st.promptIndex)
			if counts {
				starts = append(starts, len(live))
			}
			if !open && userRun >= 0 {
				join(s, userRun, i)
			} else {
				userRun = i
			}
		default:
			tracker.other()
			userRun = -1
		}
		live = append(live, i)

		// grok coalesces a streamed reply into one row until another ACP
		// update arrives, so consecutive chunks of one kind, with only its own
		// notifications between them, are one message.
		p, ok := parseUpdate(e.Raw)
		if !ok || p.xai {
			continue
		}
		kind := p.update.SessionUpdate
		if kind != "agent_message_chunk" && kind != "agent_thought_chunk" {
			agentRun = -1
			continue
		}
		if agentRun >= 0 && stepKind(s.Entries[agentRun].Raw) == kind {
			join(s, agentRun, i)
		} else {
			agentRun = i
		}
	}
}

func stepKind(raw json.RawMessage) string {
	p, _ := parseUpdate(raw)
	return p.update.SessionUpdate
}

// join appends entry i's content to entry into and leaves i an opaque row.
func join(s *transcript.Session, into, i int) {
	e := &s.Entries[i]
	s.Entries[into].Content = mergeText(s.Entries[into].Content, e.Content)
	e.Role = transcript.RoleOpaque
	e.Content = nil
}

// mergeText appends blocks, extending a trailing block of the same text kind
// rather than starting another.
func mergeText(into, more []transcript.Block) []transcript.Block {
	for _, b := range more {
		if n := len(into); n > 0 && into[n-1].Kind == b.Kind && (b.Kind == transcript.BlockText || b.Kind == transcript.BlockReasoning) {
			into[n-1].Text += b.Text
			continue
		}
		into = append(into, b)
	}
	return into
}

// prompts is grok's prompt list for update rows, as
// collect_prompts_from_events builds it: each counted run of user chunks is
// one prompt, blank ones are dropped, and a rewind to N keeps the first N.
func prompts(rows []json.RawMessage) []string {
	var out []string
	var current strings.Builder
	var run *int
	in, counts, seenMarker := false, false, false
	flush := func() {
		if in && counts {
			if t := strings.TrimSpace(current.String()); t != "" {
				out = append(out, t)
			}
		}
		current.Reset()
		in, counts, run = false, false, nil
	}
	for _, raw := range rows {
		st := stepOf(raw)
		switch {
		case st.user && st.prompt:
			if st.promptIndex != nil {
				seenMarker = true
			}
			c := !seenMarker || st.promptIndex != nil
			if !in || ((seenMarker || st.promptIndex != nil) && !sameIndex(st.promptIndex, run)) {
				flush()
				in, run, counts = true, st.promptIndex, c
			} else if run == nil && st.promptIndex != nil {
				run, counts = st.promptIndex, true
			}
			current.WriteString(st.text)
		case st.rewind:
			flush()
			if st.target < len(out) {
				out = out[:st.target]
			}
		default:
			flush()
		}
	}
	flush()
	return out
}

// eventSeq is the counter in an update's event id, <session id>-<n>.
func eventSeq(raw json.RawMessage) int {
	p, ok := parseUpdate(raw)
	if !ok {
		return 0
	}
	i := strings.LastIndexByte(p.trace.EventID, '-')
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(p.trace.EventID[i+1:])
	return n
}

// encodeUpdates is the updates.jsonl rows for a new entry the person is
// shown, as grok writes them while a turn runs: a prompt as a user chunk
// with its prompt index, a reply as message and thought chunks and tool
// calls, a tool result as the update that completes the call. A user entry
// the model is not given is written as a host turn, which grok shows and
// does not count as a prompt.
func (p *plan) encodeUpdates(e transcript.Entry, s *transcript.Session) ([]json.RawMessage, error) {
	if c := p.compactions[e.ID]; c != nil && e.Compaction != nil {
		return p.checkpointRow(c, e, s)
	}
	var updates []update
	switch e.Role {
	case transcript.RoleUser:
		// The prompt's text, then a chunk for each image, as grok echoes a
		// prompt with images attached.
		meta := &updateMeta{HostTurn: true}
		if n, ok := p.prompt[e.ID]; ok {
			meta = &updateMeta{PromptIndex: &n}
		}
		imgs := acpImages(e, "")
		if e.Text() != "" || imgs == nil {
			updates = append(updates, update{SessionUpdate: "user_message_chunk", Content: mustJSON(contentBlock{Type: "text", Text: e.Text()}), Meta: meta})
		}
		for _, img := range imgs {
			updates = append(updates, update{SessionUpdate: "user_message_chunk", Content: mustJSON(img), Meta: meta})
		}
	case transcript.RoleAssistant:
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				updates = append(updates, update{SessionUpdate: "agent_message_chunk", Content: mustJSON(contentBlock{Type: "text", Text: b.Text})})
			case transcript.BlockReasoning:
				updates = append(updates, update{SessionUpdate: "agent_thought_chunk", Content: mustJSON(contentBlock{Type: "text", Text: b.Text})})
			case transcript.BlockToolUse:
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				updates = append(updates, update{SessionUpdate: "tool_call", ToolCallID: b.ToolID, Title: b.Name, Status: "pending", RawInput: input})
			}
		}
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			status := "completed"
			if b.Status == transcript.StatusError {
				status = "failed"
			}
			contents := []any{toolContent{Type: "content", Content: contentBlock{Type: "text", Text: b.Text}}}
			imgs := acpImages(e, b.ToolID)
			if b.Text == "" && imgs != nil {
				contents = nil
			}
			for _, img := range imgs {
				contents = append(contents, toolImage{Type: "content", Content: img})
			}
			content := mustJSON(contents)
			updates = append(updates, update{SessionUpdate: "tool_call_update", ToolCallID: b.ToolID, Status: status, Content: content})
		}
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	rows := make([]json.RawMessage, 0, len(updates))
	for _, u := range updates {
		p.seq++
		params := mustJSON(updateParams{
			SessionID: s.ID,
			Update:    u,
			Meta:      &updateTrace{EventID: s.ID + "-" + strconv.Itoa(p.seq), AgentTimestampMs: t.UnixMilli()},
		})
		row, err := json.Marshal(updateLine{Timestamp: t.Unix(), Method: acpMethod, Params: params})
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("grok: marshal " + err.Error())
	}
	return b
}

// checkpointParams is the compaction_checkpoint update grok sends itself
// once a compaction completes (CompactionCheckpointInfo in
// extensions/notification.rs). Every update row before it in the log shows
// history the model no longer has.
type checkpointParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate  string `json:"sessionUpdate"`
		CheckpointID   string `json:"checkpoint_id"`
		PromptIndex    int    `json:"prompt_index_at_compaction"`
		CheckpointFile string `json:"checkpoint_file"`
		SchemaVersion  int    `json:"schema_version"`
		CreatedAt      string `json:"created_at"`
	} `json:"update"`
	Meta updateTrace `json:"_meta"`
}

func (p *plan) checkpointRow(c *compaction, e transcript.Entry, s *transcript.Session) ([]json.RawMessage, error) {
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	p.seq++
	var params checkpointParams
	params.SessionID = s.ID
	params.Update.SessionUpdate = "compaction_checkpoint"
	params.Update.CheckpointID = c.id
	params.Update.PromptIndex = c.prompt
	params.Update.CheckpointFile = c.file()
	params.Update.SchemaVersion = 1
	params.Update.CreatedAt = t.UTC().Format(time.RFC3339Nano)
	params.Meta = updateTrace{EventID: s.ID + "-" + strconv.Itoa(p.seq), AgentTimestampMs: t.UnixMilli()}
	row, err := json.Marshal(updateLine{Timestamp: t.Unix(), Method: xaiMethod, Params: mustJSON(params)})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{row}, nil
}
