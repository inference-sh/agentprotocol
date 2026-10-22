// Package opencode reads opencode's account and plan limits. opencode
// reports neither over ACP, and of the providers it signs in to only one has
// limits it can read with the saved login: the OpenCode Go subscription,
// whose five-hour, weekly and monthly windows the Go usage endpoint reports
// for the Go key. Prober reads that with the key opencode saved; any other
// login (Zen's pay-as-you-go key included) has no plan limits to read.
package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// ProviderGo is the auth.json key of an OpenCode Go key.
const ProviderGo = "opencode-go"

// DefaultUsageURL is OpenCode Go's usage endpoint. It stays on Zen while
// inference moved to /inference/go.
const DefaultUsageURL = "https://opencode.ai/zen/go/v1/usage"

// Window IDs, the keys the usage endpoint reports them under.
const (
	WindowRolling = "rolling"
	WindowWeekly  = "weekly"
	WindowMonthly = "monthly"
)

// ProbeTimeout bounds a probe: one API call.
const ProbeTimeout = 10 * time.Second

// Prober reads the OpenCode Go windows with the key opencode saved. It never
// starts the CLI and never calls a model.
type Prober struct {
	// Client makes the request; nil uses http.DefaultClient.
	Client *http.Client
	// UsageURL replaces DefaultUsageURL, for tests.
	UsageURL string
	// Session is sent as x-opencode-session, which the endpoint requires.
	// opencode sends its session's ID there; a probe has no session, so
	// empty sends a random ID per probe.
	Session string
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	now := time.Now()

	auth, err := ReadAuth(env)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	name := SessionProvider(env.Args, auth)
	entry, ok := auth[name]
	switch {
	case !ok:
		return usage.NoSavedLogin(env, now), nil
	case name != ProviderGo || entry.Type != "api":
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: entry.AccountType()},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "plan limits are read only for an OpenCode Go key", now),
		}, nil
	}

	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: ProviderGo, PlanLabel: "OpenCode Go"}
	snap := ap.UsageSnapshot{Account: &acct}
	u := p.UsageURL
	if u == "" {
		u = DefaultUsageURL
	}
	var r UsageResponse
	err = webjson.Get(ctx, p.Client, u, map[string]string{
		"Authorization":      "Bearer " + entry.Key,
		"Accept":             "application/json",
		"x-opencode-session": p.session(),
	}, &r)
	var se *webjson.StatusError
	if errors.As(err, &se) && se.Code == http.StatusForbidden {
		// The key's workspace has no Go subscription: it bills as Zen does.
		acct.Type, acct.Plan, acct.PlanLabel = ap.AccountAPIKey, "", ""
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "no OpenCode Go subscription on this key", now)
		return snap, nil
	}
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, fmt.Sprintf("opencode: Go usage: %v", err), now)
		return snap, nil
	}
	ws := Windows(r)
	if len(ws) == 0 {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "opencode: Go usage reported no windows", now)
		return snap, nil
	}
	snap.Limits = usage.Limits(ws, now)
	return snap, nil
}

func (p Prober) session() string {
	if p.Session != "" {
		return p.Session
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "ses_" + hex.EncodeToString(b)
}

// Go's windows: rolling is the five-hour session; the monthly one runs from
// the subscription's anniversary, so it has no fixed length.
var goWindows = []struct {
	id      string
	kind    ap.UsageWindowKind
	label   string
	minutes int
}{
	{WindowRolling, ap.UsageWindowSession, "5-hour", 5 * 60},
	{WindowWeekly, ap.UsageWindowWeekly, "Weekly", 7 * 24 * 60},
	{WindowMonthly, ap.UsageWindowMonthly, "Monthly", 0},
}

// Windows maps the usage response, skipping a window it leaves out. A
// rate-limited window reads as used up whatever its percent says.
func Windows(r UsageResponse) []ap.UsageWindow {
	var ws []ap.UsageWindow
	for _, g := range goWindows {
		gw, ok := r.Usage[g.id]
		if !ok || gw.Percent == nil {
			continue
		}
		w := ap.UsageWindow{ID: g.id, Kind: g.kind, Label: g.label, UsedPercent: *gw.Percent, DurationMinutes: g.minutes}
		if gw.Status == "rate-limited" {
			w.UsedPercent = 100
		}
		if t, err := time.Parse(time.RFC3339Nano, gw.ResetsAt); err == nil {
			w.ResetsAt = t.UTC()
		}
		ws = append(ws, w)
	}
	return ws
}

// UsageResponse is the Go usage endpoint's body: per window, "ok" or
// "rate-limited", the percent used (0 to 100) and when it resets.
type UsageResponse struct {
	Usage map[string]struct {
		Status   string   `json:"status"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resetsAt"`
	} `json:"usage"`
}

// SessionProvider is the auth.json entry opencode's sessions use: the
// provider of a --model (-m) provider/model in args, else the Go key, else
// the first other login by name. opencode also picks a model from its
// config; the probe does not read that.
func SessionProvider(args []string, auth Auth) string {
	if p := ModelProvider(args); p != "" {
		return p
	}
	if _, ok := auth[ProviderGo]; ok {
		return ProviderGo
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

// Auth is auth.json: a login per provider.
type Auth map[string]AuthEntry

// AuthEntry is one login: an OAuth token, an API key, or a well-known
// provider's token.
type AuthEntry struct {
	Type string `json:"type"` // oauth, api, wellknown
	Key  string `json:"key,omitempty"`
}

// AccountType is how a login signs in: an OAuth login is that provider's
// plan (ChatGPT, Copilot, SuperGrok), anything else a key.
func (e AuthEntry) AccountType() ap.AccountType {
	if e.Type == "oauth" {
		return ap.AccountSubscription
	}
	return ap.AccountAPIKey
}

// ReadAuth reads the logins as opencode does: OPENCODE_AUTH_CONTENT (the
// whole auth.json as a string) when it is set, else
// $XDG_DATA_HOME/opencode/auth.json. A missing file is no logins.
func ReadAuth(env usage.Env) (Auth, error) {
	if c := env.Lookup("OPENCODE_AUTH_CONTENT"); c != "" {
		var a Auth
		if json.Unmarshal([]byte(c), &a) == nil {
			return a, nil
		}
	}
	dir := env.Lookup("XDG_DATA_HOME")
	if dir == "" {
		dir = filepath.Join(env.Home(), ".local", "share")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "opencode", "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Auth{}, nil
	}
	if err != nil {
		return nil, err
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("opencode: auth.json: %w", err)
	}
	return a, nil
}
