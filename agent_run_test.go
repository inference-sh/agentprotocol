package agentprotocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentRunState_IsTerminal(t *testing.T) {
	t.Parallel()

	terminal := []AgentRunState{
		AgentRunStateCompleted,
		AgentRunStateFailed,
		AgentRunStateCanceled,
		AgentRunStateRejected,
	}
	nonTerminal := []AgentRunState{
		AgentRunStateSubmitted,
		AgentRunStateWorking,
		AgentRunStateInputRequired,
		AgentRunStateAuthRequired,
	}

	for _, s := range terminal {
		assert.True(t, s.IsTerminal(), "%s should be terminal", s)
	}
	for _, s := range nonTerminal {
		assert.False(t, s.IsTerminal(), "%s should not be terminal", s)
	}
}

func TestAgentRunState_IsInterrupted(t *testing.T) {
	t.Parallel()

	interrupted := []AgentRunState{
		AgentRunStateInputRequired,
		AgentRunStateAuthRequired,
	}
	nonInterrupted := []AgentRunState{
		AgentRunStateSubmitted,
		AgentRunStateWorking,
		AgentRunStateCompleted,
		AgentRunStateFailed,
		AgentRunStateCanceled,
		AgentRunStateRejected,
	}

	for _, s := range interrupted {
		assert.True(t, s.IsInterrupted(), "%s should be interrupted", s)
	}
	for _, s := range nonInterrupted {
		assert.False(t, s.IsInterrupted(), "%s should not be interrupted", s)
	}
}

func TestAgentRunState_constants(t *testing.T) {
	t.Parallel()

	// Regression guard: DB stores these exact strings; partial unique indexes filter on them.
	assert.Equal(t, AgentRunState("submitted"), AgentRunStateSubmitted)
	assert.Equal(t, AgentRunState("working"), AgentRunStateWorking)
	assert.Equal(t, AgentRunState("input_required"), AgentRunStateInputRequired)
	assert.Equal(t, AgentRunState("auth_required"), AgentRunStateAuthRequired)
	assert.Equal(t, AgentRunState("completed"), AgentRunStateCompleted)
	assert.Equal(t, AgentRunState("failed"), AgentRunStateFailed)
	assert.Equal(t, AgentRunState("canceled"), AgentRunStateCanceled)
	assert.Equal(t, AgentRunState("rejected"), AgentRunStateRejected)
}

func TestInterruptReason_constants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, InterruptReason("tool_approval"), InterruptReasonToolApproval)
	assert.Equal(t, InterruptReason("client_tool"), InterruptReasonClientTool)
	assert.Equal(t, InterruptReason("widget"), InterruptReasonWidget)
	assert.Equal(t, InterruptReason("auth"), InterruptReasonAuth)
	assert.Equal(t, InterruptReason("confirmation"), InterruptReasonConfirmation)
}
