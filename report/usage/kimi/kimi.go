// Package kimi reads Kimi Code's account and plan limits. The CLI reports
// neither over ACP, so Prober reads them the way the CLI's own /usage does:
// with the login kimi saved, from the Kimi Code API's /me (the account and
// its membership level) and /usages (the five-hour, weekly and monthly
// quotas). Signed in with an API key, through KIMI_MODEL_* or a provider of
// another type, there is no plan to read.
package kimi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/tomlstr"
	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// The Kimi Code API and OAuth host of a mainland-China login, the ones kimi
// uses unless the environment or the saved login names others.
// KIMI_CODE_BASE_URL in the agent's environment moves the API, as it does
// for the CLI.
const (
	DefaultBaseURL   = "https://api.kimi.com/coding/v1"
	defaultOAuthHost = "https://auth.kimi.com"
)

// ManagedProvider is the config.toml provider kimi login writes.
const ManagedProvider = "managed:kimi-code"

// defaultOAuthKey is the credential slot of a login to the default hosts;
// any other hosts get a slot of their own (resolveKimiCodeOAuthKey).
const defaultOAuthKey = "oauth/kimi-code"

// ProbeTimeout bounds a probe: two API calls.
const ProbeTimeout = 15 * time.Second

// Prober reads the account and the quotas from the login kimi saved. It
// never starts the CLI and never calls a model. An expired token is reported
// as a failed read rather than refreshed: a refresh rotates the refresh token
// and kimi would have to save it, so the probe leaves that to kimi.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	home := kimiHome(env)
	now := time.Now()

	l, err := ReadLogin(env, home)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	switch l.Kind {
	case LoginNone:
		return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
	case LoginAPIKey:
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "plan quota applies only to a Kimi Code login", now),
		}, nil
	}

	creds, err := readCreds(filepath.Join(home, "credentials", l.CredentialFile))
	if errors.Is(err, os.ErrNotExist) {
		return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
	}
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	var expires time.Time
	if creds.ExpiresAt > 0 {
		expires = time.Unix(int64(creds.ExpiresAt), 0)
	}
	return Snapshot(ctx, p.Client, l.BaseURL, creds.AccessToken, expires, now), nil
}

// Snapshot reads a Kimi Code login's account and quota with its access
// token, from base (the coding API root; empty uses kimi's default). A token
// past expires reads as probe_failed. pi's kimi-coding login reads the same.
func Snapshot(ctx context.Context, client *http.Client, base, token string, expires, now time.Time) ap.UsageSnapshot {
	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}
	snap := ap.UsageSnapshot{Account: &acct}
	if !expires.IsZero() && !expires.After(now) {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "the saved login has expired; the agent refreshes it on its next run", now)
		return snap
	}
	if base == "" {
		base = DefaultBaseURL
	}
	c := api{client: client, base: base, token: token}
	var me Me
	if err := c.get(ctx, "me", &me); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap
	}
	acct.Email = me.Email
	acct.Plan, acct.PlanLabel = Plan(me)
	var u Usages
	if err := c.get(ctx, "usages", &u); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap
	}
	snap.Limits = usage.Limits(Windows(u, now), now)
	return snap
}

// LoginKind is how kimi's sessions authenticate.
type LoginKind int

const (
	LoginNone   LoginKind = iota
	LoginOAuth            // a Kimi Code login
	LoginAPIKey           // an API key, of Kimi's or another provider's
)

// Login is the login kimi's sessions run with: for an OAuth login, the
// credentials file under <home>/credentials and the API it signs in to.
type Login struct {
	Kind           LoginKind
	CredentialFile string
	BaseURL        string
}

// ReadLogin finds the login as kimi does: KIMI_MODEL_NAME (an API-key model
// from the environment) wins; else the provider of config.toml's
// default_model, an OAuth login when it carries an oauth ref; else the slot
// a login to the environment's hosts is saved in; else KIMI_API_KEY.
func ReadLogin(env usage.Env, home string) (Login, error) {
	if env.Lookup("KIMI_MODEL_NAME") != "" {
		return Login{Kind: LoginAPIKey}, nil
	}
	cfg, err := tomlstr.Read(filepath.Join(home, "config.toml"))
	if err != nil {
		return Login{}, err
	}
	name := cfg[tomlstr.Key("models", cfg[tomlstr.Key("default_model")], "provider")]
	if name == "" && cfg[tomlstr.Key("providers", ManagedProvider, "type")] != "" {
		name = ManagedProvider
	}
	if name != "" {
		prov := func(k ...string) string { return cfg[tomlstr.Key(append([]string{"providers", name}, k...)...)] }
		if key := prov("oauth", "key"); key != "" {
			base := firstNonEmpty(env.Lookup("KIMI_CODE_BASE_URL"), prov("base_url"), DefaultBaseURL)
			file, err := credentialFile(key)
			if err != nil {
				return Login{}, err
			}
			return Login{Kind: LoginOAuth, CredentialFile: file, BaseURL: strings.TrimRight(base, "/")}, nil
		}
		if prov("type") != "" {
			return Login{Kind: LoginAPIKey}, nil
		}
	}
	base := strings.TrimRight(firstNonEmpty(env.Lookup("KIMI_CODE_BASE_URL"), DefaultBaseURL), "/")
	host := firstNonEmpty(env.Lookup("KIMI_CODE_OAUTH_HOST"), env.Lookup("KIMI_OAUTH_HOST"), defaultOAuthHost)
	file, _ := credentialFile(OAuthKey(host, base))
	if _, err := os.Stat(filepath.Join(home, "credentials", file)); err == nil {
		return Login{Kind: LoginOAuth, CredentialFile: file, BaseURL: base}, nil
	}
	if env.Lookup("KIMI_API_KEY") != "" {
		return Login{Kind: LoginAPIKey}, nil
	}
	return Login{Kind: LoginNone}, nil
}

// OAuthKey is the credential slot kimi saves a login to the given hosts in:
// the shared slot for the default hosts, else one named by a hash of both.
func OAuthKey(oauthHost, baseURL string) string {
	oauthHost = strings.TrimRight(strings.TrimSpace(oauthHost), "/")
	baseURL = strings.TrimRight(baseURL, "/")
	if oauthHost == defaultOAuthHost && baseURL == DefaultBaseURL {
		return defaultOAuthKey
	}
	// JSON.stringify({oauthHost, baseUrl}), without Go's HTML escaping.
	h := sha256.Sum256([]byte(`{"oauthHost":` + jsString(oauthHost) + `,"baseUrl":` + jsString(baseURL) + `}`))
	return "oauth/kimi-code-env-" + hex.EncodeToString(h[:])[:16]
}

func jsString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// credentialFile is the file a slot is saved in (resolveKimiTokenStorageName).
func credentialFile(key string) (string, error) {
	name := strings.TrimPrefix(key, "oauth/")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("kimi: invalid OAuth key %q in config.toml", key)
	}
	return name + ".json", nil
}

// Plan is the membership level's number and display name.
func Plan(m Me) (id, label string) {
	if m.UserLevel != nil && *m.UserLevel != 0 {
		id = strconv.FormatInt(int64(*m.UserLevel), 10)
	}
	return id, m.UserLevelName
}

// The quotas /usages reports, by key: the window each covers and its label.
var quotaWindows = map[string]struct {
	kind    ap.UsageWindowKind
	label   string
	minutes int
}{
	"limit_5h":          {ap.UsageWindowSession, "5-hour", 5 * 60},
	"limit_7d":          {ap.UsageWindowWeekly, "Weekly", 7 * 24 * 60},
	"limit_month_total": {ap.UsageWindowMonthly, "Monthly total", 0},
	"limit_month_code":  {ap.UsageWindowMonthly, "Monthly code", 0},
}

// Windows maps /usages. Current servers send the usages object of used
// ratios; older ones sent a weekly summary (usage), per-window rows (limits)
// and a totalQuota, read only when usages has nothing. The older rows get
// the IDs of the matching usages keys, so a window keeps its ID across the
// two shapes.
func Windows(u Usages, now time.Time) []ap.UsageWindow {
	var ws []ap.UsageWindow
	for key, q := range u.Usages {
		if q.UsedRatio == nil {
			continue
		}
		w := ap.UsageWindow{ID: key, Kind: ap.UsageWindowOther, Label: key, UsedPercent: float64(*q.UsedRatio) * 100, ResetsAt: resetTime(q.ResetTime)}
		if d, ok := quotaWindows[key]; ok {
			w.Kind, w.Label, w.DurationMinutes = d.kind, d.label, d.minutes
		}
		ws = append(ws, w)
	}
	if len(ws) > 0 {
		return ws
	}
	if u.Usage != nil {
		if pct, ok := u.Usage.usedPercent(); ok {
			ws = append(ws, ap.UsageWindow{ID: "limit_7d", Kind: ap.UsageWindowWeekly, Label: "Weekly", UsedPercent: pct, DurationMinutes: 7 * 24 * 60, ResetsAt: u.Usage.resetsAt()})
		}
	}
	for i, l := range u.Limits {
		d := l.Detail
		if d == nil {
			d = &l.Quota
		}
		pct, ok := d.usedPercent()
		if !ok {
			continue
		}
		minutes := l.Window.minutes()
		w := ap.UsageWindow{ID: "limit_" + strconv.Itoa(i+1), Kind: usage.KindForDuration(minutes), UsedPercent: pct, DurationMinutes: minutes, ResetsAt: d.resetsAt()}
		// Named by span as the usages keys are, unless an earlier row took
		// that ID: IDs must stay unique for pushed updates to land.
		if span := spanID(minutes); span != "" && !slices.ContainsFunc(ws, func(h ap.UsageWindow) bool { return h.ID == "limit_"+span }) {
			w.ID = "limit_" + span
		}
		w.Label = firstNonEmpty(l.Name, l.Title, d.Name, d.Title, spanLabel(minutes), "Limit")
		ws = append(ws, w)
	}
	if q := u.TotalQuota; q != nil {
		if pct, ok := q.usedPercent(); ok {
			minutes := q.Window.minutes()
			ws = append(ws, ap.UsageWindow{ID: "total_quota", Kind: usage.KindForDuration(minutes), Label: firstNonEmpty(q.Name, q.Title, "Total quota"), UsedPercent: pct, DurationMinutes: minutes, ResetsAt: q.resetsAt()})
		}
	}
	return ws
}

// spanID names a window by its length as the usages keys do ("5h", "7d").
func spanID(minutes int) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes%(24*60) == 0:
		return strconv.Itoa(minutes/(24*60)) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	}
	return strconv.Itoa(minutes) + "m"
}

func spanLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes == 7*24*60:
		return "Weekly"
	case minutes%(24*60) == 0:
		return strconv.Itoa(minutes/(24*60)) + "-day"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "-hour"
	}
	return strconv.Itoa(minutes) + "-minute"
}

// The Kimi Code API's shapes, the fields a probe reads.
type (
	Me struct {
		UserID        string `json:"user_id"`
		Email         string `json:"email,omitempty"`
		UserLevel     *num   `json:"user_level,omitempty"`
		UserLevelName string `json:"user_level_name,omitempty"`
	}
	Usages struct {
		Usages     map[string]RatioQuota `json:"usages,omitempty"`
		Usage      *Quota                `json:"usage,omitempty"`
		Limits     []LimitRow            `json:"limits,omitempty"`
		TotalQuota *TotalQuota           `json:"totalQuota,omitempty"`
	}
	// RatioQuota is one entry of usages: the share used, 0 to 1.
	RatioQuota struct {
		UsedRatio *num   `json:"used_ratio,omitempty"`
		ResetTime string `json:"reset_time,omitempty"`
	}
	// Quota is an older server's amount: used of limit, or remaining.
	Quota struct {
		Name      string `json:"name,omitempty"`
		Title     string `json:"title,omitempty"`
		Limit     *num   `json:"limit,omitempty"`
		Used      *num   `json:"used,omitempty"`
		Remaining *num   `json:"remaining,omitempty"`
		// The reset, an ISO time or epoch seconds or milliseconds, under
		// whichever name the server used.
		ResetTime      any `json:"resetTime,omitempty"`
		ResetTimeSnake any `json:"reset_time,omitempty"`
		ResetAt        any `json:"resetAt,omitempty"`
		ResetAtSnake   any `json:"reset_at,omitempty"`
	}
	LimitRow struct {
		Quota
		Detail *Quota    `json:"detail,omitempty"`
		Window LimitSpan `json:"window"`
	}
	TotalQuota struct {
		Quota
		Window LimitSpan `json:"window"`
	}
	LimitSpan struct {
		Duration num    `json:"duration"`
		TimeUnit string `json:"timeUnit"` // TIME_UNIT_MINUTE, _HOUR, _DAY
	}
)

func (q Quota) usedPercent() (float64, bool) {
	if q.Limit == nil || *q.Limit <= 0 {
		return 0, false
	}
	used := q.Used
	if used == nil && q.Remaining != nil {
		u := *q.Limit - *q.Remaining
		used = &u
	}
	if used == nil {
		return 0, false
	}
	return float64(*used / *q.Limit * 100), true
}

func (q Quota) resetsAt() time.Time {
	for _, v := range []any{q.ResetTime, q.ResetTimeSnake, q.ResetAt, q.ResetAtSnake} {
		switch v := v.(type) {
		case string:
			if t := resetTime(v); !t.IsZero() {
				return t
			}
		case float64:
			if v > 1e12 {
				return time.UnixMilli(int64(v)).UTC()
			}
			if v > 0 {
				return time.Unix(int64(v), 0).UTC()
			}
		}
	}
	return time.Time{}
}

func (s LimitSpan) minutes() int {
	d := float64(s.Duration)
	switch u := strings.ToUpper(s.TimeUnit); {
	case strings.Contains(u, "SECOND"):
		return int(math.Round(d / 60))
	case strings.Contains(u, "MINUTE"):
		return int(d)
	case strings.Contains(u, "HOUR"):
		return int(d * 60)
	case strings.Contains(u, "DAY"):
		return int(d * 24 * 60)
	case strings.Contains(u, "WEEK"):
		return int(d * 7 * 24 * 60)
	}
	return 0
}

func resetTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// num is a number the API may send as a JSON number or a string.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // an unreadable value reads as absent
	}
	*n = num(f)
	return nil
}

// oauthToken is a credentials file, kimi's saved OAuth token.
type oauthToken struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresAt    float64 `json:"expires_at"` // seconds since the epoch
}

func readCreds(path string) (oauthToken, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return oauthToken{}, err
	}
	var t oauthToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return oauthToken{}, fmt.Errorf("kimi: %s: %w", filepath.Base(path), err)
	}
	if t.AccessToken == "" {
		return oauthToken{}, os.ErrNotExist
	}
	return t, nil
}

type api struct {
	client *http.Client
	base   string
	token  string
}

func (a api) get(ctx context.Context, path string, out any) error {
	if err := webjson.Get(ctx, a.client, a.base+"/"+path, map[string]string{"Authorization": "Bearer " + a.token, "Accept": "application/json"}, out); err != nil {
		return fmt.Errorf("kimi: /%s: %w", path, err)
	}
	return nil
}

// kimiHome is the CLI's data directory: KIMI_CODE_HOME when it is set, else
// ~/.kimi-code.
func kimiHome(env usage.Env) string {
	if h := env.Lookup("KIMI_CODE_HOME"); h != "" {
		return h
	}
	return filepath.Join(env.Home(), ".kimi-code")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
