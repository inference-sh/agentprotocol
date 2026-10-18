// Package driver is one contract for running an agent, whatever is on the
// other end.
//
// A sub-agent looks the same to the caller regardless of where it lives: it
// starts, streams progress, sometimes needs a human, and finishes. That shape
// is the lifecycle model in the parent package, so a driver is the small
// amount of glue between a transport and those events.
//
// The parent stays in control. A driver runs an agent; it does not own a loop
// and does not decide what happens next. Every backend reports through the
// same event stream and answers through the same Resolve call, so the code
// above a driver never branches on which kind it is.
package driver

import (
	"context"
	"encoding/json"

	ap "github.com/inference-sh/agentprotocol"
)

// Backend opens sessions against one kind of agent.
//
// A Backend is reusable and safe for concurrent use; a Session is neither.
type Backend interface {
	// Kind names the transport, for logging and for routing a request to the
	// backend that can serve it.
	Kind() string

	// Capabilities describes what this backend supports, so a caller can
	// degrade rather than call something that will fail.
	Capabilities() Capabilities

	// Open starts a session. The session is live when Open returns, and the
	// caller owns closing it.
	Open(ctx context.Context, cfg SessionConfig) (Session, error)
}

// Session is one conversation with one agent.
//
// Events is the only way progress leaves a session. A caller that wants to
// know what happened reads events; there is no second channel and no polling.
type Session interface {
	// ID identifies the session on the far side, for resuming and for
	// correlating with the agent's own logs.
	ID() string

	// Prompt sends user input.
	//
	// Whether this starts a new turn or steers the one already running is the
	// backend's business, and backends differ. Steer in Capabilities reports
	// which behaviour to expect.
	Prompt(ctx context.Context, input Input) error

	// Interrupt abandons the current turn and leaves the session usable.
	Interrupt(ctx context.Context) error

	// Resolve answers a request the agent raised through an
	// approval-required event. requestID is the tool invocation ID carried in
	// that event's payload.
	//
	// Resolving an unknown or already-resolved request returns an error
	// rather than being ignored, because a caller that loses track of which
	// requests are outstanding will otherwise hang waiting for a turn that
	// already ended.
	Resolve(ctx context.Context, requestID string, res Resolution) error

	// Events streams the session's lifecycle. The channel closes when the
	// session ends, which is the signal that no more work is coming.
	Events() <-chan ap.AgentEvent

	// Close ends the session and releases whatever it held. It is safe to
	// call more than once.
	Close() error
}

// Killer is implemented by sessions backed by a local process. Kill ends the
// process at once (SIGKILL), with no graceful shutdown and no chance to flush
// its transcript, the way a crash or OOM would; Events then closes. Close is
// the normal way to end a session; Kill exists to test what an agent does
// after dying mid-work, and for a caller that must stop a wedged agent.
type Killer interface{ Kill() error }

// Capabilities describes what a backend can do. A caller checks these instead
// of special-casing by Kind, so a new backend does not require changes
// upstream.
type Capabilities struct {
	// Steer reports that a prompt sent during an active turn redirects that
	// turn. When false, a prompt sent mid-turn is queued or rejected.
	Steer bool

	// Approvals reports that the agent can ask for permission, which means
	// approval-required events may appear and Resolve may be needed.
	Approvals bool

	// Interrupt reports that a turn can be cancelled without ending the
	// session.
	Interrupt bool

	// Resume reports that a session can be reopened after the process or
	// connection behind it has gone away.
	Resume bool

	// Tools reports that the agent can call SessionConfig.Tools, run by
	// SessionConfig.CallTool.
	Tools bool

	// OnlyCallerTools reports that the backend can turn the agent's own
	// tools off (SessionConfig.OnlyCallerTools).
	OnlyCallerTools bool

	// Images reports that an image in Input.Files reaches the model as an
	// image. When false, the agent gets the image named in the text like any
	// other file, and sees it only if it opens it with a tool of its own.
	Images bool
}

// SessionConfig describes the session to open. Backends ignore fields they
// have no equivalent for; Capabilities says in advance which those are.
type SessionConfig struct {
	// RunID and ChatID are our identifiers for the work. They are stamped on
	// every event the session emits so a caller can route events without
	// tracking which session produced them.
	RunID  string
	ChatID string

	// WorkDir is the directory the agent operates in.
	WorkDir string

	// Instructions is the system prompt or agent brief, where the backend
	// accepts one.
	Instructions string

	// Model names the model to use, where the backend lets the caller choose.
	Model string

	// Tools the agent may call, for backends that accept externally supplied
	// tools. The caller runs them: CallTool is required with Tools.
	Tools []ap.Tool

	// CallTool runs one of Tools when the agent calls it, and returns what
	// the agent sees. It may block for as long as the tool takes, within
	// the backend's tool timeout. ctx ends when the agent withdraws the
	// call (the turn was interrupted) or the session closes. The caller
	// decides whether a call may run: these calls raise no
	// approval-required event, since the caller is the one running them.
	CallTool ToolCaller

	// OnlyCallerTools turns the agent's own tools off (shell, file edits,
	// web fetch and the rest), leaving it Tools and nothing else. Open fails
	// on a backend whose Capabilities.OnlyCallerTools is false rather than
	// run the agent with its tools still on.
	OnlyCallerTools bool

	// Metadata is passed through to the backend for transport-specific
	// settings that do not deserve a field here.
	Metadata map[string]string

	// ResumeSessionID reopens a session the agent persisted earlier instead
	// of starting a new one. The conversation comes back with it: the agent
	// rebuilds its state and the next prompt continues the same thread.
	//
	// The ID is the agent's own and is opaque. It is not ours, it is not
	// portable between agents, and it should never be parsed.
	//
	// Empty starts fresh. A backend that cannot resume ignores this, which
	// Capabilities.Resume says in advance.
	ResumeSessionID string

	// Approvals is who decides the agent's permission requests. Empty
	// (ApprovalsAsk) sends every request to the caller as an
	// approval-required event. ApprovalsSkip is the caller's choice to let
	// the agent run every tool without asking; this package never picks it.
	Approvals Approvals
}

// ToolCaller runs a caller-supplied tool. An error fails the call; the agent
// sees its message as an error result.
type ToolCaller func(ctx context.Context, call ToolCall) (ToolOutput, error)

// ToolCall is one call of a caller-supplied tool.
type ToolCall struct {
	// ToolInvocationID matches the tool.started and tool.completed events
	// for this call. Empty when the agent did not say which call it is.
	ToolInvocationID string

	// Name is the tool's name as the caller supplied it in Tools.
	Name string

	// Arguments is the JSON object the agent passed.
	Arguments json.RawMessage
}

// ToolOutput is what a caller-supplied tool returns to the agent.
type ToolOutput struct {
	Text string

	// Images reach the model as images after Text, on every backend that
	// takes caller tools.
	Images []ToolImage

	IsError bool
}

// ToolImage is an image a caller-supplied tool returns.
type ToolImage struct {
	MimeType string
	Data     []byte
}

// Approvals is how a session's permission requests are decided.
type Approvals string

const (
	// ApprovalsAsk sends every permission request the agent raises to the
	// caller as an approval-required event, to answer with Resolve.
	ApprovalsAsk Approvals = ""

	// ApprovalsSkip launches the agent in its own skip-permissions mode:
	// claude's bypassPermissions, codex's "never" approval policy, an ACP
	// agent's ACPAutoApproveArgs. An ACP agent with no such flag has each
	// request allowed by the backend, without an event. A request an agent
	// in its skip mode still raises (a question for the user, say) reaches
	// the caller as usual. The agent's sandbox, where it has one, is left as
	// configured.
	ApprovalsSkip Approvals = "skip"
)

// Input is what a caller sends into a session: ap.TurnInput, the prompt's
// shape from the server to the agent.
//
// Every backend passes every file on. An image whose bytes are local (a
// file:// URI, a path, or a data: URI) reaches the model as an image where
// the agent takes images (Capabilities.Images); codex also takes an image by
// URL. Every other file is named in the text with its local path, or its URI
// when it has none, for the agent to open with its own tools.
type Input = ap.TurnInput

// TextInput builds the common case.
func TextInput(s string) Input { return Input{Text: s} }

// Resolution is a decision about a request the agent raised.
type Resolution struct {
	// Decision is whether the agent may proceed.
	Decision ap.InterruptResolution

	// Scope says how long the decision lasts. Backends that cannot express
	// anything beyond a single call treat ScopeSession and ScopeAlways as
	// ScopeOnce, which is the safe direction to round: it asks again rather
	// than assuming standing permission.
	Scope Scope

	// Reason is an optional explanation, shown to the agent where the
	// transport carries one.
	Reason string

	// Values answers a structured question rather than an approval, for
	// backends that ask them.
	Values map[string]any
}

// Scope is how long a decision applies.
type Scope string

const (
	// ScopeOnce applies to this request only.
	ScopeOnce Scope = "once"
	// ScopeSession applies for the rest of the session.
	ScopeSession Scope = "session"
	// ScopeAlways applies beyond this session, where the agent persists it.
	ScopeAlways Scope = "always"
)

// Allow approves a single request.
func Allow() Resolution {
	return Resolution{Decision: ap.InterruptResolutionAllow, Scope: ScopeOnce}
}

// AllowFor approves a request with the given scope.
func AllowFor(s Scope) Resolution {
	return Resolution{Decision: ap.InterruptResolutionAllow, Scope: s}
}

// Deny rejects a request, with an optional reason for the agent.
func Deny(reason string) Resolution {
	return Resolution{Decision: ap.InterruptResolutionDeny, Scope: ScopeOnce, Reason: reason}
}
