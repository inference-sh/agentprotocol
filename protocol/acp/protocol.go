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
	"sort"

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
	ErrCodeInternal       = -32603
)

// ExtMethod is an extension method's name without the leading underscore
// ACP reserves for them: agents send _x.ai/ask_user_question and its docs
// call it x.ai/ask_user_question. A name that is not an extension is
// returned unchanged.
func ExtMethod(method string) string {
	if len(method) > 1 && method[0] == '_' {
		return method[1:]
	}
	return method
}

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
	LoadSession bool               `json:"loadSession"`
	Prompt      PromptCapabilities `json:"promptCapabilities"`
	MCP         MCPCapabilities    `json:"mcpCapabilities"`
}

// MCPCapabilities are the MCP transports the agent connects to beyond stdio,
// which every agent must take.
type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// PromptCapabilities are the content blocks a prompt may carry beyond text
// and resource links, which every agent must take.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
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
//
// A server with a Command is stdio: the agent launches it. One with a URL is
// http, or sse when Type says so; an http server needs the agent's
// mcpCapabilities.http. On the wire Env and Headers are lists of name/value
// pairs, as the ACP schema has them; MarshalJSON writes that form and
// UnmarshalJSON reads it and the object form alike.
type MCPServer struct {
	// Type is "http" or "sse" for a URL server; empty means http. A stdio
	// server has none on the wire.
	Type    string
	Name    string
	URL     string
	Command string
	Args    []string
	Env     map[string]string
	Headers map[string]string
}

// NameValue is an ACP HttpHeader or EnvVariable.
type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func pairs(m map[string]string) []NameValue {
	out := make([]NameValue, 0, len(m))
	for k, v := range m {
		out = append(out, NameValue{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MarshalJSON writes the ACP schema's McpServer: stdio when Command is set,
// otherwise http or sse. The schema requires args and env on stdio and
// headers on http and sse, so they are written even when empty.
func (m MCPServer) MarshalJSON() ([]byte, error) {
	if m.Command != "" {
		args := m.Args
		if args == nil {
			args = []string{}
		}
		return json.Marshal(struct {
			Name    string      `json:"name"`
			Command string      `json:"command"`
			Args    []string    `json:"args"`
			Env     []NameValue `json:"env"`
		}{m.Name, m.Command, args, pairs(m.Env)})
	}
	typ := m.Type
	if typ == "" {
		typ = "http"
	}
	return json.Marshal(struct {
		Type    string      `json:"type"`
		Name    string      `json:"name"`
		URL     string      `json:"url"`
		Headers []NameValue `json:"headers"`
	}{typ, m.Name, m.URL, pairs(m.Headers)})
}

// UnmarshalJSON reads either wire form, with env and headers as name/value
// lists or as objects.
func (m *MCPServer) UnmarshalJSON(data []byte) error {
	var w struct {
		Type    string          `json:"type"`
		Name    string          `json:"name"`
		URL     string          `json:"url"`
		Command string          `json:"command"`
		Args    []string        `json:"args"`
		Env     json.RawMessage `json:"env"`
		Headers json.RawMessage `json:"headers"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	env, err := readPairs(w.Env)
	if err != nil {
		return err
	}
	headers, err := readPairs(w.Headers)
	if err != nil {
		return err
	}
	*m = MCPServer{Type: w.Type, Name: w.Name, URL: w.URL, Command: w.Command, Args: w.Args, Env: env, Headers: headers}
	return nil
}

func readPairs(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '{' {
		var m map[string]string
		err := json.Unmarshal(raw, &m)
		return m, err
	}
	var list []NameValue
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(list))
	for _, p := range list {
		m[p.Name] = p.Value
	}
	return m, nil
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

	// Data is an image block's base64 bytes, with MimeType and optionally
	// the URI it came from.
	Data string `json:"data,omitempty"`
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

// ImageBlock is an image, base64-encoded. Only an agent whose
// promptCapabilities include image takes one; uri says where it came from.
func ImageBlock(data, mimeType, uri string) ContentBlock {
	return ContentBlock{Type: "image", Data: data, MimeType: mimeType, URI: uri}
}

// --- session/update ---

// UpdateNotification wraps an update with the session it belongs to.
type UpdateNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
	// Meta is the notification's _meta, where agents put their own
	// bookkeeping (Grok: the promptId of the turn the update belongs to).
	Meta json.RawMessage `json:"_meta,omitempty"`

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
	RawOutput  json.RawMessage `json:"rawOutput,omitempty"`
	// Meta is the update's _meta.
	Meta json.RawMessage `json:"_meta,omitempty"`

	// Used, Size and Cost are a usage_update's fields.
	Used *int64 `json:"used,omitempty"`
	Size *int64 `json:"size,omitempty"`
	Cost *Cost  `json:"cost,omitempty"`
}

// UsageUpdate is a usage_update: the tokens in the session's context, the
// context window's size, and the session's cumulative cost when the agent
// knows it.
type UsageUpdate struct {
	Used int64
	Size int64
	Cost *Cost
}

// Cost is a session's cumulative cost.
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Usage is the update as a usage_update; ok is false for any other kind or
// one missing its required fields.
func (u SessionUpdate) Usage() (UsageUpdate, bool) {
	if u.Kind != UpdateKindUsageUpdate || u.Used == nil || u.Size == nil {
		return UsageUpdate{}, false
	}
	return UsageUpdate{Used: *u.Used, Size: *u.Size, Cost: u.Cost}, true
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
	// Meta is the tool call's _meta.
	Meta json.RawMessage `json:"_meta,omitempty"`
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
