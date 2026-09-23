package transcript

import (
	"encoding/json"
	"time"

	"github.com/inference-sh/agentprotocol"
)

// Events projects a session onto the agent event stream every other consumer
// reads. Each user entry opens a turn; assistant text and reasoning become
// content deltas; tool_use blocks become tool started and tool entries
// become tool completed; the next user entry, or the end, closes the turn.
//
// It is a projection, not a second model. The entries stay the source of
// truth, and a store never sees events.
func (s *Session) Events() []agentprotocol.AgentEvent {
	entries := s.Linearize()
	p := projector{session: s, turn: -1}
	for _, e := range entries {
		p.entry(e)
	}
	if len(entries) > 0 {
		p.closeTurn(entries[len(entries)-1].Time)
	}
	return p.out
}

type projector struct {
	session   *Session
	out       []agentprotocol.AgentEvent
	turn      int
	open      bool
	tools     int
	hasOutput bool
}

func (p *projector) emit(t agentprotocol.AgentEventType, at time.Time, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		// Every payload type here is a plain struct; a failure would be a
		// programming error, and dropping the event would hide it.
		panic("transcript: marshal event payload: " + err.Error())
	}
	p.out = append(p.out, agentprotocol.AgentEvent{
		Type:      t,
		ChatID:    p.session.ID,
		AgentID:   p.session.Agent,
		Timestamp: at,
		Payload:   raw,
	})
}

func (p *projector) closeTurn(at time.Time) {
	if !p.open {
		return
	}
	p.emit(agentprotocol.AgentEventTurnCompleted, at, agentprotocol.TurnCompletedPayload{
		TurnIndex: p.turn, ToolCount: p.tools, HasOutput: p.hasOutput,
	})
	p.open = false
}

func (p *projector) entry(e Entry) {
	switch e.Role {
	case RoleUser:
		p.closeTurn(e.Time)
		p.turn++
		p.tools, p.hasOutput, p.open = 0, false, true
		p.emit(agentprotocol.AgentEventTurnStarted, e.Time, agentprotocol.TurnStartedPayload{TurnIndex: p.turn})
	case RoleAssistant:
		for _, b := range e.Content {
			switch b.Kind {
			case BlockText:
				if b.Text != "" {
					p.hasOutput = true
					p.emit(agentprotocol.AgentEventContentDelta, e.Time, agentprotocol.ContentDeltaPayload{Kind: agentprotocol.ContentDeltaText, Delta: b.Text})
				}
			case BlockReasoning:
				if b.Text != "" {
					p.emit(agentprotocol.AgentEventContentDelta, e.Time, agentprotocol.ContentDeltaPayload{Kind: agentprotocol.ContentDeltaReasoning, Delta: b.Text})
				}
			case BlockToolUse:
				p.tools++
				p.emit(agentprotocol.AgentEventToolStarted, e.Time, agentprotocol.ToolStartedPayload{
					ToolInvocationID: b.ToolID, ToolName: b.Name, Arguments: arguments(b.Input),
				})
			}
		}
	case RoleTool:
		for _, b := range e.Content {
			if b.Kind != BlockToolResult {
				continue
			}
			p.emit(agentprotocol.AgentEventToolCompleted, e.Time, agentprotocol.ToolCompletedPayload{
				ToolInvocationID: b.ToolID, ToolName: b.Name, Status: toolStatus(b.Status), Result: b.Text,
			})
		}
	}
}

func toolStatus(s Status) agentprotocol.ToolInvocationStatus {
	if s == StatusError {
		return agentprotocol.ToolInvocationStatusFailed
	}
	return agentprotocol.ToolInvocationStatusCompleted
}

// arguments decodes tool input for the event payload. Input an agent stored
// as something other than an object (codex keeps a JSON string) is reported
// under a single key rather than dropped.
func arguments(raw json.RawMessage) agentprotocol.StringEncodedMap {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		return agentprotocol.StringEncodedMap(m)
	}
	return agentprotocol.StringEncodedMap{"input": string(raw)}
}
