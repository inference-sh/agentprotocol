package pi

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

// accountAPI serves claude.ai's /api/oauth/usage, ChatGPT's /wham/usage and
// Kimi Code's /me and /usages, each checking its token.
func accountAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/api/oauth/usage" && (auth == "Bearer sk-ant-oat01-tok" || auth == "Bearer sk-ant-oat01-env"):
			_, _ = io.WriteString(w, `{"five_hour":{"utilization":10},"seven_day":{"utilization":20}}`)
		case r.URL.Path == "/wham/usage" && r.Header.Get("ChatGPT-Account-Id") == "acct-1":
			_, _ = io.WriteString(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":3},"secondary_window":{"used_percent":9}}}`)
		case r.URL.Path == "/me" && auth == "Bearer k":
			_, _ = io.WriteString(w, `{"email":"kimi@example.com","user_level":30,"user_level_name":"Moderato"}`)
		case r.URL.Path == "/usages" && auth == "Bearer k":
			_, _ = io.WriteString(w, `{"usages":{"limit_5h":{"used_ratio":0.5},"limit_7d":{"used_ratio":0.25}}}`)
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
	later := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	earlier := strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10)
	both := `{"anthropic":{"type":"oauth","access":"sk-ant-oat01-tok","refresh":"r","expires":` + later + `},
		"openai-codex":{"type":"oauth","access":"` + jwt(time.Now().Add(time.Hour)) + `","refresh":"r","expires":` + later + `,"accountId":"acct-1"},
		"kimi-coding":{"type":"oauth","access":"k","refresh":"r","expires":` + later + `},
		"openrouter":{"type":"api_key","key":"sk-or-x"}}`
	sub := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}
	key := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}
	for _, tc := range []struct {
		name, auth, settings, project string
		args, env                     []string
		want                          ap.AgentAccount
		windows                       int
		reason                        ap.UsageUnavailableReason
	}{
		{name: "anthropic by default provider", auth: both, settings: `{"defaultProvider":"anthropic","defaultModel":"claude-opus-4-7"}`, want: sub, windows: 2},
		{name: "codex by --model", auth: both, settings: `{"defaultProvider":"anthropic"}`, args: []string{"--mode", "rpc", "--model", "openai-codex/gpt-5.5"},
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "pro", PlanLabel: "ChatGPT Pro 20x", Email: "dev@example.com", Workspace: "acct-1"}, windows: 2},
		{name: "project settings win", auth: both, settings: `{"defaultProvider":"anthropic"}`, project: `{"defaultProvider":"openrouter"}`, want: key, reason: ap.UsageUnsupported},
		{name: "kimi", auth: both, args: []string{"--provider", "kimi-coding"},
			want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "30", PlanLabel: "Moderato", Email: "kimi@example.com"}, windows: 2},
		{name: "expired login", auth: `{"anthropic":{"type":"oauth","access":"sk-ant-oat01-tok","refresh":"r","expires":` + earlier + `}}`, want: sub, reason: ap.UsageProbeFailed},
		{name: "setup-token in env", args: []string{"--provider", "anthropic"}, env: []string{"ANTHROPIC_OAUTH_TOKEN=sk-ant-oat01-env"}, want: sub, windows: 2},
		{name: "env api key", args: []string{"--provider", "openai"}, env: []string{"OPENAI_API_KEY=sk-x"}, want: key, reason: ap.UsageUnsupported},
		{name: "several logins and no default", auth: both, want: ap.AgentAccount{Status: ap.AccountAuthenticated}, reason: ap.UsageUnsupported},
		{name: "provider without a login", args: []string{"--provider", "anthropic"}, want: ap.AgentAccount{Status: ap.AccountUnknown}, reason: ap.UsageUnsupported},
		{name: "nothing", want: ap.AgentAccount{Status: ap.AccountUnknown}, reason: ap.UsageUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			agent := filepath.Join(home, "agent")
			if tc.auth != "" {
				writeFile(t, filepath.Join(agent, "auth.json"), tc.auth)
			}
			if tc.settings != "" {
				writeFile(t, filepath.Join(agent, "settings.json"), tc.settings)
			}
			if tc.project != "" {
				writeFile(t, filepath.Join(dir, ".pi", "settings.json"), tc.project)
			}
			env := append([]string{"HOME=" + home, "PI_CODING_AGENT_DIR=" + agent}, tc.env...)
			snap, err := Prober{AnthropicBase: srv.URL, ChatGPTBase: srv.URL, KimiBase: srv.URL}.Probe(context.Background(), usage.Env{Args: tc.args, Env: env, Dir: dir})
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
			switch {
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
