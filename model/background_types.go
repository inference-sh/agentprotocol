package model

// Background work: a command or a watch the agent started and left running
// while its turn went on (Grok's background shell and monitor tasks). The
// tool call that started it completes at once; these events report the
// work itself. When it ends, some agents start a turn of their own about it
// (TurnStartedPayload.Origin background).

const (
	AgentEventBackgroundStarted   AgentEventType = "background.started"
	AgentEventBackgroundCompleted AgentEventType = "background.completed"
)

// BackgroundKind is what kind of background work a task is.
type BackgroundKind string

const (
	// BackgroundCommand is a shell command left running.
	BackgroundCommand BackgroundKind = "command"
	// BackgroundMonitor watches something (a command's output, a file) and
	// reports what it sees until it is stopped or times out.
	BackgroundMonitor BackgroundKind = "monitor"
)

// BackgroundStatus is how background work ended.
type BackgroundStatus string

const (
	BackgroundCompleted BackgroundStatus = "completed"
	BackgroundFailed    BackgroundStatus = "failed"
	BackgroundCancelled BackgroundStatus = "cancelled" // killed by the agent or a person
	BackgroundTimedOut  BackgroundStatus = "timed_out"
)

// BackgroundStartedPayload is background work starting.
type BackgroundStartedPayload struct {
	// TaskID is the agent's id for the work, carried by its later events.
	TaskID string         `json:"task_id"`
	Kind   BackgroundKind `json:"kind"`
	// ToolInvocationID is the tool call that started it.
	ToolInvocationID string `json:"tool_invocation_id,omitempty"`
	Command          string `json:"command,omitempty"`
	Description      string `json:"description,omitempty"`
}

// BackgroundCompletedPayload is background work ending.
type BackgroundCompletedPayload struct {
	TaskID   string           `json:"task_id"`
	Status   BackgroundStatus `json:"status"`
	ExitCode *int             `json:"exit_code,omitempty"`
	// Output is the end of what it printed, as the agent reports it.
	Output string `json:"output,omitempty"`
}
