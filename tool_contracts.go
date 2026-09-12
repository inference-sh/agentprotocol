package agentprotocol

// Tool contracts shared by every transport: which kind of tool a call names,
// and the lifecycle an invocation of it moves through.

// ToolInvocationStatus represents the execution status of a tool invocation
type ToolInvocationStatus string

const (
	ToolInvocationStatusPending          ToolInvocationStatus = "pending"
	ToolInvocationStatusInProgress       ToolInvocationStatus = "in_progress"
	ToolInvocationStatusAwaitingInput    ToolInvocationStatus = "awaiting_input"    // Waiting for user input (widgets)
	ToolInvocationStatusAwaitingApproval ToolInvocationStatus = "awaiting_approval" // Waiting for HIL approval
	ToolInvocationStatusCompleted        ToolInvocationStatus = "completed"
	ToolInvocationStatusFailed           ToolInvocationStatus = "failed"
	ToolInvocationStatusCancelled        ToolInvocationStatus = "cancelled"
)

func (s ToolInvocationStatus) IsTerminal() bool {
	return s == ToolInvocationStatusCompleted || s == ToolInvocationStatusFailed || s == ToolInvocationStatusCancelled
}

// CanTransitionTo defines the allowed status transitions for tool invocations.
//
//	pending → in_progress, awaiting_approval, completed, failed, cancelled
//	in_progress → awaiting_input, completed, failed, cancelled
//	awaiting_input → in_progress, completed, failed, cancelled
//	awaiting_approval → in_progress, failed, cancelled
//	terminal → (nothing)
func (s ToolInvocationStatus) CanTransitionTo(next ToolInvocationStatus) bool {
	if s.IsTerminal() {
		return false
	}
	// Any non-terminal state can transition to a terminal state.
	if next.IsTerminal() {
		// Exception: awaiting_approval cannot complete directly (must be approved first).
		if s == ToolInvocationStatusAwaitingApproval && next == ToolInvocationStatusCompleted {
			return false
		}
		return true
	}
	// Non-terminal → non-terminal: only specific transitions allowed.
	switch s {
	case ToolInvocationStatusPending:
		return next == ToolInvocationStatusInProgress || next == ToolInvocationStatusAwaitingApproval
	case ToolInvocationStatusInProgress:
		return next == ToolInvocationStatusAwaitingInput
	case ToolInvocationStatusAwaitingInput:
		return next == ToolInvocationStatusInProgress
	case ToolInvocationStatusAwaitingApproval:
		return next == ToolInvocationStatusInProgress
	default:
		return false
	}
}

// ValidToolInvocationSourceStates returns the set of statuses that can
// transition to next, for use in conditional UPDATE ... WHERE status IN (?).
func ValidToolInvocationSourceStates(next ToolInvocationStatus) []ToolInvocationStatus {
	all := []ToolInvocationStatus{
		ToolInvocationStatusPending,
		ToolInvocationStatusInProgress,
		ToolInvocationStatusAwaitingInput,
		ToolInvocationStatusAwaitingApproval,
	}
	var sources []ToolInvocationStatus
	for _, s := range all {
		if s.CanTransitionTo(next) {
			sources = append(sources, s)
		}
	}
	return sources
}

// ToolFinishStatus represents the final status of a tool/agent completion
type ToolFinishStatus string

const (
	ToolFinishStatusSucceeded ToolFinishStatus = "succeeded"
	ToolFinishStatusFailed    ToolFinishStatus = "failed"
	ToolFinishStatusCancelled ToolFinishStatus = "cancelled"
)

// InternalToolScope defines which agents can use an internal tool
type InternalToolScope string

const (
	InternalToolScopeAll      InternalToolScope = "all"       // Available to all agents
	InternalToolScopeTopLevel InternalToolScope = "top_level" // Only top-level agents
	InternalToolScopeSubAgent InternalToolScope = "sub_agent" // Only sub-agents
)

// ToolType represents the type of tool (used in both AgentTool definition and ToolInvocation)
type ToolType string

const (
	ToolTypeApp      ToolType = "app"      // App tools - creates a Task
	ToolTypeAgent    ToolType = "agent"    // Sub-agent tools - creates a sub-Chat
	ToolTypeHook     ToolType = "hook"     // Webhook tools - HTTP POST to external URL (legacy)
	ToolTypeHTTP     ToolType = "http"     // HTTP tools - authenticated HTTP request/response
	ToolTypeCall     ToolType = "call"     // Call tools - authenticated HTTP request/response (preferred over "http")
	ToolTypeMCP      ToolType = "mcp"      // MCP tools - calls remote MCP server
	ToolTypeClient   ToolType = "client"   // Client tools - executed by frontend
	ToolTypeInternal ToolType = "internal" // Internal/built-in tools (plan, memory, widget, finish)
)
