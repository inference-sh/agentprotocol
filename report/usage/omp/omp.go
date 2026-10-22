// Package omp reads Oh My Pi's account and plan limits from `omp usage
// --json`, which fetches each signed-in provider's usage with omp's own
// logins and reports it in one shape for all of them. omp can be signed in to
// several providers (and several accounts of one); a probe reports the one
// its sessions use.
package omp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// DefaultBinary is the omp command.
const DefaultBinary = "omp"

// ProbeTimeout bounds a probe: omp's start and a usage request per provider.
const ProbeTimeout = 30 * time.Second

// Prober runs `omp usage --json`. It starts no session, so no model is
// called; omp itself refreshes expired logins and caches the reports in its
// agent.db, as it does for its own /usage. It is run without --redact, which
// would shorten the account emails the snapshot reports.
type Prober struct{}

func (Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	bin := env.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	provider := SessionProvider(env.Args)
	args := append(ProfileArgs(env.Args), "usage", "--json")
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// A child that keeps the output open does not hold the probe past ctx.
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir, cmd.Env = env.Dir, env.Env
	out, err := cmd.Output()
	if err != nil {
		return ap.UsageSnapshot{}, fmt.Errorf("omp: usage: %w", err)
	}
	var u Usage
	if err := json.Unmarshal(out, &u); err != nil {
		return ap.UsageSnapshot{}, fmt.Errorf("omp: usage: %w", err)
	}
	snap := Snapshot(u, provider, time.Now())
	if snap.Account == nil {
		snap = usage.NoSavedLogin(env, time.Now())
	}
	return snap, nil
}

// Snapshot picks provider's account out of omp's usage: its first report,
// else its first account omp read no usage for. With no provider named it
// takes the first report, else the first such account. Nothing at all is an
// empty snapshot: omp lists every stored login, an API key's too, but not a
// key that is only in the environment, which Probe then looks for.
func Snapshot(u Usage, provider string, now time.Time) ap.UsageSnapshot {
	for _, r := range u.Reports {
		if provider != "" && r.Provider != provider {
			continue
		}
		acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription,
			Email: r.Metadata.Email, Plan: r.Metadata.PlanType, Workspace: r.Metadata.OrgID}
		ws := Windows(r)
		if len(ws) == 0 {
			return ap.UsageSnapshot{Account: &acct, Limits: usage.Unavailable(ap.UsageProbeFailed, "omp reported no limits for "+r.Provider, now)}
		}
		return ap.UsageSnapshot{Account: &acct, Limits: usage.Limits(ws, now)}
	}
	for _, a := range u.AccountsWithoutUsage {
		if provider != "" && a.Provider != provider {
			continue
		}
		if a.Type == "oauth" {
			// A login whose provider has a usage endpoint but whose read
			// failed, or one omp has no usage reader for.
			return ap.UsageSnapshot{
				Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: a.Email, Workspace: a.OrgID},
				Limits:  usage.Unavailable(ap.UsageProbeFailed, "omp read no usage for the "+a.Provider+" login", now),
			}
		}
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "omp reads no plan limits for the "+a.Provider+" API key", now),
		}
	}
	return ap.UsageSnapshot{}
}

// Windows maps a report's limits, those whose use omp could resolve. IDs
// are the provider and the limit's own ID, so two providers' limits never
// share one.
func Windows(r Report) []ap.UsageWindow {
	ws := make([]ap.UsageWindow, 0, len(r.Limits))
	for _, l := range r.Limits {
		f, ok := l.Amount.UsedFraction()
		if !ok {
			continue
		}
		w := ap.UsageWindow{ID: r.Provider + ":" + l.ID, Kind: ap.UsageWindowOther, Label: l.Label, UsedPercent: f * 100}
		if lw := l.Window; lw != nil {
			w.DurationMinutes = int(lw.DurationMs / 60000)
			w.Kind = windowKind(lw.ID, w.DurationMinutes)
			if lw.ResetsAt > 0 {
				w.ResetsAt = time.UnixMilli(int64(lw.ResetsAt)).UTC()
			}
		}
		ws = append(ws, w)
	}
	return ws
}

// windowKind is the kind of a window of the given length, or, when omp gives
// none (a monthly window anchored on a billing date), of its ID.
func windowKind(id string, minutes int) ap.UsageWindowKind {
	if minutes > 0 {
		return usage.KindForDuration(minutes)
	}
	switch {
	case strings.Contains(id, "month") || id == "30d":
		return ap.UsageWindowMonthly
	case id == "7d" || strings.Contains(id, "week"):
		return ap.UsageWindowWeekly
	case id == "5h":
		return ap.UsageWindowSession
	}
	return ap.UsageWindowOther
}

// SessionProvider is the provider omp's sessions use when their arguments
// say: --provider, else the provider part of a --model provider/model.
// Empty when they do not, as when the model comes from omp's settings.
func SessionProvider(args []string) string {
	model := ""
	for i, a := range args {
		switch {
		case a == "--provider" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--provider="):
			return strings.TrimPrefix(a, "--provider=")
		case a == "--model" && i+1 < len(args):
			model = args[i+1]
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
		}
	}
	if p, _, ok := strings.Cut(model, "/"); ok {
		return p
	}
	return ""
}

// ProfileArgs is the --profile in args, which picks the agent directory and
// so the logins; OMP_PROFILE in the environment does the same on its own.
func ProfileArgs(args []string) []string {
	for i, a := range args {
		switch {
		case a == "--profile" && i+1 < len(args):
			return []string{a, args[i+1]}
		case strings.HasPrefix(a, "--profile="):
			return []string{a}
		}
	}
	return nil
}

// The shapes of `omp usage --json` (omp 18.12), the fields a probe reads.
type (
	Usage struct {
		GeneratedAt          float64   `json:"generatedAt"`
		Reports              []Report  `json:"reports"`
		AccountsWithoutUsage []Account `json:"accountsWithoutUsage"`
	}
	Report struct {
		Provider string  `json:"provider"`
		Limits   []Limit `json:"limits"`
		Metadata struct {
			Email     string `json:"email"`
			AccountID string `json:"accountId"`
			PlanType  string `json:"planType"`
			OrgID     string `json:"orgId"`
		} `json:"metadata"`
	}
	Limit struct {
		ID     string  `json:"id"`
		Label  string  `json:"label"`
		Window *Window `json:"window"`
		Amount Amount  `json:"amount"`
		Status string  `json:"status"` // ok, warning, exhausted, unknown
	}
	Window struct {
		ID         string  `json:"id"`
		Label      string  `json:"label"`
		DurationMs float64 `json:"durationMs"`
		ResetsAt   float64 `json:"resetsAt"` // ms since the epoch
	}
	Amount struct {
		Used              *float64 `json:"used"`
		Limit             *float64 `json:"limit"`
		Remaining         *float64 `json:"remaining"`
		Fraction          *float64 `json:"usedFraction"`
		RemainingFraction *float64 `json:"remainingFraction"`
		Unit              string   `json:"unit"`
	}
	// Account is a stored login omp read no usage for.
	Account struct {
		Provider string `json:"provider"`
		Type     string `json:"type"` // oauth, api_key
		Email    string `json:"email"`
		OrgID    string `json:"orgId"`
	}
)

// UsedFraction is the share used, 0 to 1 (more when over), resolved as omp
// does: the fraction, else used of limit, else a percent, else what the
// remaining fraction leaves.
func (a Amount) UsedFraction() (float64, bool) {
	switch {
	case a.Fraction != nil:
		return *a.Fraction, true
	case a.Used != nil && a.Limit != nil && *a.Limit > 0:
		return *a.Used / *a.Limit, true
	case a.Unit == "percent" && a.Used != nil:
		return *a.Used / 100, true
	case a.RemainingFraction != nil:
		return max(0, 1-*a.RemainingFraction), true
	}
	return 0, false
}
