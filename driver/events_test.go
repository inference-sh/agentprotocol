package driver

import (
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
)

// A consumer that has not started reading loses nothing: the producer never
// blocks, and every event arrives in order once reading starts.
func TestEventPumpKeepsEverythingForASlowConsumer(t *testing.T) {
	p := newEventPump()
	go p.run()
	const n = 1000
	done := make(chan struct{})
	go func() {
		for i := range n {
			p.push(ap.NewEvent(ap.AgentEventContentDelta, "run", "chat", ap.ContentDeltaPayload{Delta: string(rune('a' + i%26))}))
		}
		p.push(ap.NewEvent(ap.AgentEventTurnCompleted, "run", "chat", ap.TurnCompletedPayload{}))
		p.end()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked on a consumer that is not reading")
	}
	var got []ap.AgentEvent
	for ev := range p.out {
		got = append(got, ev)
	}
	if len(got) != n+1 {
		t.Fatalf("got %d events, want %d", len(got), n+1)
	}
	if got[n].Type != ap.AgentEventTurnCompleted {
		t.Errorf("last event = %s, want turn_completed", got[n].Type)
	}
}

func TestEventPumpStopNowDiscards(t *testing.T) {
	p := newEventPump()
	go p.run()
	p.push(ap.NewEvent(ap.AgentEventRunStarted, "run", "chat", ap.RunStartedPayload{}))
	p.stopNow()
	p.push(ap.NewEvent(ap.AgentEventTurnStarted, "run", "chat", ap.TurnStartedPayload{}))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-p.out:
			if !ok {
				return
			}
			if ev.Type == ap.AgentEventTurnStarted {
				t.Error("event pushed after stopNow was delivered")
			}
		case <-deadline:
			t.Fatal("events never closed after stopNow")
		}
	}
}
