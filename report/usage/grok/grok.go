// Package grok reads grok's account and plan limits. grok serves both over
// ACP as extension methods of its own, the ones its TUI calls for the
// account line and /usage: _x.ai/auth/check_subscription for the login and
// the plan tier, _x.ai/billing for the credits used this period. Prober asks
// a short-lived `grok agent stdio` for them right after initialize, with no
// session, so nothing is prompted and no model is called. grok reads and
// refreshes its own login as it does for a session; the probe never touches
// auth.json, which matters because grok guards its refresh token against
// reuse.
package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// DefaultBinary is the CLI a probe runs when usage.Env names none.
const DefaultBinary = "grok"

// The extension methods a probe calls (grok 1.0.67). The leading underscore
// is ACP's extension prefix; grok answers the bare names "Method not found".
const (
	MethodCheckSubscription = "_x.ai/auth/check_subscription"
	MethodBilling           = "_x.ai/billing"
)

// Auth modes, as check_subscription's meta.auth_mode names them. ApiKey,
// Oidc and External were observed (grok 1.0.67); the grok.com logins are
// named after auth.json's auth_mode values.
const (
	AuthAPIKey   = "ApiKey"
	AuthOIDC     = "Oidc"
	AuthExternal = "External"
	AuthGrok     = "Grok"
	AuthWebLogin = "WebLogin"
)

// Window IDs.
const (
	WindowCredits  = "credits"
	WindowOnDemand = "on_demand"
)

// ProbeTimeout bounds a probe: grok's startup, which fetches its settings
// and model list, and two calls that each ask xAI's backend.
const ProbeTimeout = 30 * time.Second

// Prober reads the account and the credits from a short-lived
// `grok agent stdio`. It opens no session and sends no prompt.
type Prober struct {
	// ClientInfo names the caller to grok. Empty uses "agentprotocol".
	ClientInfo acp.ClientInfo
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	bin := env.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	info := p.ClientInfo
	if info.Name == "" {
		info = acp.ClientInfo{Name: "agentprotocol", Version: "1"}
	}
	dir := env.Dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "grok-usage-")
		if err != nil {
			return ap.UsageSnapshot{}, err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}
	proc, err := acp.Spawn(ctx, acp.ProcessConfig{
		Command: bin, Args: append([]string{"agent", "stdio"}, env.Args...), Dir: dir, Env: env.Env,
	}, info, acp.Handler{})
	if err != nil {
		return ap.UsageSnapshot{}, fmt.Errorf("grok: %w", err)
	}
	// No session was opened, so there is nothing to close: end the input
	// and let grok exit.
	proc.ShutdownGrace = -1
	defer func() { _ = proc.Wait() }()

	var sub CheckSubscriptionResponse
	if err := call(ctx, proc, MethodCheckSubscription, &sub); err != nil {
		return ap.UsageSnapshot{}, err
	}
	now := time.Now()
	acct := Account(sub)
	if acct.Status != ap.AccountAuthenticated {
		if env.Lookup("XAI_API_KEY") != "" {
			// grok uses XAI_API_KEY only when there is no login, and does not
			// report it as one.
			return ap.UsageSnapshot{
				Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
				Limits:  usage.Unavailable(ap.UsageUnsupported, "plan credits apply only to a grok.com login", now),
			}, nil
		}
		return ap.UsageSnapshot{Account: &acct}, nil
	}
	snap := ap.UsageSnapshot{Account: &acct}
	if acct.Type == ap.AccountAPIKey {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "plan credits apply only to a grok.com login", now)
		return snap, nil
	}
	var bill BillingConfigResponse
	if err := call(ctx, proc, MethodBilling, &bill); err != nil {
		if strings.Contains(err.Error(), "requires auth with grok.com") {
			// An enterprise (OIDC, external) login has no grok.com billing.
			snap.Limits = usage.Unavailable(ap.UsageUnsupported, "plan credits apply only to a grok.com login", now)
			return snap, nil
		}
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	if acct.Plan == "" && bill.SubscriptionTier != "" {
		acct.Plan, acct.PlanLabel = bill.SubscriptionTier, PlanLabel(bill.SubscriptionTier)
	}
	ws := Windows(bill, now)
	if len(ws) == 0 {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "grok reports no credit usage for this plan", now)
		return snap, nil
	}
	snap.Limits = usage.Limits(ws, now)
	return snap, nil
}

func call(ctx context.Context, proc *acp.Process, method string, out any) error {
	raw, err := proc.Call(ctx, method, struct{}{})
	if err != nil {
		return fmt.Errorf("grok: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("grok: %s: %w", method, err)
	}
	return nil
}

// Account is the account check_subscription reports. Logged out grok answers
// {"authenticated":false,"meta":null}, with XAI_API_KEY set too.
func Account(r CheckSubscriptionResponse) ap.AgentAccount {
	if !r.Authenticated || r.Meta == nil {
		return ap.AgentAccount{Status: ap.AccountUnauthenticated}
	}
	m := r.Meta
	a := ap.AgentAccount{Status: ap.AccountAuthenticated, Email: m.Email}
	if m.AuthMode == AuthAPIKey || m.SubscriptionTier == "api_key" {
		a.Type = ap.AccountAPIKey
		return a
	}
	a.Type = ap.AccountSubscription
	if m.SubscriptionTier != "" {
		a.Plan, a.PlanLabel = m.SubscriptionTier, PlanLabel(m.SubscriptionTier)
	}
	a.Workspace = m.TeamName
	if a.Workspace == "" {
		a.Workspace = m.TeamID
	}
	return a
}

// PlanLabel is a subscription tier's display name. The ids are the ones
// grok 1.0.67 knows; another is shown as its id.
func PlanLabel(tier string) string {
	switch tier {
	case "free":
		return "Free"
	case "x_basic":
		return "X Basic"
	case "x_premium":
		return "X Premium"
	case "x_premium_plus":
		return "X Premium+"
	case "supergrok":
		return "SuperGrok"
	case "supergrok_lite":
		return "SuperGrok Lite"
	case "supergrok_plus":
		return "SuperGrok Plus"
	case "supergrok_heavy":
		return "SuperGrok Heavy"
	}
	return tier
}

// Windows maps the billing config grok fetches with ?format=credits: the
// share of the period's credits used, and the on-demand spend against its
// cap when there is one. The credits period is a week on SuperGrok plans;
// its length is taken from the period itself. A fresh period leaves
// creditUsagePercent out, which reads as nothing used while the period
// runs.
func Windows(r BillingConfigResponse, now time.Time) []ap.UsageWindow {
	c := r.Config
	var ws []ap.UsageWindow
	start, _ := time.Parse(time.RFC3339, c.CurrentPeriod.Start)
	end, _ := time.Parse(time.RFC3339, c.CurrentPeriod.End)
	if c.CreditUsagePercent != nil || end.After(now) {
		minutes := 7 * 24 * 60
		if !start.IsZero() && end.After(start) {
			minutes = int(math.Round(end.Sub(start).Minutes()))
		}
		kind := usage.KindForDuration(minutes)
		w := ap.UsageWindow{ID: WindowCredits, Kind: kind, Label: creditsLabel(kind), DurationMinutes: minutes}
		if c.CreditUsagePercent != nil {
			w.UsedPercent = *c.CreditUsagePercent
		}
		if !end.IsZero() {
			w.ResetsAt = end.UTC()
		}
		ws = append(ws, w)
	}
	if c.OnDemandCap > 0 {
		ws = append(ws, ap.UsageWindow{
			ID: WindowOnDemand, Kind: ap.UsageWindowOther, Label: "On-demand",
			UsedPercent: float64(c.OnDemandUsed) / float64(c.OnDemandCap) * 100,
		})
	}
	return ws
}

func creditsLabel(k ap.UsageWindowKind) string {
	switch k {
	case ap.UsageWindowWeekly:
		return "Weekly credits"
	case ap.UsageWindowMonthly:
		return "Monthly credits"
	}
	return "Credits"
}

// The extension methods' results (grok 1.0.67), the fields a probe reads.
// check_subscription's meta is snake_case and billing's config camelCase,
// as grok serializes them.
type (
	CheckSubscriptionResponse struct {
		Authenticated bool              `json:"authenticated"`
		Meta          *SubscriptionMeta `json:"meta"`
	}
	SubscriptionMeta struct {
		Email            string `json:"email"`
		AuthMode         string `json:"auth_mode"`
		TeamID           string `json:"team_id"`
		TeamName         string `json:"team_name"`
		SubscriptionTier string `json:"subscription_tier"`
		BackendBilled    bool   `json:"backend_billed"`
	}
	BillingConfigResponse struct {
		Config           BillingConfig `json:"config"`
		OnDemandEnabled  bool          `json:"on_demand_enabled"`
		SubscriptionTier string        `json:"subscription_tier"`
	}
	BillingConfig struct {
		CreditUsagePercent   *float64    `json:"creditUsagePercent"`
		CurrentPeriod        UsagePeriod `json:"currentPeriod"`
		MonthlyLimit         Amount      `json:"monthlyLimit"`
		OnDemandCap          Amount      `json:"onDemandCap"`
		OnDemandUsed         Amount      `json:"onDemandUsed"`
		PrepaidBalance       Amount      `json:"prepaidBalance"`
		IsUnifiedBillingUser bool        `json:"isUnifiedBillingUser"`
		BillingPeriodStart   string      `json:"billingPeriodStart"`
	}
	UsagePeriod struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
)

// Amount is a money amount as xAI's billing API writes it: {"val": n}, with
// n a number or a decimal string. A bare number is read too.
type Amount float64

func (a *Amount) UnmarshalJSON(b []byte) error {
	var obj struct {
		Val json.RawMessage `json:"val"`
	}
	if len(b) > 0 && b[0] == '{' {
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		b = obj.Val
	}
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*a = 0
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("grok: amount %s: %w", b, err)
	}
	*a = Amount(f)
	return nil
}
