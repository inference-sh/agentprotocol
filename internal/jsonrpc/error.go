// Package jsonrpc holds what the module's JSON-RPC 2.0 clients share.
package jsonrpc

import "encoding/json"

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error renders the message and, when the peer attached one, its data.
//
// Agents put their real explanation in data and a bare category in message:
// gemini answers a failed load with message "Internal error" and data that
// says the session id was not found, where it looked, and to run
// --list-sessions. A formatter that printed only the message would discard
// the one sentence that identifies the cause, which is a debugging session
// nobody should have to repeat. Data is raw JSON; it is appended verbatim
// rather than parsed, because its shape is the peer's to decide.
func (e *Error) Error() string {
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return e.Message
	}
	return e.Message + ": " + string(e.Data)
}
