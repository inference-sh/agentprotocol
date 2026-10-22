// Package copilot reads GitHub Copilot CLI's account and plan quotas. The
// CLI reports neither over ACP, so Prober reads them as the CLI does at
// startup: it finds the GitHub token the CLI would use and asks GitHub's
// copilot_internal/user endpoint, which answers with the login, the plan and
// a snapshot per quota (chat, completions, premium requests). With a
// provider of the user's own (COPILOT_PROVIDER_BASE_URL) there is no GitHub
// plan to read.
package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// DefaultHost is the GitHub server the CLI signs in to unless COPILOT_GH_HOST
// or GH_HOST names another.
const DefaultHost = "https://github.com"

// ProbeTimeout bounds a probe: possibly `gh auth token`, then one request.
const ProbeTimeout = 15 * time.Second

// Token sources, in the order the CLI tries them (@github/copilot 1.0.x).
const (
	SourceEnv     = "env"     // COPILOT_GITHUB_TOKEN, GH_TOKEN, GITHUB_TOKEN
	SourceLogin   = "login"   // copilot login, saved in config.json
	SourceKeyring = "keyring" // copilot login, saved in the OS keyring
	SourceGhCLI   = "gh"      // gh auth token
)

// Prober reads the account and the quotas with the token the CLI would use.
// It never starts the CLI, never calls a model and writes nothing.
type Prober struct {
	// Client makes the request; nil uses http.DefaultClient.
	Client *http.Client
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	now := time.Now()
	if env.Lookup("COPILOT_PROVIDER_BASE_URL") != "" {
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "plan quotas apply only to a GitHub login", now),
		}, nil
	}
	host := Host(env)
	tok, err := FindToken(ctx, env, host)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	switch {
	case tok.Source == "":
		return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
	case tok.Source == SourceKeyring:
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Workspace: tok.Login},
			Limits:  usage.Unavailable(ap.UsageProbeFailed, "copilot: the login is in the keyring", now),
		}, nil
	}
	if tok.Host != "" {
		host = tok.Host
	}
	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Workspace: tok.Login}
	snap := ap.UsageSnapshot{Account: &acct}
	var u User
	if err := webjson.Get(ctx, p.Client, APIURL(env, host)+"/copilot_internal/user", map[string]string{
		"Authorization": "Bearer " + tok.Token, "Accept": "application/json",
	}, &u); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "copilot: copilot_internal/user: "+err.Error(), now)
		return snap, nil
	}
	acct = Account(u)
	if acct.Workspace == "" {
		acct.Workspace = tok.Login
	}
	ws := Windows(u)
	if len(ws) == 0 {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "the plan has no limited quota", now)
		return snap, nil
	}
	snap.Limits = usage.Limits(ws, now)
	return snap, nil
}

// Account is the account copilot_internal/user describes. GitHub gives a
// login, not an email; the login goes in Workspace, the field that tells
// two accounts apart along with the email.
func Account(u User) ap.AgentAccount {
	a := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Workspace: u.Login}
	a.Plan, a.PlanLabel = Plan(u)
	return a
}

// Plan is the plan's id, the access SKU when GitHub sends one, and its
// display name. The SKUs seen are free_limited_copilot,
// monthly/yearly_subscriber_quota (Pro), plus_monthly/yearly_subscriber_quota
// (Pro+), copilot_for_business_seat_quota and copilot_enterprise_seat_quota;
// copilot_plan (individual, business, enterprise) decides the rest.
func Plan(u User) (id, label string) {
	id = u.AccessTypeSKU
	if id == "" {
		id = u.CopilotPlan
	}
	sku := strings.ToLower(u.AccessTypeSKU)
	switch {
	case strings.Contains(sku, "free"):
		label = "Copilot Free"
	case strings.Contains(sku, "plus"):
		label = "Copilot Pro+"
	case strings.Contains(sku, "enterprise") || u.CopilotPlan == "enterprise":
		label = "Copilot Enterprise"
	case strings.Contains(sku, "business") || u.CopilotPlan == "business":
		label = "Copilot Business"
	case strings.Contains(sku, "subscriber") || strings.Contains(sku, "educational") || u.CopilotPlan == "individual":
		label = "Copilot Pro"
	case id != "":
		label = "Copilot"
	}
	return id, label
}

// Windows maps the quota snapshots: one monthly window per quota that is
// not unlimited, used being what percent_remaining leaves. A snapshot's own
// quota_reset_at wins over the account's quota_reset_date_utc.
func Windows(u User) []ap.UsageWindow {
	reset := parseTime(u.QuotaResetDateUTC)
	if reset.IsZero() {
		reset = parseTime(u.QuotaResetDate)
	}
	ws := make([]ap.UsageWindow, 0, len(u.QuotaSnapshots))
	for id, q := range u.QuotaSnapshots {
		if q == nil || q.Unlimited || q.PercentRemaining == nil || q.Entitlement < 0 {
			continue
		}
		w := ap.UsageWindow{ID: id, Kind: ap.UsageWindowMonthly, Label: quotaLabel(id, q.TokenBasedBilling), UsedPercent: 100 - *q.PercentRemaining, ResetsAt: reset}
		if q.QuotaResetAt > 0 {
			w.ResetsAt = time.Unix(q.QuotaResetAt, 0).UTC()
		}
		ws = append(ws, w)
	}
	return ws
}

// quotaLabel names a quota. On token-based billing the premium quota is
// metered in AI credits rather than requests.
func quotaLabel(id string, tokenBased bool) string {
	switch id {
	case "premium_interactions":
		if tokenBased {
			return "AI credits"
		}
		return "Premium requests"
	case "chat":
		return "Chat"
	case "completions":
		return "Completions"
	}
	return strings.ReplaceAll(id, "_", " ")
}

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// copilot_internal/user (@github/copilot 1.0.102's CopilotUserResponse),
// the fields a probe reads.
type (
	User struct {
		Login             string            `json:"login"`
		AccessTypeSKU     string            `json:"access_type_sku"`
		CopilotPlan       string            `json:"copilot_plan"`
		QuotaResetDate    string            `json:"quota_reset_date"`
		QuotaResetDateUTC string            `json:"quota_reset_date_utc"`
		QuotaSnapshots    map[string]*Quota `json:"quota_snapshots"`
	}
	Quota struct {
		// Entitlement is the period's allowance; -1 means unlimited.
		Entitlement      float64  `json:"entitlement"`
		Remaining        float64  `json:"remaining"`
		PercentRemaining *float64 `json:"percent_remaining"` // 0 to 100
		Unlimited        bool     `json:"unlimited"`
		OveragePermitted bool     `json:"overage_permitted"`
		QuotaResetAt     int64    `json:"quota_reset_at"` // Unix seconds; 0 when unset
		// TokenBasedBilling is set on accounts billed in AI credits.
		TokenBasedBilling bool `json:"token_based_billing"`
	}
)

// Token is the GitHub token the CLI would use and where it came from. Source
// is empty when there is none; Token is empty when it is in the keyring.
type Token struct {
	Source string
	Token  string
	Login  string
	// Host is the GitHub host a saved login's token was issued by; the token
	// is sent only there. Empty for the environment's and gh's, which are
	// the host's the probe asked for.
	Host string
}

// FindToken finds the token as the CLI does: the environment, then the
// login `copilot login` saved, then `gh auth token`. A classic PAT (ghp_)
// is skipped, as the CLI refuses it. A saved login whose token is not in
// config.json is in the OS keyring, which a probe does not read.
func FindToken(ctx context.Context, env usage.Env, host string) (Token, error) {
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := env.Lookup(k); v != "" {
			if !strings.HasPrefix(v, "ghp_") {
				return Token{Source: SourceEnv, Token: v}, nil
			}
			break // the CLI reads the first one set
		}
	}
	tok, err := savedLogin(env)
	if err != nil || tok.Source != "" {
		return tok, err
	}
	if t := ghToken(ctx, env, host); t != "" && !strings.HasPrefix(t, "ghp_") {
		return Token{Source: SourceGhCLI, Token: t}, nil
	}
	return Token{}, nil
}

// Config is the part of config.json under the CLI's home that holds the
// login: the accounts signed in, and copilotTokens, "<host>:<login>" to the
// token, filled only when the token is stored in plain text.
type Config struct {
	LastLoggedInUser *LoggedInUser     `json:"lastLoggedInUser"`
	LoggedInUsers    []LoggedInUser    `json:"loggedInUsers"`
	CopilotTokens    map[string]string `json:"copilotTokens"`
}

type LoggedInUser struct {
	Host  string `json:"host"`
	Login string `json:"login"`
}

func savedLogin(env usage.Env) (Token, error) {
	raw, err := os.ReadFile(filepath.Join(Home(env), "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, nil
	}
	if err != nil {
		return Token{}, err
	}
	var c Config
	if err := json.Unmarshal(stripComments(raw), &c); err != nil {
		return Token{}, fmt.Errorf("copilot: config.json: %w", err)
	}
	user := c.LastLoggedInUser
	if user == nil && len(c.LoggedInUsers) > 0 {
		user = &c.LoggedInUsers[0]
	}
	if user == nil {
		// No account recorded: the CLI takes any token saved in plain text.
		for key, t := range c.CopilotTokens {
			if strings.TrimSpace(t) != "" {
				h, login, _ := strings.Cut(key, ":")
				return Token{Source: SourceLogin, Token: t, Login: login, Host: normalizeHost(h)}, nil
			}
		}
		return Token{}, nil
	}
	if t := c.CopilotTokens[user.Host+":"+user.Login]; strings.TrimSpace(t) != "" {
		return Token{Source: SourceLogin, Token: t, Login: user.Login, Host: normalizeHost(user.Host)}, nil
	}
	return Token{Source: SourceKeyring, Login: user.Login}, nil
}

// stripComments drops whole-line // comments, which the CLI writes at the
// top of config.json.
func stripComments(raw []byte) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	out := lines[:0]
	for _, l := range lines {
		if !bytes.HasPrefix(bytes.TrimSpace(l), []byte("//")) {
			out = append(out, l)
		}
	}
	return bytes.Join(out, []byte("\n"))
}

// ghToken is what `gh auth token --hostname <host>` prints, as the CLI runs
// it, or "" when gh is missing or not signed in.
func ghToken(ctx context.Context, env usage.Env, host string) string {
	bin := "gh"
	if path := env.Lookup("PATH"); path != "" {
		bin = lookPath("gh", path)
		if bin == "" {
			return ""
		}
	}
	cmd := exec.CommandContext(ctx, bin, "auth", "token", "--hostname", strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"))
	// A child that keeps the output open does not hold the probe past ctx.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = env.Env
	cmd.Dir = env.Dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// lookPath finds name in a PATH that is the agent's rather than this
// process's.
func lookPath(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// Home is the CLI's config directory: COPILOT_HOME, else ~/.copilot.
func Home(env usage.Env) string {
	if h := env.Lookup("COPILOT_HOME"); h != "" {
		return h
	}
	return filepath.Join(env.Home(), ".copilot")
}

// Host is the GitHub server the CLI signs in to.
func Host(env usage.Env) string {
	h := env.Lookup("COPILOT_GH_HOST")
	if h == "" {
		h = env.Lookup("GH_HOST")
	}
	if h == "" {
		return DefaultHost
	}
	return normalizeHost(h)
}

// normalizeHost is h as an https origin. A token never goes out in clear
// text, so http:// is upgraded unless it names this machine (a test
// server).
func normalizeHost(h string) string {
	h = strings.TrimRight(strings.TrimSpace(h), "/")
	if h == "" {
		return DefaultHost
	}
	if rest, ok := strings.CutPrefix(h, "http://"); ok {
		if name, _, _ := strings.Cut(rest, ":"); name == "localhost" || name == "127.0.0.1" {
			return h
		}
		h = rest
	}
	if !strings.HasPrefix(h, "https://") {
		h = "https://" + h
	}
	return h
}

// APIURL is the host's REST API: api.<host>, as the CLI derives it, unless
// COPILOT_DEBUG_GITHUB_API_URL moves it.
func APIURL(env usage.Env, host string) string {
	if u := env.Lookup("COPILOT_DEBUG_GITHUB_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	scheme, name, _ := strings.Cut(host, "://")
	if !strings.HasPrefix(name, "api.") {
		name = "api." + name
	}
	return scheme + "://" + name
}
