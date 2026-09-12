package a2a_test

import (
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/a2a"
)

// allRunStates is every state the lifecycle model defines. A new state added
// to the parent package without a mapping here will surface as a failure
// rather than as a task silently reported as failed on the wire.
var allRunStates = []ap.AgentRunState{
	ap.AgentRunStateSubmitted,
	ap.AgentRunStateWorking,
	ap.AgentRunStateInputRequired,
	ap.AgentRunStateAuthRequired,
	ap.AgentRunStateCompleted,
	ap.AgentRunStateFailed,
	ap.AgentRunStateCanceled,
	ap.AgentRunStateRejected,
}

func TestRunStateMappingRoundTrips(t *testing.T) {
	for _, state := range allRunStates {
		task := a2a.TaskStateForRunState(state)
		if back := a2a.RunStateForTaskState(task); back != state {
			t.Errorf("%s mapped to %s and back to %s", state, task, back)
		}
	}
}

func TestTerminalStatesAgree(t *testing.T) {
	for _, state := range allRunStates {
		task := a2a.TaskStateForRunState(state)
		if state.IsTerminal() != task.IsTerminal() {
			t.Errorf("%s terminal=%v but task state %s terminal=%v",
				state, state.IsTerminal(), task, task.IsTerminal())
		}
	}
}

func TestUnknownRunStateReportsFailed(t *testing.T) {
	// A state we do not recognise must not be reported as still working, or a
	// caller waits forever on a task nobody is advancing.
	if got := a2a.TaskStateForRunState("something-new"); got != a2a.TaskStateFailed {
		t.Errorf("unknown state mapped to %s, want failed", got)
	}
	if got := a2a.RunStateForTaskState("something-new"); got != ap.AgentRunStateFailed {
		t.Errorf("unknown task state mapped to %s, want failed", got)
	}
}

func TestStatusForRunAttachesFailureMessage(t *testing.T) {
	status := a2a.StatusForRun("run_1", ap.AgentRunStateFailed, time.Now(), "disk full", "")
	if status.Message == nil {
		t.Fatal("failed run produced no message; the caller learns nothing about why")
	}
	if got := a2a.TextOf(*status.Message); got != "disk full" {
		t.Errorf("message text = %q, want the failure reason", got)
	}
}

func TestStatusForRunExplainsWhatInputIsNeeded(t *testing.T) {
	status := a2a.StatusForRun("run_1", ap.AgentRunStateInputRequired, time.Now(), "", "tool_approval")
	if status.Message == nil {
		t.Fatal("interrupted run produced no message")
	}
	if got := a2a.TextOf(*status.Message); got != "Input required: tool_approval" {
		t.Errorf("message text = %q, want the interrupt reason", got)
	}
}

func TestStatusUpdateForEventReadsTheNewState(t *testing.T) {
	ev := ap.NewEvent(ap.AgentEventRunStateChanged, "run_1", "chat_1", ap.RunStateChangedPayload{
		FromState: ap.AgentRunStateWorking,
		ToState:   ap.AgentRunStateCompleted,
	})

	update, ok := a2a.StatusUpdateForEvent(ev)
	if !ok {
		t.Fatal("state change produced no status update")
	}
	if update.ID != "run_1" {
		t.Errorf("update ID = %q, want the run ID", update.ID)
	}
	if update.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %s, want completed", update.Status.State)
	}
}

func TestStatusUpdateForEventCarriesTheApprovedToolName(t *testing.T) {
	ev := ap.NewEvent(ap.AgentEventApprovalRequired, "run_1", "chat_1", ap.ApprovalRequiredPayload{
		ToolInvocationID: "inv_1",
		ToolName:         "deploy",
		Reason:           ap.InterruptReasonToolApproval,
	})

	update, ok := a2a.StatusUpdateForEvent(ev)
	if !ok {
		t.Fatal("approval request produced no status update")
	}
	if update.Status.State != a2a.TaskStateInputRequired {
		t.Errorf("state = %s, want input_required", update.Status.State)
	}
	// Naming the tool is the difference between a caller that can decide and
	// one that only knows something is blocked.
	if got := a2a.TextOf(*update.Status.Message); got != "Input required: approve tool deploy" {
		t.Errorf("message = %q, want it to name the tool", got)
	}
}

func TestStatusUpdateSkipsEventsWithNoStateChange(t *testing.T) {
	noise := []ap.AgentEventType{
		ap.AgentEventContentDelta,
		ap.AgentEventToolStarted,
		ap.AgentEventUsageUpdated,
		ap.AgentEventHookExecuted,
	}
	for _, typ := range noise {
		ev := ap.NewEvent(typ, "run_1", "chat_1", nil)
		if _, ok := a2a.StatusUpdateForEvent(ev); ok {
			t.Errorf("%s produced a status update; A2A has no shape for it", typ)
		}
	}
}

func TestStatusUpdateFillsMissingTimestamp(t *testing.T) {
	ev := ap.AgentEvent{Type: ap.AgentEventRunStarted, RunID: "run_1"}
	update, ok := a2a.StatusUpdateForEvent(ev)
	if !ok {
		t.Fatal("run started produced no status update")
	}
	if update.Status.Timestamp == "" {
		t.Error("timestamp is empty; the A2A spec requires one")
	}
}
