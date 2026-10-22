package hermes

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// accountAPI serves the Nous Portal's /api/oauth/account, claude.ai's
// /api/oauth/usage and ChatGPT's /wham/usage, each checking its token.
func accountAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/api/oauth/account" && auth == "Bearer nous-tok":
			_, _ = io.WriteString(w, `{"user":{"email":"dev@example.com","privy_did":"x"},
				"organisation":{"id":"org-1","slug":"dev","name":"Dev Org"},
				"subscription":{"plan":"plus","tier":2,"monthly_charge":20,"monthly_credits":20,"credits_remaining":15,"current_period_end":"2026-11-01T00:00:00Z","rollover_credits":0},
				"paid_service_access":{"allowed":true}}`)
		case r.URL.Path == "/api/oauth/usage" && auth == "Bearer sk-ant-oat01-tok":
			_, _ = io.WriteString(w, `{"five_hour":{"utilization":10,"resets_at":"2026-10-22T15:00:00Z"},"seven_day":{"utilization":20,"resets_at":"2026-10-27T00:00:00Z"}}`)
		case r.URL.Path == "/wham/usage" && r.Header.Get("ChatGPT-Account-Id") == "acct-1":
			_, _ = io.WriteString(w, `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":18000,"reset_at":1793000000}}}`)
		default:
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
		}
	}))
}

func jwt(exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "https://api.openai.com/profile": map[string]string{"email": "dev@example.com"}})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func TestProbe(t *testing.T) {
	srv := accountAPI(t)
	defer srv.Close()
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	codexTok := jwt(time.Now().Add(time.Hour))
	for _, tc := range []struct {
		name, auth, config, dotenv string
		env                        []string
		args                       []string
		want                       ap.AgentAccount
		windows                    []string
		reason                     ap.UsageUnavailableReason
	}{
		{name: "nous", auth: `{"version":1,"providers":{"nous":{"access_token":"nous-tok","expires_at":"` + future + `","portal_base_url":"` + srv.URL + `"}},"active_provider":"nous"}`,
			want:    ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "plus", PlanLabel: "Nous Portal Plus", Email: "dev@example.com", Workspace: "Dev Org"},
			windows: []string{WindowCredits}},
		{name: "nous expired keeps the account", auth: `{"providers":{"nous":{"access_token":"nous-tok","expires_at":"2020-01-01T00:00:00Z","portal_base_url":"` + srv.URL + `"}},"active_provider":"nous"}`,
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, PlanLabel: "Nous Portal"}, reason: ap.UsageProbeFailed},
		{name: "anthropic from the pool, provider from config", auth: `{"credential_pool":{"anthropic":[{"auth_type":"api_key","priority":0,"access_token":"sk-ant-api-x"},{"auth_type":"oauth","priority":1,"access_token":"sk-ant-oat01-tok"}]},"active_provider":"nous"}`,
			config: "# hermes\nmodel:\n  default: claude-opus-4-6\n  provider: \"anthropic\"  # set by hermes model\nproviders: {}\n",
			want:   ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}, windows: []string{"five_hour", "seven_day"}},
		{name: "anthropic api key", args: []string{"chat", "--provider", "anthropic"}, dotenv: "ANTHROPIC_API_KEY=sk-ant-api03-x\n",
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}, reason: ap.UsageUnsupported},
		{name: "openai-codex", auth: `{"providers":{"openai-codex":{"tokens":{"access_token":"` + codexTok + `","refresh_token":"r","account_id":"acct-1"},"auth_mode":"chatgpt"}},"active_provider":"openai-codex"}`,
			want:    ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "plus", PlanLabel: "ChatGPT Plus", Email: "dev@example.com", Workspace: "acct-1"},
			windows: []string{"primary"}},
		{name: "openrouter key", env: []string{"HERMES_INFERENCE_PROVIDER=openrouter"},
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}, reason: ap.UsageUnsupported},
		{name: "copilot", auth: `{"active_provider":"copilot"}`,
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, PlanLabel: "GitHub Copilot"}, reason: ap.UsageUnsupported},
		{name: "nous without a login", auth: `{"active_provider":"nous"}`, want: ap.AgentAccount{Status: ap.AccountUnauthenticated}},
		{name: "nothing", want: ap.AgentAccount{Status: ap.AccountUnauthenticated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			hh := filepath.Join(home, ".hermes")
			if tc.auth != "" {
				writeFile(t, filepath.Join(hh, "auth.json"), tc.auth)
			}
			if tc.config != "" {
				writeFile(t, filepath.Join(hh, "config.yaml"), tc.config)
			}
			if tc.dotenv != "" {
				writeFile(t, filepath.Join(hh, ".env"), tc.dotenv)
			}
			snap, err := Prober{AnthropicBase: srv.URL, ChatGPTBase: srv.URL}.Probe(context.Background(), usage.Env{Args: tc.args, Env: append([]string{"HOME=" + home}, tc.env...)})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Account == nil || *snap.Account != tc.want {
				t.Fatalf("account = %+v, want %+v", snap.Account, tc.want)
			}
			if tc.want.Status != ap.AccountAuthenticated {
				return
			}
			l := snap.Limits
			if l == nil {
				t.Fatal("no limits")
			}
			if tc.reason != "" {
				if l.Unavailable == nil || l.Unavailable.Reason != tc.reason {
					t.Fatalf("limits = %+v", l)
				}
				return
			}
			if l.Unavailable != nil || len(l.Windows) != len(tc.windows) {
				t.Fatalf("limits = %+v", l)
			}
			for i, id := range tc.windows {
				if l.Windows[i].ID != id {
					t.Fatalf("window %d = %+v", i, l.Windows[i])
				}
			}
		})
	}
}

func TestCreditsWindow(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	w, ok := CreditsWindow(&Subscription{MonthlyCredits: f(20), CreditsRemaining: f(15), CurrentPeriodEnd: "2026-11-01T00:00:00Z"})
	if !ok || w.UsedPercent != 25 || w.Kind != ap.UsageWindowMonthly || !w.ResetsAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window = %+v", w)
	}
	// Rollover beyond the grant and a plan without credits have no share.
	for _, s := range []*Subscription{nil, {MonthlyCredits: f(20), CreditsRemaining: f(25)}, {MonthlyCredits: f(0), CreditsRemaining: f(0)}} {
		if _, ok := CreditsWindow(s); ok {
			t.Fatalf("CreditsWindow(%+v) ok", s)
		}
	}
}
