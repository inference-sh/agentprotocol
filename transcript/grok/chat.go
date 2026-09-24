package grok

import (
	"encoding/json"
	"strings"

	"github.com/inference-sh/agentprotocol/transcript"
)

// chat_history.jsonl holds grok's ConversationItem rows
// (xai-grok-sampling-types/src/conversation.rs), tagged by type. Sessions
// written before chat_format_version 1 hold OpenAI-shaped
// {"role":...,"content":...} rows instead; grok still reads both
// (read_chat_history_sync_bounded in storage/jsonl/mod.rs), and so does this
// codec.

// chatRow is every field of a chat_history row this codec reads, across
// item types and both format versions.
type chatRow struct {
	Type string `json:"type"`
	// Role is set on legacy rows in place of Type.
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// SyntheticReason says who wrote a user or system item: empty for the
	// person, otherwise the runtime injecting context.
	SyntheticReason string         `json:"synthetic_reason"`
	ToolCalls       []chatToolCall `json:"tool_calls"`
	ToolCallID      string         `json:"tool_call_id"`
	// Reasoning on a v1 assistant row and ReasoningContent on a legacy one
	// are the thinking earlier clients stored on the message itself;
	// upgrade_legacy_reasoning lifts them to a reasoning item on load.
	Reasoning        *reasoningContent `json:"reasoning"`
	ReasoningContent string            `json:"reasoning_content"`
	RawOutput        []json.RawMessage `json:"raw_output"`
	// Summary is a reasoning item's summary parts; its Content holds the
	// reasoning_text parts.
	Summary []textPart `json:"summary"`
	// Kind is a backend tool call's payload.
	Kind json.RawMessage `json:"kind"`
}

// chatToolCall is a tool call on an assistant row: flat in v1, under
// function in the legacy chat-completions shape.
type chatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Function  *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function,omitempty"`
}

type reasoningContent struct {
	Text string `json:"text"`
}

// textPart is any typed part with text: a user content part, a legacy
// content block, a reasoning summary or reasoning text part.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// backendCall is the part of a backend tool call's kind the codec reads: the
// web search, X search or code interpreter call the server ran
// (BackendToolKind in conversation.rs).
type backendCall struct {
	ToolType string          `json:"tool_type"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Status   string          `json:"status"`
	Action   *backendAction  `json:"action"`
	Outputs  []backendOutput `json:"outputs"`
}

type backendAction struct {
	Sources []struct {
		URL string `json:"url"`
	} `json:"sources"`
}

type backendOutput struct {
	Type string `json:"type"`
	Logs string `json:"logs"`
}

// decodeChat reads a chat_history row, the file grok gives the model. The
// person's prompts and the model's replies are for everyone; what the
// runtime injected (the system prompt, the <user_info> prefix, reminders,
// project instructions, compaction output) is for the model only. A prompt
// reads as what the person typed, without the <user_query> frame grok puts
// around it. A row grok cannot parse it skips on load, and so it stays
// opaque here. finishChat handles the rows a compaction wrote.
func decodeChat(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r chatRow
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, nil
	}
	kind := r.Type
	if kind == "" {
		kind = r.Role
	}
	var e transcript.Entry
	switch kind {
	case "system":
		e.Role = transcript.RoleSystem
		e.Audience = transcript.AudienceModel
		e.Content = textBlocks(transcript.BlockText, contentText(r.Content))
	case "user":
		e.Role = transcript.RoleUser
		text := contentText(r.Content)
		if query, ok := userQuery(text); ok && isHuman(r.SyntheticReason) {
			text = query
		} else {
			e.Audience = transcript.AudienceModel
		}
		e.Content = textBlocks(transcript.BlockText, text)
	case "assistant":
		e.Role = transcript.RoleAssistant
		e.Content = textBlocks(transcript.BlockReasoning, legacyReasoning(r))
		e.Content = append(e.Content, textBlocks(transcript.BlockText, contentText(r.Content))...)
		for _, tc := range r.ToolCalls {
			name, args := tc.Name, tc.Arguments
			if tc.Function != nil {
				name, args = tc.Function.Name, tc.Function.Arguments
			}
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tc.ID, Name: name, Input: arguments(args)})
		}
	case "tool_result", "tool":
		// Images a tool returned travel beside the text and have no block.
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: r.ToolCallID, Text: contentText(r.Content), Status: transcript.StatusOK}}
	case "reasoning":
		e.Role = transcript.RoleAssistant
		e.Content = textBlocks(transcript.BlockReasoning, reasoningText(r.Summary, r.Content))
	case "backend_tool_call":
		var c backendCall
		if json.Unmarshal(r.Kind, &c) != nil {
			return transcript.Entry{}, false, nil
		}
		e.Role = transcript.RoleAssistant
		e.Content = backendBlocks(c, r.Kind)
	default:
		return transcript.Entry{}, false, nil
	}
	return e, true, nil
}

// isHuman reports whether a synthetic_reason is the person's: grok omits
// the field for them (SyntheticReason::Human).
func isHuman(reason string) bool {
	return reason == "" || reason == "human"
}

// userQuery is the prompt inside a <user_query> frame, trimmed, as grok's
// extract_user_query reads it. Human rows without one are not prompts: the
// <user_info> prefix a fresh session starts with is one.
func userQuery(text string) (string, bool) {
	const open, closing = "<user_query>", "</user_query>"
	i := strings.Index(text, open)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:j]), true
}

// finishChat reads what a compaction left in chat_history.jsonl. grok
// replaces the whole file with the history build_compacted_history
// assembles (xai-chat-state compaction_utils.rs): the system prompt, the
// <user_info> prefix, the last query re-pushed, the messages of a turn cut
// short, then the summary, all ahead of the turns since. Every row up to the
// summary is context grok built, shown to no one: the person saw the
// re-pushed query and the cut-short turn when they happened, in
// updates.jsonl. The summary row carries the compaction, and the context it
// replaces the history with is those rows followed by the summary itself,
// which is what the conversation was.
//
// grok marks no row as the summary (is_compaction_summary in
// conversation.rs says so); build_compacted_history makes the prefix the
// first compaction_meta row and the summary the second.
func finishChat(s *transcript.Session) error {
	metas := 0
	for i := range s.Entries {
		var r chatRow
		if json.Unmarshal(s.Entries[i].Raw, &r) != nil || r.SyntheticReason != "compaction_meta" {
			continue
		}
		if metas++; metas < 2 {
			continue
		}
		var history []transcript.Entry
		for j := range s.Entries[:i] {
			e := &s.Entries[j]
			if e.Role == transcript.RoleOpaque {
				continue
			}
			e.Audience = transcript.AudienceModel
			history = append(history, *e)
		}
		sum := &s.Entries[i]
		sum.Audience = transcript.AudienceModel
		self := *sum
		self.Audience = transcript.AudienceAll
		sum.Compaction = &transcript.Compaction{Summary: append(history, self)}
		return nil
	}
	return nil
}

// contentText is the text of a content field: a plain string, or the text
// parts of a part list. Image parts carry no text and are left out.
func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []textPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "reasoning_text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// reasoningText is a reasoning item's text: its reasoning_text parts when it
// has them, else its summary. An item with only encrypted content has none.
func reasoningText(summary []textPart, content json.RawMessage) string {
	if text := contentText(content); text != "" {
		return text
	}
	var b strings.Builder
	for _, p := range summary {
		b.WriteString(p.Text)
	}
	return b.String()
}

// legacyReasoning is the reasoning an older assistant row carries on itself,
// in the precedence upgrade_legacy_reasoning gives it: raw_output first, then
// a v1 reasoning field, then a legacy reasoning_content.
func legacyReasoning(r chatRow) string {
	if r.RawOutput != nil {
		var b strings.Builder
		for _, item := range r.RawOutput {
			var it struct {
				Type    string          `json:"type"`
				Summary []textPart      `json:"summary"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(item, &it) == nil && it.Type == "reasoning" {
				b.WriteString(reasoningText(it.Summary, it.Content))
			}
		}
		return b.String()
	}
	if r.Type == "assistant" && r.Reasoning != nil {
		return r.Reasoning.Text
	}
	if r.Role == "assistant" {
		return r.ReasoningContent
	}
	return ""
}

// backendBlocks turns a call the server ran into a tool_use and its result.
// Both halves are on the one row, because the backend executed the call and
// fed its output to the model before the client saw either.
func backendBlocks(c backendCall, raw json.RawMessage) []transcript.Block {
	name := c.ToolType
	if c.ToolType == "x_search" && c.Name != "" {
		name = c.Name
	}
	status := transcript.StatusOK
	if c.Status == "failed" {
		status = transcript.StatusError
	}
	var out []string
	if c.Action != nil {
		for _, src := range c.Action.Sources {
			out = append(out, src.URL)
		}
	}
	for _, o := range c.Outputs {
		if o.Type == "logs" {
			out = append(out, o.Logs)
		}
	}
	return []transcript.Block{
		{Kind: transcript.BlockToolUse, ToolID: c.ID, Name: name, Input: raw},
		{Kind: transcript.BlockToolResult, ToolID: c.ID, Name: name, Text: strings.Join(out, "\n"), Status: status},
	}
}

// textBlocks is one block of the kind holding text, or none for no text.
func textBlocks(kind transcript.BlockKind, text string) []transcript.Block {
	if text == "" {
		return nil
	}
	return []transcript.Block{{Kind: kind, Text: text}}
}

func arguments(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

// The rows this codec writes, in grok's v1 field order.

type systemItem struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type userItem struct {
	Type    string     `json:"type"`
	Content []textPart `json:"content"`
	// SyntheticReason marks context the runtime injected, so grok's turn
	// walkers do not count it as a prompt (SyntheticReason::starts_prompt_turn).
	SyntheticReason string `json:"synthetic_reason,omitempty"`
	PromptIndex     *int   `json:"prompt_index,omitempty"`
}

type assistantItem struct {
	Type      string         `json:"type"`
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type toolResultItem struct {
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// encodeChat is the chat_history row for a new entry the model is given. A
// prompt is wrapped in <user_query> as grok's user_query does
// (session/user_message.rs) and carries its prompt index; a user entry that
// is only model context is a system reminder. Reasoning is not written: grok
// sends reasoning items back to the API, and one this codec made up is not
// the model's.
func (p *plan) encodeChat(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	var rows []any
	switch e.Role {
	case transcript.RoleSystem:
		rows = append(rows, systemItem{Type: "system", Content: e.Text()})
	case transcript.RoleUser:
		if e.Text() == "" {
			return nil, nil
		}
		row := userItem{Type: "user"}
		if n, ok := p.prompt[e.ID]; ok {
			row.Content = []textPart{{Type: "text", Text: "<user_query>\n" + e.Text() + "\n</user_query>"}}
			row.PromptIndex = &n
		} else {
			row.Content = []textPart{{Type: "text", Text: e.Text()}}
			row.SyntheticReason = "system_reminder"
		}
		rows = append(rows, row)
	case transcript.RoleAssistant:
		row := assistantItem{Type: "assistant", Content: e.Text()}
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolUse {
				continue
			}
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			row.ToolCalls = append(row.ToolCalls, chatToolCall{ID: b.ToolID, Name: b.Name, Arguments: args})
		}
		if row.Content == "" && row.ToolCalls == nil {
			return nil, nil
		}
		rows = append(rows, row)
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolResult {
				rows = append(rows, toolResultItem{Type: "tool_result", ToolCallID: b.ToolID, Content: b.Text})
			}
		}
	}
	return joinRows(rows)
}

// joinRows marshals rows as consecutive lines of one written row, or nothing
// for no rows.
func joinRows(rows []any) (json.RawMessage, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	var b strings.Builder
	for i, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(line)
	}
	return json.RawMessage(b.String()), nil
}
