package model

// Subagents: an agent that hands part of its work to another agent (Claude
// Code's Agent tool, codex's spawned agent threads). The parent's tool call
// starts the subagent and returns its result; in between, the subagent runs
// a conversation of its own. These events report that conversation's life:
// that it started, how it is doing, how it ended and what it used.
//
// What the subagent itself says and does (its text, its tool calls) is not
// part of the session's conversation. A driver drops it unless its caller
// asks for it, and then sends it as the usual content and tool events with
// SubagentID set, so a consumer that does not know about subagents never
// mistakes a subagent's words for the agent's. An approval a subagent needs
// is always sent, since someone has to answer it, with SubagentID set.

const (
	AgentEventSubagentStarted   AgentEventType = "subagent.started"
	AgentEventSubagentUpdated   AgentEventType = "subagent.updated"
	AgentEventSubagentCompleted AgentEventType = "subagent.completed"
)

// SubagentStatus is where a subagent is.
type SubagentStatus string

const (
	SubagentRunning     SubagentStatus = "running"
	SubagentWaiting     SubagentStatus = "waiting" // paused, or waiting for input
	SubagentCompleted   SubagentStatus = "completed"
	SubagentFailed      SubagentStatus = "failed"
	SubagentCancelled   SubagentStatus = "cancelled" // stopped by the agent or a person
	SubagentInterrupted SubagentStatus = "interrupted"
)

// Ended reports whether the status is final.
func (s SubagentStatus) Ended() bool {
	switch s {
	case SubagentCompleted, SubagentFailed, SubagentCancelled, SubagentInterrupted:
		return true
	}
	return false
}

// SubagentUsage is what a subagent used so far, as its agent counts it.
type SubagentUsage struct {
	TotalTokens int   `json:"total_tokens"`
	ToolUses    int   `json:"tool_uses,omitempty"`
	DurationMs  int64 `json:"duration_ms,omitempty"`
}

// SubagentStartedPayload is a subagent starting.
type SubagentStartedPayload struct {
	// SubagentID identifies the subagent for its later events: Claude Code's
	// task id, codex's thread id for the spawned agent.
	SubagentID string `json:"subagent_id"`
	// ParentToolInvocationID is the tool call of the agent that started it,
	// when the agent links them; empty for one it started on its own.
	ParentToolInvocationID string `json:"parent_tool_invocation_id,omitempty"`
	// ParentSubagentID is the subagent that started this one, for a
	// subagent of a subagent.
	ParentSubagentID string `json:"parent_subagent_id,omitempty"`
	Description      string `json:"description,omitempty"`
	// Kind is the agent's name for what kind of subagent this is (Claude
	// Code's subagent_type, codex's agent role).
	Kind   string `json:"kind,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Model  string `json:"model,omitempty"`
	// Background is true for a subagent the agent left running while its
	// own turn went on.
	Background bool `json:"background,omitempty"`
}

// SubagentUpdatedPayload is a change in a running subagent. Fields left
// empty did not change.
type SubagentUpdatedPayload struct {
	SubagentID   string         `json:"subagent_id"`
	Status       SubagentStatus `json:"status,omitempty"`
	Description  string         `json:"description,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	LastToolName string         `json:"last_tool_name,omitempty"`
	Usage        *SubagentUsage `json:"usage,omitempty"`
	Error        string         `json:"error,omitempty"`
}

// SubagentCompletedPayload is a subagent ending.
type SubagentCompletedPayload struct {
	SubagentID string         `json:"subagent_id"`
	Status     SubagentStatus `json:"status"`
	Summary    string         `json:"summary,omitempty"`
	Usage      *SubagentUsage `json:"usage,omitempty"`
	Error      string         `json:"error,omitempty"`
}
