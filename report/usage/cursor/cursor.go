// Package cursor reads Cursor CLI's account and plan usage. The CLI reports
// neither over ACP, so Prober reads them the way the CLI's own /usage does:
// from the login cursor-agent saved, through Cursor's DashboardService (GetMe
// for the account, GetPlanInfo for the plan, GetCurrentPeriodUsage for how
// much of the plan's included usage is spent). A Cursor API key is a user's
// key and bills against that user's plan, so it has the same limits.
package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// Cursor's API. CURSOR_API_ENDPOINT in the agent's environment moves it, as
// it does for the CLI.
const (
	DefaultEndpoint = "https://api2.cursor.sh"
	dashboard       = "/aiserver.v1.DashboardService/"
)

// ProbeTimeout bounds a probe: an API key exchange and four API calls.
const ProbeTimeout = 20 * time.Second

// Prober reads the account and the plan's usage from the login cursor-agent
// saved, or from CURSOR_API_KEY or CURSOR_AUTH_TOKEN as the CLI would use
// them. It never starts the CLI and never calls a model. A token it gets for
// an API key is used for the probe and not saved, so auth.json is left as it
// was.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	now := time.Now()
	c := api{client: p.Client, base: endpoint(env)}

	l, err := readLogin(env, runtime.GOOS)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	switch {
	case l.keychain:
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountUnknown},
			Limits:  usage.Unavailable(ap.UsageProbeFailed, "the login is in the macOS keychain", now),
		}, nil
	case l.authToken == "" && l.apiKey == "" && l.accessToken == "":
		return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
	}

	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}
	if l.apiKey != "" {
		acct.Type = ap.AccountAPIKey
	}
	snap := ap.UsageSnapshot{Account: &acct}
	token, err := l.token(ctx, c, now)
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	c.token = token

	var me GetMeResponse
	if err := c.call(ctx, "GetMe", &me); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	acct.Email, acct.Workspace = me.Email, me.TeamName
	// The CLI's /usage shows the period without a plan name when this fails.
	var plan GetPlanInfoResponse
	if c.call(ctx, "GetPlanInfo", &plan) == nil && plan.PlanInfo != nil {
		acct.Plan, acct.PlanLabel = Plan(*plan.PlanInfo)
	}
	var period GetCurrentPeriodUsageResponse
	if err := c.call(ctx, "GetCurrentPeriodUsage", &period); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	ws := Windows(period, plan.PlanInfo)
	if len(ws) == 0 {
		// Enterprise plans have no included allowance, only spend.
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "the plan has no included usage to measure", now)
		return snap, nil
	}
	snap.Limits = usage.Limits(ws, now)
	return snap, nil
}

// Plan is the plan's id and display name. Cursor names plans only for
// display ("Pro", "Pro+", "Ultra"); the id is that name in lower case.
func Plan(p PlanInfo) (id, label string) {
	label = strings.TrimSpace(p.PlanName)
	return strings.ToLower(label), label
}

// Windows maps the period's included usage: the plan's whole allowance and,
// when Cursor splits it, the Auto pool (Cursor's own models) and the API
// pool (other vendors' models), as the CLI's /usage shows them. A plan with
// no included allowance has none.
func Windows(r GetCurrentPeriodUsageResponse, plan *PlanInfo) []ap.UsageWindow {
	u := r.PlanUsage
	if u == nil {
		return nil
	}
	start, end := r.BillingCycleStart.Time(), r.BillingCycleEnd.Time()
	if end.IsZero() && plan != nil {
		end = plan.BillingCycleEnd.Time()
	}
	minutes := 0
	if !start.IsZero() && end.After(start) {
		minutes = int(end.Sub(start) / time.Minute)
	}
	if plan != nil && strings.HasSuffix(plan.IncludedUsagePeriod, "WEEKLY") {
		minutes = 7 * 24 * 60
		if t := plan.IncludedUsageResetsAt.Time(); !t.IsZero() {
			end = t
		}
	}
	kind := usage.KindForDuration(minutes)
	if minutes == 0 {
		kind = ap.UsageWindowMonthly // Cursor bills by the month
	}
	window := func(id, label string, pct float64) ap.UsageWindow {
		return ap.UsageWindow{ID: id, Kind: kind, Label: label, UsedPercent: pct, ResetsAt: end, DurationMinutes: minutes}
	}

	total := 0.0
	switch {
	case u.TotalPercentUsed != nil:
		total = *u.TotalPercentUsed
	case u.Limit > 0:
		total = float64(u.IncludedSpend) / float64(u.Limit) * 100
	}
	ws := []ap.UsageWindow{window("plan", "Included", total)}
	if u.AutoPercentUsed != nil {
		ws = append(ws, window("auto", "Auto", *u.AutoPercentUsed))
	}
	if u.APIPercentUsed != nil {
		ws = append(ws, window("api", "API", *u.APIPercentUsed))
	}
	return ws
}

// DashboardService's shapes in Connect's JSON encoding (camelCase names,
// int64 as a string), the fields a probe reads.
type (
	GetMeResponse struct {
		Email    string `json:"email,omitempty"`
		TeamName string `json:"teamName,omitempty"`
	}
	GetPlanInfoResponse struct {
		PlanInfo *PlanInfo `json:"planInfo,omitempty"`
	}
	PlanInfo struct {
		PlanName        string `json:"planName"`
		BillingCycleEnd Millis `json:"billingCycleEnd,omitempty"`
		// IncludedUsagePeriod is the enum's name, ending MONTHLY or WEEKLY.
		IncludedUsagePeriod   string `json:"includedUsagePeriod,omitempty"`
		IncludedUsageResetsAt Millis `json:"includedUsageResetsAt,omitempty"`
	}
	GetCurrentPeriodUsageResponse struct {
		BillingCycleStart Millis     `json:"billingCycleStart"`
		BillingCycleEnd   Millis     `json:"billingCycleEnd"`
		PlanUsage         *PlanUsage `json:"planUsage,omitempty"`
	}
	// PlanUsage is the period's included usage; spend and limit are cents,
	// the percentages 0 to 100.
	PlanUsage struct {
		IncludedSpend    int64    `json:"includedSpend"`
		Limit            int64    `json:"limit"`
		TotalPercentUsed *float64 `json:"totalPercentUsed,omitempty"`
		AutoPercentUsed  *float64 `json:"autoPercentUsed,omitempty"`
		APIPercentUsed   *float64 `json:"apiPercentUsed,omitempty"`
	}
)

// Millis is an int64 of milliseconds since the epoch, which Connect's JSON
// writes as a string.
type Millis int64

func (m *Millis) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*m = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*m = Millis(n)
	return nil
}

// Time is m as a time; zero when m is not positive.
func (m Millis) Time() time.Time {
	if m <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(m)).UTC()
}

// login is the credential the CLI would run with, in its order: an auth
// token from the environment, an API key (the environment's, else the one
// saved with the login), else the saved login's access token.
type login struct {
	authToken   string
	apiKey      string
	accessToken string // saved in auth.json
	savedKey    string // the API key auth.json's tokens were exchanged for
	keychain    bool   // the login is in the macOS keychain, out of reach
}

// authFile is auth.json, the CLI's file credential store.
type authFile struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	APIKey       string `json:"apiKey"`
}

func readLogin(env usage.Env, goos string) (login, error) {
	l := login{authToken: env.Lookup("CURSOR_AUTH_TOKEN"), apiKey: env.Lookup("CURSOR_API_KEY")}
	if l.authToken != "" {
		l.apiKey = ""
		return l, nil
	}
	store := env.Lookup("AGENT_CLI_CREDENTIAL_STORE")
	if store == "memory" {
		return l, nil
	}
	if goos == "darwin" && store != "file" {
		l.keychain = l.apiKey == ""
		return l, nil
	}
	raw, err := os.ReadFile(authPath(env, goos))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return login{}, err
	}
	var f authFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return login{}, fmt.Errorf("cursor: auth.json: %w", err)
	}
	l.accessToken, l.savedKey = f.AccessToken, f.APIKey
	if l.apiKey == "" {
		l.apiKey = f.APIKey
	}
	return l, nil
}

// token is the access token to read with. An API key's saved token serves
// while it is fresh, else the key is exchanged for one; a browser login's
// token is used as saved, since the CLI cannot refresh one either.
func (l login) token(ctx context.Context, c api, now time.Time) (string, error) {
	switch {
	case l.authToken != "":
		return l.authToken, nil
	case l.apiKey != "":
		if l.accessToken != "" && l.savedKey == l.apiKey && !expired(l.accessToken, now.Add(5*time.Minute)) {
			return l.accessToken, nil
		}
		var tok struct {
			AccessToken string `json:"accessToken"`
		}
		if err := webjson.Post(ctx, c.client, c.base+"/auth/exchange_user_api_key", map[string]string{"Authorization": "Bearer " + l.apiKey}, struct{}{}, &tok); err != nil {
			return "", fmt.Errorf("cursor: exchanging the API key: %w", err)
		}
		if tok.AccessToken == "" {
			return "", errors.New("cursor: exchanging the API key returned no token")
		}
		return tok.AccessToken, nil
	case expired(l.accessToken, now):
		return "", errors.New("cursor: the saved login has expired; sign in again")
	default:
		return l.accessToken, nil
	}
}

// expired reports whether the JWT's exp is before t. A token without a
// readable exp counts as live; the API says when it is not.
func expired(jwt string, t time.Time) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return false
	}
	var c struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp == 0 {
		return false
	}
	return time.Unix(int64(c.Exp), 0).Before(t)
}

type api struct {
	client *http.Client
	base   string
	token  string
}

// call sends a unary DashboardService request with an empty message.
func (a api) call(ctx context.Context, method string, out any) error {
	h := map[string]string{
		"Authorization":            "Bearer " + a.token,
		"Connect-Protocol-Version": "1",
		"x-cursor-client-type":     "cli",
	}
	if err := webjson.Post(ctx, a.client, a.base+dashboard+method, h, struct{}{}, out); err != nil {
		return fmt.Errorf("cursor: %s: %w", method, err)
	}
	return nil
}

// authPath is auth.json: under ~/.cursor on macOS, else under
// XDG_CONFIG_HOME (~/.config).
func authPath(env usage.Env, goos string) string {
	if goos == "darwin" {
		return filepath.Join(env.Home(), ".cursor", "auth.json")
	}
	base := env.Lookup("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(env.Home(), ".config")
	}
	return filepath.Join(base, "cursor", "auth.json")
}

func endpoint(env usage.Env) string {
	if e := env.Lookup("CURSOR_API_ENDPOINT"); e != "" {
		return strings.TrimRight(e, "/")
	}
	return DefaultEndpoint
}
