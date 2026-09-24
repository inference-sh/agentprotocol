// Package claudecode drives Claude Code over its own stream-json control
// protocol: the wire protocol the official Claude Agent SDK speaks to the
// `claude` binary.
//
// The CLI is launched with
//
//	--output-format stream-json --verbose --input-format stream-json
//
// and from then on both directions carry one JSON object per line. The
// client writes user messages and control requests; the CLI writes
// conversation messages (system, assistant, user, result, stream_event),
// control responses to our requests, and control requests of its own: chiefly
// can_use_tool, which is how a permission prompt reaches a host that passed
// --permission-prompt-tool stdio.
//
// The shapes here were taken from the SDK's own source (sdk.mjs / sdk.d.ts,
// @anthropic-ai/claude-agent-sdk 0.3.274) and checked against claude 2.1.281.
// Only the fields this package acts on are typed; every message keeps its raw
// bytes, and unknown fields and unknown message types are tolerated rather
// than rejected, because the CLI adds both often.
package claudecode

import "encoding/json"

// Message types on the CLI's stdout (and user messages on its stdin).
const (
	TypeSystem          = "system"
	TypeAssistant       = "assistant"
	TypeUser            = "user"
	TypeResult          = "result"
	TypeStreamEvent     = "stream_event"
	TypeControlRequest  = "control_request"
	TypeControlResponse = "control_response"
	TypeControlCancel   = "control_cancel_request"
	TypeKeepAlive       = "keep_alive"
)

// System message subtypes this package names.
const (
	SystemInit            = "init"
	SystemStatus          = "status"
	SystemCompactBoundary = "compact_boundary"
)

// Control request subtypes. The first group is sent by the host, the second
// by the CLI.
const (
	SubtypeInitialize        = "initialize"
	SubtypeInterrupt         = "interrupt"
	SubtypeSetPermissionMode = "set_permission_mode"
	SubtypeSetModel          = "set_model"

	SubtypeCanUseTool   = "can_use_tool"
	SubtypeHookCallback = "hook_callback"
)

// Control response subtypes.
const (
	ResponseSuccess = "success"
	ResponseError   = "error"
)

// Permission modes, as the CLI names them. BypassPermissions is listed so a
// caller can recognise and refuse it; this package never selects it.
const (
	PermissionModeDefault           = "default"
	PermissionModeAcceptEdits       = "acceptEdits"
	PermissionModePlan              = "plan"
	PermissionModeDontAsk           = "dontAsk"
	PermissionModeBypassPermissions = "bypassPermissions"
)

// Result subtypes.
const (
	ResultSuccess              = "success"
	ResultErrorDuringExecution = "error_during_execution"
)

// Message is one line from the CLI. Type is always set; Raw holds the whole
// line so a caller can decode fields this package does not type.
type Message struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Raw     json.RawMessage
}

// Decode unmarshals the raw line into v.
func (m Message) Decode(v any) error { return json.Unmarshal(m.Raw, v) }

// SystemMessage is a system line. Init carries the session ID, model and tool
// list; other subtypes (status, compact_boundary, hook_*, task_*) share the
// envelope.
type SystemMessage struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	SessionID      string          `json:"session_id"`
	UUID           string          `json:"uuid,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	Model          string          `json:"model,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
	Tools          []string        `json:"tools,omitempty"`
	Version        string          `json:"claude_code_version,omitempty"`
	Status         string          `json:"status,omitempty"`
	CompactMeta    *CompactMeta    `json:"compact_metadata,omitempty"`
	MCPServers     []MCPServerInfo `json:"mcp_servers,omitempty"`
}

// CompactMeta describes a compaction.
type CompactMeta struct {
	Trigger    string `json:"trigger"`
	PreTokens  int    `json:"pre_tokens"`
	PostTokens int    `json:"post_tokens,omitempty"`
}

// MCPServerInfo is one MCP server as system/init reports it.
type MCPServerInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// APIMessage is the Anthropic Messages API message an assistant line wraps.
type APIMessage struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Model      string         `json:"model,omitempty"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason,omitempty"`
	Usage      *Usage         `json:"usage,omitempty"`
}

// ContentBlock is one block of message content. Which fields are set depends
// on Type: text, thinking, tool_use, tool_result, and others this package
// passes through untouched.
type ContentBlock struct {
	Type string `json:"type"`

	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ResultText flattens a tool_result's content, which is either a string or a
// list of blocks.
func (b ContentBlock) ResultText() string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var blocks []ContentBlock
	if json.Unmarshal(b.Content, &blocks) != nil {
		return ""
	}
	out := ""
	for _, c := range blocks {
		if c.Type == "text" {
			out += c.Text
		}
	}
	return out
}

// Usage is token accounting, as the Messages API and the result line report it.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// AssistantMessage is an assistant line. With --include-partial-messages the
// same content has already streamed as stream_event lines; the assistant line
// is the settled copy, usually one content block per line.
type AssistantMessage struct {
	Type            string     `json:"type"`
	Message         APIMessage `json:"message"`
	ParentToolUseID *string    `json:"parent_tool_use_id"`
	SessionID       string     `json:"session_id"`
	UUID            string     `json:"uuid,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// UserMessage is a user line: a prompt we sent (on stdin), or a tool result
// and interrupt marker the CLI echoes (on stdout).
type UserMessage struct {
	Type            string          `json:"type"`
	SessionID       string          `json:"session_id"`
	Message         UserContent     `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	UUID            string          `json:"uuid,omitempty"`
	ToolUseResult   json.RawMessage `json:"tool_use_result,omitempty"`
}

// UserContent is the Messages API user message. Content is a string or a list
// of blocks; Blocks decodes either.
type UserContent struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Blocks returns the content as blocks, wrapping a bare string as one text
// block.
func (u UserContent) Blocks() []ContentBlock {
	var s string
	if json.Unmarshal(u.Content, &s) == nil {
		return []ContentBlock{{Type: "text", Text: s}}
	}
	var blocks []ContentBlock
	_ = json.Unmarshal(u.Content, &blocks)
	return blocks
}

// NewUserMessage builds the line that sends a text prompt, in the shape the
// SDK writes it.
func NewUserMessage(text string) UserMessage {
	content, _ := json.Marshal([]ContentBlock{{Type: "text", Text: text}})
	return UserMessage{
		Type:    TypeUser,
		Message: UserContent{Role: "user", Content: content},
	}
}

// ResultMessage ends a turn. Subtype is "success" or one of the error_*
// subtypes; an interrupted turn ends with error_during_execution and a
// TerminalReason of aborted_tools or aborted_streaming.
type ResultMessage struct {
	Type              string             `json:"type"`
	Subtype           string             `json:"subtype"`
	IsError           bool               `json:"is_error"`
	Result            string             `json:"result,omitempty"`
	StopReason        string             `json:"stop_reason,omitempty"`
	TerminalReason    string             `json:"terminal_reason,omitempty"`
	SessionID         string             `json:"session_id"`
	NumTurns          int                `json:"num_turns"`
	DurationMS        int64              `json:"duration_ms"`
	TotalCostUSD      float64            `json:"total_cost_usd"`
	Usage             Usage              `json:"usage"`
	Errors            []string           `json:"errors,omitempty"`
	PermissionDenials []PermissionDenial `json:"permission_denials,omitempty"`
	QueuedTurnCount   int                `json:"queued_turn_count,omitempty"`
}

// Interrupted reports whether the turn ended because the host interrupted it.
func (r ResultMessage) Interrupted() bool {
	return r.TerminalReason == "aborted_tools" || r.TerminalReason == "aborted_streaming"
}

// PermissionDenial is a tool call the turn was refused.
type PermissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
}

// StreamEvent is a stream_event line, emitted with --include-partial-messages:
// one raw Messages API streaming event.
type StreamEvent struct {
	Type            string    `json:"type"`
	Event           StreamAPI `json:"event"`
	ParentToolUseID *string   `json:"parent_tool_use_id"`
	SessionID       string    `json:"session_id"`
	UUID            string    `json:"uuid,omitempty"`
}

// StreamAPI is a Messages API streaming event (message_start,
// content_block_start, content_block_delta, content_block_stop,
// message_delta, message_stop).
type StreamAPI struct {
	Type         string        `json:"type"`
	Index        int           `json:"index,omitempty"`
	Delta        StreamDelta   `json:"delta,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
}

// StreamDelta is the delta of a content_block_delta or message_delta.
type StreamDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

// --- control protocol ---

// ControlRequest is the envelope for a request in either direction. Request
// holds the subtype-specific body.
type ControlRequest struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// Subtype reads the subtype of the request body.
func (c ControlRequest) Subtype() string {
	var s struct {
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(c.Request, &s)
	return s.Subtype
}

// ControlResponse is the envelope for the single reply to a control request.
type ControlResponse struct {
	Type     string              `json:"type"`
	Response ControlResponseBody `json:"response"`
}

// ControlResponseBody is a success or error reply. Response carries the
// success payload; Error the failure text.
type ControlResponseBody struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// ControlCancel withdraws a control request the sender no longer wants
// answered. The CLI sends one for a pending can_use_tool when the turn it
// belongs to is interrupted.
type ControlCancel struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
}

// InitializeRequest is the first control request. It registers hooks and
// carries prompt settings the CLI would otherwise take from argv.
type InitializeRequest struct {
	Subtype            string         `json:"subtype"`
	Hooks              map[string]any `json:"hooks,omitempty"`
	SystemPrompt       []string       `json:"systemPrompt,omitempty"`
	AppendSystemPrompt string         `json:"appendSystemPrompt,omitempty"`
}

// InitializeResponse is the part of the initialize reply this package reads.
type InitializeResponse struct {
	Models                []ModelInfo `json:"models,omitempty"`
	OutputStyle           string      `json:"output_style,omitempty"`
	CurrentPermissionMode string      `json:"current_permission_mode,omitempty"`
	PID                   int         `json:"pid,omitempty"`
	Account               AccountInfo `json:"account"`

	// PendingPermissionRequests lists can_use_tool requests the CLI issued
	// and has not had answered, so a host joining late learns about them.
	PendingPermissionRequests []ControlRequest `json:"pending_permission_requests,omitempty"`
}

// ModelInfo is one selectable model.
type ModelInfo struct {
	Value       string `json:"value"`
	DisplayName string `json:"displayName,omitempty"`
}

// AccountInfo says how the CLI is authenticated, without the credential.
type AccountInfo struct {
	TokenSource  string `json:"tokenSource,omitempty"`
	APIKeySource string `json:"apiKeySource,omitempty"`
	APIProvider  string `json:"apiProvider,omitempty"`
}

// PermissionRequest is the can_use_tool body: the CLI asking whether a tool
// may run.
type PermissionRequest struct {
	Subtype        string             `json:"subtype"`
	ToolName       string             `json:"tool_name"`
	DisplayName    string             `json:"display_name,omitempty"`
	Title          string             `json:"title,omitempty"`
	Description    string             `json:"description,omitempty"`
	Input          json.RawMessage    `json:"input"`
	ToolUseID      string             `json:"tool_use_id"`
	Suggestions    []PermissionUpdate `json:"permission_suggestions,omitempty"`
	BlockedPath    string             `json:"blocked_path,omitempty"`
	DecisionReason string             `json:"decision_reason,omitempty"`
	AgentID        string             `json:"agent_id,omitempty"`

	// SuppressAlwaysAllow is set when a persistent "don't ask again" answer
	// would grant more than this request asked for.
	SuppressAlwaysAllow bool `json:"suppress_always_allow_rule,omitempty"`
}

// PermissionUpdate is one change to the permission rules, sent back with an
// allow to make it stick. The CLI suggests these in can_use_tool.
type PermissionUpdate struct {
	Type        string           `json:"type"`
	Rules       []PermissionRule `json:"rules,omitempty"`
	Behavior    string           `json:"behavior,omitempty"`
	Destination string           `json:"destination"`
	Mode        string           `json:"mode,omitempty"`
	Directories []string         `json:"directories,omitempty"`
}

// PermissionRule names a tool and optionally narrows it.
type PermissionRule struct {
	ToolName    string `json:"toolName"`
	RuleContent string `json:"ruleContent,omitempty"`
}

// Permission update destinations.
const (
	DestinationSession       = "session"
	DestinationLocalSettings = "localSettings"
)

// PermissionResult answers can_use_tool.
type PermissionResult struct {
	Behavior           string             `json:"behavior"`
	UpdatedInput       json.RawMessage    `json:"updatedInput,omitempty"`
	UpdatedPermissions []PermissionUpdate `json:"updatedPermissions,omitempty"`
	Message            string             `json:"message,omitempty"`
	Interrupt          bool               `json:"interrupt,omitempty"`
	ToolUseID          string             `json:"toolUseID,omitempty"`
}

// Permission behaviors.
const (
	BehaviorAllow = "allow"
	BehaviorDeny  = "deny"
)

// AllowResult approves a tool call with its input unchanged.
func AllowResult(input json.RawMessage) PermissionResult {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return PermissionResult{Behavior: BehaviorAllow, UpdatedInput: input}
}

// DenyResult refuses a tool call. The message is shown to the model.
func DenyResult(message string) PermissionResult {
	if message == "" {
		message = "The user denied this action."
	}
	return PermissionResult{Behavior: BehaviorDeny, Message: message}
}

// HookCallbackRequest is the hook_callback body: the CLI running a hook the
// host registered at initialize.
type HookCallbackRequest struct {
	Subtype    string          `json:"subtype"`
	CallbackID string          `json:"callback_id"`
	Input      json.RawMessage `json:"input"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
}
