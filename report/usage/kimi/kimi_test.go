package kimi

import (
	"context"
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

// kimiAPI serves /me and /usages as Kimi Code does, checking the bearer
// token. usages is the /usages body.
func kimiAPI(t *testing.T, token, usages string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/coding/v1/me":
			_, _ = io.WriteString(w, `{"user_id":"u1","nickname":"dev","email":"dev@example.com","user_level":"30","user_level_name":"Allegretto","region":"CN"}`)
		case "/coding/v1/usages":
			_, _ = io.WriteString(w, usages)
		default:
			http.NotFound(w, r)
		}
	}))
}

const usagesBody = `{"usages":{
	"limit_5h":{"used_ratio":0.25,"reset_time":"2026-10-22T20:00:00Z"},
	"limit_7d":{"used_ratio":"0.5","reset_time":"2026-10-27T00:00:00Z"},
	"limit_month_total":{"used_ratio":0.1},
	"limit_month_code":{"reset_time":"2026-11-01T00:00:00Z"}},
	"boosterWallet":null}`

func future() string { return strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) }

func TestProbe_kimiCodeLogin(t *testing.T) {
	srv := kimiAPI(t, "tok", usagesBody)
	defer srv.Close()
	home := t.TempDir()
	base := srv.URL + "/coding/v1"
	// kimi login under KIMI_CODE_BASE_URL saves to a slot of its own.
	file, _ := credentialFile(OAuthKey(defaultOAuthHost, base))
	writeFile(t, filepath.Join(home, ".kimi-code", "credentials", file), `{"access_token":"tok","refresh_token":"r","expires_at":`+future()+`,"token_type":"Bearer"}`)

	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "KIMI_CODE_BASE_URL=" + base}})
	if err != nil {
		t.Fatal(err)
	}
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: "dev@example.com", Plan: "30", PlanLabel: "Allegretto"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 3 {
		t.Fatalf("limits = %+v", l)
	}
	five, week, month := l.Windows[0], l.Windows[1], l.Windows[2]
	if five.ID != "limit_5h" || five.Kind != ap.UsageWindowSession || five.UsedPercent != 25 || five.DurationMinutes != 300 ||
		!five.ResetsAt.Equal(time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("5h = %+v", five)
	}
	if week.ID != "limit_7d" || week.Kind != ap.UsageWindowWeekly || week.UsedPercent != 50 {
		t.Fatalf("7d = %+v", week)
	}
	if month.ID != "limit_month_total" || month.Kind != ap.UsageWindowMonthly || int(month.UsedPercent+0.5) != 10 || month.Label != "Monthly total" {
		t.Fatalf("month = %+v", month)
	}
}

func TestProbe_configNamesTheLogin(t *testing.T) {
	srv := kimiAPI(t, "tok", usagesBody)
	defer srv.Close()
	home := t.TempDir()
	// A global-region login: its own hosts and slot, recorded in config.toml.
	writeFile(t, filepath.Join(home, "kh", "config.toml"), `
default_model = "kimi-code/kimi-for-coding"
[providers."managed:kimi-code"]
type = "kimi"
base_url = "`+srv.URL+`/coding/v1"
api_key = ""
oauth = { storage = "file", key = "oauth/kimi-code-env-0e4f99c69cc27850", oauth_host = "https://auth.kimi.ai" }

[models."kimi-code/kimi-for-coding"]
provider = "managed:kimi-code" # the login's
model = "kimi-for-coding"
capabilities = [
  "thinking",
  "image_in",
]
`)
	writeFile(t, filepath.Join(home, "kh", "credentials", "kimi-code-env-0e4f99c69cc27850.json"), `{"access_token":"tok","expires_at":`+future()+`}`)

	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "KIMI_CODE_HOME=" + filepath.Join(home, "kh")}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Email != "dev@example.com" || snap.Limits == nil || len(snap.Limits.Windows) != 3 {
		t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
	}
}

func TestProbe_expiredLoginKeepsAccount(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json"), `{"access_token":"tok","refresh_token":"r","expires_at":1000}`)
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home}})
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

func TestProbe_usagesFailureKeepsAccount(t *testing.T) {
	srv := kimiAPI(t, "other", usagesBody)
	defer srv.Close()
	home := t.TempDir()
	base := srv.URL + "/coding/v1"
	file, _ := credentialFile(OAuthKey(defaultOAuthHost, base))
	writeFile(t, filepath.Join(home, ".kimi-code", "credentials", file), `{"access_token":"tok","expires_at":`+future()+`}`)
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "KIMI_CODE_BASE_URL=" + base}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Status != ap.AccountAuthenticated {
		t.Fatalf("account = %+v", snap.Account)
	}
	if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_withoutAKimiCodeLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    []string
		config string
		want   ap.AgentAccount
	}{
		{"env model", []string{"KIMI_MODEL_NAME=kimi-k2", "KIMI_MODEL_API_KEY=k"}, "", ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"api key", []string{"KIMI_API_KEY=k"}, "", ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"configured provider", nil, "default_model = \"m\"\n[providers.moonshot]\ntype = \"kimi\"\napi_key = \"sk\"\n[models.m]\nprovider = \"moonshot\"\n", ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
		{"nothing", nil, "", ap.AgentAccount{Status: ap.AccountUnauthenticated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.config != "" {
				writeFile(t, filepath.Join(home, ".kimi-code", "config.toml"), tc.config)
			}
			snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: append([]string{"HOME=" + home}, tc.env...)})
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

func TestWindows_olderShapes(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	u := Usages{
		Usage: &Quota{Limit: ptr(100), Used: ptr(40), ResetTime: "2026-10-27T00:00:00Z"},
		Limits: []LimitRow{
			{Detail: &Quota{Limit: ptr(50), Remaining: ptr(45), ResetTime: "2026-10-22T15:00:00Z"}, Window: LimitSpan{Duration: 300, TimeUnit: "TIME_UNIT_MINUTE"}},
			{Quota: Quota{Name: "Daily", Limit: ptr(10), Used: ptr(1)}, Window: LimitSpan{Duration: 1, TimeUnit: "TIME_UNIT_DAY"}},
		},
	}
	ws := usage.Limits(Windows(u, now), now).Windows
	if len(ws) != 3 {
		t.Fatalf("windows = %+v", ws)
	}
	if ws[0].ID != "limit_1d" || ws[0].Label != "Daily" || ws[0].UsedPercent != 10 {
		t.Fatalf("daily = %+v", ws[0])
	}
	if ws[1].ID != "limit_5h" || ws[1].UsedPercent != 10 || ws[1].DurationMinutes != 300 || ws[1].Label != "5-hour" {
		t.Fatalf("5h = %+v", ws[1])
	}
	if ws[2].ID != "limit_7d" || ws[2].UsedPercent != 40 || ws[2].Kind != ap.UsageWindowWeekly {
		t.Fatalf("weekly = %+v", ws[2])
	}
}

func TestOAuthKey(t *testing.T) {
	if k := OAuthKey("https://auth.kimi.com/", "https://api.kimi.com/coding/v1"); k != defaultOAuthKey {
		t.Fatalf("default = %s", k)
	}
	// As kimi 2.3.0 computes it for the global region.
	if k := OAuthKey("https://auth.kimi.ai", "https://api.kimi.ai/coding/v1"); k != "oauth/kimi-code-env-0e4f99c69cc27850" {
		t.Fatalf("global = %s", k)
	}
}

func TestParseTOML(t *testing.T) {
	got := parseTOML(`
top = "a" # comment
[t."x.y"]
s = 'lit'
e = "q\"é"
n = 3
arr = [1, "two", [3]]
inline = { a = "1", b = { c = "2" } }
dotted.k = "d"
[[arr_of_tables]]
k = "hidden"
`)
	for k, want := range map[string]string{
		keyPath("top"): "a", keyPath("t", "x.y", "s"): "lit", keyPath("t", "x.y", "e"): "q\"é",
		keyPath("t", "x.y", "inline", "a"): "1", keyPath("t", "x.y", "inline", "b", "c"): "2", keyPath("t", "x.y", "dotted", "k"): "d",
	} {
		if got[k] != want {
			t.Errorf("%q = %q, want %q", k, got[k], want)
		}
	}
	if got[keyPath("k")] != "" || got[keyPath("t", "x.y", "n")] != "" {
		t.Errorf("kept values it should not: %q", got)
	}
}

func ptr(f float64) *num { n := num(f); return &n }

func TestWindowsHaveUniqueIDs(t *testing.T) {
	var u Usages
	raw := `{"usage":{"used":10,"limit":100},"limits":[
		{"window":{"duration":7,"timeUnit":"TIME_UNIT_DAY"},"detail":{"used":1,"limit":10}},
		{"window":{"duration":7,"timeUnit":"TIME_UNIT_DAY"},"detail":{"used":2,"limit":10}}]}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, w := range Windows(u, time.Now()) {
		if seen[w.ID] {
			t.Fatalf("duplicate window ID %q", w.ID)
		}
		seen[w.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("windows = %v", seen)
	}
}
