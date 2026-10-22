// Package codex maps what codex's app-server reports about usage onto the
// agentprotocol types: the account from account/read, the plan's rate limits
// from account/rateLimits/read and account/rateLimits/updated, a turn's
// tokens and the context window's fill from thread/tokenUsage/updated. The
// codex session driver and Prober share it.
package codex

import (
	"context"
	"fmt"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/codexapp"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// Window IDs: codex reports a plan's limits as a primary and a secondary
// window, positions rather than lengths.
const (
	WindowPrimary   = "primary"
	WindowSecondary = "secondary"
)

// MainLimit is the limit ID of codex's own allowance. Other IDs are limits of
// particular models, reported beside it.
const MainLimit = "codex"

// Account is the account account/read reports.
func Account(r codexapp.GetAccountResponse) ap.AgentAccount {
	if r.Account == nil {
		if r.RequiresOpenaiAuth {
			return ap.AgentAccount{Status: ap.AccountUnauthenticated}
		}
		// A provider that needs no OpenAI login (a local or custom one).
		return ap.AgentAccount{Status: ap.AccountAuthenticated}
	}
	switch r.Account.Type {
	case codexapp.AccountTypeChatgpt:
		a, _ := r.Account.AsChatgptAccount()
		out := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: string(a.PlanType), PlanLabel: PlanLabel(a.PlanType)}
		if a.Email != nil {
			out.Email = *a.Email
		}
		return out
	case codexapp.AccountTypeApiKey:
		return ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}
	case codexapp.AccountTypeAmazonBedrock:
		return ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountCloud, PlanLabel: "Amazon Bedrock"}
	}
	return ap.AgentAccount{Status: ap.AccountAuthenticated}
}

// PlanLabel is a ChatGPT plan's display name.
func PlanLabel(p codexapp.PlanType) string {
	switch p {
	case codexapp.PlanTypeFree:
		return "ChatGPT Free"
	case codexapp.PlanTypeGo:
		return "ChatGPT Go"
	case codexapp.PlanTypePlus:
		return "ChatGPT Plus"
	case codexapp.PlanTypePro:
		return "ChatGPT Pro 20x"
	case codexapp.PlanTypeProlite:
		return "ChatGPT Pro 5x"
	case codexapp.PlanTypeTeam:
		return "ChatGPT Team"
	case codexapp.PlanTypeSelfServeBusinessProlite, codexapp.PlanTypeSelfServeBusinessUsageBased, codexapp.PlanTypeBusiness:
		return "ChatGPT Business"
	case codexapp.PlanTypeEnterpriseCbpAutomation, codexapp.PlanTypeEnterpriseCbpUsageBased, codexapp.PlanTypeEnterprise:
		return "ChatGPT Enterprise"
	case codexapp.PlanTypeEdu, codexapp.PlanTypeEduPlus, codexapp.PlanTypeEduPro:
		return "ChatGPT Edu"
	case "":
		return ""
	}
	return "ChatGPT"
}

// Windows maps a snapshot of codex's main allowance. A snapshot of another
// limit maps to nothing, so a model's limit never replaces the main rows.
// The windows are positions; when codex leaves out a window's length, paid
// plans have a five-hour primary and a weekly secondary, and Free and Go one
// monthly primary.
func Windows(s codexapp.RateLimitSnapshot) []ap.UsageWindow {
	if s.LimitID != nil && *s.LimitID != MainLimit {
		return nil
	}
	monthly := s.PlanType != nil && (*s.PlanType == codexapp.PlanTypeFree || *s.PlanType == codexapp.PlanTypeGo)
	primaryMinutes := 5 * 60
	if monthly {
		primaryMinutes = 30 * 24 * 60
	}
	var ws []ap.UsageWindow
	for _, p := range []struct {
		id      string
		w       *codexapp.RateLimitWindow
		minutes int
	}{{WindowPrimary, s.Primary, primaryMinutes}, {WindowSecondary, s.Secondary, 7 * 24 * 60}} {
		if p.w == nil {
			continue
		}
		minutes := p.minutes
		if p.w.WindowDurationMins != nil {
			minutes = int(*p.w.WindowDurationMins)
		}
		kind := usage.KindForDuration(minutes)
		w := ap.UsageWindow{ID: p.id, Kind: kind, Label: kindLabel(kind), UsedPercent: float64(p.w.UsedPercent), DurationMinutes: minutes}
		if p.w.ResetsAt != nil {
			w.ResetsAt = time.Unix(*p.w.ResetsAt, 0).UTC()
		}
		ws = append(ws, w)
	}
	return ws
}

func kindLabel(k ap.UsageWindowKind) string {
	switch k {
	case ap.UsageWindowSession:
		return "5-hour"
	case ap.UsageWindowWeekly:
		return "Weekly"
	case ap.UsageWindowMonthly:
		return "Monthly"
	}
	return "Limit"
}

// Limits maps account/rateLimits/read, preferring the main allowance by its
// ID: the legacy snapshot can name another limit.
func Limits(r codexapp.GetAccountRateLimitsResponse, now time.Time) *ap.UsageLimits {
	s := r.RateLimits
	if main, ok := r.RateLimitsByLimitID[MainLimit]; ok {
		s = main
	}
	return usage.Limits(Windows(s), now)
}

// Update maps account/rateLimits/updated. codex sends it as a partial
// snapshot, so the windows it names update the held ones and the rest stay
// (usage.State.ApplyUpdate). ok is false when it names none of the main
// allowance's windows.
func Update(n codexapp.AccountRateLimitsUpdatedNotification) (ap.LimitsUpdatedPayload, bool) {
	ws := Windows(n.RateLimits)
	return ap.LimitsUpdatedPayload{Windows: ws}, len(ws) > 0
}

// Turns turns thread/tokenUsage/updated notifications into per-turn usage.
// Each carries the thread's running total and the last model call's usage;
// a turn's usage is how much the total grew. A resumed thread's first total
// includes history, so until a total has been seen the turn adds up the
// calls' last usage instead, as it does if the total ever goes down.
type Turns struct {
	total *codexapp.TokenUsageBreakdown
	turn  codexapp.TokenUsageBreakdown
}

// Fresh marks the thread as new, with nothing used before.
func (t *Turns) Fresh() { t.total = &codexapp.TokenUsageBreakdown{} }

// Add takes a notification for the running turn.
func (t *Turns) Add(u codexapp.ThreadTokenUsage) {
	if t.total != nil && u.Total.TotalTokens >= t.total.TotalTokens {
		t.turn = add(t.turn, sub(u.Total, *t.total))
	} else {
		t.turn = add(t.turn, u.Last)
	}
	total := u.Total
	t.total = &total
}

// End is the usage of the turn that just ended; the next turn starts from
// zero.
func (t *Turns) End(status ap.TurnUsageStatus) ap.UsageUpdatedPayload {
	u := t.turn
	t.turn = codexapp.TokenUsageBreakdown{}
	// codex's input counts cached tokens; reasoning is part of output.
	return ap.UsageUpdatedPayload{
		PromptTokens:        int(u.InputTokens),
		CompletionTokens:    int(u.OutputTokens),
		TotalTokens:         int(u.InputTokens + u.OutputTokens),
		CachedInputTokens:   int(min(u.CachedInputTokens, u.InputTokens)),
		CacheCreationTokens: int(deref(u.CacheWriteInputTokens)),
		ReasoningTokens:     int(min(u.ReasoningOutputTokens, u.OutputTokens)),
		Status:              status,
	}
}

func add(a, b codexapp.TokenUsageBreakdown) codexapp.TokenUsageBreakdown {
	w := deref(a.CacheWriteInputTokens) + deref(b.CacheWriteInputTokens)
	return codexapp.TokenUsageBreakdown{
		InputTokens: a.InputTokens + b.InputTokens, CachedInputTokens: a.CachedInputTokens + b.CachedInputTokens,
		CacheWriteInputTokens: &w, OutputTokens: a.OutputTokens + b.OutputTokens,
		ReasoningOutputTokens: a.ReasoningOutputTokens + b.ReasoningOutputTokens, TotalTokens: a.TotalTokens + b.TotalTokens,
	}
}

func sub(a, b codexapp.TokenUsageBreakdown) codexapp.TokenUsageBreakdown {
	w := max(deref(a.CacheWriteInputTokens)-deref(b.CacheWriteInputTokens), 0)
	return codexapp.TokenUsageBreakdown{
		InputTokens: max(a.InputTokens-b.InputTokens, 0), CachedInputTokens: max(a.CachedInputTokens-b.CachedInputTokens, 0),
		CacheWriteInputTokens: &w, OutputTokens: max(a.OutputTokens-b.OutputTokens, 0),
		ReasoningOutputTokens: max(a.ReasoningOutputTokens-b.ReasoningOutputTokens, 0), TotalTokens: max(a.TotalTokens-b.TotalTokens, 0),
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Context is the window's fill after the last model call: everything that
// call read and wrote, against the model's window.
func Context(u codexapp.ThreadTokenUsage) ap.ContextUpdatedPayload {
	return ap.ContextUpdatedPayload{UsedTokens: int(u.Last.TotalTokens), MaxTokens: int(deref(u.ModelContextWindow))}
}

// ProbeTimeout bounds a probe.
const ProbeTimeout = 10 * time.Second

// Prober reads the account and limits from a short-lived app-server:
// initialize, account/read, account/rateLimits/read. It starts no thread, so
// no model is called.
type Prober struct {
	// ClientInfo names the caller to codex. Empty uses "agentprotocol".
	ClientInfo codexapp.ClientInfo
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	info := p.ClientInfo
	if info.Name == "" {
		info = codexapp.ClientInfo{Name: "agentprotocol", Version: "1"}
	}
	proc, err := codexapp.Spawn(ctx, codexapp.ProcessConfig{Command: env.Binary, Args: env.Args, Dir: env.Dir, Env: env.Env}, info, codexapp.Handler{})
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	defer func() { _ = proc.Close() }()
	var acct codexapp.GetAccountResponse
	if err := proc.Call(ctx, codexapp.MethodAccountRead, codexapp.GetAccountParams{}, &acct); err != nil {
		return ap.UsageSnapshot{}, fmt.Errorf("codex: account/read: %w", err)
	}
	a := Account(acct)
	snap := ap.UsageSnapshot{Account: &a}
	now := time.Now()
	if a.Status != ap.AccountAuthenticated || a.Type != ap.AccountSubscription {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "plan rate limits apply only to a ChatGPT login", now)
		return snap, nil
	}
	var rl codexapp.GetAccountRateLimitsResponse
	if err := proc.Call(ctx, codexapp.MethodAccountRateLimitsRead, nil, &rl); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	if rl.AccountID != nil {
		a.Workspace = *rl.AccountID
	}
	snap.Limits = Limits(rl, now)
	return snap, nil
}
