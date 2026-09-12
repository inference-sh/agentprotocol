package a2a

import (
	"time"

	ap "github.com/inference-sh/agentprotocol"
)

// The run states and the A2A task states were chosen to use the same strings,
// so today every mapping below is an identity. They are written out anyway:
// the two vocabularies are owned by different specs and only one of them is
// ours to change, so the day they diverge should produce a compile error or a
// failing test here rather than a silently wrong status on the wire.

// TaskStateForRunState maps a run state onto the A2A task state that reports
// it. The mapping is total; an unrecognised state reports as failed, because
// claiming a task is still working when we no longer know is worse than
// reporting a failure a caller can retry.
func TaskStateForRunState(s ap.AgentRunState) TaskState {
	switch s {
	case ap.AgentRunStateSubmitted:
		return TaskStateSubmitted
	case ap.AgentRunStateWorking:
		return TaskStateWorking
	case ap.AgentRunStateInputRequired:
		return TaskStateInputRequired
	case ap.AgentRunStateAuthRequired:
		return TaskStateAuthRequired
	case ap.AgentRunStateCompleted:
		return TaskStateCompleted
	case ap.AgentRunStateFailed:
		return TaskStateFailed
	case ap.AgentRunStateCanceled:
		return TaskStateCanceled
	case ap.AgentRunStateRejected:
		return TaskStateRejected
	default:
		return TaskStateFailed
	}
}

// RunStateForTaskState maps an A2A task state onto a run state, for a client
// consuming a remote agent. Unrecognised states become failed for the same
// reason as above.
func RunStateForTaskState(s TaskState) ap.AgentRunState {
	switch s {
	case TaskStateSubmitted:
		return ap.AgentRunStateSubmitted
	case TaskStateWorking:
		return ap.AgentRunStateWorking
	case TaskStateInputRequired:
		return ap.AgentRunStateInputRequired
	case TaskStateAuthRequired:
		return ap.AgentRunStateAuthRequired
	case TaskStateCompleted:
		return ap.AgentRunStateCompleted
	case TaskStateFailed:
		return ap.AgentRunStateFailed
	case TaskStateCanceled:
		return ap.AgentRunStateCanceled
	case TaskStateRejected:
		return ap.AgentRunStateRejected
	default:
		return ap.AgentRunStateFailed
	}
}

// StatusForRun builds a TaskStatus from a run's state at a point in time.
// A failure message or an explanation of what input is needed is attached when
// there is one, because a bare state leaves the caller nothing to act on.
func StatusForRun(runID string, state ap.AgentRunState, at time.Time, failure string, interruptReason string) TaskStatus {
	status := TaskStatus{
		State:     TaskStateForRunState(state),
		Timestamp: FormatTimestamp(at),
	}
	switch {
	case state == ap.AgentRunStateFailed && failure != "":
		status.Message = agentText(runID+"-error", failure)
	case state.IsInterrupted():
		text := "Input required"
		if interruptReason != "" {
			text = "Input required: " + interruptReason
		}
		status.Message = agentText(runID+"-interrupt", text)
	}
	return status
}

// StatusUpdateForEvent projects a lifecycle event onto an A2A status update.
// It reports false for events that carry no state change, so a caller can
// stream every event and forward only the ones A2A has a shape for.
func StatusUpdateForEvent(ev ap.AgentEvent) (TaskStatusUpdateEvent, bool) {
	state, ok := runStateFromEvent(ev)
	if !ok {
		return TaskStatusUpdateEvent{}, false
	}
	at := ev.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	status := TaskStatus{
		State:     TaskStateForRunState(state),
		Timestamp: FormatTimestamp(at),
	}
	if msg := statusMessageForEvent(ev); msg != nil {
		status.Message = msg
	}
	return TaskStatusUpdateEvent{ID: ev.RunID, Status: status}, true
}

// runStateFromEvent reports the run state an event implies, if it implies one.
func runStateFromEvent(ev ap.AgentEvent) (ap.AgentRunState, bool) {
	switch ev.Type {
	case ap.AgentEventRunStarted:
		return ap.AgentRunStateWorking, true
	case ap.AgentEventRunStateChanged:
		var p ap.RunStateChangedPayload
		if ev.DecodePayload(&p) != nil {
			return "", false
		}
		return p.ToState, true
	case ap.AgentEventApprovalRequired:
		return ap.AgentRunStateInputRequired, true
	case ap.AgentEventError:
		return ap.AgentRunStateFailed, true
	default:
		return "", false
	}
}

// statusMessageForEvent extracts the human-readable reason an event carries,
// if it carries one.
func statusMessageForEvent(ev ap.AgentEvent) *Message {
	switch ev.Type {
	case ap.AgentEventError:
		var p ap.ErrorPayload
		if ev.DecodePayload(&p) == nil && p.Message != "" {
			return agentText(ev.RunID+"-error", p.Message)
		}
	case ap.AgentEventApprovalRequired:
		var p ap.ApprovalRequiredPayload
		if ev.DecodePayload(&p) == nil && p.ToolName != "" {
			return agentText(ev.RunID+"-interrupt", "Input required: approve tool "+p.ToolName)
		}
		return agentText(ev.RunID+"-interrupt", "Input required")
	}
	return nil
}

// TextOf concatenates the text parts of a message, ignoring files and
// structured data. Callers that need those should read Parts directly.
func TextOf(m Message) string {
	out := ""
	for _, p := range m.Parts {
		out += p.Text
	}
	return out
}

func agentText(id, text string) *Message {
	return &Message{
		MessageID: id,
		Role:      MessageRoleAgent,
		Parts:     []Part{{Text: text}},
	}
}
