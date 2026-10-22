// Package claude maps Claude Code's task messages onto the agentprotocol
// subagent events. A subagent is a task: task_started begins it (linked to
// the Agent tool call that started it), task_progress and task_updated
// report on it, and task_notification ends it. The subagent's own messages
// carry parent_tool_use_id, the starting tool call, which links them to the
// task.
package claude

import (
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/claudecode"
)

// Started maps task_started. A task started inside another task names it.
func Started(t claudecode.TaskStarted) ap.SubagentStartedPayload {
	kind := t.SubagentType
	if kind == "" {
		kind = t.TaskType
	}
	return ap.SubagentStartedPayload{
		SubagentID: t.TaskID, ParentToolInvocationID: t.ToolUseID, ParentSubagentID: t.ParentTaskID,
		Description: t.Description, Kind: kind, Prompt: t.Prompt, Background: t.IsBackgrounded,
	}
}

// Progress maps task_progress.
func Progress(t claudecode.TaskProgress) ap.SubagentUpdatedPayload {
	return ap.SubagentUpdatedPayload{
		SubagentID: t.TaskID, Status: ap.SubagentRunning, Description: t.Description,
		Summary: t.Summary, LastToolName: t.LastToolName, Usage: usage(t.Usage),
	}
}

// Updated maps task_updated. A patch that ends the task is reported by the
// task_notification that follows it, which carries the summary; the patch
// is then an update with its final status.
func Updated(t claudecode.TaskUpdated) ap.SubagentUpdatedPayload {
	return ap.SubagentUpdatedPayload{
		SubagentID: t.TaskID, Status: Status(t.Patch.Status), Description: t.Patch.Description, Error: t.Patch.Error,
	}
}

// Completed maps task_notification.
func Completed(t claudecode.TaskNotification) ap.SubagentCompletedPayload {
	return ap.SubagentCompletedPayload{SubagentID: t.TaskID, Status: Status(t.Status), Summary: t.Summary, Usage: usage(t.Usage)}
}

// Status maps a task status: killed and stopped are cancelled, paused and
// pending are waiting.
func Status(s string) ap.SubagentStatus {
	switch s {
	case "running":
		return ap.SubagentRunning
	case "pending", "paused":
		return ap.SubagentWaiting
	case "completed":
		return ap.SubagentCompleted
	case "failed":
		return ap.SubagentFailed
	case "killed", "stopped":
		return ap.SubagentCancelled
	}
	return ""
}

func usage(u *claudecode.TaskUsage) *ap.SubagentUsage {
	if u == nil {
		return nil
	}
	return &ap.SubagentUsage{TotalTokens: u.TotalTokens, ToolUses: u.ToolUses, DurationMs: u.DurationMS}
}
