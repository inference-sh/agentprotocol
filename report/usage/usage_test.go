package usage

import (
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
)

var (
	t0    = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	reset = time.Date(2026, 10, 10, 14, 0, 0, 0, time.UTC)
)

func probed(windows ...ap.UsageWindow) ap.UsageSnapshot {
	return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Email: "a@x.dev"}, Limits: Limits(windows, t0)}
}

func TestProbeOrdersAndClamps(t *testing.T) {
	var s State
	s.ApplyProbe(probed(
		ap.UsageWindow{ID: "seven_day", Kind: ap.UsageWindowWeekly, UsedPercent: 140},
		ap.UsageWindow{ID: "five_hour", Kind: ap.UsageWindowSession, UsedPercent: -3},
	))
	w := s.Limits.Windows
	if w[0].ID != "five_hour" || w[0].UsedPercent != 0 || w[1].ID != "seven_day" || w[1].UsedPercent != 100 {
		t.Fatalf("windows = %+v", w)
	}
}

func TestFailedProbeKeepsTheLastGoodWindows(t *testing.T) {
	var s State
	s.ApplyProbe(probed(ap.UsageWindow{ID: "five_hour", Kind: ap.UsageWindowSession, UsedPercent: 40}))
	s.ApplyProbe(ap.UsageSnapshot{Limits: Unavailable(ap.UsageProbeFailed, "timeout", t0.Add(time.Minute))})
	if len(s.Limits.Windows) != 1 || s.Limits.Windows[0].UsedPercent != 40 {
		t.Fatalf("limits = %+v", s.Limits)
	}
	// With nothing to keep, the failure is what there is.
	var empty State
	empty.ApplyProbe(ap.UsageSnapshot{Limits: Unavailable(ap.UsageProbeFailed, "timeout", t0)})
	if empty.Limits.Unavailable == nil || empty.Limits.Unavailable.Reason != ap.UsageProbeFailed {
		t.Fatalf("limits = %+v", empty.Limits)
	}
}

func TestAnotherAccountDropsTheLimits(t *testing.T) {
	var s State
	s.ApplyProbe(probed(ap.UsageWindow{ID: "five_hour", Kind: ap.UsageWindowSession, UsedPercent: 40}))
	s.ApplyAccount(ap.AgentAccount{Status: ap.AccountAuthenticated, Email: "A@X.dev"})
	if s.Limits == nil {
		t.Fatal("the same account, cased differently, dropped the limits")
	}
	s.ApplyAccount(ap.AgentAccount{Status: ap.AccountAuthenticated, Email: "b@x.dev"})
	if s.Limits != nil {
		t.Fatalf("limits kept across accounts: %+v", s.Limits)
	}
	s.ApplyProbe(probed(ap.UsageWindow{ID: "five_hour", UsedPercent: 1}))
	s.ApplyAccount(ap.AgentAccount{Email: "a@x.dev", Workspace: "org-2"})
	s.ApplyAccount(ap.AgentAccount{Email: "a@x.dev", Workspace: "org-3"})
	if s.Limits != nil {
		t.Fatal("limits kept across workspaces")
	}
}

func TestPushUpdatesOnlyTheWindowsItNames(t *testing.T) {
	var s State
	s.ApplyProbe(probed(
		ap.UsageWindow{ID: "five_hour", Kind: ap.UsageWindowSession, Label: "5h", UsedPercent: 40, ResetsAt: reset, DurationMinutes: 300},
		ap.UsageWindow{ID: "seven_day", Kind: ap.UsageWindowWeekly, UsedPercent: 10},
	))
	if !s.ApplyUpdate(ap.LimitsUpdatedPayload{Windows: []ap.UsageWindow{{ID: "five_hour", UsedPercent: 55}}}, t0.Add(time.Minute)) {
		t.Fatal("no change reported")
	}
	w := s.Limits.Windows
	if w[0].UsedPercent != 55 || !w[0].ResetsAt.Equal(reset) || w[0].DurationMinutes != 300 || w[0].Label != "5h" || w[0].Kind != ap.UsageWindowSession {
		t.Fatalf("five_hour = %+v", w[0])
	}
	if w[1].UsedPercent != 10 {
		t.Fatalf("seven_day = %+v", w[1])
	}
	if s.ApplyUpdate(ap.LimitsUpdatedPayload{Windows: []ap.UsageWindow{{ID: "five_hour", UsedPercent: 55}}}, t0.Add(2*time.Minute)) {
		t.Error("an identical push reported a change")
	}
	if !s.ApplyUpdate(ap.LimitsUpdatedPayload{Windows: []ap.UsageWindow{{ID: "new", Kind: ap.UsageWindowOther, UsedPercent: 5}}}, t0) || len(s.Limits.Windows) != 3 {
		t.Fatalf("a new window was not added: %+v", s.Limits.Windows)
	}
}

func TestPushDoesNotOverrideUnsupported(t *testing.T) {
	var s State
	s.ApplyProbe(ap.UsageSnapshot{Limits: Unavailable(ap.UsageUnsupported, "api key", t0)})
	if s.ApplyUpdate(ap.LimitsUpdatedPayload{Windows: []ap.UsageWindow{{ID: "five_hour", UsedPercent: 5}}}, t0) {
		t.Fatal("a push gave an unsupported account windows")
	}
}

func TestKindForDuration(t *testing.T) {
	for minutes, want := range map[int]ap.UsageWindowKind{0: ap.UsageWindowOther, 300: ap.UsageWindowSession, 7 * 24 * 60: ap.UsageWindowWeekly, 30 * 24 * 60: ap.UsageWindowMonthly} {
		if got := KindForDuration(minutes); got != want {
			t.Errorf("KindForDuration(%d) = %s, want %s", minutes, got, want)
		}
	}
}
