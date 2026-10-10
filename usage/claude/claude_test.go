package claude

import (
	"encoding/json"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/claudecode"
)

func TestAccount(t *testing.T) {
	for name, c := range map[string]struct {
		in   claudecode.AccountInfo
		want ap.AgentAccount
	}{
		"max login": {claudecode.AccountInfo{Email: "a@x.dev", Organization: "org-1", SubscriptionType: "max", TokenSource: "claude.ai"},
			ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "max", PlanLabel: "Max", Email: "a@x.dev", Workspace: "org-1"}},
		"api key": {claudecode.AccountInfo{TokenSource: "none", APIKeySource: "ANTHROPIC_API_KEY"},
			ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		"bedrock": {claudecode.AccountInfo{APIProvider: "bedrock"},
			ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountCloud, PlanLabel: "Amazon Bedrock"}},
		"signed out": {claudecode.AccountInfo{TokenSource: "none", APIKeySource: "none"},
			ap.AgentAccount{Status: ap.AccountUnauthenticated}},
		// A profile login reports no token source at all; that is not proof
		// of being signed out.
		"silent": {claudecode.AccountInfo{}, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}},
	} {
		if got := Account(c.in); got != c.want {
			t.Errorf("%s: Account = %+v, want %+v", name, got, c.want)
		}
	}
}

func TestPlanLabel(t *testing.T) {
	for in, want := range map[string]string{"claude_max_20x_subscription": "Max 20x", "max5": "Max 5x", "pro": "Pro", "team": "Team", "": "", "special_plan": "Special Plan"} {
		if got := PlanLabel(in); got != want {
			t.Errorf("PlanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// get_usage as the SDK types it (sdk.d.ts SDKControlGetUsageResponse,
// 0.3.296): utilization 0 to 100, ISO reset times.
const getUsage = `{
  "subscription_type": "max",
  "rate_limits_available": true,
  "rate_limits": {
    "five_hour": {"utilization": 42.5, "resets_at": "2026-10-10T14:00:00Z"},
    "seven_day": {"utilization": 12, "resets_at": "2026-10-14T00:00:00Z"},
    "seven_day_opus": {"utilization": 99, "resets_at": null},
    "model_scoped": [{"display_name": "Opus 4.8", "utilization": 30, "resets_at": "2026-10-14T00:00:00Z"}],
    "extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null}
  },
  "behaviors": null
}`

func TestLimits(t *testing.T) {
	var r claudecode.GetUsageResponse
	if err := json.Unmarshal([]byte(getUsage), &r); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	l := Limits(r, now)
	if len(l.Windows) != 3 {
		t.Fatalf("windows = %+v", l.Windows)
	}
	five, week, opus := l.Windows[0], l.Windows[1], l.Windows[2]
	if five.ID != WindowFiveHour || five.Kind != ap.UsageWindowSession || five.UsedPercent != 42.5 || five.DurationMinutes != 300 ||
		!five.ResetsAt.Equal(time.Date(2026, 10, 10, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("five_hour = %+v", five)
	}
	if week.ID != WindowSevenDay || week.Kind != ap.UsageWindowWeekly || week.UsedPercent != 12 {
		t.Errorf("seven_day = %+v", week)
	}
	if opus.ID != "seven_day_opus_4_8" || opus.Label != "Weekly · Opus 4.8" || opus.UsedPercent != 30 {
		t.Errorf("scoped = %+v", opus)
	}

	r.RateLimitsAvailable, r.RateLimits = false, nil
	if l := Limits(r, now); l.Unavailable == nil || l.Unavailable.Reason != ap.UsageUnsupported || len(l.Windows) != 0 {
		t.Errorf("unavailable = %+v", l)
	}
}

// rate_limit_event as the SDK types it (SDKRateLimitInfo): utilization 0 to
// 1, resetsAt in Unix seconds.
func TestUpdate(t *testing.T) {
	var e claudecode.RateLimitEvent
	if err := json.Unmarshal([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1791651600,"rateLimitType":"five_hour","utilization":0.81},"session_id":"s"}`), &e); err != nil {
		t.Fatal(err)
	}
	u, ok := Update(e.RateLimitInfo)
	if !ok || len(u.Windows) != 1 {
		t.Fatalf("update = %+v %v", u, ok)
	}
	w := u.Windows[0]
	if w.ID != WindowFiveHour || w.UsedPercent < 80.99 || w.UsedPercent > 81.01 || !w.ResetsAt.Equal(time.Unix(1791651600, 0)) {
		t.Errorf("window = %+v", w)
	}
	if _, ok := Update(claudecode.RateLimitInfo{RateLimitType: "seven_day_opus", Utilization: ptr(0.5)}); ok {
		t.Error("a scoped type was mapped without knowing its models")
	}
	if _, ok := Update(claudecode.RateLimitInfo{RateLimitType: "five_hour"}); ok {
		t.Error("an event without utilization was mapped")
	}
}

func ptr[T any](v T) *T { return &v }

func TestTurnsTakeTheTurnsShareOfRunningTotals(t *testing.T) {
	var turns Turns
	first := turns.Next(claudecode.ResultMessage{Subtype: "success", TotalCostUSD: 0.10,
		Usage:      claudecode.Usage{InputTokens: 10, CacheReadInputTokens: 1000, CacheCreationInputTokens: 200, OutputTokens: 50},
		ModelUsage: map[string]claudecode.ModelUsage{"claude-opus-4-8": {ThinkingTokens: 20, ContextWindow: 1_000_000}}})
	if first.PromptTokens != 1210 || first.CachedInputTokens != 1000 || first.CacheCreationTokens != 200 || first.CompletionTokens != 50 ||
		first.TotalTokens != 1260 || first.ReasoningTokens != 20 || first.CostUSD != 0.10 || first.Status != ap.TurnUsageComplete {
		t.Errorf("first = %+v", first)
	}
	second := turns.Next(claudecode.ResultMessage{Subtype: "error_during_execution", TotalCostUSD: 0.25,
		Usage:      claudecode.Usage{InputTokens: 5, OutputTokens: 30},
		ModelUsage: map[string]claudecode.ModelUsage{"claude-opus-4-8": {ThinkingTokens: 30}}})
	if second.ReasoningTokens != 10 || second.CostUSD < 0.1499 || second.CostUSD > 0.1501 || second.Status != ap.TurnUsagePartial {
		t.Errorf("second = %+v", second)
	}
	// After /clear the running total starts over.
	third := turns.Next(claudecode.ResultMessage{Subtype: "success", TotalCostUSD: 0.02})
	if third.CostUSD != 0.02 {
		t.Errorf("third = %+v", third)
	}
}

func TestContextWindow(t *testing.T) {
	res := claudecode.ResultMessage{ModelUsage: map[string]claudecode.ModelUsage{"claude-opus-4-8[1m]": {ContextWindow: 1_000_000}}}
	if got := ContextWindow(res, "claude-opus-4-8"); got != 1_000_000 {
		t.Errorf("one model = %d", got)
	}
	res.ModelUsage["claude-haiku-4-5"] = claudecode.ModelUsage{ContextWindow: 200_000}
	if got := ContextWindow(res, "claude-haiku-4-5"); got != 200_000 {
		t.Errorf("named = %d", got)
	}
	if got := ContextWindow(res, "other"); got != 0 {
		t.Errorf("unknown among several = %d", got)
	}
	if got := ContextUsed(claudecode.Usage{InputTokens: 1, CacheReadInputTokens: 2, CacheCreationInputTokens: 3, OutputTokens: 4}); got != 10 {
		t.Errorf("ContextUsed = %d", got)
	}
}
