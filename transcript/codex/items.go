package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/inference-sh/agentprotocol/transcript"
)

// item is a response_item payload, one of the ResponseItem variants in
// protocol/src/models.rs. The fields are the union of the variants this
// codec reads; arguments is a JSON string on function_call and a JSON value
// on tool_search_call.
type item struct {
	Type          string          `json:"type"`
	ID            string          `json:"id,omitempty"`
	Role          string          `json:"role,omitempty"`
	Content       json.RawMessage `json:"content,omitempty"`
	Name          string          `json:"name,omitempty"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	CallID        string          `json:"call_id,omitempty"`
	Output        json.RawMessage `json:"output,omitempty"`
	Summary       []part          `json:"summary,omitempty"`
	Input         string          `json:"input,omitempty"`
	Action        json.RawMessage `json:"action,omitempty"`
	Status        string          `json:"status,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	RevisedPrompt string          `json:"revised_prompt,omitempty"`
	Author        string          `json:"author,omitempty"`
	Recipient     string          `json:"recipient,omitempty"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// The shapes encode writes.
type (
	message struct {
		Type    string `json:"type"`
		ID      string `json:"id,omitempty"`
		Role    string `json:"role"`
		Content []part `json:"content"`
	}
	reasoningItem struct {
		Type    string `json:"type"`
		ID      string `json:"id,omitempty"`
		Summary []part `json:"summary"`
	}
	functionCall struct {
		Type      string `json:"type"`
		ID        string `json:"id,omitempty"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		CallID    string `json:"call_id"`
	}
	functionCallOutput struct {
		Type   string          `json:"type"`
		ID     string          `json:"id,omitempty"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
)

// itemKind is what history replay needs to know about an item beyond its
// entry: whether it opens a user turn a rollback counts
// (context_manager/history.rs, is_user_turn_boundary), and whether a
// rollback trims it along with the turn after it when it sits just before
// that turn (history.rs, trim_pre_turn_context_updates).
type itemKind struct {
	turn     bool
	preamble bool
}

// decodeItem turns a response item into an entry. ok is false for an item
// that is no message: a request control, or a variant this codec does not
// know.
func decodeItem(payload json.RawMessage) (transcript.Entry, itemKind, bool, error) {
	var it item
	if err := json.Unmarshal(payload, &it); err != nil {
		return transcript.Entry{}, itemKind{}, false, err
	}
	e := transcript.Entry{ID: it.ID}
	var kind itemKind
	switch it.Type {
	case "message":
		texts, err := contentTexts(it.Content)
		if err != nil {
			return transcript.Entry{}, kind, false, fmt.Errorf("item %s: content: %w", it.ID, err)
		}
		for _, t := range texts {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: t})
		}
		switch it.Role {
		case "user":
			e.Role = transcript.RoleUser
			switch {
			case hookPrompt(texts):
				// A hook's prompt, which the UI shows as the hook's and a
				// rollback does not count as a turn.
				kind.preamble = true
			case contextualUser(texts):
				// Context Codex injects under the user role: AGENTS.md,
				// the environment, skill bodies, notifications. The person
				// never typed it and the UI does not show it.
				e.Audience = transcript.AudienceModel
				kind.preamble = true
			case len(texts) > 0 && strings.HasPrefix(texts[0], summaryPrefix+"\n"):
				// A compaction summary standing in for retired history.
				e.Audience = transcript.AudienceModel
				kind.turn = true
			default:
				kind.turn = true
			}
		case "assistant":
			e.Role = transcript.RoleAssistant
			kind.turn = interAgentInstruction(texts)
		case "developer":
			e.Role = transcript.RoleSystem
			e.Audience = transcript.AudienceModel
			kind.preamble = contextualDeveloper(texts)
		case "system":
			// Codex never sends a raw system message and never shows one
			// (context_manager/history.rs, is_api_message).
			e.Role = transcript.RoleSystem
			e.Audience = transcript.AudienceNone
		default:
			return transcript.Entry{}, kind, false, fmt.Errorf("item %s: role %q", it.ID, it.Role)
		}
	case "agent_message":
		// A message from another agent, which the model reads as input and
		// which opens a turn like a user message.
		texts, err := contentTexts(it.Content)
		if err != nil {
			return transcript.Entry{}, kind, false, fmt.Errorf("item %s: content: %w", it.ID, err)
		}
		e.Role = transcript.RoleUser
		for _, t := range texts {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: t})
		}
		kind.turn = true
	case "reasoning":
		e.Role = transcript.RoleAssistant
		for _, p := range it.Summary {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
		}
	case "function_call":
		var args string
		if err := json.Unmarshal(it.Arguments, &args); err != nil {
			return transcript.Entry{}, kind, false, fmt.Errorf("item %s: arguments: %w", it.ID, err)
		}
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: it.CallID, Name: it.Name, Input: arguments(args)}}
	case "custom_tool_call":
		// A freeform tool such as apply_patch: its input is raw text.
		input, err := json.Marshal(it.Input)
		if err != nil {
			return transcript.Entry{}, kind, false, err
		}
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: it.CallID, Name: it.Name, Input: input}}
	case "local_shell_call":
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: it.CallID, Name: "local_shell", Input: it.Action}}
	case "tool_search_call":
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: it.CallID, Name: "tool_search", Input: it.Arguments}}
	case "web_search_call":
		// Run by the provider; no output item follows it.
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: it.ID, Name: "web_search", Input: it.Action}}
	case "image_generation_call":
		// Run by the provider. The result is the image itself, which a
		// text block cannot hold; the prompt it was drawn from stands in.
		input, err := json.Marshal(map[string]string{"prompt": it.RevisedPrompt})
		if err != nil {
			return transcript.Entry{}, kind, false, err
		}
		e.Role = transcript.RoleAssistant
		e.Content = []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: it.ID, Name: "image_generation", Input: input}}
	case "function_call_output", "custom_tool_call_output":
		text, err := outputText(it.Output)
		if err != nil {
			return transcript.Entry{}, kind, false, fmt.Errorf("item %s: output: %w", it.ID, err)
		}
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: it.CallID, Name: it.Name, Text: text, Status: transcript.StatusOK}}
	case "tool_search_output":
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: it.CallID, Name: "tool_search", Text: string(it.Tools), Status: toolStatus(it.Status)}}
	case "compaction", "compaction_summary", "context_compaction":
		// The provider's encrypted stand-in for compacted history. The
		// model is given it; nobody can read it.
		e.Role = transcript.RoleSystem
		e.Audience = transcript.AudienceModel
	default:
		// additional_tools and configuration_update are request controls,
		// compaction_trigger is never kept, and other is a variant this
		// Codex does not know either.
		return transcript.Entry{}, kind, false, nil
	}
	return e, kind, true, nil
}

// communication is an inter_agent_communication row's payload
// (protocol.rs, InterAgentCommunication). The model is given it as an
// agent_message item (to_model_input_item).
type communication struct {
	ID          string `json:"id,omitempty"`
	Author      string `json:"author"`
	Recipient   string `json:"recipient"`
	Content     string `json:"content"`
	TriggerTurn bool   `json:"trigger_turn"`
}

func decodeCommunication(payload json.RawMessage) (transcript.Entry, bool, error) {
	var c communication
	if err := json.Unmarshal(payload, &c); err != nil {
		return transcript.Entry{}, false, err
	}
	e := transcript.Entry{ID: c.ID, Role: transcript.RoleUser}
	if c.Content != "" {
		e.Content = []transcript.Block{{Kind: transcript.BlockText, Text: c.Content}}
	}
	return e, true, nil
}

func toolStatus(s string) transcript.Status {
	if s == "failed" || s == "incomplete" {
		return transcript.StatusError
	}
	return transcript.StatusOK
}

// contentTexts reads the text parts of a message's content. Image and
// audio parts carry no text and are skipped; an agent_message's encrypted
// parts are too.
func contentTexts(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text":
			out = append(out, p.Text)
		}
	}
	return out, nil
}

// arguments turns the JSON-encoded string Codex stores into the object it
// encodes, or keeps the string when it is not valid JSON.
func arguments(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

// outputText reads a tool output: a string, or an array of content parts
// (models.rs, FunctionCallOutputPayload).
func outputText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// summaryPrefix opens the user message a compaction leaves in place of the
// history it retired (prompts/templates/compact/summary_prefix.md).
const summaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"

// userFragments are the start and end markers of the context Codex injects
// as user messages (core/src/context/contextual_user_message.rs,
// CONTEXTUAL_USER_FRAGMENT_MATCHERS). <user_instructions> is how versions
// before AGENTS.md headings wrapped the same instructions.
var userFragments = [][2]string{
	{"# AGENTS.md instructions", "</INSTRUCTIONS>"},
	{"<user_instructions>", "</user_instructions>"},
	{"<environment_context>", "</environment_context>"},
	{"<skill>", "</skill>"},
	{"<user_shell_command>", "</user_shell_command>"},
	{"<turn_aborted>", "</turn_aborted>"},
	{"<subagent_notification>", "</subagent_notification>"},
	{"<agent_message_board_notification>", "</agent_message_board_notification>"},
	{"<recommended_plugins>", "</recommended_plugins>"},
	{"<goal_context>", "</goal_context>"},
	{"<codex_internal_context", "</codex_internal_context>"},
	{"<hook_prompt", "</hook_prompt>"},
}

// hookPrompt reports whether a user message is a hook's prompt: every part
// a <hook_prompt> element (contextual_user_message.rs,
// parse_visible_hook_prompt_message).
func hookPrompt(texts []string) bool {
	for _, t := range texts {
		if !marked(t, "<hook_prompt", "</hook_prompt>") {
			return false
		}
	}
	return len(texts) > 0
}

// userWarnings open the warnings older versions recorded as user messages.
var userWarnings = []string{
	"Warning: The maximum number of unified exec processes you can keep open is",
	"Warning: apply_patch was requested via ",
	"Warning: Your account was flagged for potentially high-risk cyber activity",
}

// contextualUser reports whether a user message is context Codex injected
// rather than something the person said: any of its parts is a fragment.
func contextualUser(texts []string) bool {
	for _, t := range texts {
		for _, m := range userFragments {
			if marked(t, m[0], m[1]) {
				return true
			}
		}
		trimmed := strings.TrimSpace(t)
		for _, w := range userWarnings {
			if strings.HasPrefix(trimmed, w) {
				return true
			}
		}
		// Hook-supplied context: <external_KEY>...</external_KEY>
		// (context-fragments/src/additional_context.rs).
		if rest, ok := strings.CutPrefix(trimmed, "<external_"); ok {
			if key, body, ok := strings.Cut(rest, ">"); ok && strings.HasSuffix(body, "</external_"+key+">") {
				return true
			}
		}
	}
	return false
}

// developerFragments open the developer messages a rollback trims along
// with the turn after them (core/src/event_mapping.rs,
// CONTEXTUAL_DEVELOPER_PREFIXES).
var developerFragments = []string{
	"<permissions instructions>",
	"<model_switch>",
	"<managed_developer_instructions>",
	"<persistent_mode>",
	"<apps_instructions>",
	"<collaboration_mode>",
	"<multi_agent_role>",
	"<multi_agent_mode>",
	"<environments_instructions>",
	"<git_attribution>",
	"<plugins_instructions>",
	"<realtime_conversation>",
	"<skills_instructions>",
	"<tools>",
	"<personality_spec>",
	"<token_budget>",
	"<context_window>",
	"<context_window_guidance>",
	"<rollout_budget>",
}

func contextualDeveloper(texts []string) bool {
	for _, t := range texts {
		trimmed := strings.TrimLeft(t, " \t\r\n")
		for _, p := range developerFragments {
			if len(trimmed) >= len(p) && strings.EqualFold(trimmed[:len(p)], p) {
				return true
			}
		}
	}
	return false
}

// marked reports whether text is wrapped in start and end, ignoring
// surrounding whitespace and ASCII case (context-fragments/src/fragment.rs,
// matches_marked_text).
func marked(text, start, end string) bool {
	t := strings.TrimSpace(text)
	return len(t) >= len(start)+len(end) &&
		strings.EqualFold(t[:len(start)], start) &&
		strings.EqualFold(t[len(t)-len(end):], end)
}

// interAgentInstruction reports whether an assistant message is another
// agent's instruction serialized as JSON, which opens a turn
// (protocol.rs, InterAgentCommunication::is_message_content).
func interAgentInstruction(texts []string) bool {
	if len(texts) != 1 {
		return false
	}
	var c struct {
		Author      *string `json:"author"`
		Recipient   *string `json:"recipient"`
		Content     *string `json:"content"`
		TriggerTurn *bool   `json:"trigger_turn"`
	}
	if json.Unmarshal([]byte(texts[0]), &c) != nil {
		return false
	}
	return c.Author != nil && c.Recipient != nil && c.Content != nil && c.TriggerTurn != nil
}
