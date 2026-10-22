// Package claude maps what Claude Code reports about usage onto the
// agentprotocol types: the account from initialize, the plan's rate limits
// from get_usage and rate_limit_event, a turn's tokens from its result, and
// the context window's fill from the assistant messages and modelUsage. The
// claude session driver and Prober share it.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/protocol/claudecode"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// Window IDs. Scoped weekly windows are "seven_day_" and a slug of the
// models they apply to.
const (
	WindowFiveHour = "five_hour"
	WindowSevenDay = "seven_day"
)

// Account is the account initialize reports. The CLI reports no token
// source and no key when nothing is signed in; it also leaves fields out for
// a login it does not describe (an external profile, an old CLI), so only
// that combination is taken as signed out. A cloud backend authenticates
// outside the CLI and always counts as signed in.
func Account(a claudecode.AccountInfo) ap.AgentAccount {
	out := ap.AgentAccount{Email: a.Email, Workspace: a.Organization, Plan: a.SubscriptionType, PlanLabel: PlanLabel(a.SubscriptionType)}
	switch {
	case a.APIProvider != "" && a.APIProvider != "firstParty":
		out.Status, out.Type, out.PlanLabel = ap.AccountAuthenticated, ap.AccountCloud, cloudLabel(a.APIProvider)
	case isAPIKey(a.TokenSource) || (a.APIKeySource != "" && a.APIKeySource != "none"):
		out.Status, out.Type = ap.AccountAuthenticated, ap.AccountAPIKey
	case a.TokenSource != "none" || a.Email != "" || a.SubscriptionType != "":
		out.Status, out.Type = ap.AccountAuthenticated, ap.AccountSubscription
	default:
		out.Status = ap.AccountUnauthenticated
	}
	return out
}

func isAPIKey(tokenSource string) bool {
	switch normalized(tokenSource) {
	case "apikey", "anthropicapikey", "anthropicauthtoken":
		return true
	}
	return false
}

func cloudLabel(provider string) string {
	switch provider {
	case "bedrock", "anthropicAws":
		return "Amazon Bedrock"
	case "vertex", "anthropicGoogleCloud":
		return "Google Vertex AI"
	case "foundry":
		return "Microsoft Foundry"
	case "gateway":
		return "Gateway"
	}
	return provider
}

// PlanLabel is a subscription type's display name: claude_max_20x and max20
// are both "Max 20x". An unknown type is title-cased.
func PlanLabel(subscriptionType string) string {
	switch normalized(subscriptionType) {
	case "":
		return ""
	case "claudemaxsubscription", "max", "maxplan":
		return "Max"
	case "claudemax5xsubscription", "max5":
		return "Max 5x"
	case "claudemax20xsubscription", "max20":
		return "Max 20x"
	case "claudeenterprisesubscription", "enterprise":
		return "Enterprise"
	case "claudeteamsubscription", "team":
		return "Team"
	case "claudeprosubscription", "pro":
		return "Pro"
	case "claudefreesubscription", "free":
		return "Free"
	}
	var words []string
	for _, w := range strings.FieldsFunc(subscriptionType, func(r rune) bool { return r == '_' || r == '-' || r == ' ' }) {
		words = append(words, strings.ToUpper(w[:1])+strings.ToLower(w[1:]))
	}
	return strings.Join(words, " ")
}

func normalized(s string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(s))
}

// Limits maps a get_usage answer. The five-hour and weekly windows and the
// weekly windows scoped to models are kept; seven_day_opus and
// seven_day_sonnet are older names for scoped windows and are left out, as
// are extra-usage credits, which are spend rather than a window.
func Limits(r claudecode.GetUsageResponse, now time.Time) *ap.UsageLimits {
	if !r.RateLimitsAvailable || r.RateLimits == nil {
		return usage.Unavailable(ap.UsageUnsupported, "plan rate limits do not apply to this login", now)
	}
	var ws []ap.UsageWindow
	add := func(id string, kind ap.UsageWindowKind, label string, minutes int, w *claudecode.UsageWindow) {
		if w == nil || w.Utilization == nil {
			return
		}
		ws = append(ws, ap.UsageWindow{ID: id, Kind: kind, Label: label, UsedPercent: *w.Utilization, ResetsAt: isoTime(w.ResetsAt), DurationMinutes: minutes})
	}
	add(WindowFiveHour, ap.UsageWindowSession, "5-hour", 5*60, r.RateLimits.FiveHour)
	add(WindowSevenDay, ap.UsageWindowWeekly, "Weekly", 7*24*60, r.RateLimits.SevenDay)
	for _, s := range r.RateLimits.ModelScoped {
		if s.Utilization == nil || s.DisplayName == "" {
			continue
		}
		ws = append(ws, ap.UsageWindow{ID: ScopedWindowID(s.DisplayName), Kind: ap.UsageWindowWeekly, Label: "Weekly · " + s.DisplayName,
			UsedPercent: *s.Utilization, ResetsAt: isoTime(s.ResetsAt), DurationMinutes: 7 * 24 * 60})
	}
	return usage.Limits(ws, now)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// ScopedWindowID is the ID of a weekly window scoped to the named models.
func ScopedWindowID(displayName string) string {
	return WindowSevenDay + "_" + strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(displayName), "_"), "_")
}

func isoTime(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Update maps a rate_limit_event onto the window it reports. Only the
// five-hour and weekly windows are mapped: the event names a scoped window by
// a type that does not say which models it covers, so it cannot be matched
// to a get_usage row. ok is false for an event that carries no usage.
func Update(info claudecode.RateLimitInfo) (ap.LimitsUpdatedPayload, bool) {
	if info.Utilization == nil {
		return ap.LimitsUpdatedPayload{}, false
	}
	w := ap.UsageWindow{UsedPercent: *info.Utilization * 100}
	switch info.RateLimitType {
	case WindowFiveHour:
		w.ID, w.Kind, w.Label, w.DurationMinutes = WindowFiveHour, ap.UsageWindowSession, "5-hour", 5*60
	case WindowSevenDay:
		w.ID, w.Kind, w.Label, w.DurationMinutes = WindowSevenDay, ap.UsageWindowWeekly, "Weekly", 7*24*60
	default:
		return ap.LimitsUpdatedPayload{}, false
	}
	if info.ResetsAt > 0 {
		w.ResetsAt = time.Unix(info.ResetsAt, 0).UTC()
	}
	return ap.LimitsUpdatedPayload{Windows: []ap.UsageWindow{w}}, true
}

// Turns turns results into per-turn usage. A result's usage is its turn's,
// but its cost and modelUsage run for the whole session, so the turn's share
// is the difference from the previous result. A session that resumed starts
// from the total its transcript saved, so the first difference can include
// earlier turns; the CLI documents its total as an estimate in any case.
type Turns struct {
	cost     float64
	thinking int
}

// Next is the usage of the turn res ends.
func (t *Turns) Next(res claudecode.ResultMessage) ap.UsageUpdatedPayload {
	u := res.Usage
	in := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	out := ap.UsageUpdatedPayload{
		PromptTokens:        in,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         in + u.OutputTokens,
		CachedInputTokens:   u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
		Status:              ap.TurnUsageComplete,
	}
	if res.Subtype != "success" {
		out.Status = ap.TurnUsagePartial
	}
	thinking := 0
	for _, m := range res.ModelUsage {
		thinking += m.ThinkingTokens
	}
	if d := thinking - t.thinking; d > 0 {
		out.ReasoningTokens = min(d, out.CompletionTokens)
	}
	t.thinking = thinking
	// A /clear resets the running total, so a smaller total starts over.
	if res.TotalCostUSD >= t.cost {
		out.CostUSD = res.TotalCostUSD - t.cost
	} else {
		out.CostUSD = res.TotalCostUSD
	}
	t.cost = res.TotalCostUSD
	return out
}

// ContextUsed is how much of the window a model call took: what it was given,
// cached or not, and what it wrote. Only the main agent's calls count; a
// subagent has a window of its own.
func ContextUsed(u claudecode.Usage) int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens
}

// ContextWindow is the window of the model a result names, from modelUsage;
// 0 when it is not there.
func ContextWindow(res claudecode.ResultMessage, model string) int {
	if m, ok := res.ModelUsage[model]; ok {
		return m.ContextWindow
	}
	// A single model is the one in use whatever it was called.
	if len(res.ModelUsage) == 1 {
		for _, m := range res.ModelUsage {
			return m.ContextWindow
		}
	}
	return 0
}

// RateLimitEvent decodes a rate_limit_event line.
func RateLimitEvent(raw json.RawMessage) (claudecode.RateLimitInfo, error) {
	var e claudecode.RateLimitEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return claudecode.RateLimitInfo{}, fmt.Errorf("claude: rate_limit_event: %w", err)
	}
	return e.RateLimitInfo, nil
}

// ProbeTimeout bounds a probe. A cloud backend can take long to initialize
// (Bedrock runs its credential refresh first).
const ProbeTimeout = 25 * time.Second

// Prober reads the account and limits by starting the CLI in stream-json
// mode and asking: initialize answers with the account, get_usage with the
// limits. It sends no prompt, so no session is created and no model is
// called, and it starts the CLI with every hook and MCP server off so a probe
// runs nothing of the user's.
type Prober struct{}

func (Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	opts := claudecode.Options{
		Command:   env.Binary,
		Dir:       env.Dir,
		Env:       env.Env,
		ExtraArgs: append(append([]string(nil), env.Args...), "--settings", `{"disableAllHooks":true}`, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`),
	}
	p, err := claudecode.Spawn(opts, claudecode.Handler{})
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	defer func() { _ = p.Kill() }()
	init, err := p.Initialize(ctx, claudecode.InitializeRequest{Subtype: claudecode.SubtypeInitialize})
	if err != nil {
		return ap.UsageSnapshot{}, fmt.Errorf("claude: initialize: %w", err)
	}
	acct := Account(init.Account)
	snap := ap.UsageSnapshot{Account: &acct}
	now := time.Now()
	if acct.Status != ap.AccountAuthenticated || acct.Type != ap.AccountSubscription {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "plan rate limits apply only to a claude.ai login", now)
		return snap, nil
	}
	res, err := p.GetUsage(ctx)
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	snap.Limits = Limits(res, now)
	return snap, nil
}
