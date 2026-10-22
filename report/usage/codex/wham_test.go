package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
)

// testToken is an unsigned JWT with ChatGPT's claims.
func testToken(t *testing.T, accountID, email string, exp time.Time) string {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{
		"exp":                            exp.Unix(),
		"https://api.openai.com/auth":    map[string]string{"chatgpt_account_id": accountID, "chatgpt_plan_type": "plus"},
		"https://api.openai.com/profile": map[string]string{"email": email},
	})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// wham serves /wham/usage as ChatGPT does, checking the token and account.
func wham(t *testing.T, token, account, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-Id") != account {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
}

func TestWhamSnapshot(t *testing.T) {
	now := time.Now()
	tok := testToken(t, "acct-1", "dev@example.com", now.Add(time.Hour))
	srv := wham(t, tok, "acct-1", `{"plan_type":"pro","rate_limit":{"allowed":true,
		"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1793000000},
		"secondary_window":{"used_percent":7,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)
	defer srv.Close()
	snap := WhamSnapshot(context.Background(), nil, srv.URL, tok, "", time.Time{}, now)
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "pro", PlanLabel: "ChatGPT Pro 20x", Email: "dev@example.com", Workspace: "acct-1"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 2 {
		t.Fatalf("limits = %+v", l)
	}
	p, s := l.Windows[0], l.Windows[1]
	if p.ID != WindowPrimary || p.Kind != ap.UsageWindowSession || p.Label != "5-hour" || p.UsedPercent != 42 || p.DurationMinutes != 300 || !p.ResetsAt.Equal(time.Unix(1793000000, 0)) {
		t.Fatalf("primary = %+v", p)
	}
	if s.ID != WindowSecondary || s.Kind != ap.UsageWindowWeekly || s.UsedPercent != 7 || s.ResetsAt.Unix() != now.Unix()+86400 {
		t.Fatalf("secondary = %+v", s)
	}
}

func TestWhamSnapshot_failures(t *testing.T) {
	now := time.Now()
	srv := wham(t, "other", "", `{}`)
	defer srv.Close()
	for name, tok := range map[string]string{
		"rejected": testToken(t, "acct-1", "dev@example.com", now.Add(time.Hour)),
		"expired":  testToken(t, "acct-1", "dev@example.com", now.Add(-time.Hour)),
	} {
		snap := WhamSnapshot(context.Background(), nil, srv.URL, tok, "", time.Time{}, now)
		if snap.Account == nil || snap.Account.Email != "dev@example.com" || snap.Account.Plan != "plus" {
			t.Fatalf("%s: account = %+v", name, snap.Account)
		}
		if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
			t.Fatalf("%s: limits = %+v", name, snap.Limits)
		}
	}
}

func TestWhamWindows_freePlanIsMonthly(t *testing.T) {
	used := 50.0
	ws := WhamWindows(WhamUsageResponse{PlanType: "free", RateLimit: &WhamRateLimit{PrimaryWindow: &WhamWindow{UsedPercent: &used}}}, time.Now())
	if len(ws) != 1 || ws[0].Kind != ap.UsageWindowMonthly {
		t.Fatalf("windows = %+v", ws)
	}
}
