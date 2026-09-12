package agentprotocol

// AgentRunState tracks the lifecycle of an agent run (one user→agent turn).
// Maps to A2A TaskState and AG-UI Run outcome for protocol compliance.
type AgentRunState string

const (
	AgentRunStateSubmitted     AgentRunState = "submitted"      // acknowledged, not yet processing
	AgentRunStateWorking       AgentRunState = "working"        // agent is actively processing
	AgentRunStateInputRequired AgentRunState = "input_required" // waiting for user input (tool approval, widget)
	AgentRunStateAuthRequired  AgentRunState = "auth_required"  // missing integration credentials (future)
	AgentRunStateCompleted     AgentRunState = "completed"      // finished successfully
	AgentRunStateFailed        AgentRunState = "failed"         // finished with error
	AgentRunStateCanceled      AgentRunState = "canceled"       // canceled by user
	AgentRunStateRejected      AgentRunState = "rejected"       // agent refused the task (future)
)

func (s AgentRunState) IsTerminal() bool {
	return s == AgentRunStateCompleted || s == AgentRunStateFailed ||
		s == AgentRunStateCanceled || s == AgentRunStateRejected
}

func (s AgentRunState) IsInterrupted() bool {
	return s == AgentRunStateInputRequired || s == AgentRunStateAuthRequired
}

// IsSettled returns true when the run won't produce further events in this
// turn — either terminal or waiting for external input.
func (s AgentRunState) IsSettled() bool {
	return s.IsTerminal() || s.IsInterrupted()
}

func (s AgentRunState) CanTransitionTo(next AgentRunState) bool {
	if s.IsTerminal() {
		return false
	}
	switch s {
	case AgentRunStateSubmitted:
		return next == AgentRunStateWorking ||
			next == AgentRunStateCompleted ||
			next == AgentRunStateFailed ||
			next == AgentRunStateCanceled
	case AgentRunStateWorking:
		return next == AgentRunStateInputRequired ||
			next == AgentRunStateAuthRequired ||
			next == AgentRunStateCompleted ||
			next == AgentRunStateFailed ||
			next == AgentRunStateCanceled
	case AgentRunStateInputRequired, AgentRunStateAuthRequired:
		return next == AgentRunStateWorking ||
			next == AgentRunStateFailed ||
			next == AgentRunStateCanceled
	default:
		return false
	}
}

func ValidAgentRunSourceStates(next AgentRunState) []AgentRunState {
	all := []AgentRunState{
		AgentRunStateSubmitted,
		AgentRunStateWorking,
		AgentRunStateInputRequired,
		AgentRunStateAuthRequired,
	}
	var sources []AgentRunState
	for _, s := range all {
		if s.CanTransitionTo(next) {
			sources = append(sources, s)
		}
	}
	return sources
}

// InterruptReason describes why an agent run is in an interrupted state.
// Aligns with AG-UI interrupt outcome reasons.
type InterruptReason string

const (
	InterruptReasonToolApproval InterruptReason = "tool_approval" // HIL: tool needs user approval
	InterruptReasonClientTool   InterruptReason = "client_tool"   // tool executes in the client (browser)
	InterruptReasonWidget       InterruptReason = "widget"        // interactive widget needs user input
	InterruptReasonAuth         InterruptReason = "auth"          // missing integration credentials
	InterruptReasonConfirmation InterruptReason = "confirmation"  // agent asking for explicit confirmation
	InterruptReasonHookGate     InterruptReason = "hook_gate"     // lifecycle hook gate awaiting external decision
)
