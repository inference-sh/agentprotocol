// Package codex maps codex's spawned agents onto the agentprotocol subagent
// events. A subagent is a thread of its own: the parent's collabAgentToolCall
// item spawns it (receiverThreadIds), reports the spawned threads' states as
// it goes (agentsStates), and the spawned thread's own notifications carry
// its thread id.
package codex

import (
	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/codexapp"
)

// Spawned is the subagents a spawnAgent call starts, one per spawned thread.
// Other collab tools (sendInput, wait, closeAgent) start none.
func Spawned(c codexapp.CollabAgentToolCallThreadItem) []ap.SubagentStartedPayload {
	if c.Tool != codexapp.CollabAgentToolSpawnAgent {
		return nil
	}
	out := make([]ap.SubagentStartedPayload, 0, len(c.ReceiverThreadIDs))
	for _, id := range c.ReceiverThreadIDs {
		p := ap.SubagentStartedPayload{SubagentID: id, ParentToolInvocationID: c.ID}
		if c.Prompt != nil {
			p.Prompt = *c.Prompt
			p.Description = clip(*c.Prompt)
		}
		if c.Model != nil {
			p.Model = *c.Model
		}
		out = append(out, p)
	}
	return out
}

// Status maps a spawned agent's state. A thread shut down after its work is
// cancelled only if it had not completed; the caller keeps the first final
// status it saw.
func Status(s codexapp.CollabAgentStatus) ap.SubagentStatus {
	switch s {
	case codexapp.CollabAgentStatusPendingInit:
		return ap.SubagentWaiting
	case codexapp.CollabAgentStatusRunning:
		return ap.SubagentRunning
	case codexapp.CollabAgentStatusInterrupted:
		return ap.SubagentInterrupted
	case codexapp.CollabAgentStatusCompleted:
		return ap.SubagentCompleted
	case codexapp.CollabAgentStatusErrored, codexapp.CollabAgentStatusNotFound:
		return ap.SubagentFailed
	case codexapp.CollabAgentStatusShutdown:
		return ap.SubagentCancelled
	}
	return ""
}

// Usage is a spawned thread's usage from its running token total.
func Usage(u codexapp.ThreadTokenUsage) *ap.SubagentUsage {
	return &ap.SubagentUsage{TotalTokens: int(u.Total.TotalTokens)}
}

// clip shortens a prompt to a one-line description.
func clip(s string) string {
	for i, r := range s {
		if r == '\n' {
			s = s[:i]
			break
		}
	}
	if r := []rune(s); len(r) > 80 {
		return string(r[:77]) + "..."
	}
	return s
}

// TurnDone is a spawned thread's turn ending, however it ended. codex keeps
// a spawned agent open after its turn, waiting for more input until the
// parent closes it, so the turn's end is a wait with the turn's last message
// as its summary, not the subagent's end: that comes from the parent's report
// of its state (agentsStates).
func TurnDone(thread string, t codexapp.Turn) ap.SubagentUpdatedPayload {
	p := ap.SubagentUpdatedPayload{SubagentID: thread, Status: ap.SubagentWaiting}
	for _, item := range t.Items {
		if m, ok := item.AsAgentMessageThreadItem(); ok && m.Text != "" {
			p.Summary = m.Text
		}
	}
	// A failed or interrupted turn leaves the agent open as well; the
	// error says what went wrong.
	if t.Error != nil {
		p.Error = t.Error.Message
	}
	return p
}
