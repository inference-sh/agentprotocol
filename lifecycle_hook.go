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
	HookHandlerBuiltin HookHandlerType = "builtin"
)

var validHandlerTypes = map[HookHandlerType]bool{
	HookHandlerWebhook: true,
	HookHandlerTask:    true,
	HookHandlerGate:    true,
	HookHandlerBuiltin: true,
}

func (t HookHandlerType) IsValid() bool {
	return validHandlerTypes[t]
}

// BuiltinHook names a hook handler the platform implements itself. A builtin
// runs in-process on the turn that fired it: no URL to host, no round trip, no
// agent spawn. That is what lets it return an injection at all — a task hook
// spawns an agent and discards its answer, so only webhook, gate and builtin
// can put anything into context.
//
// This registry is the single source of truth. The runtime dispatches from it,
// agent config is validated against it, and clients enumerate it to show what
// an agent can switch on without hosting anything.
type BuiltinHook string

const (
	// BuiltinHookBeltSuggest searches the team's skills, knowledge and apps
	// for what the turn is about and injects the matches, so an agent picks up
	// procedural knowledge it was never prompted with.
	BuiltinHookBeltSuggest BuiltinHook = "belt:suggest"
)

// BuiltinHookDefinition describes a builtin hook and where it may be used.
type BuiltinHookDefinition struct {
	Name        BuiltinHook `json:"name"`
	Description string      `json:"description"`
	// Events the builtin may be attached to. A builtin that reads the turn's
	// prompt is meaningless on agent.complete, so the set is part of its
	// definition rather than a convention.
	Events []HookEvent `json:"events"`
}

var builtinHookDefs = []BuiltinHookDefinition{
	{
		Name:        BuiltinHookBeltSuggest,
		Description: "Search skills, knowledge and apps for this turn's prompt and inject the matches",
		Events:      []HookEvent{HookEventTurnStart},
	},
}

var builtinHooksByName map[BuiltinHook]BuiltinHookDefinition

func init() {
	builtinHooksByName = make(map[BuiltinHook]BuiltinHookDefinition, len(builtinHookDefs))
	for _, d := range builtinHookDefs {
		builtinHooksByName[d.Name] = d
	}
}

func (b BuiltinHook) IsValid() bool {
	_, ok := builtinHooksByName[b]
	return ok
}

// SupportsEvent reports whether the builtin may be attached to an event.
func (b BuiltinHook) SupportsEvent(e HookEvent) bool {
	def, ok := builtinHooksByName[b]
	if !ok {
		return false
	}
	for _, allowed := range def.Events {
		if allowed == e {
			return true
		}
	}
	return false
}

// BuiltinHookDefinitions returns the canonical list of builtin hooks.
func BuiltinHookDefinitions() []BuiltinHookDefinition {
	return builtinHookDefs
}
