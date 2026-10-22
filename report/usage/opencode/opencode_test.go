package opencode

import (
	"context"
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

// goUsage serves the Go usage endpoint as OpenCode does: 401 for an unknown
// key, 403 for a key without a Go subscription, and it requires the session
// header.
func goUsage(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("x-opencode-session"), "ses_") {
			http.Error(w, `{"error":{"message":"missing session"}}`, http.StatusBadRequest)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer go-key":
			_, _ = io.WriteString(w, `{"usage":{
				"rolling":{"status":"ok","percent":12,"resetsAt":"2026-10-22T17:00:00.000Z"},
				"weekly":{"status":"ok","percent":40,"resetsAt":"2026-10-27T00:00:00.000Z"},
				"monthly":{"status":"rate-limited","percent":97,"resetsAt":"2026-11-03T00:00:00.000Z"}}}`)
		case "Bearer zen-key":
			http.Error(w, `{"error":{"message":"no Go subscription"}}`, http.StatusForbidden)
		default:
			http.Error(w, `{"error":{"message":"invalid key"}}`, http.StatusUnauthorized)
		}
	}))
}

func probe(t *testing.T, srv *httptest.Server, auth string, args ...string) ap.UsageSnapshot {
	t.Helper()
	home := t.TempDir()
	if auth != "" {
		writeFile(t, filepath.Join(home, "data", "opencode", "auth.json"), auth)
	}
	p := Prober{}
	if srv != nil {
		p.UsageURL = srv.URL + "/zen/go/v1/usage"
	}
	snap, err := p.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "XDG_DATA_HOME=" + filepath.Join(home, "data")}, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestProbe_goSubscription(t *testing.T) {
	srv := goUsage(t)
	defer srv.Close()
	snap := probe(t, srv, `{"opencode-go":{"type":"api","key":"go-key"},"openai":{"type":"oauth","access":"a","refresh":"r","expires":1}}`)
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: ProviderGo, PlanLabel: "OpenCode Go"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 3 {
		t.Fatalf("limits = %+v", l)
	}
	rolling, weekly, monthly := l.Windows[0], l.Windows[1], l.Windows[2]
	if rolling.ID != WindowRolling || rolling.Kind != ap.UsageWindowSession || rolling.UsedPercent != 12 || rolling.DurationMinutes != 300 ||
		!rolling.ResetsAt.Equal(time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("rolling = %+v", rolling)
	}
	if weekly.ID != WindowWeekly || weekly.UsedPercent != 40 {
		t.Fatalf("weekly = %+v", weekly)
	}
	if monthly.ID != WindowMonthly || monthly.Kind != ap.UsageWindowMonthly || monthly.UsedPercent != 100 {
		t.Fatalf("monthly = %+v", monthly)
	}
}

func TestProbe_keyWithoutGo(t *testing.T) {
	srv := goUsage(t)
	defer srv.Close()
	snap := probe(t, srv, `{"opencode-go":{"type":"api","key":"zen-key"}}`)
	if snap.Account == nil || snap.Account.Type != ap.AccountAPIKey || snap.Account.Plan != "" {
		t.Fatalf("account = %+v", snap.Account)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageUnsupported {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_badKeyKeepsAccount(t *testing.T) {
	srv := goUsage(t)
	defer srv.Close()
	snap := probe(t, srv, `{"opencode-go":{"type":"api","key":"revoked"}}`)
	if snap.Account == nil || snap.Account.Plan != ProviderGo {
		t.Fatalf("account = %+v", snap.Account)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageProbeFailed || !strings.Contains(u.Message, "401") {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_otherLogins(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth string
		args []string
		want ap.AgentAccount
	}{
		{"zen key", `{"opencode":{"type":"api","key":"zen-key"}}`, nil, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"chatgpt login", `{"openai":{"type":"oauth","access":"a","refresh":"r","expires":1}}`, nil, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}},
		{"model names another provider", `{"opencode-go":{"type":"api","key":"go-key"},"anthropic":{"type":"api","key":"sk"}}`, []string{"-m", "anthropic/claude-sonnet-4-5"}, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
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

func TestReadAuth_content(t *testing.T) {
	a, err := ReadAuth(usage.Env{Env: []string{"HOME=" + t.TempDir(), `OPENCODE_AUTH_CONTENT={"opencode-go":{"type":"api","key":"k"}}`}})
	if err != nil || a[ProviderGo].Key != "k" {
		t.Fatalf("auth = %+v, %v", a, err)
	}
}
