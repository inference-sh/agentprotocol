package acp

import (
	"encoding/json"

	ap "github.com/inference-sh/agentprotocol"
)

// Session update kinds. Agents do not agree on these strings, so the mapper
// accepts the variants observed across Claude Code, Codex, Gemini CLI, Cursor
// and Grok rather than insisting on one spelling.
const (
	UpdateKindUserMessageChunk  = "user_message_chunk"
	UpdateKindAgentMessageChunk = "agent_message_chunk"
	UpdateKindContentChunk      = "content_chunk"
	UpdateKindAgentThoughtChunk = "agent_thought_chunk"
	UpdateKindToolCall          = "tool_call"
	UpdateKindToolCallUpdate    = "tool_call_update"
	UpdateKindPlan              = "plan"
	UpdateKindUsage             = "usage"
)

// Session update kinds that describe the session rather than the conversation.
// An agent emits these when a session opens, whether or not anything has ever
// been said in it.
const (
	UpdateKindCurrentMode       = "current_mode_update"
	UpdateKindAvailableCommands = "available_commands_update"
	UpdateKindConfigOptions     = "config_options_update"
)

// IsConversation reports whether an update carries something that was said or
// done, as opposed to the session describing itself.
//
// The distinction matters when reading a replay. An agent opening any session
// announces its mode and its available commands; only a session with a history
// has messages and tool calls in it.
func IsConversation(u SessionUpdate) bool {
	switch u.Kind {
	case UpdateKindUserMessageChunk,
		UpdateKindAgentMessageChunk,
		UpdateKindContentChunk,
		UpdateKindAgentThoughtChunk,
		UpdateKindToolCall,
		UpdateKindToolCallUpdate:
		return true
	}
	return false
}

// IsUserTurn reports whether an update carries something the user said.
//
// This is the one update a fresh session cannot produce, because on a fresh
// session the user has not spoken yet. In a replay it is therefore proof that
// the agent restored a real conversation rather than opening a blank session
// and sending its usual openers — which otherwise looks identical from the
// outside, and which at least one agent is suspected of doing.
func IsUserTurn(u SessionUpdate) bool {
	return u.Kind == UpdateKindUserMessageChunk
}

// turnDoneKinds are the kind or status values that mean the agent has stopped
// working. Different agents signal this differently and some signal it in
// status rather than kind, so both fields are checked.
var turnDoneKinds = map[string]bool{
	"turn_complete": true,
	"end_turn":      true,
	"completed":     true,
	"idle":          true,
}

// IsTurnDone reports whether an update marks the end of a turn.
func IsTurnDone(u SessionUpdate) bool {
	return turnDoneKinds[u.Kind] || turnDoneKinds[u.Status]
}

// EventForUpdate projects a session update onto a lifecycle event.
//
// It reports false for updates that carry nothing our model represents, so a
// caller can forward every update and emit only what maps. runID and chatID
// identify the run on our side; ACP knows nothing about either.
func EventForUpdate(u SessionUpdate, runID, chatID string) (ap.AgentEvent, bool) {
	switch u.Kind {
	case UpdateKindAgentMessageChunk, UpdateKindContentChunk:
		text := u.Text()
		if text == "" {
			return ap.AgentEvent{}, false
		}
		return ap.NewEvent(ap.AgentEventContentDelta, runID, chatID, ap.ContentDeltaPayload{
			Kind:  ap.ContentDeltaText,
			Delta: text,
		}), true

	case UpdateKindAgentThoughtChunk:
		text := u.Text()
		if text == "" {
			return ap.AgentEvent{}, false
		}
		return ap.NewEvent(ap.AgentEventContentDelta, runID, chatID, ap.ContentDeltaPayload{
			Kind:  ap.ContentDeltaReasoning,
			Delta: text,
		}), true

	case UpdateKindToolCall:
		return ap.NewEvent(ap.AgentEventToolStarted, runID, chatID, ap.ToolStartedPayload{
			ToolInvocationID: u.ToolCallID,
			ToolName:         u.Title,
			DisplayName:      u.Title,
			Arguments:        argumentsOf(u.RawInput),
		}), true

	case UpdateKindToolCallUpdate:
		status := invocationStatus(u.Status)
		if !status.IsTerminal() {
			return ap.AgentEvent{}, false
		}
		return ap.NewEvent(ap.AgentEventToolCompleted, runID, chatID, ap.ToolCompletedPayload{
			ToolInvocationID: u.ToolCallID,
			ToolName:         u.Title,
			Status:           status,
			Result:           u.Text(),
		}), true

	default:
		return ap.AgentEvent{}, false
	}
}

// ApprovalForPermission projects a permission request onto the payload of an
// approval-required event, so a client can raise the agent's question as an
// interrupt on our side and answer it with whatever a human decides.
//
// The returned payload carries the agent's own tool call ID. A caller needs it
// to match the eventual decision back to the request it answers.
func ApprovalForPermission(r PermissionRequest) ap.ApprovalRequiredPayload {
	p := ap.ApprovalRequiredPayload{Reason: ap.InterruptReasonToolApproval}
	if r.ToolCall != nil {
		p.ToolInvocationID = r.ToolCall.ToolCallID
		p.ToolName = r.ToolCall.Title
		p.Arguments = argumentsOf(r.ToolCall.RawInput)
	}
	return p
}

// ResponseForResolution turns a decision made on our side back into the answer
// ACP expects, choosing from the options the agent actually offered.
//
// An approval with no matching allow option, or a rejection with no matching
// reject option, cancels. Cancelling is the only safe fallback: picking an
// arbitrary option could authorise something nobody approved.
func ResponseForResolution(r PermissionRequest, resolution ap.InterruptResolution) PermissionResponse {
	switch resolution {
	case ap.InterruptResolutionAllow:
		if id, ok := r.PickOption(OptionKindAllowOnce, OptionKindAllowAlways); ok {
			return Selected(id)
		}
	case ap.InterruptResolutionDeny:
		if id, ok := r.PickOption(OptionKindRejectOnce, OptionKindRejectAlways); ok {
			return Selected(id)
		}
	}
	return Cancelled()
}

// invocationStatus maps an ACP tool status onto ours. Unknown statuses are
// reported as failed rather than silently dropped, so a tool that ended in a
// way we do not model still terminates instead of hanging.
func invocationStatus(s string) ap.ToolInvocationStatus {
	switch s {
	case "pending":
		return ap.ToolInvocationStatusPending
	case "in_progress", "running":
		return ap.ToolInvocationStatusInProgress
	case "completed", "success":
		return ap.ToolInvocationStatusCompleted
	case "cancelled", "canceled":
		return ap.ToolInvocationStatusCancelled
	case "failed", "error":
		return ap.ToolInvocationStatusFailed
	default:
		return ap.ToolInvocationStatusFailed
	}
}

func argumentsOf(raw json.RawMessage) ap.StringEncodedMap {
	if len(raw) == 0 {
		return nil
	}
	var m ap.StringEncodedMap
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}
