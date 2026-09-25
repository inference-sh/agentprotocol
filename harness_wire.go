package agentprotocol

// Harness vocabulary the api and belt's daemon exchange about coding agents.
// Declared here so the wire types can alias them; the harness and transcript
// packages re-export them under their own names.

// HarnessID is a harness registry id: a key of harness.All and a
// Harness.Name ("claude", "codex", ...). Profiles and sessions carry it.
type HarnessID string

// DriverKind names a session driver. The driver package's backends report
// it as their Kind.
type DriverKind string

const (
	DriverACP        DriverKind = "acp"
	DriverClaudeCode DriverKind = "claude-code"
	DriverCodex      DriverKind = "codex"
	DriverPi         DriverKind = "pi"
)

// SupportLevel is what harness.Support concluded about an installed version.
type SupportLevel string

const (
	// SupportLevelSupported: the version is inside the tested range.
	SupportLevelSupported SupportLevel = "supported"
	// SupportLevelNewerThanTested: newer than anything tested. Likely fine; warn.
	SupportLevelNewerThanTested SupportLevel = "newer-than-tested"
	// SupportLevelOlderThanTested: older than anything tested but not below
	// Harness.Requires. It may work; warn.
	SupportLevelOlderThanTested SupportLevel = "older-than-tested"
	// SupportLevelOlderThanSupported: below Harness.Requires, so something the
	// driver needs is missing. Do not drive it; show Reason and UpgradeCmd.
	SupportLevelOlderThanSupported SupportLevel = "older-than-supported"
	// SupportLevelUnknown: the version could not be read, the agent is not in
	// the registry, or nothing about it has been tested. Warn.
	SupportLevelUnknown SupportLevel = "unknown"
)

// LiveState is whether a process is using a harness session right now.
type LiveState string

const (
	// LiveUnknown: nothing available could tell.
	LiveUnknown LiveState = "unknown"
	// LiveIdle: no process is using the session.
	LiveIdle LiveState = "idle"
	// LiveActive: a process is, or may be, using the session.
	LiveActive LiveState = "active"
)

// Evidence is what a SessionLiveness answer rests on.
type Evidence string

const (
	// EvidenceLockFile: the agent's own in-use marker for this session names
	// a running process. Proof.
	EvidenceLockFile Evidence = "lock-file"
	// EvidenceOpenFile: a process holds the session's own file or directory
	// open. Proof.
	EvidenceOpenFile Evidence = "open-file"
	// EvidenceNoProcess: no process of the agent is running. Proof of idle.
	EvidenceNoProcess Evidence = "no-process"
	// EvidenceProcessInCwd: a process of the agent runs in the session's
	// directory. It may be serving another session there. Heuristic.
	EvidenceProcessInCwd Evidence = "process-in-cwd"
	// EvidenceNoProcessInCwd: the agent runs, but not in the session's
	// directory. Heuristic: agents rarely serve a session from elsewhere.
	EvidenceNoProcessInCwd Evidence = "no-process-in-cwd"
	// EvidenceHeldElsewhere: every agent process in the session's directory
	// holds another session by the agent's own in-use markers. Proof of idle.
	EvidenceHeldElsewhere Evidence = "held-elsewhere"
	// EvidenceRecentWrite: the session was written within the recent window.
	// Heuristic.
	EvidenceRecentWrite Evidence = "recent-write"
	// EvidenceNone: nothing to go on.
	EvidenceNone Evidence = "none"
)

// SessionLiveness is whether a harness session is in use, and why that is
// believed.
type SessionLiveness struct {
	State    LiveState `json:"state"`
	Evidence Evidence  `json:"evidence"`
	// Heuristic is true when the answer is inferred rather than proven.
	Heuristic bool `json:"heuristic"`
	// PID is the process the evidence points at, when there is one.
	PID    int    `json:"pid,omitempty"`
	Detail string `json:"detail,omitempty"`
}
