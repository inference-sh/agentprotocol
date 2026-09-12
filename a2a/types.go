// Package a2a holds the Agent2Agent wire format and its mapping to and from
// the lifecycle model in the parent package.
//
// Wire types only: nothing here reads a database, opens a connection, or knows
// what a server is. The mapping functions are total and pure, so a server that
// speaks A2A and a client that calls one can share them.
//
// Spec: https://a2a-protocol.org/latest/specification/ v1.0
package a2a

import (
	"encoding/json"
	"time"
)

// Version is the protocol version this package implements.
const Version = "1.0"

// HeaderVersion is the HTTP header used for version negotiation.
const HeaderVersion = "A2A-Version"

// JSON-RPC method names, spec section 9.
const (
	MethodSendMessage          = "a2a/SendMessage"
	MethodSendStreamingMessage = "a2a/SendStreamingMessage"
	MethodGetTask              = "a2a/GetTask"
	MethodListTasks            = "a2a/ListTasks"
	MethodCancelTask           = "a2a/CancelTask"
	MethodSubscribeToTask      = "a2a/SubscribeToTask"
)

// A2A-specific JSON-RPC error codes.
const (
	ErrCodeTaskNotFound        = -32100
	ErrCodeTaskNotCancelable   = -32101
	ErrCodeVersionNotSupported = -32102
)

// TaskState is the lifecycle state of an A2A task.
type TaskState string

const (
	TaskStateSubmitted     TaskState = "submitted"
	TaskStateWorking       TaskState = "working"
	TaskStateInputRequired TaskState = "input_required"
	TaskStateCompleted     TaskState = "completed"
	TaskStateFailed        TaskState = "failed"
	TaskStateCanceled      TaskState = "canceled"
	TaskStateRejected      TaskState = "rejected"
	TaskStateAuthRequired  TaskState = "auth_required"
)

// IsTerminal reports whether no further work will happen on the task.
func (s TaskState) IsTerminal() bool {
	return s == TaskStateCompleted || s == TaskStateFailed ||
		s == TaskStateCanceled || s == TaskStateRejected
}

// Task is an A2A unit of work.
type Task struct {
	ID        string          `json:"id"`
	ContextID string          `json:"contextId"`
	Status    TaskStatus      `json:"status"`
	History   []Message       `json:"history,omitempty"`
	Artifacts []Artifact      `json:"artifacts,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// TaskStatus is the current state, an optional explanatory message, and when
// it was observed.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp"`
}

// Message is exchanged between agents or with a user.
type Message struct {
	MessageID string          `json:"messageId"`
	Role      MessageRole     `json:"role"`
	Parts     []Part          `json:"parts"`
	ContextID string          `json:"contextId,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// MessageRole distinguishes the sender.
type MessageRole string

const (
	MessageRoleUser  MessageRole = "user"
	MessageRoleAgent MessageRole = "agent"
)

// Part is one content element of a Message. Exactly one of Text, File or Data
// carries the content.
type Part struct {
	Text     string          `json:"text,omitempty"`
	File     *FilePart       `json:"file,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// FilePart is a file attachment, either by reference or inline.
type FilePart struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Bytes    string `json:"bytes,omitempty"`
}

// Artifact is an output the agent produced while working.
type Artifact struct {
	Name     string `json:"name,omitempty"`
	Parts    []Part `json:"parts"`
	Index    int    `json:"index"`
	Append   bool   `json:"append,omitempty"`
	LastPart bool   `json:"lastChunk,omitempty"`
}

// --- request and response params ---

// SendMessageParams is the params object for SendMessage and
// SendStreamingMessage.
type SendMessageParams struct {
	Message  Message         `json:"message"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// GetTaskParams is the params object for GetTask.
type GetTaskParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength,omitempty"`
}

// CancelTaskParams is the params object for CancelTask.
type CancelTaskParams struct {
	ID string `json:"id"`
}

// ListTasksParams is the params object for ListTasks.
type ListTasksParams struct {
	ContextID string `json:"contextId,omitempty"`
}

// SubscribeParams is the params object for SubscribeToTask.
type SubscribeParams struct {
	ID string `json:"id"`
}

// TaskListResult is the result of ListTasks.
type TaskListResult struct {
	Tasks []*Task `json:"tasks"`
}

// --- streaming ---

// SSE event names used by the streaming methods.
const (
	SSEEventTask         = "task"
	SSEEventStatusUpdate = "statusUpdate"
)

// TaskStatusUpdateEvent is the payload of a statusUpdate SSE event.
type TaskStatusUpdateEvent struct {
	ID     string     `json:"id"`
	Status TaskStatus `json:"status"`
}

// FormatTimestamp renders a time the way the A2A spec expects it.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
