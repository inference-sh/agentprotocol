package grok

import (
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

// contentBlock is an ACP content block, as a chunk carries it.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Meta *struct {
		BashCommand json.RawMessage `json:"bashCommand"`
	} `json:"_meta,omitempty"`
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
		// An image the person attached is a chunk with no text: still their
		// message, with no block this model can hold.
		e.Role = transcript.RoleUser
		e.Content = textBlocks(transcript.BlockText, chunkText(u.Content))
	case "agent_message_chunk":
		e.Role = transcript.RoleAssistant
		e.Content = textBlocks(transcript.BlockText, chunkText(u.Content))
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
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: u.ToolCallID, Text: toolOutput(u), Status: status}}
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

// toolOutput is what a finished call shows: its text content, or, for a
// call the server ran, which reports only raw output, that output.
func toolOutput(u update) string {
	var contents []toolContent
	_ = json.Unmarshal(u.Content, &contents)
	var b strings.Builder
	for _, c := range contents {
		if c.Type == "content" && c.Content.Type == "text" {
			b.WriteString(c.Content.Text)
		}
	}
	if b.Len() == 0 && len(u.RawOutput) > 0 && string(u.RawOutput) != "null" {
		var text string
		if json.Unmarshal(u.RawOutput, &text) == nil {
			return text
		}
		return string(u.RawOutput)
	}
	return b.String()
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
// its rows here are left opaque. That includes the echo of a command such as
// /compact, which has no chat row to be placed by and would otherwise be
// shown ahead of turns it followed. A rewind is applied first: a rewind to
// prompt N drops everything from prompt N's first chunk up to the marker,
// as filter_rewind_by does before replaying the file to the client, and a
// compaction it drops no longer counts. A message grok wrote as several
// chunk rows is joined into its first row's entry.
func finishUpdates(s *transcript.Session) error {
	joinChunks(s)
	retired := 0
	for i, e := range s.Entries {
		if e.Audience != transcript.AudienceNone && stepKind(e.Raw) == "compaction_checkpoint" {
			retired = i
		}
	}
	for i := retired; i < len(s.Entries); i++ {
		if e := &s.Entries[i]; e.Audience != transcript.AudienceNone {
			e.Role, e.Content = transcript.RoleOpaque, nil
		}
	}
	return nil
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
	var updates []update
	switch e.Role {
	case transcript.RoleUser:
		u := update{SessionUpdate: "user_message_chunk", Content: mustJSON(contentBlock{Type: "text", Text: e.Text()})}
		if n, ok := p.prompt[e.ID]; ok {
			u.Meta = &updateMeta{PromptIndex: &n}
		} else {
			u.Meta = &updateMeta{HostTurn: true}
		}
		updates = append(updates, u)
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
			content := mustJSON([]toolContent{{Type: "content", Content: contentBlock{Type: "text", Text: b.Text}}})
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
