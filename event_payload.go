package agentprotocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Payload access.
//
// An AgentEvent carries its payload as raw JSON so that a consumer can route
// on Type without paying to decode bodies it will discard, and so that an
// event written by a newer producer still round-trips through an older reader.
// These helpers are the typed way back out.

// DecodePayload unmarshals the event payload into v. A missing payload is not
// an error: it leaves v untouched and returns nil, because several event types
// legitimately carry nothing.
func (e AgentEvent) DecodePayload(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("agentprotocol: decode %s payload: %w", e.Type, err)
	}
	return nil
}

// PayloadAs decodes an event payload into T.
//
// It reports false when the event is not of the expected type, so a caller can
// switch on the result instead of checking Type and decoding as two steps that
// can disagree.
func PayloadAs[T any](e AgentEvent, want AgentEventType) (T, bool) {
	var v T
	if e.Type != want {
		return v, false
	}
	if err := e.DecodePayload(&v); err != nil {
		return v, false
	}
	return v, true
}

// NewEvent builds an event of the given type, marshalling payload if one is
// given. A payload that cannot be marshalled is dropped rather than panicking
// or returning an error: losing the body of one event is recoverable, and
// forcing every emit site to handle an impossible error is not worth it. The
// event type, run and timestamp always survive.
func NewEvent(typ AgentEventType, runID, chatID string, payload any) AgentEvent {
	ev := AgentEvent{
		Type:      typ,
		RunID:     runID,
		ChatID:    chatID,
		Timestamp: time.Now().UTC(),
	}
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			ev.Payload = data
		}
	}
	return ev
}
