package driver

import (
	"sync"

	ap "github.com/inference-sh/agentprotocol"
)

// eventPump delivers events in order without ever blocking the producer: the
// agent's read loop must keep moving while a consumer is slow, and dropping
// events would lose turn boundaries and approvals. Every backend's Events is
// one.
type eventPump struct {
	mu    sync.Mutex
	queue []ap.AgentEvent
	ended bool

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	out      chan ap.AgentEvent
}

func newEventPump() *eventPump {
	return &eventPump{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		out:  make(chan ap.AgentEvent),
	}
}

func (p *eventPump) push(ev ap.AgentEvent) {
	p.mu.Lock()
	if p.ended {
		p.mu.Unlock()
		return
	}
	p.queue = append(p.queue, ev)
	p.mu.Unlock()
	p.signal()
}

// end lets the queue drain, then closes the channel.
func (p *eventPump) end() {
	p.mu.Lock()
	p.ended = true
	p.mu.Unlock()
	p.signal()
}

// stopNow closes the channel without draining.
func (p *eventPump) stopNow() {
	p.end()
	p.stopOnce.Do(func() { close(p.stop) })
}

func (p *eventPump) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *eventPump) run() {
	defer close(p.out)
	for {
		p.mu.Lock()
		batch := p.queue
		p.queue = nil
		ended := p.ended
		p.mu.Unlock()

		for _, ev := range batch {
			select {
			case p.out <- ev:
			case <-p.stop:
				return
			}
		}
		if len(batch) > 0 {
			continue
		}
		if ended {
			return
		}
		select {
		case <-p.wake:
		case <-p.stop:
			return
		}
	}
}
