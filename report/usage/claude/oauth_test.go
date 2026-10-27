package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
)

// oauthAPI serves /api/oauth/usage and /api/oauth/profile as Anthropic does,
// checking the bearer token and the beta header.
func oauthAPI(t *testing.T, token, usageBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("anthropic-beta") != oauthBeta {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/oauth/usage":
			_, _ = io.WriteString(w, usageBody)
		case "/api/oauth/profile":
			_, _ = io.WriteString(w, `{"account":{"uuid":"u1","email":"dev@example.com"},"organization":{"uuid":"o1","name":"Dev Org","organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestOAuthSnapshot(t *testing.T) {
	srv := oauthAPI(t, "tok", `{
		"five_hour":{"utilization":12.5,"resets_at":"2026-10-22T15:00:00Z"},
		"seven_day":null,
		"seven_day_opus":{"utilization":40,"resets_at":"2026-10-27T00:00:00Z"},
		"seven_day_sonnet":null,
		"limits":[
			{"kind":"weekly_all","percent":30,"resets_at":"2026-10-27T00:00:00Z"},
			{"kind":"weekly_scoped","percent":5,"resets_at":"2026-10-27T00:00:00Z","scope":{"model":{"display_name":"Opus"}}},
			{"kind":"weekly_scoped","percent":77,"scope":{"model":{"display_name":"Fable"}},"is_active":false}
		]}`)
	defer srv.Close()
	now := time.Now()
	snap := OAuthSnapshot(context.Background(), nil, srv.URL, "tok", now.Add(time.Hour), now)
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "max", PlanLabel: "Max 20x", Email: "dev@example.com", Workspace: "Dev Org"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 4 {
		t.Fatalf("limits = %+v", l)
	}
	got := map[string]ap.UsageWindow{}
	for _, w := range l.Windows {
		got[w.ID] = w
	}
	if w := got[WindowFiveHour]; w.UsedPercent != 12.5 || w.Kind != ap.UsageWindowSession || w.Label != "5-hour" || !w.ResetsAt.Equal(time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("five_hour = %+v", w)
	}
	// seven_day falls back to the weekly_all entry.
	if w := got[WindowSevenDay]; w.UsedPercent != 30 || w.Kind != ap.UsageWindowWeekly || w.Label != "Weekly" {
		t.Fatalf("seven_day = %+v", w)
	}
	// The scoped entry wins over the legacy bucket for the same models.
	if w := got[ScopedWindowID("Opus")]; w.UsedPercent != 5 || w.Label != "Weekly · Opus" {
		t.Fatalf("opus = %+v", w)
	}
	if w := got["seven_day_fable"]; w.UsedPercent != 77 {
		t.Fatalf("fable = %+v", w)
	}
}

func TestOAuthSnapshot_failures(t *testing.T) {
	srv := oauthAPI(t, "other", `{}`)
	defer srv.Close()
	now := time.Now()
	for name, expires := range map[string]time.Time{"rejected": {}, "expired": now.Add(-time.Minute)} {
		snap := OAuthSnapshot(context.Background(), nil, srv.URL, "tok", expires, now)
		if snap.Account == nil || snap.Account.Type != ap.AccountSubscription {
			t.Fatalf("%s: account = %+v", name, snap.Account)
		}
		if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
			t.Fatalf("%s: limits = %+v", name, snap.Limits)
		}
	}
}

func TestIsOAuthToken(t *testing.T) {
	for tok, want := range map[string]bool{"sk-ant-oat01-x": true, "sk-ant-api03-x": false, "eyJhbGc": true, "cc-x": true, "sk-or-x": false} {
		if IsOAuthToken(tok) != want {
			t.Errorf("IsOAuthToken(%q) = %v", tok, !want)
		}
	}
}

// setupTokenAPI is the API as a setup-token sees it: the account API refuses
// it, and a Messages response carries the plan's windows in its headers.
func setupTokenAPI(t *testing.T, token string, status int, headers map[string]string, asked *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("anthropic-beta") != oauthBeta {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			http.Error(w, `{"error":{"type":"permission_error"}}`, http.StatusForbidden)
			return
		}
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.MaxTokens != 1 {
			t.Errorf("quota check asked for %d tokens", req.MaxTokens)
		}
		*asked = req.Model
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{}`)
	}))
}

func TestOAuthSnapshot_setupToken(t *testing.T) {
	windows := map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.25",
		"anthropic-ratelimit-unified-5h-reset":       "1793113200",
		"anthropic-ratelimit-unified-7d-utilization": "0.5",
		"anthropic-ratelimit-unified-7d-reset":       "1793458800",
	}
	now := time.Now()
	// A plan at its limit is refused, and still reports its windows.
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		var asked string
		srv := setupTokenAPI(t, "tok", status, windows, &asked)
		snap := OAuthSnapshot(context.Background(), nil, srv.URL, "tok", time.Time{}, now)
		srv.Close()
		if asked != QuotaModel {
			t.Errorf("%d: quota check asked %q", status, asked)
		}
		l := snap.Limits
		if l == nil || l.Unavailable != nil || len(l.Windows) != 2 {
			t.Fatalf("%d: limits = %+v", status, l)
		}
		if w := l.Windows[0]; w.ID != WindowFiveHour || w.UsedPercent != 25 || w.Kind != ap.UsageWindowSession || !w.ResetsAt.Equal(time.Unix(1793113200, 0)) {
			t.Errorf("%d: five_hour = %+v", status, w)
		}
		if w := l.Windows[1]; w.ID != WindowSevenDay || w.UsedPercent != 50 || w.Kind != ap.UsageWindowWeekly || w.DurationMinutes != 7*24*60 {
			t.Errorf("%d: seven_day = %+v", status, w)
		}
	}

	// No windows in the headers (an API key's response) is a failed probe.
	var asked string
	srv := setupTokenAPI(t, "tok", http.StatusOK, nil, &asked)
	defer srv.Close()
	snap := OAuthSnapshot(context.Background(), nil, srv.URL, "tok", time.Time{}, now)
	if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}
