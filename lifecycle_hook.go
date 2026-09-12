package agentprotocol

// HookEvent is a lifecycle event in the agent conversation loop.
// Events fire at well-defined points in the turn cycle, giving external
// handlers the ability to observe, inject context, or halt execution.
type HookEvent string

const (
	HookEventAgentStart    HookEvent = "agent.start"
	HookEventTurnStart     HookEvent = "agent.turn_start"
	HookEventToolCall      HookEvent = "agent.tool_call"
	HookEventToolResult    HookEvent = "agent.tool_result"
	HookEventTurnComplete  HookEvent = "agent.turn_complete"
	HookEventAgentError    HookEvent = "agent.error"
	HookEventAgentComplete HookEvent = "agent.complete"
	HookEventAgentIdle     HookEvent = "agent.idle"
	HookEventPreCompact    HookEvent = "agent.pre_compact"
	HookEventPostCompact   HookEvent = "agent.post_compact"
)

// HookEventDefinition describes a lifecycle hook event and its capabilities.
type HookEventDefinition struct {
	Event       HookEvent `json:"event"`
	Description string    `json:"description"`
	CanGate     bool      `json:"can_gate"`
}

// hookEventDefs is the single source of truth for all hook events.
var hookEventDefs = []HookEventDefinition{
	{HookEventAgentStart, "Fires when the agent run begins", true},
	{HookEventTurnStart, "Fires at the start of each turn", true},
	{HookEventToolCall, "Fires before a tool is executed", true},
	{HookEventToolResult, "Fires after a tool completes", false},
	{HookEventTurnComplete, "Fires after all tool calls in a turn complete", false},
	{HookEventAgentError, "Fires when the agent encounters an error", false},
	{HookEventAgentComplete, "Fires when the agent run completes", false},
	{HookEventAgentIdle, "Fires when the agent has no pending work", false},
	{HookEventPreCompact, "Fires before context compaction", true},
	{HookEventPostCompact, "Fires after context compaction", false},
}

var validHookEvents map[HookEvent]bool
var gatableHookEvents map[HookEvent]bool

func init() {
	validHookEvents = make(map[HookEvent]bool, len(hookEventDefs))
	gatableHookEvents = make(map[HookEvent]bool)
	for _, d := range hookEventDefs {
		validHookEvents[d.Event] = true
		if d.CanGate {
			gatableHookEvents[d.Event] = true
		}
	}
}

func (e HookEvent) IsValid() bool {
	return validHookEvents[e]
}

func (e HookEvent) CanGate() bool {
	return gatableHookEvents[e]
}

// HookEventDefinitions returns the canonical list of hook events with metadata.
func HookEventDefinitions() []HookEventDefinition {
	return hookEventDefs
}

// HookDecision is the handler's verdict on whether execution should continue.
type HookDecision string

const (
	HookDecisionAllow   HookDecision = "allow"
	HookDecisionDeny    HookDecision = "deny"
	HookDecisionStop    HookDecision = "stop"
	HookDecisionSuspend HookDecision = "suspend"
)

// HookHandlerType distinguishes how a lifecycle hook is executed.
type HookHandlerType string

const (
	HookHandlerWebhook HookHandlerType = "webhook"
	HookHandlerTask    HookHandlerType = "task"
	HookHandlerGate    HookHandlerType = "gate"
)
