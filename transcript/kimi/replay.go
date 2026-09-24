package kimi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/inference-sh/agentprotocol/transcript"
)

// This file is the reader's half of kimi's resume: which rows of the wire log
// are still live, and what the model's context folds to from them. Kimi
// rebuilds both the model's context (agent/contextMemory/contextOps.ts) and
// the transcript it shows (agent/contextMemory/contextTranscript.ts) from the
// context.* rows, folding loop events into messages with the same fold
// (agent/contextMemory/loopEventFold.ts). finish replays that fold over the
// entries decode produced.

// wireRow is a wire log row as far as the reader needs it. The fields kimi
// type-checks at run time stay raw, so a row with an unexpected shape is
// ignored the way kimi ignores it rather than failing the read.
type wireRow struct {
	Type    string          `json:"type"`
	Time    json.RawMessage `json:"time"`
	Message json.RawMessage `json:"message"`
	Event   json.RawMessage `json:"event"`

	// context.undo
	Count json.RawMessage `json:"count"`

	// agent.switched
	Branch         json.RawMessage `json:"branch"`
	Base           json.RawMessage `json:"base"`
	LegacyUndoLine json.RawMessage `json:"legacyUndoLine"`

	// context.apply_compaction
	Summary              json.RawMessage `json:"summary"`
	ContextSummary       json.RawMessage `json:"contextSummary"`
	CompactedCount       json.RawMessage `json:"compactedCount"`
	KeptUserMessageCount json.RawMessage `json:"keptUserMessageCount"`
	LegacyTail           json.RawMessage `json:"legacyTail"`
}

// contextMsg is a message as the context rows carry it.
type contextMsg struct {
	ID         string     `json:"id,omitempty"`
	Role       string     `json:"role"`
	Content    []part     `json:"content"`
	ToolCalls  []toolCall `json:"toolCalls"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	IsError    bool       `json:"isError,omitempty"`
	Origin     *origin    `json:"origin,omitempty"`
}

// loopEvent is a context.append_loop_event's event.
type loopEvent struct {
	Type         string          `json:"type"`
	UUID         string          `json:"uuid"`
	StepUUID     string          `json:"stepUuid"`
	ToolCallID   string          `json:"toolCallId"`
	Name         string          `json:"name"`
	Args         json.RawMessage `json:"args"`
	Part         part            `json:"part"`
	FinishReason string          `json:"finishReason"`
	Result       struct {
		Output  json.RawMessage `json:"output"`
		IsError bool            `json:"isError"`
	} `json:"result"`
}

func number(raw json.RawMessage) (float64, bool) {
	var f float64
	if len(raw) == 0 || raw[0] == '"' || json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return f, true
}

func str(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// interruptedOutput is what kimi gives the model for a tool call whose result
// was never recorded (loopEventFold.ts:12).
const interruptedOutput = "Tool execution was interrupted before its result was recorded. Do not assume the tool completed successfully."

// audienceOf says who a context message is for. Kimi gives the model every
// message in its context and hides injected reminders, system triggers,
// retries and system rows from the person: the TUI replay skips them
// (tui/controllers/session-replay.ts renderUserMessage) and so does the
// server transcript (transcript/src/history/groupTurns.ts:59). Hook results
// and task notifications are shown, as notices.
func audienceOf(m contextMsg) transcript.Audience {
	if m.Role == "system" {
		return transcript.AudienceModel
	}
	if m.Role == "user" && m.Origin != nil {
		switch m.Origin.Kind {
		case "injection", "system_trigger", "retry", "compaction_summary":
			return transcript.AudienceModel
		}
	}
	return transcript.AudienceAll
}

// messageEntry maps a context message to an entry.
func messageEntry(m contextMsg) transcript.Entry {
	e := transcript.Entry{ID: m.ID, Audience: audienceOf(m)}
	switch m.Role {
	case "user":
		e.Role = transcript.RoleUser
	case "assistant":
		e.Role = transcript.RoleAssistant
	case "system":
		e.Role = transcript.RoleSystem
	case "tool":
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{toolResult(m.ToolCallID, textOf(m.Content), m.IsError)}
		for _, p := range m.Content {
			if b, ok := media(p, m.ToolCallID); ok {
				e.Content = append(e.Content, b)
			}
		}
		return e
	default:
		return transcript.Entry{}
	}
	for _, p := range m.Content {
		if b, ok := contentBlock(p); ok {
			e.Content = append(e.Content, b)
		}
	}
	for _, tc := range m.ToolCalls {
		e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tc.ID, Name: tc.Name, Input: arguments(tc.Arguments)})
	}
	return e
}

func contentBlock(p part) (transcript.Block, bool) {
	switch p.Type {
	case "text":
		return transcript.Block{Kind: transcript.BlockText, Text: p.Text}, true
	case "think":
		return transcript.Block{Kind: transcript.BlockReasoning, Text: p.Think}, true
	}
	return media(p, "")
}

// media reads an image_url, video_url or audio_url part: an image, or a
// file for video and audio. A data: URL gives the bytes and media type;
// any other URL, a web URL or kimi's media:// reference, is the location.
// toolID is set for media a tool returned.
func media(p part, toolID string) (transcript.Block, bool) {
	var m *mediaURL
	kind := transcript.BlockFile
	switch p.Type {
	case "image_url":
		m, kind = p.ImageURL, transcript.BlockImage
	case "video_url":
		m = p.VideoURL
	case "audio_url":
		m = p.AudioURL
	}
	if m == nil || m.URL == "" {
		return transcript.Block{}, false
	}
	b := transcript.Block{Kind: kind, ToolID: toolID, Name: m.Name}
	if mediaType, data, ok := transcript.ParseDataURL(m.URL); ok {
		b.MediaType, b.Data = mediaType, data
	} else {
		b.URI = m.URL
	}
	return b, true
}

func toolResult(id, text string, isError bool) transcript.Block {
	b := transcript.Block{Kind: transcript.BlockToolResult, ToolID: id, Text: text, Status: transcript.StatusOK}
	if isError {
		b.Status = transcript.StatusError
	}
	return b
}

func textOf(parts []part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// output is a tool result's output, which kimi stores as a string or as
// content parts: its text, and the media it returned for the call toolID.
func output(raw json.RawMessage, toolID string) (string, []transcript.Block) {
	if s, ok := str(raw); ok {
		return s, nil
	}
	var parts []part
	_ = json.Unmarshal(raw, &parts)
	var out []transcript.Block
	for _, p := range parts {
		if b, ok := media(p, toolID); ok {
			out = append(out, b)
		}
	}
	return textOf(parts), out
}

// finish replays kimi's restore over the decoded rows: it drops what an undo
// abandoned, folds loop events into assistant and tool messages, and applies
// compactions, clears and legacy undos to the model's context.
func finish(s *transcript.Session) error {
	rows := make([]wireRow, len(s.Entries))
	lines := make([]int, len(s.Entries))
	n := 0
	for i, e := range s.Entries {
		// A torn line never reaches kimi's reader, so it takes no line number.
		if json.Unmarshal(e.Raw, &rows[i]) != nil {
			continue
		}
		n++
		lines[i] = n
	}
	live, err := restorable(rows, lines)
	if err != nil {
		return err
	}
	r := &replay{s: s, rows: rows, open: -1, callRow: map[string]int{}, abandoned: map[string]int{}}
	for i := range s.Entries {
		if lines[i] == 0 {
			continue
		}
		if live[i] {
			if err := r.apply(i); err != nil {
				return err
			}
		} else {
			r.abandon(i)
		}
	}
	r.settleOpen()
	r.flush()
	for _, i := range r.abandoned {
		if len(s.Entries[i].Content) == 0 {
			s.Entries[i].Role = transcript.RoleOpaque
		}
	}
	r.link()
	return nil
}

// switchEdge is an agent.switched row: an undo that moved the conversation to
// a new branch forking from base.
type switchEdge struct {
	line           int
	branch         string
	baseBranch     string
	baseLine       int
	legacyUndoLine int
}

type segment struct {
	branch   string
	edge     *switchEdge
	from, to int
}

// restorable reports, per entry, whether kimi restores the row: whether it
// is on the active branch's chain of segments (wire/tree/tree.ts parseTree,
// activeChain), and not a bookkeeping row of an undo (wire/tree/fork.ts
// restorableChain). An undo is a branch: agent.switched starts a segment
// whose history is the base branch up to the fork line, so the turns undone
// stay in the file and off the chain.
func restorable(rows []wireRow, lines []int) ([]bool, error) {
	var edges []switchEdge
	paired := map[int]bool{}
	last := 0
	for i, r := range rows {
		last = max(last, lines[i])
		if lines[i] == 0 || r.Type != "agent.switched" {
			continue
		}
		e, ok := readEdge(r, lines[i])
		if !ok {
			continue
		}
		edges = append(edges, e)
		if e.legacyUndoLine != 0 {
			paired[e.legacyUndoLine] = true
		}
	}
	var segments []segment
	branch, from := "main", 1
	var opening *switchEdge
	for i := range edges {
		segments = append(segments, segment{branch: branch, edge: opening, from: from, to: edges[i].line})
		branch, opening, from = edges[i].branch, &edges[i], edges[i].line+1
	}
	segments = append(segments, segment{branch: branch, edge: opening, from: from, to: max(last, from-1)})
	byBranch := map[string]segment{}
	for _, sg := range segments {
		byBranch[sg.branch] = sg
	}

	chain := map[int]bool{}
	visited := map[string]bool{}
	var walk func(branch string, upto int) error
	walk = func(branch string, upto int) error {
		if visited[branch] {
			return fmt.Errorf("agent.switched base chain cycles at branch %q", branch)
		}
		visited[branch] = true
		sg, ok := byBranch[branch]
		if !ok {
			return fmt.Errorf("agent.switched base chain references unknown branch %q", branch)
		}
		floor := 1
		if sg.edge != nil {
			if err := walk(sg.edge.baseBranch, sg.edge.baseLine); err != nil {
				return err
			}
			floor = sg.edge.line + 1
		}
		ceil := sg.to
		if upto >= 0 {
			ceil = upto
		}
		for l := floor; l <= ceil; l++ {
			chain[l] = true
		}
		return nil
	}
	if err := walk(branch, -1); err != nil {
		return nil, err
	}

	live := make([]bool, len(rows))
	surviving := map[string]bool{}
	for i, r := range rows {
		l := lines[i]
		if l == 0 || !chain[l] {
			continue
		}
		switch {
		case r.Type == "agent.switched", r.Type == "context.undone":
		case r.Type == "context.undo" && paired[l]:
		default:
			live[i] = true
			if m, ok := appendedMessage(r); ok && m.ID != "" && isUndoAnchor(m) {
				surviving[m.ID] = true
			}
		}
	}
	// Injections outside the chain come back when the prompt that owns them
	// survived, or when no prompt owns them.
	for i, r := range rows {
		if lines[i] == 0 || chain[lines[i]] {
			continue
		}
		m, ok := appendedMessage(r)
		if !ok || m.Origin == nil || m.Origin.Kind != "injection" {
			continue
		}
		if m.Origin.OwnerPromptID == "" || surviving[m.Origin.OwnerPromptID] {
			live[i] = true
		}
	}
	return live, nil
}

func readEdge(r wireRow, line int) (switchEdge, bool) {
	branch, ok := str(r.Branch)
	if !ok {
		return switchEdge{}, false
	}
	var base struct {
		Branch json.RawMessage `json:"branch"`
		Line   json.RawMessage `json:"line"`
	}
	if len(r.Base) == 0 || r.Base[0] != '{' || json.Unmarshal(r.Base, &base) != nil {
		return switchEdge{}, false
	}
	baseBranch, ok := str(base.Branch)
	if !ok {
		return switchEdge{}, false
	}
	baseLine, ok := number(base.Line)
	if !ok {
		return switchEdge{}, false
	}
	e := switchEdge{line: line, branch: branch, baseBranch: baseBranch, baseLine: int(baseLine)}
	if l, ok := number(r.LegacyUndoLine); ok {
		e.legacyUndoLine = int(l)
	}
	return e, true
}

func appendedMessage(r wireRow) (contextMsg, bool) {
	if r.Type != "context.append_message" {
		return contextMsg{}, false
	}
	var m contextMsg
	if json.Unmarshal(r.Message, &m) != nil {
		return contextMsg{}, false
	}
	return m, true
}

// isUndoAnchor is conversationTime.ts isUndoAnchor: a prompt the person
// typed, which is what an undo counts back to.
func isUndoAnchor(m contextMsg) bool {
	if m.Role != "user" {
		return false
	}
	o := m.Origin
	if o == nil {
		return true
	}
	if o.InTurn {
		return false
	}
	switch o.Kind {
	case "user":
		return true
	case "skill_activation", "plugin_command":
		return o.Trigger == "user-slash"
	}
	return false
}

func isPromptOwnedInjection(m, prompt contextMsg) bool {
	return m.Origin != nil && m.Origin.Kind == "injection" && m.Origin.OwnerPromptID != "" && m.Origin.OwnerPromptID == prompt.ID
}

// held is one message of the model's context as the replay folds it: an
// entry of the session, or a message kimi made when it compacted.
type held struct {
	at  int // index into Entries, or -1
	msg contextMsg
	e   *transcript.Entry // the made-up message's entry when at is -1
	// summary marks a message of the latest compaction's Summary, at slot
	// in the Summary of the compaction entry from.
	summary    bool
	from, slot int
}

func (h held) entry(s *transcript.Session) transcript.Entry {
	if h.e != nil {
		return *h.e
	}
	e := s.Entries[h.at]
	e.Raw = nil
	e.Content = append([]transcript.Block(nil), e.Content...)
	return e
}

func madeUp(m contextMsg) held {
	e := messageEntry(m)
	return held{at: -1, msg: m, e: &e}
}

// madeUpSummary is a compaction's summary. kimi does not show it, but it is
// conversation, the one record of what the compaction retired, so it moves
// with the session to another agent.
func madeUpSummary(m contextMsg) held {
	h := madeUp(m)
	h.e.Audience = transcript.AudienceAll
	return h
}

// replay is loopEventFold with kimi's context operations around it.
type replay struct {
	s    *transcript.Session
	rows []wireRow
	hist []held

	open        int // entry of the step being folded, or -1
	openUUID    string
	openCalls   bool
	openVacuous bool
	pending     []string       // tool calls awaiting a result, in call order
	callRow     map[string]int // tool call id -> its tool.call row
	abandoned   map[string]int // step uuid -> step.begin entry, off the chain
	deferred    []int          // messages held back until the pending results arrive

	// order is the entries in the order kimi delivers them to its context,
	// compaction rows included.
	order []int
}

// deliver puts an entry into the context.
func (r *replay) deliver(i int, m contextMsg) {
	r.hist = append(r.hist, held{at: i, msg: m})
	r.order = append(r.order, i)
}

// flush delivers the held-back messages once no call is waiting
// (loopEventFold.ts flushDeferred).
func (r *replay) flush() {
	if len(r.pending) > 0 {
		return
	}
	for _, i := range r.deferred {
		m, _ := appendedMessage(r.rows[i])
		r.deliver(i, m)
	}
	r.deferred = nil
}

func (r *replay) apply(i int) error {
	row := r.rows[i]
	e := &r.s.Entries[i]
	switch row.Type {
	case "context.append_message":
		// Kimi holds back a message appended while tool calls await results
		// until they arrive (loopEventFold.ts appendMessage); link puts it
		// after them.
		if m, ok := appendedMessage(row); ok && e.Role != transcript.RoleOpaque {
			if len(r.pending) > 0 {
				r.deferred = append(r.deferred, i)
			} else {
				r.deliver(i, m)
			}
		}
	case "context.append_loop_event":
		var ev loopEvent
		if json.Unmarshal(row.Event, &ev) != nil {
			return nil
		}
		r.loopEvent(i, ev)
	case "context.clear":
		r.reset()
		r.hist = nil
		e.Compaction = &transcript.Compaction{}
		r.order = append(r.order, i)
	case "context.apply_compaction":
		next, err := r.compact(row)
		if err != nil {
			return err
		}
		r.reset()
		c := &transcript.Compaction{}
		for k := range next {
			c.Summary = append(c.Summary, next[k].entry(r.s))
			next[k].summary, next[k].from, next[k].slot = true, i, k
		}
		r.hist = next
		e.Compaction = c
		r.order = append(r.order, i)
	case "context.undo":
		// Only an undo no agent.switched pairs with reaches here: a paired
		// one is off the restorable chain.
		if count, ok := number(row.Count); ok && count > 0 && count == math.Trunc(count) {
			r.undo(int(count))
		}
	}
	return nil
}

func (r *replay) loopEvent(i int, ev loopEvent) {
	e := &r.s.Entries[i]
	switch ev.Type {
	case "step.begin":
		r.settleOpen()
		r.open, r.openUUID, r.openCalls, r.openVacuous = i, ev.UUID, false, true
		r.deliver(i, contextMsg{Role: "assistant"})
	case "step.end":
		// An interrupted or failed step stays open: its partial content is
		// kept when the next step settles it.
		if ev.FinishReason == "interrupted" || ev.FinishReason == "error" {
			return
		}
		r.settleOpen()
		r.flush()
	case "content.part":
		if r.open < 0 || ev.StepUUID != r.openUUID {
			return
		}
		if b, ok := contentBlock(ev.Part); ok {
			r.s.Entries[r.open].Content = append(r.s.Entries[r.open].Content, b)
		}
		r.openVacuous = r.openVacuous && vacuous(ev.Part)
	case "tool.call":
		if r.open < 0 || ev.StepUUID != r.openUUID {
			return
		}
		r.s.Entries[r.open].Content = append(r.s.Entries[r.open].Content, toolUse(ev))
		r.pending = append(r.pending, ev.ToolCallID)
		r.callRow[ev.ToolCallID] = i
		r.openCalls = true
	case "tool.result":
		k := indexOf(r.pending, ev.ToolCallID)
		if k < 0 {
			// A result for no pending call is ignored.
			*e = transcript.Entry{Time: e.Time, Raw: e.Raw}
			return
		}
		r.pending = append(r.pending[:k], r.pending[k+1:]...)
		r.deliver(i, contextMsg{Role: "tool"})
		r.flush()
	}
}

// settleOpen closes the step being folded: calls still waiting get an
// interrupted result, and a step that said nothing and called nothing is
// dropped.
func (r *replay) settleOpen() {
	if r.open < 0 {
		return
	}
	for _, id := range r.pending {
		// The result has no row of its own; the call's row carries it.
		at := r.callRow[id]
		c := &r.s.Entries[at]
		*c = transcript.Entry{Role: transcript.RoleTool, Time: c.Time, Raw: c.Raw,
			Content: []transcript.Block{toolResult(id, interruptedOutput, true)}}
		r.deliver(at, contextMsg{Role: "tool"})
	}
	r.pending = nil
	r.flush()
	if !r.openCalls && r.openVacuous {
		o := &r.s.Entries[r.open]
		*o = transcript.Entry{Time: o.Time, Raw: o.Raw}
		for k, h := range r.hist {
			if h.at == r.open {
				r.hist = append(r.hist[:k], r.hist[k+1:]...)
				break
			}
		}
		for k, at := range r.order {
			if at == r.open {
				r.order = append(r.order[:k], r.order[k+1:]...)
				break
			}
		}
	}
	r.open = -1
}

// reset starts the fold over, as kimi does after it replaces the context.
// A message still held back never reaches the context.
func (r *replay) reset() {
	r.open, r.pending, r.openCalls = -1, nil, false
	for _, i := range r.deferred {
		r.s.Entries[i].Audience = transcript.AudienceNone
	}
	r.deferred = nil
}

// abandon marks a row off the restorable chain. Its message is kept for the
// store and given to no one; a step's content still lands on its message.
func (r *replay) abandon(i int) {
	e := &r.s.Entries[i]
	if e.Role != transcript.RoleOpaque {
		e.Audience = transcript.AudienceNone
	}
	if r.rows[i].Type != "context.append_loop_event" {
		return
	}
	var ev loopEvent
	if json.Unmarshal(r.rows[i].Event, &ev) != nil {
		return
	}
	switch ev.Type {
	case "step.begin":
		r.abandoned[ev.UUID] = i
	case "content.part", "tool.call":
		at, ok := r.abandoned[ev.StepUUID]
		if !ok {
			return
		}
		if ev.Type == "tool.call" {
			r.s.Entries[at].Content = append(r.s.Entries[at].Content, toolUse(ev))
		} else if b, ok := contentBlock(ev.Part); ok {
			r.s.Entries[at].Content = append(r.s.Entries[at].Content, b)
		}
	}
}

// undo is the context's onUndo (contextOps.ts:91-96, computeUndoCut): it
// cuts back to the count-th prompt from the end, with the injections that
// prompt owns, and never across a compaction. The messages cut go to no one.
func (r *replay) undo(count int) {
	if len(r.hist) == 0 {
		return
	}
	remaining, removed, cut := count, 0, -1
	for i := len(r.hist) - 1; i >= 0 && remaining > 0; i-- {
		m := r.hist[i].msg
		if m.Origin != nil && m.Origin.Kind == "injection" {
			continue
		}
		if m.Origin != nil && m.Origin.Kind == "compaction_summary" {
			break
		}
		if isUndoAnchor(m) {
			remaining--
			removed++
			cut = i
			for cut > 0 && isPromptOwnedInjection(r.hist[cut-1].msg, m) {
				cut--
			}
		}
	}
	if cut < 0 || removed < count {
		return
	}
	// A legacy compaction keeps history after its summary, so the cut can
	// reach into the compaction's Summary.
	cutSummary := map[int]map[int]bool{}
	for _, h := range r.hist[cut:] {
		switch {
		case h.at >= 0:
			r.s.Entries[h.at].Audience = transcript.AudienceNone
		case h.summary:
			if cutSummary[h.from] == nil {
				cutSummary[h.from] = map[int]bool{}
			}
			cutSummary[h.from][h.slot] = true
		}
	}
	for from, slots := range cutSummary {
		c := r.s.Entries[from].Compaction
		var kept []transcript.Entry
		for k, e := range c.Summary {
			if !slots[k] {
				kept = append(kept, e)
			}
		}
		c.Summary = kept
	}
	r.hist = r.hist[:cut]
	r.reset()
}

func toolUse(ev loopEvent) transcript.Block {
	b := transcript.Block{Kind: transcript.BlockToolUse, ToolID: ev.ToolCallID, Name: ev.Name}
	if len(ev.Args) > 0 && string(ev.Args) != "null" {
		b.Input = ev.Args
	}
	return b
}

func vacuous(p part) bool {
	switch p.Type {
	case "text":
		return strings.TrimSpace(p.Text) == ""
	case "think":
		return p.Encrypted == nil && strings.TrimSpace(p.Think) == ""
	}
	return false
}

func indexOf(ids []string, id string) int {
	for i, x := range ids {
		if x == id {
			return i
		}
	}
	return -1
}

// Compaction, as agent/contextMemory/compactionHandoff.ts shapes it.

const (
	compactUserMessageMaxTokens  = 20_000
	compactUserMessageHeadTokens = 2_000
	mediaTokenEstimate           = 2000
)

// compact returns the context kimi replaces the history with at a
// context.apply_compaction row (buildContextCompactionShape): the person's
// own prompts, trimmed to a token budget, then the summary and a note to
// carry on. A record without keptUserMessageCount is the legacy shape: the
// summary followed by the history from compactedCount on.
func (r *replay) compact(row wireRow) ([]held, error) {
	compacted, ok := number(row.CompactedCount)
	if !ok {
		if compacted, ok = number(row.Count); !ok {
			return nil, errors.New("context.apply_compaction without compactedCount")
		}
	}
	var legacySummary *contextMsg
	summary, ok := str(row.Summary)
	if !ok {
		var m contextMsg
		if json.Unmarshal(row.Summary, &m) == nil && m.Role != "" && m.Content != nil {
			legacySummary = &m
		}
	}
	contextSummary, hasContext := str(row.ContextSummary)
	if !ok && !hasContext && legacySummary == nil {
		return nil, errors.New("context.apply_compaction without summary")
	}
	text := summary
	if hasContext {
		text = contextSummary
	} else if !ok && legacySummary != nil {
		text = textOf(legacySummary.Content)
	}
	summaryMsg := contextMsg{Role: "user", Content: []part{{Type: "text", Text: text}}, Origin: &origin{Kind: "compaction_summary"}}

	_, kept := number(row.KeptUserMessageCount)
	legacy := !kept
	if len(row.LegacyTail) > 0 {
		var b bool
		if json.Unmarshal(row.LegacyTail, &b) == nil {
			legacy = b
		}
	}
	if legacy {
		first := summaryMsg
		if legacySummary != nil {
			first = *legacySummary
		}
		out := []held{madeUpSummary(first)}
		for _, h := range r.hist[min(int(compacted), len(r.hist)):] {
			e := h.entry(r.s)
			out = append(out, held{at: -1, msg: h.msg, e: &e})
		}
		return out, nil
	}

	var users []held
	for _, h := range r.hist {
		if isRealUserInput(h.msg) {
			users = append(users, h)
		}
	}
	head, tail, elided, omitted := selectUserMessages(users)
	var out []held
	for _, h := range head {
		out = append(out, h.fixed(r.s))
	}
	if elided {
		out = append(out, madeUp(contextMsg{Role: "user", Content: []part{{Type: "text", Text: systemReminder(fmt.Sprintf(
			"Some of this conversation's user messages were omitted here during compaction: the messages above this note are the oldest user input, the messages below are the most recent, and roughly %d tokens in between were dropped. The omitted content is covered by the compaction summary at the end of the conversation.", omitted))}},
			Origin: &origin{Kind: "injection", Variant: "compaction_elision"}}))
	}
	for _, h := range tail {
		out = append(out, h.fixed(r.s))
	}
	out = append(out, madeUpSummary(summaryMsg), madeUp(contextMsg{Role: "user",
		Content: []part{{Type: "text", Text: systemReminder("Context compaction is complete — continue the work that was in progress when it began.")}},
		Origin:  &origin{Kind: "injection", Variant: "compaction_continuation"}}))
	return out, nil
}

// fixed makes a held message a made-up one, so a later compaction or undo
// does not reach back to the entry it came from.
func (h held) fixed(s *transcript.Session) held {
	e := h.entry(s)
	return held{at: -1, msg: h.msg, e: &e}
}

func systemReminder(s string) string {
	return "<system-reminder>\n" + strings.TrimSpace(s) + "\n</system-reminder>"
}

// isRealUserInput is compactionHandoff.ts isRealUserInput: a prompt a
// compaction keeps.
func isRealUserInput(m contextMsg) bool {
	if m.Role != "user" {
		return false
	}
	if m.Origin == nil {
		return true
	}
	switch m.Origin.Kind {
	case "user":
		return true
	case "skill_activation", "plugin_command":
		return m.Origin.Trigger == "user-slash"
	}
	return false
}

// selectUserMessages is selectCompactionUserMessages: every prompt when they
// fit the budget, else the oldest few and the most recent, cut to fit.
func selectUserMessages(msgs []held) (head, tail []held, elided bool, omitted int) {
	total := 0
	for _, h := range msgs {
		total += messageTokens(h.msg)
	}
	if total <= compactUserMessageMaxTokens {
		return nil, msgs, false, 0
	}
	headBudget := min(max(compactUserMessageHeadTokens, 0), compactUserMessageMaxTokens)
	remaining := compactUserMessageMaxTokens - headBudget
	headEnd := len(msgs)
	var droppedPrefix *held
	for i := len(msgs) - 1; i >= 0 && remaining > 0; i-- {
		h := msgs[i]
		tokens := messageTokens(h.msg)
		if tokens <= remaining {
			tail = append(tail, h)
			remaining -= tokens
			headEnd = i
			continue
		}
		full := textOf(h.msg.Content)
		suffix := truncateFromEnd(full, remaining)
		tail = append(tail, replaceText(h.msg, suffix))
		headEnd = i
		if prefix := full[:len(full)-len(suffix)]; prefix != "" {
			p := replaceText(h.msg, prefix)
			droppedPrefix = &p
		}
		break
	}
	for l, r := 0, len(tail)-1; l < r; l, r = l+1, r-1 {
		tail[l], tail[r] = tail[r], tail[l]
	}
	candidates := append([]held(nil), msgs[:headEnd]...)
	if droppedPrefix != nil {
		candidates = append(candidates, *droppedPrefix)
	}
	headRemaining := headBudget
	for _, h := range candidates {
		if headRemaining <= 0 {
			break
		}
		tokens := messageTokens(h.msg)
		if tokens <= headRemaining {
			head = append(head, h)
			headRemaining -= tokens
			continue
		}
		head = append(head, replaceText(h.msg, truncate(textOf(h.msg.Content), headRemaining)))
		break
	}
	kept := 0
	for _, h := range head {
		kept += messageTokens(h.msg)
	}
	for _, h := range tail {
		kept += messageTokens(h.msg)
	}
	return head, tail, true, max(0, total-kept)
}

// replaceText is a prompt cut down to one text part.
func replaceText(m contextMsg, text string) held {
	m.Content = []part{{Type: "text", Text: text}}
	m.ToolCalls = nil
	return madeUp(m)
}

// estimateTokens is kimi's estimate: a token per four ASCII characters and
// one per other character.
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, c := range s {
		if c <= 127 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

func messageTokens(m contextMsg) int {
	total := estimateTokens(m.Role)
	for _, p := range m.Content {
		switch p.Type {
		case "text":
			total += estimateTokens(p.Text)
		case "think":
			total += estimateTokens(p.Think)
		case "image_url", "audio_url", "video_url":
			total += mediaTokenEstimate
		}
	}
	for _, tc := range m.ToolCalls {
		quoted, _ := json.Marshal(tc.Arguments)
		total += estimateTokens(tc.Name) + estimateTokens(string(quoted))
	}
	return total
}

// truncate keeps the longest prefix of s within maxTokens.
func truncate(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	ascii, other, end := 0, 0, 0
	for i, c := range s {
		if c <= 127 {
			ascii++
		} else {
			other++
		}
		if (ascii+3)/4+other > maxTokens {
			break
		}
		end = i + utf8.RuneLen(c)
	}
	return s[:end]
}

// truncateFromEnd keeps the longest suffix of s within maxTokens.
func truncateFromEnd(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	ascii, other, start := 0, 0, len(s)
	for i := len(s); i > 0; {
		c, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		if c <= 127 {
			ascii++
		} else {
			other++
		}
		if (ascii+3)/4+other > maxTokens {
			break
		}
		start = i
	}
	return s[start:]
}

// link threads the entries in the order kimi delivers them, when that is
// not the order of the file: a message held back behind tool results
// reaches the model after them. The links are the model's only; the rows
// keep theirs. Entries without an id get one for the chain, and Leaf pins
// the last delivered entry, so an appended turn continues from it.
func (r *replay) link() {
	sorted := true
	for k := 1; k < len(r.order); k++ {
		if r.order[k] < r.order[k-1] {
			sorted = false
			break
		}
	}
	if sorted {
		return
	}
	prev := ""
	for _, i := range r.order {
		e := &r.s.Entries[i]
		if e.ID == "" {
			e.ID = fmt.Sprintf("kimi-row-%d", i)
		}
		e.ParentID = prev
		prev = e.ID
	}
	last := ""
	for _, e := range r.s.Entries {
		if e.ID != "" {
			last = e.ID
		}
	}
	if prev != last {
		r.s.Leaf = prev
	}
}
