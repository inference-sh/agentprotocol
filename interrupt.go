package agentprotocol

// InterruptStatus tracks the lifecycle of an interrupt gate.
type InterruptStatus string

const (
	InterruptStatusPending   InterruptStatus = "pending"
	InterruptStatusResolved  InterruptStatus = "resolved"
	InterruptStatusExpired   InterruptStatus = "expired"
	InterruptStatusCancelled InterruptStatus = "cancelled"
)

func (s InterruptStatus) IsTerminal() bool {
	return s == InterruptStatusResolved || s == InterruptStatusExpired || s == InterruptStatusCancelled
}

// InterruptResolution records how a pending interrupt was resolved.
type InterruptResolution string

const (
	InterruptResolutionAllow InterruptResolution = "allow"
	InterruptResolutionDeny  InterruptResolution = "deny"
)

// InterruptResourceType identifies the kind of resource an interrupt gates.
type InterruptResourceType string

const (
	InterruptResourceToolInvocation InterruptResourceType = "tool_invocation"
	InterruptResourceHookEvent      InterruptResourceType = "hook_event"
)
