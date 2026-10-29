package driver

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
)

// ACPExtension adapts one agent's extensions of ACP: the requests and
// notifications it sends outside the protocol, and what its standard
// messages carry in _meta. Without one, an extension request is answered
// method-not-found and an extension notification is reported as a
// diagnostic, which is right for an agent we know nothing about and wrong
// for one whose question then fails or whose background work is never seen
// to end.
//
// A harness names its extension in harness.Harness.ACPExtension, and
// ForHarness looks it up (ACPExtensionFor). A session gets its own instance
// (the extension may keep per-session state), built by the registered
// constructor.
type ACPExtension interface {
	// Request answers an agent-initiated request ACP does not define.
	// method is as sent (_x.ai/ask_user_question); acp.ExtMethod drops the
	// leading underscore. handled false leaves it unanswered by the
	// extension, and the client replies method-not-found.
	Request(ctx context.Context, h ACPHost, method string, params json.RawMessage) (result any, handled bool, err error)

	// Notification takes a notification ACP does not define, and reports
	// whether it did. It runs in order with the session's updates.
	Notification(h ACPHost, method string, params json.RawMessage) bool

	// Update sees each live session/update before it becomes an event. An
	// extension reads what the agent puts beyond the protocol in it
	// (rawOutput, _meta): background work a tool call started, a turn the
	// agent started on its own.
	Update(h ACPHost, n acp.UpdateNotification)

	// ContextWindow is the context window, in tokens, of the session's
	// model, from the agent's initialize and session/new (or session/load)
	// results; 0 when they do not say.
	ContextWindow(initialize, session json.RawMessage) int
}

// ACPHost is what an extension may do in its session.
type ACPHost interface {
	// Ask puts questions to the person as a form and waits for the answer.
	// answered is false when the session ended or ctx did first.
	Ask(ctx context.Context, toolCallID, toolName string, args ap.StringEncodedMap, input ap.InputRequest) (res Resolution, answered bool)
	// Approve asks the person to allow something the agent wants to do
	// (leave plan mode, say), and waits for the decision.
	Approve(ctx context.Context, toolCallID, toolName string, args ap.StringEncodedMap) (res Resolution, answered bool)
	// Emit sends an event of the session.
	Emit(t ap.AgentEventType, payload any)
	// BeginAgentTurn opens a turn the agent started on its own, with no
	// prompt; EndAgentTurn closes it. Both do nothing while a prompted turn
	// is open, since the agent's work then belongs to that turn, and
	// EndAgentTurn does nothing for a turn that was never begun.
	BeginAgentTurn(id string, origin ap.TurnOrigin, taskID string)
	EndAgentTurn(id string, stopReason string)
	// Diagnose reports a non-fatal problem (ACPBackend.OnDiagnostic).
	Diagnose(msg string)
}

var (
	acpExtMu sync.RWMutex
	acpExts  = map[string]func() ACPExtension{}
)

// RegisterACPExtension makes an extension available by name, for a harness
// whose ACPExtension names it. newExt builds one per session.
func RegisterACPExtension(name string, newExt func() ACPExtension) {
	acpExtMu.Lock()
	defer acpExtMu.Unlock()
	acpExts[name] = newExt
}

// ACPExtensionFor builds the extension registered as name, nil when none is.
func ACPExtensionFor(name string) ACPExtension {
	if name == "" {
		return nil
	}
	acpExtMu.RLock()
	newExt := acpExts[name]
	acpExtMu.RUnlock()
	if newExt == nil {
		return nil
	}
	return newExt()
}

// acpExtensionBuilder is ACPBackend.Extension for the extension registered
// as name, nil when none is.
func acpExtensionBuilder(name string) func() ACPExtension {
	if ACPExtensionFor(name) == nil {
		return nil
	}
	return func() ACPExtension { return ACPExtensionFor(name) }
}

// ACPExtensions lists the registered extension names.
func ACPExtensions() []string {
	acpExtMu.RLock()
	defer acpExtMu.RUnlock()
	names := make([]string, 0, len(acpExts))
	for n := range acpExts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// acpHost is a session's ACPHost.
type acpHost struct{ s *acpSession }

func (h acpHost) Ask(ctx context.Context, toolCallID, toolName string, args ap.StringEncodedMap, input ap.InputRequest) (Resolution, bool) {
	return h.s.ask(ctx, toolCallID, toolName, args, input)
}

func (h acpHost) Approve(ctx context.Context, toolCallID, toolName string, args ap.StringEncodedMap) (Resolution, bool) {
	h.s.alive()
	if h.s.autoAllow {
		return Allow(), true
	}
	a := h.s.park(ctx, ap.ApprovalRequiredPayload{
		ToolInvocationID: toolCallID,
		ToolName:         toolName,
		Arguments:        args,
		Reason:           ap.InterruptReasonToolApproval,
	})
	return a.res, !a.cancelled
}

func (h acpHost) Emit(t ap.AgentEventType, payload any) {
	h.s.emit(ap.NewEvent(t, h.s.runID, h.s.chatID, payload))
}

func (h acpHost) BeginAgentTurn(id string, origin ap.TurnOrigin, taskID string) {
	s := h.s
	s.mu.Lock()
	if s.turns > 0 || s.agentTurn != "" {
		s.mu.Unlock()
		return
	}
	s.agentTurn = id
	s.turns++
	s.mu.Unlock()
	s.emit(ap.NewEvent(ap.AgentEventTurnStarted, s.runID, s.chatID, ap.TurnStartedPayload{Origin: origin, TaskID: taskID}))
}

func (h acpHost) EndAgentTurn(id string, stopReason string) {
	s := h.s
	s.mu.Lock()
	if id == "" || s.agentTurn != id {
		s.mu.Unlock()
		return
	}
	s.agentTurn = ""
	s.turns--
	s.mu.Unlock()
	s.emit(ap.NewEvent(ap.AgentEventTurnCompleted, s.runID, s.chatID, ap.TurnCompletedPayload{StopReason: stopReason}))
}

func (h acpHost) Diagnose(msg string) { h.s.diagnose(msg) }

func (s *acpSession) onExtRequest(ctx context.Context, method string, params json.RawMessage) (any, bool, error) {
	if s.ext == nil {
		return nil, false, nil
	}
	return s.ext.Request(ctx, acpHost{s}, method, params)
}

func (s *acpSession) onExtNotification(method string, params json.RawMessage) bool {
	if s.ext == nil {
		return false
	}
	return s.ext.Notification(acpHost{s}, method, params)
}
