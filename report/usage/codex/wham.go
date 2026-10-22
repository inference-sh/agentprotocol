package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/codexapp"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// ChatGPT's usage endpoint, the one codex's app-server reads for
// account/rateLimits/read. Agents that sign in with a ChatGPT login of their
// own (hermes, pi, goose) read their limits here, mapped onto the same
// primary and secondary windows.
const DefaultChatGPTBase = "https://chatgpt.com/backend-api"

// WhamUsageResponse is /wham/usage, the fields a probe reads.
type WhamUsageResponse struct {
	PlanType  string         `json:"plan_type"`
	RateLimit *WhamRateLimit `json:"rate_limit"`
}

// WhamRateLimit is the main allowance's two windows.
type WhamRateLimit struct {
	PrimaryWindow   *WhamWindow `json:"primary_window"`
	SecondaryWindow *WhamWindow `json:"secondary_window"`
}

// WhamWindow is one window. ResetAt is Unix seconds; ResetAfterSeconds is
// the same moment relative to the response.
type WhamWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

// WhamUsage reads the plan and windows with a ChatGPT OAuth access token.
// accountID is the ChatGPT account (workspace) the token acts for; empty
// leaves the header out and ChatGPT picks the default one. base is the
// backend API's root; empty uses DefaultChatGPTBase.
func WhamUsage(ctx context.Context, client *http.Client, base, accessToken, accountID string) (WhamUsageResponse, error) {
	if base == "" {
		base = DefaultChatGPTBase
	}
	h := map[string]string{"Authorization": "Bearer " + accessToken, "Accept": "application/json", "User-Agent": "codex-cli"}
	if accountID != "" {
		h["ChatGPT-Account-Id"] = accountID
	}
	var r WhamUsageResponse
	if err := webjson.Get(ctx, client, strings.TrimRight(base, "/")+"/wham/usage", h, &r); err != nil {
		return WhamUsageResponse{}, fmt.Errorf("codex: wham usage: %w", err)
	}
	return r, nil
}

// WhamWindows maps /wham/usage onto Windows, so the windows get the IDs,
// kinds and labels account/rateLimits/read gives them.
func WhamWindows(r WhamUsageResponse, now time.Time) []ap.UsageWindow {
	if r.RateLimit == nil {
		return nil
	}
	s := codexapp.RateLimitSnapshot{Primary: whamWindow(r.RateLimit.PrimaryWindow, now), Secondary: whamWindow(r.RateLimit.SecondaryWindow, now)}
	if r.PlanType != "" {
		p := codexapp.PlanType(r.PlanType)
		s.PlanType = &p
	}
	return Windows(s)
}

func whamWindow(w *WhamWindow, now time.Time) *codexapp.RateLimitWindow {
	if w == nil || w.UsedPercent == nil {
		return nil
	}
	out := &codexapp.RateLimitWindow{UsedPercent: int64(*w.UsedPercent + 0.5)}
	if w.LimitWindowSeconds != nil && *w.LimitWindowSeconds > 0 {
		m := *w.LimitWindowSeconds / 60
		out.WindowDurationMins = &m
	}
	switch {
	case w.ResetAt != nil && *w.ResetAt > 0:
		at := *w.ResetAt
		if at > 1e12 { // milliseconds
			at /= 1000
		}
		out.ResetsAt = &at
	case w.ResetAfterSeconds != nil:
		at := now.Unix() + *w.ResetAfterSeconds
		out.ResetsAt = &at
	}
	return out
}

// TokenClaims is what a ChatGPT access token (a JWT) says about its account.
type TokenClaims struct {
	AccountID string
	Email     string
	PlanType  string
	Expires   time.Time
}

// ParseAccessToken reads a ChatGPT access token's claims without verifying
// it; ok is false when it is not a JWT.
func ParseAccessToken(token string) (TokenClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return TokenClaims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return TokenClaims{}, false
	}
	var c struct {
		Exp  int64 `json:"exp"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return TokenClaims{}, false
	}
	out := TokenClaims{AccountID: c.Auth.AccountID, Email: c.Profile.Email, PlanType: c.Auth.PlanType}
	if c.Exp > 0 {
		out.Expires = time.Unix(c.Exp, 0).UTC()
	}
	return out, true
}

// WhamSnapshot is the account and windows of a ChatGPT login another agent
// holds. The account comes from the token's claims and the plan from
// /wham/usage; accountID empty uses the token's. A token past expires (zero
// uses the token's own expiry) is not used, since refreshing it would rotate
// the refresh token out from under the agent; the limits read as
// probe_failed until the agent refreshes it.
func WhamSnapshot(ctx context.Context, client *http.Client, base, accessToken, accountID string, expires, now time.Time) ap.UsageSnapshot {
	claims, _ := ParseAccessToken(accessToken)
	if accountID == "" {
		accountID = claims.AccountID
	}
	if expires.IsZero() {
		expires = claims.Expires
	}
	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: claims.Email, Workspace: accountID,
		Plan: claims.PlanType, PlanLabel: PlanLabel(codexapp.PlanType(claims.PlanType))}
	snap := ap.UsageSnapshot{Account: &acct}
	if !expires.IsZero() && !expires.After(now) {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "the saved ChatGPT login has expired; the agent refreshes it on its next run", now)
		return snap
	}
	r, err := WhamUsage(ctx, client, base, accessToken, accountID)
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap
	}
	if r.PlanType != "" {
		acct.Plan, acct.PlanLabel = r.PlanType, PlanLabel(codexapp.PlanType(r.PlanType))
	}
	snap.Limits = usage.Limits(WhamWindows(r, now), now)
	return snap
}
