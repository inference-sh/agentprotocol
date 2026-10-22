package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// jwt is an unsigned token expiring at exp.
func jwt(exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"sub": "auth0|user_1", "exp": exp.Unix()})
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

var (
	cycleStart = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	cycleEnd   = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
)

// cursorAPI serves DashboardService in Connect's JSON encoding, as Cursor
// does, checking the bearer token, and the API key exchange.
func cursorAPI(t *testing.T, token string) *httptest.Server {
	t.Helper()
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/auth/exchange_user_api_key" {
			if r.Header.Get("Authorization") != "Bearer key_good" {
				http.Error(w, `{"code":"error","message":"Invalid User API Key"}`, http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"accessToken":"`+token+`","refreshToken":"r"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"code":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/aiserver.v1.DashboardService/GetMe":
			_, _ = io.WriteString(w, `{"authId":"auth0|user_1","userId":7,"email":"dev@example.com","teamId":12,"teamName":"Acme"}`)
		case "/aiserver.v1.DashboardService/GetPlanInfo":
			_, _ = io.WriteString(w, `{"planInfo":{"planName":"Pro+","includedAmountCents":7000,"billingCycleEnd":"`+ms(cycleEnd)+`","includedUsagePeriod":"INCLUDED_USAGE_PERIOD_MONTHLY"}}`)
		case "/aiserver.v1.DashboardService/GetCurrentPeriodUsage":
			_, _ = io.WriteString(w, `{"billingCycleStart":"`+ms(cycleStart)+`","billingCycleEnd":"`+ms(cycleEnd)+`",
				"planUsage":{"totalSpend":2100,"includedSpend":2100,"remaining":4900,"limit":7000,"autoPercentUsed":12.5,"apiPercentUsed":40,"totalPercentUsed":30},
				"spendLimitUsage":{"individualUsed":0,"limitType":"user"},"enabled":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestProbe_login(t *testing.T) {
	token := jwt(time.Now().Add(time.Hour))
	srv := cursorAPI(t, token)
	defer srv.Close()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "cursor", "auth.json"), `{"accessToken":"`+token+`","refreshToken":"`+token+`"}`)

	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "CURSOR_API_ENDPOINT=" + srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "pro+", PlanLabel: "Pro+", Email: "dev@example.com", Workspace: "Acme"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 3 {
		t.Fatalf("limits = %+v", l)
	}
	// Ordered by ID within a kind.
	api, auto, plan := l.Windows[0], l.Windows[1], l.Windows[2]
	if plan.ID != "plan" || plan.UsedPercent != 30 || plan.Kind != ap.UsageWindowMonthly || !plan.ResetsAt.Equal(cycleEnd) || plan.DurationMinutes != 31*24*60 {
		t.Fatalf("plan = %+v", plan)
	}
	if auto.ID != "auto" || auto.UsedPercent != 12.5 || api.ID != "api" || api.UsedPercent != 40 {
		t.Fatalf("auto = %+v, api = %+v", auto, api)
	}
}

func TestProbe_apiKey(t *testing.T) {
	srv := cursorAPI(t, jwt(time.Now().Add(time.Hour)))
	defer srv.Close()
	home := t.TempDir()
	path := filepath.Join(home, "xdg", "cursor", "auth.json")
	// A token saved for the key that expires within five minutes is exchanged again.
	saved := `{"accessToken":"` + jwt(time.Now().Add(time.Minute)) + `","refreshToken":"r","apiKey":"key_good"}`
	writeFile(t, path, saved)

	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "xdg"), "CURSOR_API_ENDPOINT=" + srv.URL, "CURSOR_API_KEY=key_good",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if a := snap.Account; a == nil || a.Type != ap.AccountAPIKey || a.Email != "dev@example.com" {
		t.Fatalf("account = %+v", a)
	}
	if snap.Limits == nil || snap.Limits.Unavailable != nil || len(snap.Limits.Windows) != 3 {
		t.Fatalf("limits = %+v", snap.Limits)
	}
	// The exchanged token is not saved.
	if raw, _ := os.ReadFile(path); string(raw) != saved {
		t.Fatalf("auth.json rewritten: %s", raw)
	}

	// A key Cursor refuses keeps the account.
	snap, err = Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + t.TempDir(), "CURSOR_API_ENDPOINT=" + srv.URL, "CURSOR_API_KEY=key_bad"}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Type != ap.AccountAPIKey || snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
		t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
	}
}

func TestProbe_failures(t *testing.T) {
	srv := cursorAPI(t, "other")
	defer srv.Close()
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"expired login", jwt(time.Now().Add(-time.Hour))},
		{"refused token", jwt(time.Now().Add(time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, filepath.Join(home, ".config", "cursor", "auth.json"), `{"accessToken":"`+tc.token+`","refreshToken":"r"}`)
			snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "CURSOR_API_ENDPOINT=" + srv.URL}})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Account == nil || snap.Account.Status != ap.AccountAuthenticated || snap.Account.Type != ap.AccountSubscription {
				t.Fatalf("account = %+v", snap.Account)
			}
			if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
				t.Fatalf("limits = %+v", snap.Limits)
			}
		})
	}
}

func TestProbe_notSignedIn(t *testing.T) {
	for _, env := range [][]string{nil, {"AGENT_CLI_CREDENTIAL_STORE=memory"}} {
		home := t.TempDir()
		snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: append([]string{"HOME=" + home}, env...)})
		if err != nil {
			t.Fatal(err)
		}
		if snap.Account == nil || *snap.Account != (ap.AgentAccount{Status: ap.AccountUnauthenticated}) || snap.Limits != nil {
			t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
		}
	}
}

func TestReadLogin_macOSKeychain(t *testing.T) {
	home := t.TempDir()
	l, err := readLogin(usage.Env{Env: []string{"HOME=" + home}}, "darwin")
	if err != nil || !l.keychain {
		t.Fatalf("login = %+v, %v", l, err)
	}
	writeFile(t, filepath.Join(home, ".cursor", "auth.json"), `{"accessToken":"a","refreshToken":"r"}`)
	l, err = readLogin(usage.Env{Env: []string{"HOME=" + home, "AGENT_CLI_CREDENTIAL_STORE=file"}}, "darwin")
	if err != nil || l.keychain || l.accessToken != "a" {
		t.Fatalf("login = %+v, %v", l, err)
	}
}

func TestWindows(t *testing.T) {
	// Without percentages the included spend against the limit is the share
	// used; a weekly allowance resets when the plan says.
	resets := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	var r GetCurrentPeriodUsageResponse
	if err := json.Unmarshal([]byte(`{"billingCycleStart":"1791158400000","billingCycleEnd":"1793836800000","planUsage":{"includedSpend":500,"limit":2000}}`), &r); err != nil {
		t.Fatal(err)
	}
	ws := Windows(r, &PlanInfo{IncludedUsagePeriod: "INCLUDED_USAGE_PERIOD_WEEKLY", IncludedUsageResetsAt: Millis(resets.UnixMilli())})
	if len(ws) != 1 || ws[0].UsedPercent != 25 || ws[0].Kind != ap.UsageWindowWeekly || ws[0].DurationMinutes != 7*24*60 || !ws[0].ResetsAt.Equal(resets) {
		t.Fatalf("windows = %+v", ws)
	}
	// An enterprise plan reports spend only.
	if ws := Windows(GetCurrentPeriodUsageResponse{}, nil); ws != nil {
		t.Fatalf("windows = %+v", ws)
	}
}
