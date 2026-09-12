package agentprotocol

import (
	"encoding/json"
	"time"
)

// AgentEventType identifies what happened in an agent run.
// These are the backbone protocol events — every consumer (A2A, SDK, frontend)
// projects from this set.
type AgentEventType string

const (
	// Run lifecycle
	AgentEventRunStarted      AgentEventType = "run.started"
	AgentEventRunStateChanged AgentEventType = "run.state_changed"

	// Turn lifecycle
	AgentEventTurnStarted   AgentEventType = "turn.started"
	AgentEventTurnCompleted AgentEventType = "turn.completed"

	// Content streaming — structural wrapper; high-frequency token deltas
	// still flow via the existing DeltaEvent channel for efficiency.
	AgentEventContentDelta AgentEventType = "content.delta"

	// Tool lifecycle
	AgentEventToolStarted   AgentEventType = "tool.started"
	AgentEventToolCompleted AgentEventType = "tool.completed"

	// Approval flow
	AgentEventApprovalRequired AgentEventType = "approval.required"
	AgentEventApprovalResolved AgentEventType = "approval.resolved"

	// Hook lifecycle
	AgentEventHookExecuted AgentEventType = "hook.executed"

	// Usage
	AgentEventUsageUpdated AgentEventType = "usage.updated"

	// Context management
	AgentEventContextCompacted AgentEventType = "context.compacted"

	// Errors
	AgentEventError AgentEventType = "error"
)

// AgentEvent is the backbone protocol event for agent runs.
// Published to "runs:<runID>" and "chats:<chatID>" keys on the event bus.
type AgentEvent struct {
	ID        string          `json:"id"`
	Type      AgentEventType  `json:"type"`
	RunID     string          `json:"run_id"`
	ChatID    string          `json:"chat_id"`
	AgentID   string          `json:"agent_id,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// --- Typed payloads ---

type RunStartedPayload struct {
	AgentID        string `json:"agent_id"`
	AgentVersionID string `json:"agent_version_id,omitempty"`
	UserMessageID  string `json:"user_message_id,omitempty"`
}

type RunStateChangedPayload struct {
	FromState AgentRunState `json:"from_state"`
	ToState   AgentRunState `json:"to_state"`
	Error     string        `json:"error,omitempty"`
}

type TurnStartedPayload struct {
	TurnIndex int    `json:"turn_index"`
	Model     string `json:"model,omitempty"`
}

type TurnCompletedPayload struct {
	TurnIndex  int    `json:"turn_index"`
	ToolCount  int    `json:"tool_count"`
	HasOutput  bool   `json:"has_output"`
	StopReason string `json:"stop_reason,omitempty"`
}

type ContentDeltaPayload struct {
	Kind  ContentDeltaKind `json:"kind"`
	Delta string           `json:"delta"`
}

type ContentDeltaKind string

const (
	ContentDeltaText      ContentDeltaKind = "text"
	ContentDeltaReasoning ContentDeltaKind = "reasoning"
)

type ToolStartedPayload struct {
	ToolInvocationID string           `json:"tool_invocation_id"`
	ToolName         string           `json:"tool_name"`
	ToolType         ToolType         `json:"tool_type,omitempty"`
	DisplayName      string           `json:"display_name,omitempty"`
	Arguments        StringEncodedMap `json:"arguments,omitempty"`
}

type ToolCompletedPayload struct {
	ToolInvocationID string               `json:"tool_invocation_id"`
	ToolName         string               `json:"tool_name"`
	Status           ToolInvocationStatus `json:"status"`
	Result           string               `json:"result,omitempty"`
	DurationMs       int64                `json:"duration_ms,omitempty"`
}

type ApprovalRequiredPayload struct {
	ToolInvocationID string           `json:"tool_invocation_id"`
	ToolName         string           `json:"tool_name"`
	Arguments        StringEncodedMap `json:"arguments,omitempty"`
	Reason           InterruptReason  `json:"reason"`
}

type ApprovalResolvedPayload struct {
	ToolInvocationID string `json:"tool_invocation_id"`
	ToolName         string `json:"tool_name"`
	Decision         string `json:"decision"` // "allow", "deny"
	Reason           string `json:"reason,omitempty"`
}

type HookExecutedPayload struct {
	HookEvent  HookEvent    `json:"hook_event"`
	Decision   HookDecision `json:"decision"`
	Reason     string       `json:"reason,omitempty"`
	DurationMs int64        `json:"duration_ms,omitempty"`
}

type UsageUpdatedPayload struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}

type ContextCompactedPayload struct {
	BeforeTokens int `json:"before_tokens"`
	AfterTokens  int `json:"after_tokens"`
}

type ErrorPayload struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}
