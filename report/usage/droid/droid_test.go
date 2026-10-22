package droid

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
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

// seal encrypts as the CLI does: iv:tag:ciphertext, base64, AES-256-GCM
// with a 16-byte IV.
func seal(t *testing.T, plain string, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, 16)
	_, _ = rand.Read(iv)
	out := gcm.Seal(nil, iv, []byte(plain), nil)
	ct, tag := out[:len(out)-16], out[len(out)-16:]
	enc := base64.StdEncoding.EncodeToString
	return enc(iv) + ":" + enc(tag) + ":" + enc(ct)
}

// saveLogin writes a login to auth.v2.file and its key to auth.v2.key, as
// the CLI does without a keyring.
func saveLogin(t *testing.T, home string, creds Credentials) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	raw, _ := json.Marshal(creds)
	writeFile(t, filepath.Join(home, ".factory", "auth.v2.key"), base64.StdEncoding.EncodeToString(key))
	writeFile(t, filepath.Join(home, ".factory", "auth.v2.file"), seal(t, string(raw), key))
}

// jwt is an unsigned WorkOS access token.
func jwt(exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"sub": "user_01", "email": "dev@example.com", "org_id": "org_jwt", "exp": exp.Unix()})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

var weekEnd = time.Date(2099, 1, 7, 0, 0, 0, 0, time.UTC)

// factory serves whoami and the billing limits as Factory does, checking
// the bearer token and the organization header.
func factory(t *testing.T, token, org, region string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Factory-Org-Id") != org {
			http.Error(w, `{"status":401,"title":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/cli/whoami":
			if r.Header.Get("X-Factory-Whoami-Extended") != "true" {
				http.Error(w, "not extended", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"userId":"user_01","orgId":"org_01","region":"`+region+`","isEnterprise":false}`)
		case "/api/billing/limits":
			_, _ = io.WriteString(w, `{"limits":{
				"standard":{"fiveHour":{"usedPercent":42,"windowEnd":"2099-01-01T05:00:00Z"},"weekly":{"usedPercent":10,"windowEnd":"`+weekEnd.Format(time.RFC3339)+`"},"monthly":{"usedPercent":100,"windowEnd":"2020-01-01T00:00:00Z"}},
				"core":{"fiveHour":{"usedPercent":5,"windowEnd":"2099-01-01T05:00:00Z"}}},
				"extraUsageBalanceCents":0,"overagePreference":"droidCore"}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestProbe_login(t *testing.T) {
	token := jwt(time.Now().Add(time.Hour))
	srv := factory(t, token, "org_01", "global")
	defer srv.Close()
	home := t.TempDir()
	saveLogin(t, home, Credentials{AccessToken: token, RefreshToken: "refresh", ActiveOrganizationID: "org_01"})

	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{"HOME=" + home, "FACTORY_API_BASE_URL=" + srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: "dev@example.com", Workspace: "org_01"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 4 {
		t.Fatalf("limits = %+v", l)
	}
	// Session, weekly, monthly; by ID within a kind.
	core, five, weekly, monthly := l.Windows[0], l.Windows[1], l.Windows[2], l.Windows[3]
	if core.ID != "core_five_hour" || core.UsedPercent != 5 || core.Label != "Droid Core 5-hour" {
		t.Fatalf("core = %+v", core)
	}
	if five.ID != "five_hour" || five.UsedPercent != 42 || five.Kind != ap.UsageWindowSession || five.DurationMinutes != 300 {
		t.Fatalf("five hour = %+v", five)
	}
	if weekly.ID != "weekly" || weekly.UsedPercent != 10 || !weekly.ResetsAt.Equal(weekEnd) {
		t.Fatalf("weekly = %+v", weekly)
	}
	// A lapsed window reads as unused, with no reset.
	if monthly.ID != "monthly" || monthly.UsedPercent != 0 || !monthly.ResetsAt.IsZero() {
		t.Fatalf("monthly = %+v", monthly)
	}
}

func TestProbe_apiKeyInEU(t *testing.T) {
	global := factory(t, "fk-key", "", "eu")
	defer global.Close()
	eu := factory(t, "fk-key", "", "eu")
	defer eu.Close()
	// The global API answers whoami only; limits come from the EU one.
	globalOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cli/whoami" {
			http.Error(w, "wrong region", http.StatusMisdirectedRequest)
			return
		}
		global.Config.Handler.ServeHTTP(w, r)
	}))
	defer globalOnly.Close()

	snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: []string{
		"HOME=" + t.TempDir(), "FACTORY_API_KEY=fk-key", "FACTORY_API_BASE_URL=" + globalOnly.URL, "FACTORY_API_BASE_URL_EU=" + eu.URL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey, Workspace: "org_01"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	if snap.Limits == nil || snap.Limits.Unavailable != nil || len(snap.Limits.Windows) != 4 {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_failuresKeepAccount(t *testing.T) {
	srv := factory(t, "other", "", "global")
	defer srv.Close()
	for _, tc := range []struct {
		name  string
		setup func(home string) []string
		want  ap.AccountType
	}{
		{"expired login", func(home string) []string {
			saveLogin(t, home, Credentials{AccessToken: jwt(time.Now().Add(-time.Minute)), RefreshToken: "r"})
			return nil
		}, ap.AccountSubscription},
		{"refused login", func(home string) []string {
			saveLogin(t, home, Credentials{AccessToken: jwt(time.Now().Add(time.Hour)), RefreshToken: "r"})
			return nil
		}, ap.AccountSubscription},
		{"refused key", func(string) []string { return []string{"FACTORY_API_KEY=fk-bad"} }, ap.AccountAPIKey},
		{"keyring", func(home string) []string {
			writeFile(t, filepath.Join(home, ".factory", "auth.v2.keyring"), "sealed")
			return nil
		}, ap.AccountSubscription},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := append([]string{"HOME=" + home, "FACTORY_API_BASE_URL=" + srv.URL}, tc.setup(home)...)
			snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: env})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Account == nil || snap.Account.Status != ap.AccountAuthenticated || snap.Account.Type != tc.want {
				t.Fatalf("account = %+v", snap.Account)
			}
			if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
				t.Fatalf("limits = %+v", snap.Limits)
			}
		})
	}
}

func TestProbe_notSignedIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(home string)
		env   []string
	}{
		{"nothing", func(string) {}, nil},
		{"key file missing", func(home string) {
			writeFile(t, filepath.Join(home, ".factory", "auth.v2.file"), "a:b:c")
		}, nil},
		{"keyring disabled", func(home string) {
			writeFile(t, filepath.Join(home, ".factory", "auth.v2.keyring"), "sealed")
		}, []string{"FACTORY_DISABLE_KEYRING=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.setup(home)
			snap, err := Prober{}.Probe(context.Background(), usage.Env{Env: append([]string{"HOME=" + home}, tc.env...)})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Account == nil || *snap.Account != (ap.AgentAccount{Status: ap.AccountUnauthenticated}) || snap.Limits != nil {
				t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
			}
		})
	}
}

func TestReadCredentials_homeOverride(t *testing.T) {
	base := t.TempDir()
	saveLogin(t, base, Credentials{AccessToken: "tok", RefreshToken: "r", ActiveOrganizationID: " org_9 "})
	c, err := ReadCredentials(usage.Env{Env: []string{"HOME=" + t.TempDir(), "FACTORY_HOME_OVERRIDE=" + base}})
	if err != nil || c.AccessToken != "tok" || c.ActiveOrganizationID != " org_9 " {
		t.Fatalf("creds = %+v, %v", c, err)
	}
}

func TestWindows_notTokenBilling(t *testing.T) {
	off := false
	if ws := Windows(BillingLimits{UsesTokenRateLimitsBilling: &off}, time.Now()); ws != nil {
		t.Fatalf("windows = %+v", ws)
	}
}
