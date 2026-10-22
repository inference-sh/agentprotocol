package gemini

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

// codeAssist serves loadCodeAssist and retrieveUserQuota as Google does,
// checking the bearer token.
func codeAssist(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = io.WriteString(w, `{"currentTier":{"id":"standard-tier","name":"Gemini Code Assist"},"paidTier":{"id":"g1-ultra-tier","name":"Google AI Ultra"},"cloudaicompanionProject":"proj-123"}`)
		case "/v1internal:retrieveUserQuota":
			var req RetrieveUserQuotaRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Project != "proj-123" {
				http.Error(w, "wrong project", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"buckets":[
				{"modelId":"gemini-3.8-flash","tokenType":"REQUESTS","remainingFraction":0.75,"resetTime":"2026-10-23T07:00:00Z"},
				{"modelId":"gemini-3.1-pro","tokenType":"REQUESTS","remainingFraction":0.1,"resetTime":"2026-10-23T07:00:00Z"},
				{"modelId":"no-fraction"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestProbe_googleLogin(t *testing.T) {
	srv := codeAssist(t, "fresh")
	defer srv.Close()
	home := t.TempDir()
	expiry := time.Now().Add(time.Hour).UnixMilli()
	writeFile(t, filepath.Join(home, ".gemini", "oauth_creds.json"), `{"access_token":"fresh","refresh_token":"refresh","expiry_date":`+itoa(expiry)+`}`)
	writeFile(t, filepath.Join(home, ".gemini", "google_accounts.json"), `{"active":"dev@example.com","old":[]}`)
	writeFile(t, filepath.Join(home, ".gemini", "settings.json"), `{"security":{"auth":{"selectedType":"oauth-personal"}}}`)

	snap, err := Prober{}.Probe(context.Background(), usage.Env{
		Env: []string{"HOME=" + home, "CODE_ASSIST_ENDPOINT=" + srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := snap.Account
	if a == nil || a.Status != ap.AccountAuthenticated || a.Type != ap.AccountSubscription || a.Email != "dev@example.com" || a.Plan != "g1-ultra-tier" || a.PlanLabel != "Google AI Ultra" {
		t.Fatalf("account = %+v", a)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 2 {
		t.Fatalf("limits = %+v", l)
	}
	// Ordered by ID within a kind.
	pro, flash := l.Windows[0], l.Windows[1]
	if pro.ID != "gemini-3.1-pro" || int(pro.UsedPercent+0.5) != 90 || pro.DurationMinutes != 24*60 || pro.Kind != ap.UsageWindowOther {
		t.Fatalf("pro = %+v", pro)
	}
	if flash.UsedPercent != 25 || !flash.ResetsAt.Equal(time.Date(2026, 10, 23, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("flash = %+v", flash)
	}
}

func TestProbe_quotaFailureKeepsAccount(t *testing.T) {
	srv := codeAssist(t, "other")
	defer srv.Close()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".gemini", "oauth_creds.json"), `{"access_token":"tok","expiry_date":0}`)
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "CODE_ASSIST_ENDPOINT=" + srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Type != ap.AccountSubscription {
		t.Fatalf("account = %+v", snap.Account)
	}
	if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_withoutAGoogleLogin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      []string
		settings string
		want     ap.AgentAccount
	}{
		{"api key", []string{"GEMINI_API_KEY=k"}, "", ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"gateway", []string{"GOOGLE_GEMINI_BASE_URL=http://x", "GEMINI_API_KEY=k"}, "", ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"vertex", []string{"GOOGLE_GENAI_USE_VERTEXAI=true"}, "", ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountCloud, PlanLabel: "Google Vertex AI"}},
		{"settings win over env", []string{"GOOGLE_GENAI_USE_VERTEXAI=true"}, `{"security":{"auth":{"selectedType":"gemini-api-key"}}}`, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"nothing", nil, "", ap.AgentAccount{Status: ap.AccountUnauthenticated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := t.TempDir()
			if tc.settings != "" {
				writeFile(t, filepath.Join(dir, ".gemini", "settings.json"), tc.settings)
			}
			snap, err := Prober{}.Probe(context.Background(), usage.Env{Dir: dir, Env: append([]string{"HOME=" + home}, tc.env...)})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Account == nil || *snap.Account != tc.want {
				t.Fatalf("account = %+v, want %+v", snap.Account, tc.want)
			}
			if tc.want.Status == ap.AccountAuthenticated && (snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageUnsupported) {
				t.Fatalf("limits = %+v", snap.Limits)
			}
		})
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestProbe_expiredLoginIsNotRefreshed(t *testing.T) {
	srv := codeAssist(t, "stale")
	defer srv.Close()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".gemini", "oauth_creds.json"), `{"access_token":"stale","refresh_token":"refresh","expiry_date":`+itoa(time.Now().Add(-time.Hour).UnixMilli())+`}`)
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "CODE_ASSIST_ENDPOINT=" + srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Type != ap.AccountSubscription {
		t.Fatalf("account = %+v", snap.Account)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageProbeFailed || !strings.Contains(u.Message, "expired") {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}
