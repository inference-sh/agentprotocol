package agentprotocol_test

import (
	"testing"

	ap "github.com/inference-sh/agentprotocol"
)

func TestToolInvocationCanTransitionTo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		from    ap.ToolInvocationStatus
		to      ap.ToolInvocationStatus
		allowed bool
	}{
		// pending → in_progress, awaiting_approval, completed, failed, cancelled
		{ap.ToolInvocationStatusPending, ap.ToolInvocationStatusInProgress, true},
		{ap.ToolInvocationStatusPending, ap.ToolInvocationStatusAwaitingApproval, true},
		{ap.ToolInvocationStatusPending, ap.ToolInvocationStatusCompleted, true},
		{ap.ToolInvocationStatusPending, ap.ToolInvocationStatusFailed, true},
		{ap.ToolInvocationStatusPending, ap.ToolInvocationStatusCancelled, true},
		{ap.ToolInvocationStatusPending, ap.ToolInvocationStatusAwaitingInput, false},

		// in_progress → awaiting_input, completed, failed, cancelled
		{ap.ToolInvocationStatusInProgress, ap.ToolInvocationStatusAwaitingInput, true},
		{ap.ToolInvocationStatusInProgress, ap.ToolInvocationStatusCompleted, true},
		{ap.ToolInvocationStatusInProgress, ap.ToolInvocationStatusFailed, true},
		{ap.ToolInvocationStatusInProgress, ap.ToolInvocationStatusCancelled, true},
		{ap.ToolInvocationStatusInProgress, ap.ToolInvocationStatusPending, false},
		{ap.ToolInvocationStatusInProgress, ap.ToolInvocationStatusAwaitingApproval, false},

		// awaiting_input → in_progress, completed, failed, cancelled
		{ap.ToolInvocationStatusAwaitingInput, ap.ToolInvocationStatusInProgress, true},
		{ap.ToolInvocationStatusAwaitingInput, ap.ToolInvocationStatusCompleted, true},
		{ap.ToolInvocationStatusAwaitingInput, ap.ToolInvocationStatusFailed, true},
		{ap.ToolInvocationStatusAwaitingInput, ap.ToolInvocationStatusCancelled, true},
		{ap.ToolInvocationStatusAwaitingInput, ap.ToolInvocationStatusPending, false},
		{ap.ToolInvocationStatusAwaitingInput, ap.ToolInvocationStatusAwaitingApproval, false},

		// awaiting_approval → in_progress (approved), failed, cancelled
		{ap.ToolInvocationStatusAwaitingApproval, ap.ToolInvocationStatusInProgress, true},
		{ap.ToolInvocationStatusAwaitingApproval, ap.ToolInvocationStatusFailed, true},
		{ap.ToolInvocationStatusAwaitingApproval, ap.ToolInvocationStatusCancelled, true},
		{ap.ToolInvocationStatusAwaitingApproval, ap.ToolInvocationStatusCompleted, false},
		{ap.ToolInvocationStatusAwaitingApproval, ap.ToolInvocationStatusAwaitingInput, false},

		// terminal → nothing
		{ap.ToolInvocationStatusCompleted, ap.ToolInvocationStatusInProgress, false},
		{ap.ToolInvocationStatusCompleted, ap.ToolInvocationStatusFailed, false},
		{ap.ToolInvocationStatusFailed, ap.ToolInvocationStatusCompleted, false},
		{ap.ToolInvocationStatusFailed, ap.ToolInvocationStatusInProgress, false},
		{ap.ToolInvocationStatusCancelled, ap.ToolInvocationStatusCompleted, false},
		{ap.ToolInvocationStatusCancelled, ap.ToolInvocationStatusInProgress, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			t.Parallel()
			got := tt.from.CanTransitionTo(tt.to)
			if got != tt.allowed {
				t.Errorf("CanTransitionTo(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.allowed)
			}
		})
	}
}

func TestValidToolInvocationSourceStates(t *testing.T) {
	t.Parallel()

	// completed reachable from pending, in_progress, awaiting_input
	sources := ap.ValidToolInvocationSourceStates(ap.ToolInvocationStatusCompleted)
	found := map[ap.ToolInvocationStatus]bool{}
	for _, s := range sources {
		found[s] = true
	}
	if !found[ap.ToolInvocationStatusPending] {
		t.Error("pending should be a valid source for completed")
	}
	if !found[ap.ToolInvocationStatusInProgress] {
		t.Error("in_progress should be a valid source for completed")
	}
	if !found[ap.ToolInvocationStatusAwaitingInput] {
		t.Error("awaiting_input should be a valid source for completed")
	}
	if found[ap.ToolInvocationStatusAwaitingApproval] {
		t.Error("awaiting_approval should NOT be a valid source for completed")
	}

	// in_progress reachable from pending, awaiting_input, awaiting_approval
	sources = ap.ValidToolInvocationSourceStates(ap.ToolInvocationStatusInProgress)
	found = map[ap.ToolInvocationStatus]bool{}
	for _, s := range sources {
		found[s] = true
	}
	if !found[ap.ToolInvocationStatusPending] {
		t.Error("pending should be a valid source for in_progress")
	}
	if !found[ap.ToolInvocationStatusAwaitingInput] {
		t.Error("awaiting_input should be a valid source for in_progress")
	}
	if !found[ap.ToolInvocationStatusAwaitingApproval] {
		t.Error("awaiting_approval should be a valid source for in_progress")
	}
}
