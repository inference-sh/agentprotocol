package agentprotocol

import (
	"encoding/json"
)

// LifecycleHookConfig registers a handler for an agent lifecycle event.
// Stored on AgentVersion alongside Tools and Skills.
type LifecycleHookConfig struct {
	Event   HookEvent       `json:"event" yaml:"event"`
	Type    HookHandlerType `json:"type" yaml:"type"`
	Handler string          `json:"handler,omitempty" yaml:"handler,omitempty"`
	Async   bool            `json:"async,omitempty" yaml:"async,omitempty"`
	Timeout int             `json:"timeout,omitempty" yaml:"timeout,omitempty"` // seconds, 0 = default (30s for webhook, 300s for gate)

	// Gate-specific fields (type: "gate")
	DefaultResolution InterruptResolution `json:"default_resolution,omitempty" yaml:"default_resolution,omitempty"` // auto-resolve on timeout: "allow" (default) or "deny"
}

// LifecycleHookPayload is sent to hook handlers on lifecycle events.
type LifecycleHookPayload struct {
	Event     HookEvent `json:"event"`
	Timestamp string    `json:"timestamp"`

	AgentID   string `json:"agent_id"`
	ChatID    string `json:"chat_id"`
	RunID     string `json:"run_id,omitempty"`
	TurnCount int    `json:"turn_count"`

	Data json.RawMessage `json:"data,omitempty"`
}

// LifecycleHookResponse is returned by hook handlers.
// All fields are optional — an empty 200 response is equivalent to {decision: "allow"}.
type LifecycleHookResponse struct {
	Inject   *ContextInjection `json:"inject,omitempty"`
	Decision HookDecision      `json:"decision,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	Override json.RawMessage   `json:"override,omitempty"`
	System   string            `json:"system,omitempty"`
}

// ContextInjection adds ephemeral content to the agent's context window.
// Injections are stored as ChatMessages and filtered at context-build time.
type ContextInjection struct {
	Content  string `json:"content"`
	Role     string `json:"role,omitempty"`      // default "system"
	TTLTurns int    `json:"ttl_turns,omitempty"` // 0 = permanent
	DedupKey string `json:"dedup_key,omitempty"` // new injection with same key supersedes prior
}

// ContextInjectionMeta is stored on ChatMessage.InjectionMeta for injected messages.
type ContextInjectionMeta struct {
	InjectedAtTurn int    `json:"injected_at_turn"`
	TTLTurns       int    `json:"ttl_turns"`
	DedupKey       string `json:"dedup_key,omitempty"`
	Source         string `json:"source,omitempty"` // hook handler that produced this injection
}

// CompactionMeta is stored on ChatMessage for context compaction markers.
type CompactionMeta struct {
	CompactedUpToOrder int    `json:"compacted_up_to_order"`
	CompactedCount     int    `json:"compacted_count"`
	EstimatedTokens    int    `json:"estimated_tokens"`
	Method             string `json:"method,omitempty"` // "extractive" or "llm"
	Analysis           string `json:"analysis,omitempty"`
}

// CompactionConfig controls how auto-compaction runs.
type CompactionConfig struct {
	AppID      string  `json:"app_id,omitempty"`
	AppVersion *string `json:"app_version,omitempty"`
}

// ToolCallEventData is the typed payload for agent.tool_call events.
type ToolCallEventData struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ToolResultEventData is the typed payload for agent.tool_result events.
type ToolResultEventData struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
}

// ErrorEventData is the typed payload for agent.error events.
type ErrorEventData struct {
	Error string `json:"error"`
}
