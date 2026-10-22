package goose

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

func jwt(exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "https://api.openai.com/profile": map[string]string{"email": "dev@example.com"}})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" || r.Header.Get("ChatGPT-Account-Id") != "acct-1" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000},"secondary_window":{"used_percent":40,"limit_window_seconds":604800}}}`)
	}))
	defer srv.Close()
	soon := time.Now().Add(time.Hour)
	tokens := func(exp time.Time) string {
		return `{"access_token":"` + jwt(soon) + `","refresh_token":"r","id_token":null,"expires_at":"` + exp.UTC().Format(time.RFC3339Nano) + `","account_id":"acct-1"}`
	}
	chatgpt := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "plus", PlanLabel: "ChatGPT Plus", Email: "dev@example.com", Workspace: "acct-1"}
	for _, tc := range []struct {
		name, config, tokens string
		env, args            []string
		want                 ap.AgentAccount
		windows              int
		reason               ap.UsageUnavailableReason
	}{
		{name: "chatgpt login", config: "GOOSE_PROVIDER: chatgpt_codex\nGOOSE_MODEL: gpt-5.5\n", tokens: tokens(soon), want: chatgpt, windows: 2},
		{name: "expired login", config: "GOOSE_PROVIDER: \"chatgpt_codex\"  # set by configure\n", tokens: tokens(time.Now().Add(-time.Hour)),
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: "dev@example.com", Workspace: "acct-1"}, reason: ap.UsageProbeFailed},
		{name: "chatgpt without a login", env: []string{"GOOSE_PROVIDER=chatgpt_codex"}, want: ap.AgentAccount{Status: ap.AccountUnauthenticated}},
		{name: "env over config", config: "GOOSE_PROVIDER: chatgpt_codex\n", tokens: tokens(soon), env: []string{"GOOSE_PROVIDER=anthropic"},
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}, reason: ap.UsageUnsupported},
		{name: "delegating provider", args: []string{"run", "--provider", "claude-acp"}, want: ap.AgentAccount{Status: ap.AccountUnknown}, reason: ap.UsageUnsupported},
		{name: "copilot", config: "GOOSE_PROVIDER: github_copilot\n",
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, PlanLabel: "GitHub Copilot"}, reason: ap.UsageUnsupported},
		{name: "not configured", want: ap.AgentAccount{Status: ap.AccountUnauthenticated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "config")
			if tc.config != "" {
				writeFile(t, filepath.Join(dir, "config.yaml"), tc.config)
			}
			if tc.tokens != "" {
				writeFile(t, filepath.Join(dir, "chatgpt_codex", "tokens.json"), tc.tokens)
			}
			env := append([]string{"HOME=" + t.TempDir(), "GOOSE_PATH_ROOT=" + root}, tc.env...)
			snap, err := Prober{ChatGPTBase: srv.URL}.Probe(context.Background(), usage.Env{Args: tc.args, Env: env})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Account == nil || *snap.Account != tc.want {
				t.Fatalf("account = %+v, want %+v", snap.Account, tc.want)
			}
			l := snap.Limits
			switch {
			case tc.want.Status == ap.AccountUnauthenticated:
			case l == nil:
				t.Fatal("no limits")
			case tc.reason != "" && (l.Unavailable == nil || l.Unavailable.Reason != tc.reason):
				t.Fatalf("limits = %+v", l)
			case tc.reason == "" && (l.Unavailable != nil || len(l.Windows) != tc.windows):
				t.Fatalf("limits = %+v", l)
			}
		})
	}
}

func TestConfigDir(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{[]string{"HOME=/h"}, "/h/.config/goose"},
		{[]string{"HOME=/h", "XDG_CONFIG_HOME=/x"}, "/x/goose"},
		{[]string{"HOME=/h", "XDG_CONFIG_HOME=/x", "GOOSE_PATH_ROOT=/r"}, "/r/config"},
		{[]string{"HOME=/h", "GOOSE_PATH_ROOT=relative"}, "/h/.config/goose"},
	} {
		if got := configDir(usage.Env{Env: tc.env}); got != tc.want {
			t.Errorf("configDir(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}
