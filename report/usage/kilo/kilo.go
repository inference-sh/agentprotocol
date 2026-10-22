// Package kilo reads Kilo's account and plan limits. The CLI reports neither
// over ACP, so Prober reads them with the login kilo saved, from the Kilo
// API the CLI itself calls: the profile for the account, Kilo Pass's state
// for the one plan with a limit (a monthly credit allowance), and the credit
// balance otherwise. kilo can hold logins to other providers too; signed in
// to one of those there is no Kilo plan to read.
package kilo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// DefaultBaseURL is the Kilo API. KILO_API_URL in the agent's environment
// moves it, as it does for the CLI.
const DefaultBaseURL = "https://api.kilo.ai"

// Provider is the auth.json key of a Kilo login.
const Provider = "kilo"

// WindowPass is the ID of Kilo Pass's monthly allowance.
const WindowPass = "kilo_pass"

// ProbeTimeout bounds a probe: up to three API calls.
const ProbeTimeout = 15 * time.Second

// Prober reads the account and Kilo Pass's allowance from the login kilo
// saved. It never starts the CLI and never calls a model.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	now := time.Now()

	auth, err := ReadAuth(filepath.Join(dataHome(env), "kilo", "auth.json"))
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	name := SessionProvider(env.Args, auth)
	entry, ok := auth[name]
	switch {
	case !ok:
		return usage.NoSavedLogin(env, now), nil
	case name != Provider:
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: entry.AccountType()},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "plan limits are read only for a Kilo login", now),
		}, nil
	}

	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Workspace: entry.AccountID}
	snap := ap.UsageSnapshot{Account: &acct}
	c := api{client: p.Client, base: baseURL(env), token: entry.Token()}
	var prof Profile
	if err := c.get(ctx, "/api/profile", nil, &prof); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	acct.Email = prof.Email()
	if entry.AccountID != "" {
		// Sessions bill the organization; Kilo Pass is the user's own.
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "pays with organization credits", now)
		return snap, nil
	}
	var state PassState
	q := url.Values{"batch": {"1"}, "input": {`{"0":null}`}}
	if err := c.get(ctx, "/api/trpc/kiloPass.getState?"+q.Encode(), nil, &state); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	sub := state.Subscription()
	if sub == nil {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "pay as you go, with no Kilo Pass", now)
		return snap, nil
	}
	acct.Plan, acct.PlanLabel = "kilo_pass", "Kilo Pass"
	snap.Limits = usage.Limits([]ap.UsageWindow{PassWindow(*sub)}, now)
	return snap, nil
}

// PassWindow maps a live Kilo Pass subscription: one window for the billing
// period, used as the period's spend against its base and bonus credits.
func PassWindow(s PassSubscription) ap.UsageWindow {
	w := ap.UsageWindow{ID: WindowPass, Kind: ap.UsageWindowMonthly, Label: "Kilo Pass"}
	used := deref(s.UsageUSD)
	switch credits := deref(s.BaseCreditsUSD) + deref(s.BonusCreditsUSD); {
	case credits > 0:
		w.UsedPercent = used / credits * 100
	case used > 0:
		w.UsedPercent = 100
	}
	next := s.NextBillingAt
	if next == "" {
		next = s.NextRenewalAt
	}
	if t, err := time.Parse(time.RFC3339Nano, next); err == nil {
		w.ResetsAt = t.UTC()
	}
	return w
}

// SessionProvider is the auth.json entry kilo's sessions use: the provider
// of a --model (-m) provider/model in args, else the Kilo login, else the
// first other login by name. kilo also picks a model from its config; the
// probe does not read that.
func SessionProvider(args []string, auth Auth) string {
	if p := ModelProvider(args); p != "" {
		return p
	}
	if _, ok := auth[Provider]; ok {
		return Provider
	}
	names := make([]string, 0, len(auth))
	for n := range auth {
		names = append(names, n)
	}
	slices.Sort(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// ModelProvider is the provider part of a --model or -m argument.
func ModelProvider(args []string) string {
	for i, a := range args {
		v, ok := "", false
		switch {
		case (a == "--model" || a == "-m") && i+1 < len(args):
			v, ok = args[i+1], true
		case strings.HasPrefix(a, "--model="):
			v, ok = strings.TrimPrefix(a, "--model="), true
		}
		if p, _, found := strings.Cut(v, "/"); ok && found {
			return p
		}
	}
	return ""
}

// Auth is auth.json: a login per provider, as kilo (and opencode, which it
// forks) saves them.
type Auth map[string]AuthEntry

// AuthEntry is one login: an OAuth token, an API key, or a well-known
// provider's token.
type AuthEntry struct {
	Type      string `json:"type"` // oauth, api, wellknown
	Access    string `json:"access,omitempty"`
	Key       string `json:"key,omitempty"`
	AccountID string `json:"accountId,omitempty"` // a Kilo login's organization
}

// Token is the bearer the login sends.
func (e AuthEntry) Token() string {
	if e.Type == "oauth" {
		return e.Access
	}
	return e.Key
}

// AccountType is how a login of another provider signs in: an OAuth login
// is that provider's plan (ChatGPT, Copilot), anything else a key.
func (e AuthEntry) AccountType() ap.AccountType {
	if e.Type == "oauth" {
		return ap.AccountSubscription
	}
	return ap.AccountAPIKey
}

// ReadAuth reads auth.json; a missing file is no logins.
func ReadAuth(path string) (Auth, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Auth{}, nil
	}
	if err != nil {
		return nil, err
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("kilo: auth.json: %w", err)
	}
	return a, nil
}

// The Kilo API's shapes, the fields a probe reads.
type (
	Profile struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		EmailTop string `json:"email"`
	}
	PassSubscription struct {
		Status          string   `json:"status"`
		BaseCreditsUSD  *float64 `json:"currentPeriodBaseCreditsUsd"`
		UsageUSD        *float64 `json:"currentPeriodUsageUsd"`
		BonusCreditsUSD *float64 `json:"currentPeriodBonusCreditsUsd"`
		NextBillingAt   string   `json:"nextBillingAt"`
		NextRenewalAt   string   `json:"nextRenewalAt"`
	}
)

// PassState is kiloPass.getState's response: a tRPC batch, [0].result.data
// with the value under json (superjson) or bare.
type PassState json.RawMessage

func (s *PassState) UnmarshalJSON(b []byte) error {
	*s = append((*s)[:0], b...)
	return nil
}

// Email is the profile's email, nested or top-level.
func (p Profile) Email() string {
	if p.User.Email != "" {
		return p.User.Email
	}
	return p.EmailTop
}

// Subscription is the live Kilo Pass, nil when there is none: no
// subscription, one with no credits, or one that is not active, past due or
// trialing.
func (s PassState) Subscription() *PassSubscription {
	var batch []json.RawMessage
	raw := json.RawMessage(s)
	if json.Unmarshal(raw, &batch) == nil {
		if len(batch) == 0 {
			return nil
		}
		raw = batch[0]
	}
	var r struct {
		Result struct {
			Data json.RawMessage `json:"data"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &r) == nil && len(r.Result.Data) > 0 {
		raw = r.Result.Data
	}
	var d struct {
		JSON         json.RawMessage   `json:"json"`
		Subscription *PassSubscription `json:"subscription"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	sub := d.Subscription
	if len(d.JSON) > 0 {
		var j struct {
			Subscription *PassSubscription `json:"subscription"`
		}
		if json.Unmarshal(d.JSON, &j) == nil {
			sub = j.Subscription
		}
	}
	if sub == nil || sub.BaseCreditsUSD == nil && sub.UsageUSD == nil {
		return nil
	}
	switch sub.Status {
	case "active", "past_due", "trialing", "":
		return sub
	}
	return nil
}

type api struct {
	client *http.Client
	base   string
	token  string
}

func (a api) get(ctx context.Context, path string, header map[string]string, out any) error {
	h := map[string]string{"Authorization": "Bearer " + a.token, "Content-Type": "application/json"}
	for k, v := range header {
		h[k] = v
	}
	if err := webjson.Get(ctx, a.client, a.base+path, h, out); err != nil {
		p, _, _ := strings.Cut(path, "?")
		return fmt.Errorf("kilo: %s: %w", p, err)
	}
	return nil
}

// dataHome is XDG_DATA_HOME, else ~/.local/share.
func dataHome(env usage.Env) string {
	if d := env.Lookup("XDG_DATA_HOME"); d != "" {
		return d
	}
	return filepath.Join(env.Home(), ".local", "share")
}

func baseURL(env usage.Env) string {
	if u := env.Lookup("KILO_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultBaseURL
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
