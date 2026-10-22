package kilo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// kiloAPI serves the profile, balance and Kilo Pass endpoints as Kilo does,
// checking the bearer token. pass is kiloPass.getState's body.
func kiloAPI(t *testing.T, token, pass string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"Invalid token"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/profile":
			_, _ = io.WriteString(w, `{"user":{"email":"dev@example.com","name":"Dev"},"organizations":[{"id":"org-1","name":"Acme"}]}`)
		case "/api/profile/balance":
			if r.Header.Get("x-kilocode-organizationid") == "org-1" {
				_, _ = io.WriteString(w, `{"balance":250}`)
				return
			}
			_, _ = io.WriteString(w, `{"balance":12.345}`)
		case "/api/trpc/kiloPass.getState":
			if r.URL.Query().Get("batch") != "1" || r.URL.Query().Get("input") != `{"0":null}` {
				http.Error(w, "bad input", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, pass)
		default:
			http.NotFound(w, r)
		}
	}))
}

const livePass = `[{"result":{"data":{"json":{"subscription":{"status":"active","currentPeriodBaseCreditsUsd":19,"currentPeriodUsageUsd":10,"currentPeriodBonusCreditsUsd":1,"nextBillingAt":"2026-11-05T00:00:00.000Z"}}}}}]`

func probe(t *testing.T, srv *httptest.Server, auth string, args ...string) ap.UsageSnapshot {
	t.Helper()
	home := t.TempDir()
	if auth != "" {
		writeFile(t, filepath.Join(home, ".local", "share", "kilo", "auth.json"), auth)
	}
	env := []string{"HOME=" + home}
	if srv != nil {
		env = append(env, "KILO_API_URL="+srv.URL+"/")
	}
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: env, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestProbe_kiloPass(t *testing.T) {
	srv := kiloAPI(t, "tok", livePass)
	defer srv.Close()
	snap := probe(t, srv, `{"kilo":{"type":"oauth","access":"tok","refresh":"tok","expires":99999999999999}}`)
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: "dev@example.com", Plan: "kilo_pass", PlanLabel: "Kilo Pass"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 1 {
		t.Fatalf("limits = %+v", l)
	}
	w := l.Windows[0]
	if w.ID != WindowPass || w.Kind != ap.UsageWindowMonthly || w.UsedPercent != 50 || !w.ResetsAt.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window = %+v", w)
	}
}

func TestProbe_payAsYouGo(t *testing.T) {
	for name, pass := range map[string]string{
		"no subscription": `[{"result":{"data":{"json":{"subscription":null}}}}]`,
		"cancelled":       strings.Replace(livePass, "active", "canceled", 1),
	} {
		t.Run(name, func(t *testing.T) {
			srv := kiloAPI(t, "key", pass)
			defer srv.Close()
			// A key-type login (migrated from the old CLI) is a Kilo token too.
			snap := probe(t, srv, `{"kilo":{"type":"api","key":"key"}}`)
			if snap.Account == nil || snap.Account.Type != ap.AccountSubscription || snap.Account.Plan != "" || snap.Account.Email != "dev@example.com" {
				t.Fatalf("account = %+v", snap.Account)
			}
			u := snap.Limits.Unavailable
			if u == nil || u.Reason != ap.UsageUnsupported || u.Message != "pay as you go, with no Kilo Pass" {
				t.Fatalf("limits = %+v", snap.Limits)
			}
		})
	}
}

func TestProbe_organization(t *testing.T) {
	srv := kiloAPI(t, "tok", livePass)
	defer srv.Close()
	snap := probe(t, srv, `{"kilo":{"type":"oauth","access":"tok","refresh":"tok","expires":1,"accountId":"org-1"}}`)
	if snap.Account == nil || snap.Account.Workspace != "org-1" || snap.Account.Plan != "" {
		t.Fatalf("account = %+v", snap.Account)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageUnsupported || u.Message != "pays with organization credits" {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_profileFailureKeepsAccount(t *testing.T) {
	srv := kiloAPI(t, "other", livePass)
	defer srv.Close()
	snap := probe(t, srv, `{"kilo":{"type":"oauth","access":"tok"}}`)
	if snap.Account == nil || snap.Account.Status != ap.AccountAuthenticated {
		t.Fatalf("account = %+v", snap.Account)
	}
	if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_otherProviders(t *testing.T) {
	both := `{"kilo":{"type":"oauth","access":"tok"},"openai":{"type":"oauth","access":"a","refresh":"r","expires":1},"anthropic":{"type":"api","key":"sk"}}`
	for _, tc := range []struct {
		name string
		auth string
		args []string
		want ap.AgentAccount
	}{
		{"chatgpt login", `{"openai":{"type":"oauth","access":"a","refresh":"r","expires":1}}`, nil, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}},
		{"model names another provider", both, []string{"--model", "anthropic/claude-sonnet-4-5"}, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"model names one with no login", both, []string{"-m", "xai/grok-4"}, ap.AgentAccount{Status: ap.AccountUnknown}},
		{"nothing", "", nil, ap.AgentAccount{Status: ap.AccountUnknown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := probe(t, nil, tc.auth, tc.args...)
			if snap.Account == nil || *snap.Account != tc.want {
				t.Fatalf("account = %+v, want %+v", snap.Account, tc.want)
			}
			if tc.want.Status == ap.AccountAuthenticated && (snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageUnsupported) {
				t.Fatalf("limits = %+v", snap.Limits)
			}
		})
	}
}

func TestPassState_bareData(t *testing.T) {
	var s PassState
	if err := json.Unmarshal([]byte(`{"result":{"data":{"subscription":{"currentPeriodUsageUsd":5,"nextRenewalAt":"2026-11-05T00:00:00Z"}}}}`), &s); err != nil {
		t.Fatal(err)
	}
	sub := s.Subscription()
	if sub == nil {
		t.Fatal("no subscription")
	}
	if w := PassWindow(*sub); w.UsedPercent != 100 || w.ResetsAt.IsZero() {
		t.Fatalf("window = %+v", w)
	}
}
