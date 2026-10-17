// Package llm runs a coding-agent CLI as a language model: a conversation
// and a tool list go in, one model response comes out, the way a call to the
// model's own API works.
//
// The agent never runs a tool. The caller's tools reach the model through an
// MCP server this package serves itself, over the agent's own control
// channel where it has one; a tool call the model makes ends the response
// and goes back to the caller, who runs it and sends the result in the next
// call. Nothing of the agent's own is offered: no built-in tools, no
// settings, skills or slash commands, and the caller's system prompt in
// place of the agent's.
//
// What differs from an API call is who holds the conversation. The agent
// keeps it, so a call is served one of two ways:
//
//   - Continued: the call extends the last one on the same Key with the
//     results of the tool calls it returned, or with a new user turn. The
//     agent's process is still running, and the call is answered from it:
//     the held tool calls are allowed and their results returned from the
//     MCP server. The model gets exactly what an API call would send.
//   - Restarted: anything else. The conversation before the call is written
//     as a session the agent loads (package transcript) and the agent is
//     resumed from it. See Claude for what a restart can and cannot carry.
//
// Settings the agent has no flag for (sampling, forced tool choice, output
// schemas) are refused with ErrUnsupported rather than silently dropped;
// Capabilities says which are which in advance.
package llm

import (
	"context"
	"encoding/json"
	"errors"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/transcript"
)

// Model answers calls with one model response each. A Model is safe for
// concurrent use; calls on the same Key are served one at a time.
type Model interface {
	// Generate runs one call. onDelta, when not nil, receives the response
	// as it streams, on the calling goroutine's behalf but not on it; it
	// must not block for long.
	Generate(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error)

	// Capabilities says which Request fields this model honours.
	Capabilities() Capabilities

	// Close ends every live conversation.
	Close() error
}

// Request is one call: the whole conversation, the current turn last, and
// the settings for this response.
type Request struct {
	// Key names the conversation, so the next call on it can continue the
	// agent process this one leaves running. Empty never continues: every
	// call restarts, and the process ends with the call.
	Key string

	// System replaces the agent's system prompt.
	System string

	// Messages is the conversation. User turns carry text, images and
	// files; assistant turns carry text, reasoning and tool_use blocks;
	// tool turns carry tool_result blocks, with any image a tool returned
	// following its result, ToolID set. Reasoning with a Signature the
	// model's vendor issued is sent back; reasoning without one is left
	// out, as the vendor's API would refuse it.
	Messages []transcript.Entry

	// Tools the model may call.
	Tools []Tool

	// ToolChoice is "" or "auto" (the model decides) or "none" (tools are
	// not offered). Forcing a call is not something an agent can be told;
	// see Capabilities.
	ToolChoice string

	// Model names the model, in the agent's own terms. Empty is the
	// agent's default.
	Model string

	// Effort is the reasoning effort, in the agent's own terms. Empty is
	// the agent's default.
	Effort string

	// MaxTokens caps the response, where the agent takes a cap. Zero is the
	// agent's default.
	MaxTokens int
}

// Tool is a tool the caller offers. InputSchema is a JSON Schema object.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Response is one model response.
type Response struct {
	// Content is the response in order: reasoning (with the vendor's
	// Signature, to send back in a later call), text, and tool_use blocks.
	Content []transcript.Block

	// StopReason is why the model stopped, in the vendor's terms:
	// end_turn, tool_use, max_tokens, stop_sequence, refusal.
	StopReason string

	// Model is the model that answered, as the agent reports it.
	Model string

	// Usage is this response's tokens. PromptTokens counts every input
	// token, cached ones included.
	Usage ap.LLMUsage

	// Restarted reports that the call was served by restarting the agent
	// from a written session, not by continuing a live one.
	Restarted bool
}

// Text is the response's text blocks joined.
func (r *Response) Text() string {
	var s string
	for _, b := range r.Content {
		if b.Kind == transcript.BlockText {
			s += b.Text
		}
	}
	return s
}

// ToolCalls is the response's tool_use blocks.
func (r *Response) ToolCalls() []transcript.Block {
	var out []transcript.Block
	for _, b := range r.Content {
		if b.Kind == transcript.BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// DeltaKind is what a Delta adds to.
type DeltaKind string

const (
	DeltaText      DeltaKind = "text"
	DeltaReasoning DeltaKind = "reasoning"
	// DeltaToolCall starts a tool call (ToolID and ToolName set) or adds a
	// fragment of its JSON arguments (Text).
	DeltaToolCall DeltaKind = "tool_call"
)

// Delta is a piece of the response as it streams. Index is the content
// block it belongs to, and tool calls are numbered among themselves by
// ToolIndex, the way a streamed API response numbers them.
type Delta struct {
	Kind      DeltaKind `json:"kind"`
	Index     int       `json:"index"`
	ToolIndex int       `json:"tool_index,omitempty"`
	ToolID    string    `json:"tool_id,omitempty"`
	ToolName  string    `json:"tool_name,omitempty"`
	Text      string    `json:"text,omitempty"`
}

// Capabilities says which Request settings a model honours.
type Capabilities struct {
	Tools     bool
	Images    bool
	Reasoning bool // reasoning is returned, signed, and sent back
	Effort    bool
	MaxTokens bool
	// ExactContinue reports that a continued call sends the model exactly
	// what an API call would. A restart may not; see the model's doc.
	ExactContinue bool
}

// ErrUnsupported is returned for a Request the model cannot honour.
var ErrUnsupported = errors.New("llm: unsupported by this agent")

// userText is a user entry of text.
func userText(text string) transcript.Entry {
	return transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: text}}}
}
