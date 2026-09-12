package agentprotocol_test

import (
	"encoding/json"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
)

func TestAgentEventRoundTrip(t *testing.T) {
	payload, _ := json.Marshal(ap.RunStartedPayload{
		AgentID:        "agent_123",
		AgentVersionID: "v_456",
		UserMessageID:  "msg_789",
	})

	event := ap.AgentEvent{
		ID:        "evt_1",
		Type:      ap.AgentEventRunStarted,
		RunID:     "run_1",
		ChatID:    "chat_1",
		AgentID:   "agent_123",
		Timestamp: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Payload:   payload,
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ap.AgentEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Type != ap.AgentEventRunStarted {
		t.Errorf("type = %q, want %q", decoded.Type, ap.AgentEventRunStarted)
	}
	if decoded.RunID != "run_1" {
		t.Errorf("run_id = %q, want %q", decoded.RunID, "run_1")
	}
	if decoded.ChatID != "chat_1" {
		t.Errorf("chat_id = %q, want %q", decoded.ChatID, "chat_1")
	}

	var p ap.RunStartedPayload
	if err := json.Unmarshal(decoded.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.AgentID != "agent_123" {
		t.Errorf("payload.agent_id = %q, want %q", p.AgentID, "agent_123")
	}
}

func TestAgentEventPayloadTypes(t *testing.T) {
	tests := []struct {
		name    string
		typ     ap.AgentEventType
		payload any
	}{
		{"run_started", ap.AgentEventRunStarted, ap.RunStartedPayload{AgentID: "a"}},
		{"run_state_changed", ap.AgentEventRunStateChanged, ap.RunStateChangedPayload{FromState: ap.AgentRunStateSubmitted, ToState: ap.AgentRunStateWorking}},
		{"turn_started", ap.AgentEventTurnStarted, ap.TurnStartedPayload{TurnIndex: 0, Model: "gpt-4"}},
		{"turn_completed", ap.AgentEventTurnCompleted, ap.TurnCompletedPayload{TurnIndex: 0, ToolCount: 3}},
		{"content_delta", ap.AgentEventContentDelta, ap.ContentDeltaPayload{Kind: ap.ContentDeltaText, Delta: "hello"}},
		{"tool_started", ap.AgentEventToolStarted, ap.ToolStartedPayload{ToolInvocationID: "ti_1", ToolName: "Read"}},
		{"tool_completed", ap.AgentEventToolCompleted, ap.ToolCompletedPayload{ToolInvocationID: "ti_1", Status: ap.ToolInvocationStatusCompleted}},
		{"approval_required", ap.AgentEventApprovalRequired, ap.ApprovalRequiredPayload{ToolName: "Bash", Reason: ap.InterruptReasonToolApproval}},
		{"approval_resolved", ap.AgentEventApprovalResolved, ap.ApprovalResolvedPayload{Decision: "allow"}},
		{"hook_executed", ap.AgentEventHookExecuted, ap.HookExecutedPayload{HookEvent: ap.HookEventTurnStart, Decision: ap.HookDecisionAllow}},
		{"usage_updated", ap.AgentEventUsageUpdated, ap.UsageUpdatedPayload{TotalTokens: 1000}},
		{"context_compacted", ap.AgentEventContextCompacted, ap.ContextCompactedPayload{BeforeTokens: 100000, AfterTokens: 5000}},
		{"error", ap.AgentEventError, ap.ErrorPayload{Message: "something broke", Code: "internal"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payloadJSON, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}

			event := ap.AgentEvent{
				ID:        "evt_test",
				Type:      tt.typ,
				RunID:     "run_1",
				ChatID:    "chat_1",
				Timestamp: time.Now().UTC(),
				Payload:   payloadJSON,
			}

			data, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}

			var decoded ap.AgentEvent
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal event: %v", err)
			}

			if decoded.Type != tt.typ {
				t.Errorf("type = %q, want %q", decoded.Type, tt.typ)
			}
			if len(decoded.Payload) == 0 {
				t.Error("payload is empty after round-trip")
			}
		})
	}
}

func TestAgentEventNilPayload(t *testing.T) {
	event := ap.AgentEvent{
		ID:        "evt_1",
		Type:      ap.AgentEventRunStarted,
		RunID:     "run_1",
		ChatID:    "chat_1",
		Timestamp: time.Now().UTC(),
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ap.AgentEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Payload != nil {
		t.Errorf("expected nil payload, got %s", string(decoded.Payload))
	}
}

func TestAllEventTypesAreDistinct(t *testing.T) {
	types := []ap.AgentEventType{
		ap.AgentEventRunStarted,
		ap.AgentEventRunStateChanged,
		ap.AgentEventTurnStarted,
		ap.AgentEventTurnCompleted,
		ap.AgentEventContentDelta,
		ap.AgentEventToolStarted,
		ap.AgentEventToolCompleted,
		ap.AgentEventApprovalRequired,
		ap.AgentEventApprovalResolved,
		ap.AgentEventHookExecuted,
		ap.AgentEventUsageUpdated,
		ap.AgentEventContextCompacted,
		ap.AgentEventError,
	}

	seen := make(map[ap.AgentEventType]bool)
	for _, typ := range types {
		if seen[typ] {
			t.Errorf("duplicate event type: %s", typ)
		}
		seen[typ] = true
	}

	if len(types) != 13 {
		t.Errorf("expected 13 event types, got %d", len(types))
	}
}
