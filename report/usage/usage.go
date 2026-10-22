// Package usage reads what coding agents report about usage: a turn's
// tokens, how full the context window is, a plan's rate limits and the
// account an agent runs as. Every agent reports these in a format of its own;
// this package holds what they share, and a package per agent maps that
// agent's messages onto the agentprotocol types (UsageUpdatedPayload,
// ContextUpdatedPayload, UsageLimits, AgentAccount). The session drivers call
// those mappings for what arrives during a session; a Prober reads the
// account and limits outside one.
//
// State keeps one agent's latest account and limits as probes and pushes
// arrive, with the rules for combining them: a push updates the windows it
// names, a failed probe keeps the last good windows, and a different account
// drops the previous account's limits.
package usage

import (
	"context"
	"slices"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
)

// Env is how a probe runs the agent: the binary, its working directory and
// environment, and Args, the arguments the caller's sessions start it with
// (config overrides, a provider), so the probe reads the account those
// sessions run as. An empty Binary uses the agent's default; a nil Env
// inherits the caller's.
type Env struct {
	Binary string
	Args   []string
	Env    []string
	Dir    string
}

// Prober reads an agent's account and rate limits outside a session. It does
// not start a model turn. A probe that can read the account but not the
// limits returns the account with Limits.Unavailable set; an error means
// neither could be read.
type Prober interface {
	Probe(ctx context.Context, env Env) (ap.UsageSnapshot, error)
}

// State is one agent's latest account and limits. The zero value is empty
// and ready to use. It is not safe for concurrent use.
type State struct {
	Account *ap.AgentAccount
	Limits  *ap.UsageLimits
}

// ApplyProbe takes a probe's result. A probe that read limits replaces them;
// one whose limits failed to read keeps the last good windows. An account
// different from the one held drops the held limits first, since they were
// another account's.
func (s *State) ApplyProbe(snap ap.UsageSnapshot) {
	if snap.Account != nil {
		s.ApplyAccount(*snap.Account)
	}
	if snap.Limits == nil {
		return
	}
	if snap.Limits.Unavailable != nil && snap.Limits.Unavailable.Reason == ap.UsageProbeFailed &&
		s.Limits != nil && len(s.Limits.Windows) > 0 {
		return
	}
	l := normalize(*snap.Limits)
	s.Limits = &l
}

// ApplyAccount takes the account an agent reported. A different account
// drops the limits held for the previous one.
func (s *State) ApplyAccount(a ap.AgentAccount) {
	if s.Account != nil && !SameAccount(*s.Account, a) {
		s.Limits = nil
	}
	s.Account = &a
}

// ApplyUpdate takes windows an agent pushed during a session: each replaces
// the held window with its ID, keeping that window's reset time and duration
// when the push leaves them out, and the others stay. Limits read as
// unsupported stay that way, since a push cannot make an API-key account
// have plan limits. It reports whether anything changed.
func (s *State) ApplyUpdate(u ap.LimitsUpdatedPayload, now time.Time) bool {
	if len(u.Windows) == 0 {
		return false
	}
	if s.Limits != nil && s.Limits.Unavailable != nil && s.Limits.Unavailable.Reason == ap.UsageUnsupported {
		return false
	}
	var next ap.UsageLimits
	if s.Limits != nil {
		next.Windows = slices.Clone(s.Limits.Windows)
	}
	changed := false
	for _, w := range u.Windows {
		w.UsedPercent = clampPercent(w.UsedPercent)
		i := slices.IndexFunc(next.Windows, func(h ap.UsageWindow) bool { return h.ID == w.ID })
		if i < 0 {
			next.Windows = append(next.Windows, w)
			changed = true
			continue
		}
		held := next.Windows[i]
		if w.ResetsAt.IsZero() {
			w.ResetsAt = held.ResetsAt
		}
		if w.DurationMinutes == 0 {
			w.DurationMinutes = held.DurationMinutes
		}
		if w.Label == "" {
			w.Label = held.Label
		}
		if w.Kind == "" {
			w.Kind = held.Kind
		}
		if w != held {
			next.Windows[i] = w
			changed = true
		}
	}
	if !changed {
		return false
	}
	next.CheckedAt = now
	next = normalize(next)
	s.Limits = &next
	return true
}

// SameAccount reports whether two reports name the same account: the same
// email, ignoring case, and the same workspace when both name one.
func SameAccount(a, b ap.AgentAccount) bool {
	if !strings.EqualFold(a.Email, b.Email) {
		return false
	}
	return a.Workspace == "" || b.Workspace == "" || a.Workspace == b.Workspace
}

// Unavailable is limits with no windows, for the reason given.
func Unavailable(reason ap.UsageUnavailableReason, msg string, now time.Time) *ap.UsageLimits {
	return &ap.UsageLimits{CheckedAt: now, Windows: []ap.UsageWindow{}, Unavailable: &ap.UsageUnavailable{Reason: reason, Message: msg}}
}

// Limits is windows read at now, in display order.
func Limits(windows []ap.UsageWindow, now time.Time) *ap.UsageLimits {
	l := normalize(ap.UsageLimits{CheckedAt: now, Windows: windows})
	return &l
}

// KindForDuration is the window kind for a window of the given length.
func KindForDuration(minutes int) ap.UsageWindowKind {
	switch {
	case minutes >= 28*24*60:
		return ap.UsageWindowMonthly
	case minutes >= 7*24*60:
		return ap.UsageWindowWeekly
	case minutes > 0:
		return ap.UsageWindowSession
	default:
		return ap.UsageWindowOther
	}
}

// normalize clamps percentages and orders windows session, weekly, monthly,
// other, then by ID, so a display and a comparison see one order.
func normalize(l ap.UsageLimits) ap.UsageLimits {
	if l.Windows == nil {
		l.Windows = []ap.UsageWindow{}
	}
	for i := range l.Windows {
		l.Windows[i].UsedPercent = clampPercent(l.Windows[i].UsedPercent)
	}
	slices.SortStableFunc(l.Windows, func(a, b ap.UsageWindow) int {
		if d := kindRank(a.Kind) - kindRank(b.Kind); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	return l
}

func kindRank(k ap.UsageWindowKind) int {
	switch k {
	case ap.UsageWindowSession:
		return 0
	case ap.UsageWindowWeekly:
		return 1
	case ap.UsageWindowMonthly:
		return 2
	default:
		return 3
	}
}

func clampPercent(p float64) float64 {
	switch {
	case p < 0 || p != p: // NaN
		return 0
	case p > 100:
		return 100
	default:
		return p
	}
}
