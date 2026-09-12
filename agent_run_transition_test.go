package agentprotocol_test

import (
	"testing"

	ap "github.com/inference-sh/agentprotocol"
)

func TestCanTransitionTo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		from    ap.AgentRunState
		to      ap.AgentRunState
		allowed bool
	}{
		// submitted → working, completed, failed, canceled
		{ap.AgentRunStateSubmitted, ap.AgentRunStateWorking, true},
		{ap.AgentRunStateSubmitted, ap.AgentRunStateCompleted, true},
		{ap.AgentRunStateSubmitted, ap.AgentRunStateFailed, true},
		{ap.AgentRunStateSubmitted, ap.AgentRunStateCanceled, true},
		{ap.AgentRunStateSubmitted, ap.AgentRunStateInputRequired, false},

		// working → input_required, auth_required, completed, failed, canceled
		{ap.AgentRunStateWorking, ap.AgentRunStateInputRequired, true},
		{ap.AgentRunStateWorking, ap.AgentRunStateAuthRequired, true},
		{ap.AgentRunStateWorking, ap.AgentRunStateCompleted, true},
		{ap.AgentRunStateWorking, ap.AgentRunStateFailed, true},
		{ap.AgentRunStateWorking, ap.AgentRunStateCanceled, true},
		{ap.AgentRunStateWorking, ap.AgentRunStateSubmitted, false},

		// input_required → working (resume), failed, canceled
		{ap.AgentRunStateInputRequired, ap.AgentRunStateWorking, true},
		{ap.AgentRunStateInputRequired, ap.AgentRunStateFailed, true},
		{ap.AgentRunStateInputRequired, ap.AgentRunStateCanceled, true},
		{ap.AgentRunStateInputRequired, ap.AgentRunStateCompleted, false},

		// auth_required → working, failed, canceled
		{ap.AgentRunStateAuthRequired, ap.AgentRunStateWorking, true},
		{ap.AgentRunStateAuthRequired, ap.AgentRunStateFailed, true},
		{ap.AgentRunStateAuthRequired, ap.AgentRunStateCanceled, true},
		{ap.AgentRunStateAuthRequired, ap.AgentRunStateCompleted, false},

		// terminal → nothing
		{ap.AgentRunStateCompleted, ap.AgentRunStateWorking, false},
		{ap.AgentRunStateCompleted, ap.AgentRunStateFailed, false},
		{ap.AgentRunStateFailed, ap.AgentRunStateWorking, false},
		{ap.AgentRunStateCanceled, ap.AgentRunStateWorking, false},
		{ap.AgentRunStateRejected, ap.AgentRunStateWorking, false},
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

func TestValidAgentRunSourceStates(t *testing.T) {
	t.Parallel()

	// working can be reached from submitted only
	sources := ap.ValidAgentRunSourceStates(ap.AgentRunStateWorking)
	found := map[ap.AgentRunState]bool{}
	for _, s := range sources {
		found[s] = true
	}
	if !found[ap.AgentRunStateSubmitted] {
		t.Error("submitted should be a valid source for working")
	}
	if !found[ap.AgentRunStateInputRequired] {
		t.Error("input_required should be a valid source for working (resume)")
	}
	if !found[ap.AgentRunStateAuthRequired] {
		t.Error("auth_required should be a valid source for working (resume)")
	}

	// completed can be reached from submitted and working
	sources = ap.ValidAgentRunSourceStates(ap.AgentRunStateCompleted)
	found = map[ap.AgentRunState]bool{}
	for _, s := range sources {
		found[s] = true
	}
	if !found[ap.AgentRunStateSubmitted] {
		t.Error("submitted should be a valid source for completed")
	}
	if !found[ap.AgentRunStateWorking] {
		t.Error("working should be a valid source for completed")
	}
	if found[ap.AgentRunStateInputRequired] {
		t.Error("input_required should NOT be a valid source for completed")
	}

	// terminal states have no valid source states
	sources = ap.ValidAgentRunSourceStates(ap.AgentRunStateRejected)
	// rejected is only reachable from non-terminal, but no non-terminal state allows it in CanTransitionTo
	// since rejected is not in any allowed list. So sources should be empty.
	// Actually let me check: submitted can't → rejected, working can't → rejected, etc.
	// So this should be empty.
	if len(sources) != 0 {
		t.Errorf("rejected should have no valid source states, got %v", sources)
	}
}
