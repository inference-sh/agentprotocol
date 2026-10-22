// Package hermes reads Hermes Agent's account and plan limits. hermes keeps
// a login per inference provider in auth.json; Prober reads the one the
// agent's sessions use, the way hermes' own /usage does: a Nous Portal login
// through the portal's account API (the subscription's monthly credits), a
// Claude login through claude.ai's usage API and a ChatGPT login through
// ChatGPT's (the shared readers in the claude and codex packages). An API
// key or another provider has no plan to read.
package hermes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
	"github.com/inference-sh/agentprotocol/report/usage/claude"
	"github.com/inference-sh/agentprotocol/report/usage/codex"
)

// Provider IDs, as auth.json and config.yaml name them.
const (
	ProviderNous      = "nous"
	ProviderAnthropic = "anthropic"
	ProviderCodex     = "openai-codex"
)

// DefaultPortal is the Nous Portal; a login's portal_base_url replaces it.
const DefaultPortal = "https://portal.nousresearch.com"

// WindowCredits is the ID of a Nous subscription's monthly credits.
const WindowCredits = "credits"

// ProbeTimeout bounds a probe: at most two API calls.
const ProbeTimeout = 15 * time.Second

// Prober reads the account and limits from the login hermes saved. It never
// starts hermes, never calls a model and never refreshes a token: an expired
// one reads as probe_failed until hermes refreshes it, since a refresh
// rotates the refresh token hermes holds.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
	// AnthropicBase and ChatGPTBase replace claude.ai's and ChatGPT's API
	// roots, for tests.
	AnthropicBase string
	ChatGPTBase   string
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	home := hermesHome(env)
	now := time.Now()
	store, err := readStore(home)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	dotenv := readDotenv(filepath.Join(home, ".env"))
	lookup := func(k string) string {
		if v := env.Lookup(k); v != "" {
			return v
		}
		return dotenv[k]
	}

	switch provider := Provider(env, home, store, lookup); provider {
	case ProviderNous:
		token, expires, portal := store.nousLogin()
		if token == "" {
			return unauthenticated(), nil
		}
		return p.nous(ctx, token, expires, portal, now), nil
	case ProviderAnthropic:
		token, expires := anthropicToken(env, store, lookup)
		switch {
		case token == "":
			return unauthenticated(), nil
		case !claude.IsOAuthToken(token):
			return apiKey("plan limits apply only to a Claude login", now), nil
		}
		return claude.OAuthSnapshot(ctx, p.Client, p.AnthropicBase, token, expires, now), nil
	case ProviderCodex:
		token, account := store.codexLogin()
		if token == "" {
			return unauthenticated(), nil
		}
		return codex.WhamSnapshot(ctx, p.Client, p.ChatGPTBase, token, account, time.Time{}, now), nil
	case "":
		if len(store.Providers) == 0 && len(store.CredentialPool) == 0 {
			return unauthenticated(), nil
		}
		// hermes picks among the saved logins per request; which one is
		// not known until it does.
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "no provider is configured (model.provider in config.yaml)", now),
		}, nil
	default:
		if label, ok := subscriptions[provider]; ok {
			return ap.UsageSnapshot{
				Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, PlanLabel: label},
				Limits:  usage.Unavailable(ap.UsageUnsupported, "hermes exposes no quota read for "+provider, now),
			}, nil
		}
		return apiKey("plan limits do not apply to the "+provider+" provider", now), nil
	}
}

// subscriptions are the other providers hermes signs in to with a
// subscription login rather than a key.
var subscriptions = map[string]string{
	"xai-oauth":     "SuperGrok",
	"copilot":       "GitHub Copilot",
	"qwen-oauth":    "Qwen OAuth",
	"minimax-oauth": "MiniMax",
}

func unauthenticated() ap.UsageSnapshot {
	return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}
}

func apiKey(msg string, now time.Time) ap.UsageSnapshot {
	return ap.UsageSnapshot{
		Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
		Limits:  usage.Unavailable(ap.UsageUnsupported, msg, now),
	}
}

// Provider is the provider hermes' sessions run on, as hermes resolves it:
// --provider in the arguments, else config.yaml's model.provider, else
// HERMES_INFERENCE_PROVIDER, else the login auth.json marks active. "auto"
// defers to the next.
func Provider(env usage.Env, home string, store Store, lookup func(string) string) string {
	if i := slices.Index(env.Args, "--provider"); i >= 0 && i+1 < len(env.Args) {
		if v := strings.ToLower(env.Args[i+1]); v != "auto" {
			return v
		}
	}
	for _, a := range env.Args {
		if v, ok := strings.CutPrefix(a, "--provider="); ok && v != "auto" {
			return strings.ToLower(v)
		}
	}
	for _, v := range []string{configProvider(filepath.Join(home, "config.yaml")), lookup("HERMES_INFERENCE_PROVIDER"), store.ActiveProvider} {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && v != "auto" {
			return v
		}
	}
	return ""
}

// configProvider is model.provider in config.yaml, read with a line scan of
// the model block (hermes writes it as a two-level mapping).
func configProvider(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	in := false
	indent := -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		depth := len(line) - len(strings.TrimLeft(line, " \t"))
		if depth == 0 {
			in = strings.TrimSpace(strings.SplitN(trimmed, "#", 2)[0]) == "model:"
			indent = -1
			continue
		}
		if !in {
			continue
		}
		if indent < 0 {
			indent = depth
		}
		if depth != indent {
			continue
		}
		if v, ok := strings.CutPrefix(trimmed, "provider:"); ok {
			return yamlScalar(v)
		}
	}
	return ""
}

func yamlScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// Store is auth.json: a login per provider, a pool of extra credentials per
// provider, and the provider last signed in to.
type Store struct {
	Providers      map[string]json.RawMessage `json:"providers"`
	CredentialPool map[string][]PoolEntry     `json:"credential_pool"`
	ActiveProvider string                     `json:"active_provider"`
}

// PoolEntry is one credential of a provider's pool, the fields a probe reads.
type PoolEntry struct {
	AuthType      string `json:"auth_type"` // oauth or api_key
	Priority      int    `json:"priority"`
	AccessToken   string `json:"access_token"`
	ExpiresAt     string `json:"expires_at"`
	ExpiresAtMs   int64  `json:"expires_at_ms"`
	PortalBaseURL string `json:"portal_base_url"`
}

func readStore(home string) (Store, error) {
	raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Store{}, nil
	}
	if err != nil {
		return Store{}, err
	}
	var s Store
	if err := json.Unmarshal(raw, &s); err != nil {
		return Store{}, fmt.Errorf("hermes: auth.json: %w", err)
	}
	return s, nil
}

func (s Store) provider(id string, into any) bool {
	raw, ok := s.Providers[id]
	return ok && json.Unmarshal(raw, into) == nil
}

// poolOAuth is the pool's first OAuth credential by priority, as hermes'
// fill-first selection takes it.
func (s Store) poolOAuth(id string) (PoolEntry, bool) {
	var best *PoolEntry
	for i, e := range s.CredentialPool[id] {
		if e.AccessToken == "" || (e.AuthType != "" && !strings.HasPrefix(e.AuthType, "oauth")) {
			continue
		}
		if best == nil || e.Priority < best.Priority {
			best = &s.CredentialPool[id][i]
		}
	}
	if best == nil {
		return PoolEntry{}, false
	}
	return *best, true
}

func (e PoolEntry) expires() time.Time {
	if e.ExpiresAtMs > 0 {
		return time.UnixMilli(e.ExpiresAtMs)
	}
	return isoTime(e.ExpiresAt)
}

// nousLogin is the Nous Portal login: providers.nous, else the pool's.
func (s Store) nousLogin() (token string, expires time.Time, portal string) {
	var st struct {
		AccessToken   string `json:"access_token"`
		ExpiresAt     string `json:"expires_at"`
		PortalBaseURL string `json:"portal_base_url"`
	}
	if s.provider(ProviderNous, &st) && st.AccessToken != "" {
		return st.AccessToken, isoTime(st.ExpiresAt), st.PortalBaseURL
	}
	if e, ok := s.poolOAuth(ProviderNous); ok {
		return e.AccessToken, e.expires(), e.PortalBaseURL
	}
	return "", time.Time{}, ""
}

// codexLogin is the ChatGPT login: providers.openai-codex.tokens, else the
// pool's. Pool credentials carry no account ID; the token's claim names it.
func (s Store) codexLogin() (token, account string) {
	var st struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if s.provider(ProviderCodex, &st) && st.Tokens.AccessToken != "" {
		return st.Tokens.AccessToken, st.Tokens.AccountID
	}
	if e, ok := s.poolOAuth(ProviderCodex); ok {
		return e.AccessToken, ""
	}
	return "", ""
}

// anthropicToken is the Anthropic credential hermes uses, in its order
// (agent/anthropic_adapter.py resolve_anthropic_token): ANTHROPIC_TOKEN,
// CLAUDE_CODE_OAUTH_TOKEN, Claude Code's saved login, the pool's OAuth
// credential, ANTHROPIC_API_KEY. expires is zero when unknown.
func anthropicToken(env usage.Env, s Store, lookup func(string) string) (token string, expires time.Time) {
	for _, k := range []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if v := strings.TrimSpace(lookup(k)); v != "" {
			return v, time.Time{}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(env.Home(), ".claude", ".credentials.json")); err == nil {
		var c struct {
			ClaudeAiOauth struct {
				AccessToken string `json:"accessToken"`
				ExpiresAt   int64  `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(raw, &c) == nil && c.ClaudeAiOauth.AccessToken != "" {
			var exp time.Time
			if c.ClaudeAiOauth.ExpiresAt > 0 {
				exp = time.UnixMilli(c.ClaudeAiOauth.ExpiresAt)
			}
			return c.ClaudeAiOauth.AccessToken, exp
		}
	}
	if e, ok := s.poolOAuth(ProviderAnthropic); ok {
		return e.AccessToken, e.expires()
	}
	return strings.TrimSpace(lookup("ANTHROPIC_API_KEY")), time.Time{}
}

// AccountResponse is the portal's /api/oauth/account (hermes_cli/
// nous_account.py), the fields a probe reads. Credits are dollars.
type AccountResponse struct {
	Error string `json:"error"`
	User  struct {
		Email string `json:"email"`
	} `json:"user"`
	Organisation struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"organisation"`
	Subscription *Subscription `json:"subscription"`
}

// Subscription is a Nous Portal plan and its credits for the period.
type Subscription struct {
	Plan             string   `json:"plan"`
	Tier             *int     `json:"tier"`
	MonthlyCredits   *float64 `json:"monthly_credits"`
	CreditsRemaining *float64 `json:"credits_remaining"`
	CurrentPeriodEnd string   `json:"current_period_end"`
	RolloverCredits  *float64 `json:"rollover_credits"`
}

func (p Prober) nous(ctx context.Context, token string, expires time.Time, portal string, now time.Time) ap.UsageSnapshot {
	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, PlanLabel: "Nous Portal"}
	snap := ap.UsageSnapshot{Account: &acct}
	if !expires.IsZero() && !expires.After(now) {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "the saved Nous Portal login has expired; hermes refreshes it on its next run", now)
		return snap
	}
	if portal == "" {
		portal = DefaultPortal
	}
	var r AccountResponse
	err := webjson.Get(ctx, p.Client, strings.TrimRight(portal, "/")+"/api/oauth/account", map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}, &r)
	if err == nil && r.Error != "" {
		err = errors.New(r.Error)
	}
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, fmt.Sprintf("hermes: nous account: %v", err), now)
		return snap
	}
	acct.Email = r.User.Email
	acct.Workspace = r.Organisation.Name
	if acct.Workspace == "" {
		acct.Workspace = r.Organisation.Slug
	}
	if s := r.Subscription; s != nil && s.Plan != "" {
		acct.Plan, acct.PlanLabel = s.Plan, "Nous Portal "+titleCase(s.Plan)
	}
	if w, ok := CreditsWindow(r.Subscription); ok {
		snap.Limits = usage.Limits([]ap.UsageWindow{w}, now)
	} else {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "no subscription credits to measure; the account runs on purchased credits", now)
	}
	return snap
}

// CreditsWindow is the share of the period's subscription credits used, as
// hermes' /usage gauges it: only when the plan grants credits and what is
// left does not exceed them (a rollover balance has no meaningful share).
func CreditsWindow(s *Subscription) (ap.UsageWindow, bool) {
	if s == nil || s.MonthlyCredits == nil || s.CreditsRemaining == nil {
		return ap.UsageWindow{}, false
	}
	total, left := *s.MonthlyCredits, *s.CreditsRemaining
	if total <= 0 || math.IsNaN(left) || math.IsInf(left, 0) || math.IsInf(total, 0) || left > total {
		return ap.UsageWindow{}, false
	}
	return ap.UsageWindow{
		ID: WindowCredits, Kind: ap.UsageWindowMonthly, Label: "Monthly credits",
		UsedPercent: (1 - left/total) * 100, ResetsAt: isoTime(s.CurrentPeriodEnd),
	}, true
}

func titleCase(s string) string {
	var words []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || r == ' ' }) {
		words = append(words, strings.ToUpper(w[:1])+w[1:])
	}
	return strings.Join(words, " ")
}

func isoTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// readDotenv reads KEY=VALUE lines of hermes' .env, where API keys live.
func readDotenv(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// hermesHome is HERMES_HOME (a profile sets it to ~/.hermes/profiles/<name>),
// else ~/.hermes.
func hermesHome(env usage.Env) string {
	if h := env.Lookup("HERMES_HOME"); h != "" {
		return h
	}
	return filepath.Join(env.Home(), ".hermes")
}
