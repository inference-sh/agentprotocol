package copilot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// userJSON is copilot_internal/user for a Pro account, in the shape
// @github/copilot 1.0.102 types it: chat and completions unlimited, premium
// requests limited.
const userJSON = `{"login":"octocat","access_type_sku":"monthly_subscriber_quota","copilot_plan":"individual","chat_enabled":true,
	"quota_reset_date":"2026-11-01","quota_reset_date_utc":"2026-11-01T00:00:00.000Z",
	"quota_snapshots":{
		"chat":{"entitlement":-1,"remaining":0,"percent_remaining":100,"unlimited":true,"overage_permitted":false,"quota_id":"chat"},
		"completions":{"entitlement":-1,"remaining":0,"percent_remaining":100,"unlimited":true,"quota_id":"completions"},
		"premium_interactions":{"entitlement":300,"remaining":219,"percent_remaining":73,"unlimited":false,"overage_permitted":true,"overage_count":0,"quota_id":"premium_interactions"}}}`

// github serves copilot_internal/user to the token given.
func github(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, userJSON)
	}))
}

// probeEnv is an environment with an empty HOME and PATH, and the API
// pointed at srv.
func probeEnv(t *testing.T, srv *httptest.Server, extra ...string) (usage.Env, string) {
	home := t.TempDir()
	env := []string{"HOME=" + home, "PATH=" + t.TempDir()}
	if srv != nil {
		env = append(env, "COPILOT_DEBUG_GITHUB_API_URL="+srv.URL)
	}
	return usage.Env{Env: append(env, extra...)}, home
}

func wantPro(t *testing.T, snap ap.UsageSnapshot) {
	t.Helper()
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "monthly_subscriber_quota", PlanLabel: "Copilot Pro", Workspace: "octocat"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 1 {
		t.Fatalf("limits = %+v", l)
	}
	w := l.Windows[0]
	if w.ID != "premium_interactions" || w.Kind != ap.UsageWindowMonthly || w.UsedPercent != 27 || w.Label != "Premium requests" ||
		!w.ResetsAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window = %+v", w)
	}
}

func TestProbe_envTokenInPrecedence(t *testing.T) {
	srv := github(t, "gho_copilot")
	defer srv.Close()
	env, _ := probeEnv(t, srv, "GITHUB_TOKEN=gho_other", "GH_TOKEN=gho_gh", "COPILOT_GITHUB_TOKEN=gho_copilot")
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	wantPro(t, snap)
}

func TestProbe_plainTextLogin(t *testing.T) {
	srv := github(t, "gho_saved")
	defer srv.Close()
	env, home := probeEnv(t, srv)
	writeFile(t, filepath.Join(home, ".copilot", "config.json"), `// User settings belong in settings.json.
// This file is managed automatically.
{
  "loggedInUsers": [{"host": "https://github.com", "login": "octocat"}],
  "lastLoggedInUser": {"host": "https://github.com", "login": "octocat"},
  "copilotTokens": {"https://github.com:someone-else": "gho_other", "https://github.com:octocat": "gho_saved"}
}`, 0o600)
	before, _ := os.ReadFile(filepath.Join(home, ".copilot", "config.json"))
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	wantPro(t, snap)
	after, _ := os.ReadFile(filepath.Join(home, ".copilot", "config.json"))
	if string(before) != string(after) {
		t.Error("config.json was rewritten")
	}
}

func TestProbe_copilotHome(t *testing.T) {
	srv := github(t, "gho_saved")
	defer srv.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.json"), `{"copilotTokens":{"https://github.com:octocat":"gho_saved"}}`, 0o600)
	env, _ := probeEnv(t, srv, "COPILOT_HOME="+dir)
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	wantPro(t, snap)
}

func TestProbe_keyringLogin(t *testing.T) {
	env, home := probeEnv(t, nil)
	writeFile(t, filepath.Join(home, ".copilot", "config.json"), `{"lastLoggedInUser":{"host":"https://github.com","login":"octocat"}}`, 0o600)
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if a := snap.Account; a == nil || a.Status != ap.AccountAuthenticated || a.Workspace != "octocat" {
		t.Fatalf("account = %+v", a)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageProbeFailed || u.Message != "copilot: the login is in the keyring" {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_ghCLI(t *testing.T) {
	srv := github(t, "gho_from_gh")
	defer srv.Close()
	env, _ := probeEnv(t, srv, "GH_HOST=github.com")
	bin := t.TempDir()
	// gh prints the token only for the host the CLI asks about.
	writeFile(t, filepath.Join(bin, "gh"), "#!/bin/sh\n[ \"$*\" = \"auth token --hostname github.com\" ] && echo gho_from_gh && exit 0\nexit 1\n", 0o755)
	env.Env = append(env.Env, "PATH="+bin)
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	wantPro(t, snap)
}

func TestProbe_classicPATIsSkipped(t *testing.T) {
	env, _ := probeEnv(t, nil, "GH_TOKEN=ghp_classic")
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Status != ap.AccountUnauthenticated {
		t.Fatalf("account = %+v", snap.Account)
	}
}

func TestProbe_withoutAGitHubLogin(t *testing.T) {
	env, _ := probeEnv(t, nil)
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || *snap.Account != (ap.AgentAccount{Status: ap.AccountUnauthenticated}) || snap.Limits != nil {
		t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
	}

	env, _ = probeEnv(t, nil, "COPILOT_PROVIDER_BASE_URL=http://localhost:1234/v1", "COPILOT_PROVIDER_API_KEY=k", "GH_TOKEN=gho_x")
	snap, err = Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Type != ap.AccountAPIKey || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageUnsupported {
		t.Fatalf("byok = %+v %+v", snap.Account, snap.Limits)
	}
}

func TestProbe_rejectedTokenKeepsTheAccount(t *testing.T) {
	srv := github(t, "gho_good")
	defer srv.Close()
	env, home := probeEnv(t, srv)
	writeFile(t, filepath.Join(home, ".copilot", "config.json"), `{"lastLoggedInUser":{"host":"https://github.com","login":"octocat"},"copilotTokens":{"https://github.com:octocat":"gho_revoked"}}`, 0o600)
	snap, err := Prober{}.Probe(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if a := snap.Account; a == nil || a.Status != ap.AccountAuthenticated || a.Workspace != "octocat" {
		t.Fatalf("account = %+v", a)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageProbeFailed {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestPlan(t *testing.T) {
	for _, tc := range []struct{ sku, plan, id, label string }{
		{"free_limited_copilot", "individual", "free_limited_copilot", "Copilot Free"},
		{"plus_yearly_subscriber_quota", "individual", "plus_yearly_subscriber_quota", "Copilot Pro+"},
		{"copilot_for_business_seat_quota", "business", "copilot_for_business_seat_quota", "Copilot Business"},
		{"copilot_enterprise_seat_quota", "enterprise", "copilot_enterprise_seat_quota", "Copilot Enterprise"},
		{"", "individual", "individual", "Copilot Pro"},
		{"", "", "", ""},
	} {
		if id, label := Plan(User{AccessTypeSKU: tc.sku, CopilotPlan: tc.plan}); id != tc.id || label != tc.label {
			t.Errorf("%s/%s = %s %q", tc.sku, tc.plan, id, label)
		}
	}
}

func TestWindowsOfAFreePlan(t *testing.T) {
	pct := func(f float64) *float64 { return &f }
	ws := Windows(User{QuotaResetDate: "2026-11-01", QuotaSnapshots: map[string]*Quota{
		"chat":                 {Entitlement: 50, PercentRemaining: pct(90)},
		"completions":          {Entitlement: 2000, PercentRemaining: pct(40), QuotaResetAt: 1793491200},
		"premium_interactions": {Entitlement: 1500, PercentRemaining: pct(100), TokenBasedBilling: true},
		"broken":               nil,
	}})
	if len(ws) != 3 {
		t.Fatalf("windows = %+v", ws)
	}
	for _, w := range ws {
		switch w.ID {
		case "chat":
			if w.UsedPercent != 10 || !w.ResetsAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("chat = %+v", w)
			}
		case "premium_interactions":
			if w.UsedPercent != 0 || w.Label != "AI credits" {
				t.Errorf("premium = %+v", w)
			}
		case "completions":
			if w.UsedPercent != 60 || !w.ResetsAt.Equal(time.Unix(1793491200, 0)) {
				t.Errorf("completions = %+v", w)
			}
		}
	}
}

func TestAPIURL(t *testing.T) {
	env := usage.Env{Env: []string{}}
	for host, want := range map[string]string{
		"https://github.com":         "https://api.github.com",
		"https://acme.ghe.com":       "https://api.acme.ghe.com",
		"https://api.custom.example": "https://api.custom.example",
	} {
		if got := APIURL(env, host); got != want {
			t.Errorf("%s = %s", host, got)
		}
	}
	if got := Host(usage.Env{Env: []string{"GH_HOST=acme.ghe.com/"}}); got != "https://acme.ghe.com" {
		t.Errorf("host = %s", got)
	}
}

func TestSavedLoginKeepsItsHost(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"lastLoggedInUser":{"host":"https://github.com","login":"octo"},"copilotTokens":{"https://github.com:octo":"gho_x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// gh pointed at an Enterprise host must not receive github.com's token.
	env := usage.Env{Env: []string{"COPILOT_HOME=" + home, "GH_HOST=ghe.example.com", "PATH="}}
	tok, err := FindToken(context.Background(), env, Host(env))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "gho_x" || tok.Host != "https://github.com" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"ghe.example.com":         "https://ghe.example.com",
		"http://ghe.example.com/": "https://ghe.example.com",
		"http://127.0.0.1:8080":   "http://127.0.0.1:8080",
		"https://github.com":      "https://github.com",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
