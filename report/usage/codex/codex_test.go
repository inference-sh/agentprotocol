package codex

import (
	"encoding/json"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/codexapp"
)

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAccount(t *testing.T) {
	for raw, want := range map[string]ap.AgentAccount{
		`{"account":{"type":"chatgpt","email":"a@x.dev","planType":"pro"},"requiresOpenaiAuth":true}`: {Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "pro", PlanLabel: "ChatGPT Pro 20x", Email: "a@x.dev"},
		`{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`:                                     {Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
		`{"account":null,"requiresOpenaiAuth":true}`:                                                  {Status: ap.AccountUnauthenticated},
		`{"requiresOpenaiAuth":false}`:                                                                {Status: ap.AccountAuthenticated},
	} {
		if got := Account(decode[codexapp.GetAccountResponse](t, raw)); got != want {
			t.Errorf("%s: %+v, want %+v", raw, got, want)
		}
	}
}

// account/rateLimits/read as codex 0.156.1's schema types it: usedPercent 0
// to 100, resetsAt in Unix seconds, a legacy snapshot plus one per limit ID.
const rateLimits = `{
  "accountId": "acct-1",
  "rateLimits": {"limitId": "spark", "primary": {"usedPercent": 90, "windowDurationMins": 300}},
  "rateLimitsByLimitId": {
    "codex": {"limitId": "codex", "planType": "pro",
      "primary": {"usedPercent": 37, "resetsAt": 1791651600, "windowDurationMins": 300},
      "secondary": {"usedPercent": 8, "resetsAt": 1792000000, "windowDurationMins": 10080}},
    "spark": {"limitId": "spark", "primary": {"usedPercent": 90}}
  }
}`

func TestLimitsPreferTheMainAllowance(t *testing.T) {
	l := Limits(decode[codexapp.GetAccountRateLimitsResponse](t, rateLimits), time.Unix(0, 0))
	if len(l.Windows) != 2 {
		t.Fatalf("windows = %+v", l.Windows)
	}
	p, s := l.Windows[0], l.Windows[1]
	if p.ID != WindowPrimary || p.Kind != ap.UsageWindowSession || p.UsedPercent != 37 || !p.ResetsAt.Equal(time.Unix(1791651600, 0)) || p.DurationMinutes != 300 {
		t.Errorf("primary = %+v", p)
	}
	if s.ID != WindowSecondary || s.Kind != ap.UsageWindowWeekly || s.UsedPercent != 8 {
		t.Errorf("secondary = %+v", s)
	}
}

func TestWindowsWithoutDurations(t *testing.T) {
	free := codexapp.PlanTypeFree
	ws := Windows(codexapp.RateLimitSnapshot{PlanType: &free, Primary: &codexapp.RateLimitWindow{UsedPercent: 3}})
	if len(ws) != 1 || ws[0].Kind != ap.UsageWindowMonthly {
		t.Errorf("free plan = %+v", ws)
	}
	ws = Windows(codexapp.RateLimitSnapshot{Primary: &codexapp.RateLimitWindow{UsedPercent: 3}, Secondary: &codexapp.RateLimitWindow{UsedPercent: 1}})
	if ws[0].Kind != ap.UsageWindowSession || ws[1].Kind != ap.UsageWindowWeekly {
		t.Errorf("paid plan = %+v", ws)
	}
}

func TestUpdateIgnoresOtherLimits(t *testing.T) {
	if _, ok := Update(decode[codexapp.AccountRateLimitsUpdatedNotification](t, `{"rateLimits":{"limitId":"spark","primary":{"usedPercent":50}}}`)); ok {
		t.Error("a model's limit was mapped onto the main windows")
	}
	u, ok := Update(decode[codexapp.AccountRateLimitsUpdatedNotification](t, `{"rateLimits":{"limitId":"codex","secondary":{"usedPercent":12}}}`))
	if !ok || len(u.Windows) != 1 || u.Windows[0].ID != WindowSecondary {
		t.Errorf("update = %+v %v", u, ok)
	}
}

func usageAt(total, last int64) codexapp.ThreadTokenUsage {
	b := func(n int64) codexapp.TokenUsageBreakdown {
		return codexapp.TokenUsageBreakdown{InputTokens: n * 8 / 10, CachedInputTokens: n / 2, OutputTokens: n - n*8/10, ReasoningOutputTokens: n / 20, TotalTokens: n}
	}
	w := int64(400_000)
	return codexapp.ThreadTokenUsage{Total: b(total), Last: b(last), ModelContextWindow: &w}
}

func TestTurnsAreTheGrowthOfTheTotal(t *testing.T) {
	var turns Turns
	turns.Fresh()
	turns.Add(usageAt(1000, 1000))
	turns.Add(usageAt(2500, 1500))
	first := turns.End(ap.TurnUsageComplete)
	if first.TotalTokens != 2500 || first.PromptTokens != 2000 || first.CompletionTokens != 500 || first.Status != ap.TurnUsageComplete {
		t.Errorf("first = %+v", first)
	}
	turns.Add(usageAt(4000, 1500))
	if second := turns.End(ap.TurnUsageComplete); second.TotalTokens != 1500 {
		t.Errorf("second = %+v", second)
	}
}

func TestResumedThreadCountsTheCallsUntilItHasATotal(t *testing.T) {
	var turns Turns // resumed: the first total includes history
	turns.Add(usageAt(90_000, 1000))
	turns.Add(usageAt(91_500, 1500))
	if got := turns.End(ap.TurnUsageComplete); got.TotalTokens != 2500 {
		t.Errorf("resumed turn = %+v, want 2500 tokens, not the thread's history", got)
	}
}

func TestContext(t *testing.T) {
	c := Context(usageAt(5000, 1200))
	if c.UsedTokens != 1200 || c.MaxTokens != 400_000 {
		t.Errorf("context = %+v", c)
	}
}
