// Package acp speaks the Agent Client Protocol: JSON-RPC 2.0 over a byte
// stream, one message per line, in both directions.
//
// ACP is how an editor drives a coding agent that runs as a local process.
// Zed, JetBrains and VS Code extensions all use it, and Claude Code, Codex,
// Gemini CLI and Cursor all answer it. That makes it the way to drive an agent
// on someone's machine without ever handling their credentials: the agent is
// already logged in as them, and we only send it work.
//
// This package is the client half. It deliberately holds no policy. What to do
// when the agent asks permission, or asks to read a file, is the caller's
// decision, supplied as a Handler. A test suite auto-approves; a product
// forwards the question to a human.
//
// Spec: https://agentclientprotocol.com
package acp

import (
	"encoding/json"

	"github.com/inference-sh/agentprotocol/internal/jsonrpc"
)

// ProtocolVersion is the ACP revision this client negotiates.
const ProtocolVersion = 1

// Methods the client calls on the agent.
const (
	MethodInitialize    = "initialize"
	MethodAuthenticate  = "authenticate"
	MethodSessionNew    = "session/new"
	MethodSessionLoad   = "session/load"
	MethodSessionPrompt = "session/prompt"
	MethodSessionCancel = "session/cancel"
	MethodSessionClose  = "session/close"
)

// Methods the agent calls back on the client.
const (
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
	MethodFsReadTextFile    = "fs/read_text_file"
	MethodFsWriteTextFile   = "fs/write_text_file"
	MethodElicitationCreate = "elicitation/create"
)

// JSON-RPC error codes this package produces.
const (
	ErrCodeMethodNotFound = jsonrpc.CodeMethodNotFound
	ErrCodeInvalidRequest = jsonrpc.CodeInvalidRequest
)

// --- JSON-RPC envelope ---

// Request is an outgoing call or notification.
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Message is any inbound line: a response to something we sent, or a request
// or notification the agent originated. Which one it is depends on whether ID
// and Method are set.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// IsResponse reports whether the message answers a call we made.
func (m Message) IsResponse() bool { return m.ID != nil && m.Method == "" }

// IsRequest reports whether the agent is asking us something and expects a
// reply addressed to the same ID.
func (m Message) IsRequest() bool { return m.ID != nil && m.Method != "" }

// IsNotification reports whether the agent is telling us something that needs
// no reply.
func (m Message) IsNotification() bool { return m.ID == nil && m.Method != "" }

// Error is a JSON-RPC error object. Its text includes the data the agent
// attached, which is where agents put the cause.
type Error = jsonrpc.Error

// --- initialize ---

// InitializeParams announces the client to the agent.
type InitializeParams struct {
	ProtocolVersion int                `json:"protocolVersion"`
	ClientInfo      ClientInfo         `json:"clientInfo"`
	Capabilities    ClientCapabilities `json:"capabilities"`
}

// ClientInfo identifies the program driving the agent.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities tells the agent which callbacks it may use.
type ClientCapabilities struct {
	PermissionRequests bool `json:"permissionRequests"`
	FileSystem         bool `json:"fs,omitempty"`
}

// InitializeResult is the agent's half of the handshake.
//
// Only the fields worth acting on are typed. Agents put a great deal else in
// here and spell much of it differently, so Client.InitializeRaw keeps the
// original for anyone who needs more than this.
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentInfo         AgentInfo         `json:"agentInfo"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`

	// AuthMethods are the ways this agent will accept being authenticated.
	// Empty means it asks for none, which is the common case for an agent the
	// user already logged in through its own CLI.
	AuthMethods []AuthMethod `json:"authMethods,omitempty"`
}

// AgentInfo identifies the agent on the far side.
type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// AgentCapabilities is what the agent says it can do.
//
// Treat it as a claim, not a contract. Every agent measured declares
// LoadSession, and whether a given load succeeds has turned out to depend on
// things the flag cannot know — how the previous process ended, whether it has
// been reaped yet — so it has no power to predict the outcome of a call. It is
// worth showing a person and worth recording; it is not worth branching on.
type AgentCapabilities struct {
	LoadSession bool `json:"loadSession"`
}

// AuthMethod is one way an agent will accept being authenticated.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// AuthenticateParams selects one of the agent's advertised auth methods.
type AuthenticateParams struct {
	MethodID string `json:"methodId"`
}

// LoadSessionParams reopens a session the agent persisted earlier.
//
// SessionID is the agent's own identifier and is opaque: observed shapes
// include UUIDs, "20260913_1", "ses_f647af…" and "session_9309c0b5-…". Never
// parse one, and never assume an ID from one agent means anything to another.
type LoadSessionParams struct {
	SessionID  string      `json:"sessionId"`
	CWD        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// --- sessions ---

// NewSessionParams opens a session rooted at a working directory.
type NewSessionParams struct {
	CWD        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// MCPServer is an MCP server the agent should connect to for this session.
// This is how a caller injects its own tools into somebody else's agent.
type MCPServer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// NewSessionResult carries the session identifier every later call repeats.
type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// PromptParams sends user input into a session.
type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// SessionRef names a session for calls that carry nothing else.
type SessionRef struct {
	SessionID string `json:"sessionId"`
}

// ContentBlock is one piece of prompt or response content.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// URI, Name, MimeType and Size describe a resource_link block.
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// TextBlock builds the common case: a block of plain text.
func TextBlock(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

// ResourceLinkBlock points the agent at a resource it can read itself. Every
// agent must accept one in a prompt, unlike image and embedded resource
// blocks, which depend on promptCapabilities. An empty name falls back to the
// URI, since the protocol requires one.
func ResourceLinkBlock(uri, name, mimeType string, size int64) ContentBlock {
	if name == "" {
		name = uri
	}
	return ContentBlock{Type: "resource_link", URI: uri, Name: name, MimeType: mimeType, Size: size}
}

// --- session/update ---

// UpdateNotification wraps an update with the session it belongs to.
type UpdateNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`

	// Replay marks an update that arrived while LoadSession was rebuilding a
	// session, meaning it is history rather than progress.
	//
	// It is set by this package, never by the agent, because the agent sends
	// replay down the same notification channel as live work and gives no
	// sign which is which. A caller that ignores this will re-announce every
	// tool call from an old conversation as if it were happening now.
	Replay bool `json:"-"`
}

// SessionUpdate is the agent reporting progress. Kind discriminates; agents
// differ in which kinds they emit and how they spell them, so treat an
// unrecognised kind as information rather than an error.
type SessionUpdate struct {
	Kind string `json:"sessionUpdate"`

	Content   json.RawMessage `json:"content,omitempty"`
	MessageID string          `json:"messageId,omitempty"`

	ToolCallID string          `json:"toolCallId,omitempty"`
	Status     string          `json:"status,omitempty"`
	Title      string          `json:"title,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

// Text flattens the update's content blocks into a single string. Content
// arrives either as one block or an array of them depending on the agent, so
// both are accepted.
func (u SessionUpdate) Text() string {
	if len(u.Content) == 0 {
		return ""
	}
	if u.Content[0] == '[' {
		var blocks []ContentBlock
		if json.Unmarshal(u.Content, &blocks) != nil {
			return ""
		}
		out := ""
		for _, b := range blocks {
			out += b.Text
		}
		return out
	}
	var block ContentBlock
	if json.Unmarshal(u.Content, &block) != nil {
		return ""
	}
	return block.Text
}

// --- permission ---

// PermissionRequest is the agent asking whether it may do something. The
// options are the agent's own; a client answers by returning one of their IDs
// rather than inventing a verdict of its own.
type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  *PermissionToolRef `json:"toolCall,omitempty"`
	Options   []PermissionOption `json:"options"`

	// DuringLoad marks a request that arrived while LoadSession was rebuilding
	// the session, set by this package and never by the agent.
	//
	// Deliberately not called Replay, because unlike an update this probably
	// is not history. A notification during replay describes something that
	// already happened; a request is the agent waiting for an answer, and an
	// agent whose process died with a tool call parked has good reason to ask
	// again on resume. So the honest name states when it arrived and leaves
	// the interpretation to the caller.
	//
	// What it must not do is go unanswered. The agent blocks on the reply
	// either way, so a caller that ignores these hangs the session it just
	// resumed.
	DuringLoad bool `json:"-"`
}

// PermissionToolRef describes what the agent wants to run.
type PermissionToolRef struct {
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      string          `json:"title,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

// PermissionOption is one answer the agent will accept. Kind is the
// machine-readable intent, typically allow_once, allow_always, reject_once or
// reject_always. Name is for display.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
}

// Permission option kinds seen in the wild.
const (
	OptionKindAllowOnce    = "allow_once"
	OptionKindAllowAlways  = "allow_always"
	OptionKindRejectOnce   = "reject_once"
	OptionKindRejectAlways = "reject_always"
)

// PermissionResponse answers a PermissionRequest.
//
// The nesting is not a mistake: the spec really is
// {"outcome":{"outcome":"selected","optionId":"..."}}. Sending the flat
// {"outcome":"approved"} instead is accepted by lenient agents and rejected by
// schema-validating ones, where it fails every tool call before it runs.
type PermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// PermissionOutcome is either a selected option or a cancellation.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// Permission outcome values.
const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

// Selected answers with one of the offered options.
func Selected(optionID string) PermissionResponse {
	return PermissionResponse{Outcome: PermissionOutcome{Outcome: OutcomeSelected, OptionID: optionID}}
}

// Cancelled declines to answer, which aborts the tool call.
func Cancelled() PermissionResponse {
	return PermissionResponse{Outcome: PermissionOutcome{Outcome: OutcomeCancelled}}
}

// PickOption returns the ID of the first offered option whose kind matches one
// of prefer, in the order given, falling back to any option whose kind starts
// with the first preference's verb. It reports false when nothing matches,
// which a caller should treat as cancel rather than guessing.
func (r PermissionRequest) PickOption(prefer ...string) (string, bool) {
	for _, kind := range prefer {
		for _, o := range r.Options {
			if o.Kind == kind {
				return o.OptionID, true
			}
		}
	}
	if len(prefer) > 0 {
		verb := prefer[0]
		if i := indexByte(verb, '_'); i > 0 {
			verb = verb[:i]
		}
		for _, o := range r.Options {
			if len(o.Kind) >= len(verb) && o.Kind[:len(verb)] == verb {
				return o.OptionID, true
			}
		}
	}
	return "", false
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// --- filesystem ---

// ReadTextFileParams is the agent asking the client to read a file. The agent
// asks rather than reading directly so the client can sandbox or virtualise
// the filesystem.
type ReadTextFileParams struct {
	SessionID string `json:"sessionId,omitempty"`
	Path      string `json:"path"`
}

// ReadTextFileResult returns file contents.
type ReadTextFileResult struct {
	Content string `json:"content"`
}

// WriteTextFileParams is the agent asking the client to write a file.
type WriteTextFileParams struct {
	SessionID string `json:"sessionId,omitempty"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

// --- elicitation ---

// ElicitationParams is the agent asking the user a structured question. This
// is distinct from a permission request: it gathers information rather than
// authorising an action.
type ElicitationParams struct {
	SessionID string          `json:"sessionId,omitempty"`
	Message   string          `json:"message,omitempty"`
	Schema    json.RawMessage `json:"requestedSchema,omitempty"`
}

// ElicitationResponse answers an elicitation.
type ElicitationResponse struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
}

// Elicitation actions.
const (
	ElicitationConfirm = "confirm"
	ElicitationDecline = "decline"
	ElicitationCancel  = "cancel"
)
