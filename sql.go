package agentprotocol

import "database/sql/driver"

// Optional database integration.
//
// The lifecycle enums are plain strings, but drivers that use the extended
// query protocol (pgx in exec mode, for example) will not encode a named
// string type unless it implements driver.Valuer. Anyone persisting run state
// needs these, so they ship with the types rather than being redefined by each
// consumer. database/sql/driver is stdlib; this adds no dependencies.
//
// Nothing else in this package imports it. Delete this file and the package
// still describes the protocol completely.

func (v AgentRunState) Value() (driver.Value, error)        { return string(v), nil }
func (v InterruptReason) Value() (driver.Value, error)      { return string(v), nil }
func (v InterruptStatus) Value() (driver.Value, error)      { return string(v), nil }
func (v InterruptResolution) Value() (driver.Value, error)  { return string(v), nil }
func (v ToolType) Value() (driver.Value, error)             { return string(v), nil }
func (v ToolInvocationStatus) Value() (driver.Value, error) { return string(v), nil }
func (v ToolFinishStatus) Value() (driver.Value, error)     { return string(v), nil }
