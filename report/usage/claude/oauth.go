package claude

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// The claude.ai account API that a Claude Pro/Max OAuth token can read: the
// plan's windows (/api/oauth/usage, what Claude Code's /usage shows) and the
// account (/api/oauth/profile). Agents that sign in with a Claude login of
// their own (hermes, pi) read their limits here, mapped onto the same window
// IDs as get_usage so a window means the same thing whichever way it was
// read.
const (
	DefaultOAuthBase = "https://api.anthropic.com"
	oauthBeta        = "oauth-2025-04-20"
)

// OAuthUsageResponse is /api/oauth/usage, the fields a probe reads.
// Utilization is a percentage, 0 to 100. Since mid-2026 the per-model weekly
// buckets come as limits entries of kind weekly_scoped and seven_day_opus and
// seven_day_sonnet stay null.
type OAuthUsageResponse struct {
	FiveHour       *OAuthBucket `json:"five_hour"`
	SevenDay       *OAuthBucket `json:"seven_day"`
	SevenDayOpus   *OAuthBucket `json:"seven_day_opus"`
	SevenDaySonnet *OAuthBucket `json:"seven_day_sonnet"`
	Limits         []OAuthLimit `json:"limits"`
}

// OAuthBucket is one window of /api/oauth/usage.
type OAuthBucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// OAuthLimit is an entry of /api/oauth/usage's limits: kind session,
// weekly_all or weekly_scoped (with the models it covers in scope).
type OAuthLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// OAuthProfileResponse is /api/oauth/profile, the fields a probe reads.
type OAuthProfileResponse struct {
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		UUID             string `json:"uuid"`
		Name             string `json:"name"`
		OrganizationType string `json:"organization_type"` // claude_max, claude_pro, claude_team, claude_enterprise
		RateLimitTier    string `json:"rate_limit_tier"`   // default_claude_max_20x, ...
	} `json:"organization"`
}

// OAuthUsage reads the plan's windows with a Claude OAuth access token. base
// is the API's origin; empty uses DefaultOAuthBase.
func OAuthUsage(ctx context.Context, client *http.Client, base, accessToken string) ([]ap.UsageWindow, error) {
	var r OAuthUsageResponse
	if err := webjson.Get(ctx, client, oauthURL(base, "usage"), oauthHeader(accessToken), &r); err != nil {
		return nil, fmt.Errorf("claude: oauth usage: %w", err)
	}
	return OAuthWindows(r), nil
}

// OAuthProfile reads the account an OAuth token belongs to.
func OAuthProfile(ctx context.Context, client *http.Client, base, accessToken string) (OAuthProfileResponse, error) {
	var p OAuthProfileResponse
	if err := webjson.Get(ctx, client, oauthURL(base, "profile"), oauthHeader(accessToken), &p); err != nil {
		return OAuthProfileResponse{}, fmt.Errorf("claude: oauth profile: %w", err)
	}
	return p, nil
}

func oauthURL(base, path string) string {
	if base == "" {
		base = DefaultOAuthBase
	}
	return strings.TrimRight(base, "/") + "/api/oauth/" + path
}

func oauthHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, "anthropic-beta": oauthBeta, "Accept": "application/json"}
}

// OAuthWindows maps /api/oauth/usage onto get_usage's windows: five_hour,
// seven_day, and a seven_day_<models> window per scoped weekly limit. The
// legacy seven_day_opus and seven_day_sonnet buckets are the scoped windows
// for Opus and Sonnet (ScopedWindowID gives them the same IDs) and are used
// only when limits does not report those models.
func OAuthWindows(r OAuthUsageResponse) []ap.UsageWindow {
	var ws []ap.UsageWindow
	add := func(id string, kind ap.UsageWindowKind, label string, minutes int, pct *float64, resets *string) {
		if pct == nil || slices.ContainsFunc(ws, func(w ap.UsageWindow) bool { return w.ID == id }) {
			return
		}
		ws = append(ws, ap.UsageWindow{ID: id, Kind: kind, Label: label, UsedPercent: *pct, ResetsAt: isoTime(resets), DurationMinutes: minutes})
	}
	bucket := func(b *OAuthBucket, kind string) (*float64, *string) {
		if b != nil && b.Utilization != nil {
			return b.Utilization, b.ResetsAt
		}
		for _, l := range r.Limits {
			if l.Kind == kind && l.Percent != nil {
				return l.Percent, l.ResetsAt
			}
		}
		return nil, nil
	}
	pct, resets := bucket(r.FiveHour, "session")
	add(WindowFiveHour, ap.UsageWindowSession, "5-hour", 5*60, pct, resets)
	pct, resets = bucket(r.SevenDay, "weekly_all")
	add(WindowSevenDay, ap.UsageWindowWeekly, "Weekly", 7*24*60, pct, resets)
	scoped := func(name string, pct *float64, resets *string) {
		add(ScopedWindowID(name), ap.UsageWindowWeekly, "Weekly · "+name, 7*24*60, pct, resets)
	}
	for _, l := range r.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil || strings.TrimSpace(l.Scope.Model.DisplayName) == "" {
			continue
		}
		scoped(strings.TrimSpace(l.Scope.Model.DisplayName), l.Percent, l.ResetsAt)
	}
	if b := r.SevenDayOpus; b != nil {
		scoped("Opus", b.Utilization, b.ResetsAt)
	}
	if b := r.SevenDaySonnet; b != nil {
		scoped("Sonnet", b.Utilization, b.ResetsAt)
	}
	return ws
}

// ProfileAccount is the account a profile names. The organization type is
// the plan (claude_max is "max", as Claude Code reports it); the rate-limit
// tier tells Max 5x from Max 20x.
func ProfileAccount(p OAuthProfileResponse) ap.AgentAccount {
	plan := strings.TrimPrefix(p.Organization.OrganizationType, "claude_")
	label := PlanLabel(plan)
	switch tier := p.Organization.RateLimitTier; {
	case strings.Contains(tier, "max_20x"):
		label = PlanLabel("max20")
	case strings.Contains(tier, "max_5x"):
		label = PlanLabel("max5")
	}
	return ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: plan, PlanLabel: label,
		Email: p.Account.Email, Workspace: p.Organization.Name}
}

// IsOAuthToken reports whether an Anthropic credential is a Claude login
// (an OAuth access token or a claude setup-token) rather than a Console API
// key.
func IsOAuthToken(token string) bool {
	return (strings.HasPrefix(token, "sk-ant-") && !strings.HasPrefix(token, "sk-ant-api")) ||
		strings.HasPrefix(token, "eyJ") || strings.HasPrefix(token, "cc-")
}

// OAuthSnapshot is the account and windows of a Claude login another agent
// holds: the profile for the account, /api/oauth/usage for the windows, or
// the quota check (QuotaCheck) for a token that may not read them. A
// token past expires (zero for unknown) is not used, since refreshing it
// would rotate the refresh token out from under the agent; the limits read
// as probe_failed until the agent refreshes it.
func OAuthSnapshot(ctx context.Context, client *http.Client, base, accessToken string, expires, now time.Time) ap.UsageSnapshot {
	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}
	snap := ap.UsageSnapshot{Account: &acct}
	if !expires.IsZero() && !expires.After(now) {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "the saved Claude login has expired; the agent refreshes it on its next run", now)
		return snap
	}
	// A setup-token cannot read the profile; the windows are still worth
	// reading without it.
	if p, err := OAuthProfile(ctx, client, base, accessToken); err == nil {
		acct = ProfileAccount(p)
	}
	ws, err := OAuthUsage(ctx, client, base, accessToken)
	if se := (*webjson.StatusError)(nil); errors.As(err, &se) {
		// The account API refused: a setup-token, which only has the
		// inference scope (403), or the API's own rate limit (429), which
		// it reaches long before the plan's. The quota check reads the
		// same windows from the Messages API.
		ws, err = QuotaCheck(ctx, client, base, accessToken, "")
	}
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap
	}
	snap.Limits = usage.Limits(ws, now)
	return snap
}
