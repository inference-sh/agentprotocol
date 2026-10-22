package model

import "time"

// The usage model: what a turn cost in tokens, how full the context is, how
// much of a plan's rate limits is used, and which account an agent runs as.
// Every agent reports these in a format of its own; the usage package and
// its per-agent packages map them onto these types, and the drivers emit
// them as events.

// TurnUsageStatus says how much of a turn's usage the agent reported.
type TurnUsageStatus string

const (
	// TurnUsageComplete is the whole turn, as the agent counted it.
	TurnUsageComplete TurnUsageStatus = "complete"
	// TurnUsagePartial is what was reported before the turn ended early
	// (interrupted, failed).
	TurnUsagePartial TurnUsageStatus = "partial"
)

// ContextUpdatedPayload is how full the agent's context window is after its
// latest model call: the tokens the model was given plus what it wrote,
// against the window of the model in use. MaxTokens is 0 when the agent does
// not say.
type ContextUpdatedPayload struct {
	UsedTokens int `json:"used_tokens"`
	MaxTokens  int `json:"max_tokens,omitempty"`
}

// UsageWindowKind is the period a rate-limit window covers.
type UsageWindowKind string

const (
	UsageWindowSession UsageWindowKind = "session" // a rolling window of hours (Claude's five-hour limit)
	UsageWindowWeekly  UsageWindowKind = "weekly"
	UsageWindowMonthly UsageWindowKind = "monthly"
	UsageWindowOther   UsageWindowKind = "other"
)

// UsageWindow is one rate limit of a plan and how much of it is used.
type UsageWindow struct {
	// ID is stable per agent ("five_hour", "seven_day", "primary"), so a
	// pushed update lands on the window a probe reported.
	ID          string          `json:"id"`
	Kind        UsageWindowKind `json:"kind"`
	Label       string          `json:"label"`
	UsedPercent float64         `json:"used_percent"` // 0 to 100
	// ResetsAt is when the window resets; zero when the agent does not say.
	ResetsAt time.Time `json:"resets_at,omitzero"`
	// DurationMinutes is the window's length; 0 when unknown.
	DurationMinutes int `json:"duration_minutes,omitempty"`
}

// UsageUnavailableReason says why an agent has no rate limits to show.
type UsageUnavailableReason string

const (
	// UsageUnsupported: the account has no plan limits to read (an API key
	// bills per token) or the agent exposes none.
	UsageUnsupported UsageUnavailableReason = "unsupported"
	// UsageProbeFailed: reading them failed this time.
	UsageProbeFailed UsageUnavailableReason = "probe_failed"
)

// UsageLimits is an agent's rate-limit windows at CheckedAt.
type UsageLimits struct {
	CheckedAt time.Time     `json:"checked_at"`
	Windows   []UsageWindow `json:"windows"`
	// Unavailable is set when there are no windows to show, and why.
	Unavailable *UsageUnavailable `json:"unavailable,omitempty"`
}

// UsageUnavailable explains an empty UsageLimits.
type UsageUnavailable struct {
	Reason  UsageUnavailableReason `json:"reason"`
	Message string                 `json:"message,omitempty"`
}

// LimitsUpdatedPayload carries rate-limit windows an agent pushed during a
// session. It is sparse: it holds the windows that changed, which replace the
// windows with the same ID and leave the others as they were.
type LimitsUpdatedPayload struct {
	Windows []UsageWindow `json:"windows"`
}

// AccountStatus is whether an agent is signed in.
type AccountStatus string

const (
	AccountAuthenticated   AccountStatus = "authenticated"
	AccountUnauthenticated AccountStatus = "unauthenticated"
	AccountUnknown         AccountStatus = "unknown"
)

// AccountType is how an agent is signed in.
type AccountType string

const (
	AccountSubscription AccountType = "subscription" // a vendor plan (Claude Pro/Max, ChatGPT Plus/Pro)
	AccountAPIKey       AccountType = "api_key"
	AccountCloud        AccountType = "cloud" // a cloud provider's credentials (Bedrock, Vertex)
)

// AgentAccount is the account an agent runs as, without its credential.
type AgentAccount struct {
	Status AccountStatus `json:"status"`
	Type   AccountType   `json:"type,omitempty"`
	// Plan is the vendor's plan id as the agent reports it ("max",
	// "claude_max_20x", "plus"); PlanLabel is its display name ("Max 20x").
	Plan      string `json:"plan,omitempty"`
	PlanLabel string `json:"plan_label,omitempty"`
	Email     string `json:"email,omitempty"`
	// Workspace is the organization or workspace the account belongs to,
	// when the agent says.
	Workspace string `json:"workspace,omitempty"`
}

// AccountUpdatedPayload is the account an agent reported during a session.
type AccountUpdatedPayload struct {
	Account AgentAccount `json:"account"`
}

// UsageSnapshot is what a probe reads about an agent outside a session.
type UsageSnapshot struct {
	Account *AgentAccount `json:"account,omitempty"`
	Limits  *UsageLimits  `json:"limits,omitempty"`
}
