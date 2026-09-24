// Package pirpc speaks pi's RPC mode: the JSONL protocol `pi --mode rpc`
// reads on stdin and writes on stdout.
//
// The definitions follow pi's own, as shipped in
// @earendil-works/pi-coding-agent 0.87.1: dist/modes/rpc/rpc-types.d.ts for
// commands, responses and extension UI records, dist/modes/rpc/rpc-mode.js
// for how they are handled, and docs/rpc.md, docs/rpc-commands.md,
// docs/json.md and docs/message-types.md for the event stream. Only the
// fields this module reads are typed; everything else stays in Raw.
//
// Three kinds of record arrive on stdout: a response to a command (type
// "response", carrying the command's id), a session event (every other type,
// no id), and an extension UI request. Records are split on LF only: pi's
// strings may contain U+2028 and U+2029, which are not boundaries.
package pirpc

import (
	"encoding/json"
	"strings"
)

// Command types this package sends (rpc-types.d.ts, RpcCommand).
const (
	CmdPrompt   = "prompt"
	CmdSteer    = "steer"
	CmdFollowUp = "follow_up"
	CmdAbort    = "abort"
	CmdGetState = "get_state"
	CmdCompact  = "compact"
)

// Streaming behaviours for a prompt sent while a run is active. pi rejects a
// prompt during a run that names neither.
const (
	StreamingSteer    = "steer"
	StreamingFollowUp = "followUp"
)

// Record types on stdout.
const (
	TypeResponse           = "response"
	TypeExtensionUIRequest = "extension_ui_request"

	EventAgentStart      = "agent_start"
	EventAgentEnd        = "agent_end"
	EventAgentSettled    = "agent_settled"
	EventTurnStart       = "turn_start"
	EventTurnEnd         = "turn_end"
	EventMessageStart    = "message_start"
	EventMessageUpdate   = "message_update"
	EventMessageEnd      = "message_end"
	EventToolStart       = "tool_execution_start"
	EventToolUpdate      = "tool_execution_update"
	EventToolEnd         = "tool_execution_end"
	EventQueueUpdate     = "queue_update"
	EventCompactionStart = "compaction_start"
	EventCompactionEnd   = "compaction_end"
	EventAutoRetryStart  = "auto_retry_start"
	EventAutoRetryEnd    = "auto_retry_end"
	EventExtensionError  = "extension_error"
)

// Record types on stdin that are not commands.
const TypeExtensionUIResponse = "extension_ui_response"

// Extension UI methods. The first four are dialogs that wait for an
// extension_ui_response; the rest are fire-and-forget.
const (
	UISelect  = "select"
	UIConfirm = "confirm"
	UIInput   = "input"
	UIEditor  = "editor"
	UINotify  = "notify"
)

// IsDialog reports whether pi waits for an answer to this UI method.
func IsDialog(method string) bool {
	switch method {
	case UISelect, UIConfirm, UIInput, UIEditor:
		return true
	}
	return false
}

// Stop reasons on an assistant message (message-types.md).
const (
	StopStop    = "stop"
	StopLength  = "length"
	StopToolUse = "toolUse"
	StopError   = "error"
	StopAborted = "aborted"
)

// Record is one line from stdout, decoded far enough to route it.
type Record struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

// Decode unmarshals the whole record into v.
func (r Record) Decode(v any) error { return json.Unmarshal(r.Raw, v) }

// Command is one stdin record. Fields beyond ID and Type are per command.
type Command struct {
	ID                 string `json:"id,omitempty"`
	Type               string `json:"type"`
	Message            string `json:"message,omitempty"`
	StreamingBehavior  string `json:"streamingBehavior,omitempty"`
	CustomInstructions string `json:"customInstructions,omitempty"`
}

// Response answers a command.
type Response struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// State is get_state's data (RpcSessionState).
type State struct {
	Model                 *Model `json:"model,omitempty"`
	ThinkingLevel         string `json:"thinkingLevel"`
	IsStreaming           bool   `json:"isStreaming"`
	IsCompacting          bool   `json:"isCompacting"`
	SessionFile           string `json:"sessionFile,omitempty"`
	SessionID             string `json:"sessionId"`
	SessionName           string `json:"sessionName,omitempty"`
	AutoCompactionEnabled bool   `json:"autoCompactionEnabled"`
	MessageCount          int    `json:"messageCount"`
	PendingMessageCount   int    `json:"pendingMessageCount"`
}

// Model is the part of pi's model object this package reads.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	API      string `json:"api"`
}

// CompactionResult is compact's data and compaction_end's result.
type CompactionResult struct {
	Summary              string `json:"summary"`
	FirstKeptEntryID     string `json:"firstKeptEntryId"`
	TokensBefore         int    `json:"tokensBefore"`
	EstimatedTokensAfter int    `json:"estimatedTokensAfter"`
}

// Usage is pi-ai's usage block.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Reasoning  int `json:"reasoning,omitempty"`
	Total      int `json:"totalTokens"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// Content is one content block of a message.
type Content struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Message is an AgentMessage. Content is a string for some user messages and
// a block list otherwise; Blocks normalises it.
type Message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content,omitempty"`
	Model        string          `json:"model,omitempty"`
	Provider     string          `json:"provider,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
	StopReason   string          `json:"stopReason,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
}

// Blocks returns the message's content as blocks.
func (m Message) Blocks() []Content {
	if len(m.Content) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []Content{{Type: "text", Text: s}}
	}
	var out []Content
	_ = json.Unmarshal(m.Content, &out)
	return out
}

// Text joins the message's text blocks.
func (m Message) Text() string {
	var b strings.Builder
	for _, c := range m.Blocks() {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// MessageEvent is message_start and message_end.
type MessageEvent struct {
	Type    string  `json:"type"`
	Message Message `json:"message"`
}

// MessageUpdate is message_update. The wire form carries deltas only; the
// cumulative partial message is stripped (json.md).
type MessageUpdate struct {
	Type                  string                `json:"type"`
	AssistantMessageEvent AssistantMessageEvent `json:"assistantMessageEvent"`
}

// AssistantMessageEvent is the nested streaming event.
type AssistantMessageEvent struct {
	Type         string   `json:"type"` // text_delta, thinking_delta, toolcall_start, ...
	ContentIndex int      `json:"contentIndex"`
	Delta        string   `json:"delta,omitempty"`
	Content      string   `json:"content,omitempty"`
	ID           string   `json:"id,omitempty"`
	ToolName     string   `json:"toolName,omitempty"`
	ToolCall     *Content `json:"toolCall,omitempty"`
}

// ToolEvent is tool_execution_start, _update and _end.
type ToolEvent struct {
	Type       string          `json:"type"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args,omitempty"`
	Result     *ToolResult     `json:"result,omitempty"`
	IsError    bool            `json:"isError"`
}

// ToolResult is a tool's result: content blocks plus tool-specific details.
type ToolResult struct {
	Content []Content `json:"content"`
}

// Text joins the result's text blocks.
func (r *ToolResult) Text() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// AgentEnd is agent_end.
type AgentEnd struct {
	Type      string    `json:"type"`
	Messages  []Message `json:"messages"`
	WillRetry bool      `json:"willRetry"`
}

// CompactionEnd is compaction_end.
type CompactionEnd struct {
	Type         string            `json:"type"`
	Reason       string            `json:"reason"` // manual, threshold, overflow
	Result       *CompactionResult `json:"result,omitempty"`
	Aborted      bool              `json:"aborted"`
	WillRetry    bool              `json:"willRetry"`
	ErrorMessage string            `json:"errorMessage,omitempty"`
}

// AutoRetry is auto_retry_start and auto_retry_end.
type AutoRetry struct {
	Type         string `json:"type"`
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	DelayMs      int    `json:"delayMs"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Success      bool   `json:"success"`
	FinalError   string `json:"finalError,omitempty"`
}

// ExtensionError is extension_error.
type ExtensionError struct {
	ExtensionPath string `json:"extensionPath"`
	Event         string `json:"event"`
	Error         string `json:"error"`
}

// UIRequest is extension_ui_request (RpcExtensionUIRequest).
type UIRequest struct {
	ID          string   `json:"id"`
	Method      string   `json:"method"`
	Title       string   `json:"title,omitempty"`
	Message     string   `json:"message,omitempty"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Prefill     string   `json:"prefill,omitempty"`
	Timeout     int      `json:"timeout,omitempty"` // milliseconds; pi answers itself when it expires
	NotifyType  string   `json:"notifyType,omitempty"`
}

// UIResponse is extension_ui_response. Exactly one of Value, Confirmed or
// Cancelled is meaningful; the constructors below build each form.
type UIResponse struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	Value     *string `json:"value,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
	Cancelled bool    `json:"cancelled,omitempty"`
}

// UIValue answers select, input or editor.
func UIValue(id, v string) UIResponse {
	return UIResponse{Type: TypeExtensionUIResponse, ID: id, Value: &v}
}

// UIConfirmed answers confirm.
func UIConfirmed(id string, ok bool) UIResponse {
	return UIResponse{Type: TypeExtensionUIResponse, ID: id, Confirmed: &ok}
}

// UICancelled dismisses any dialog.
func UICancelled(id string) UIResponse {
	return UIResponse{Type: TypeExtensionUIResponse, ID: id, Cancelled: true}
}
