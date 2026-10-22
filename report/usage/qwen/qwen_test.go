package qwen

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

func TestProbe(t *testing.T) {
	codingPlan := `{"modelProviders":{"openai":[{"id":"qwen3-coder-plus","baseUrl":"https://coding.dashscope.aliyuncs.com/v1","envKey":"BAILIAN_CODING_PLAN_API_KEY"},
		{"id":"qwen3.8-max","baseUrl":"https://maas.qwencloudapi.com/compatible-mode/v1","envKey":"QWENCLOUD_API_KEY"}]},
		"env":{"BAILIAN_CODING_PLAN_API_KEY":"sk-sp-x","QWENCLOUD_API_KEY":"sk-y"},
		"security":{"auth":{"selectedType":"openai"}},"model":{"name":"qwen3-coder-plus"}}`
	sub := func(plan string) ap.AgentAccount {
		return ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: plan, PlanLabel: planLabels[plan]}
	}
	key := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}
	out := ap.AgentAccount{Status: ap.AccountUnauthenticated}
	for _, tc := range []struct {
		name, settings, project, dotenv string
		oauth                           bool
		env, args                       []string
		want                            ap.AgentAccount
	}{
		{name: "coding plan", settings: codingPlan, want: sub(PlanCodingPlan)},
		{name: "pay-as-you-go model of the same settings", settings: codingPlan, project: `{"model":{"name":"qwen3.8-max"}}`, want: key},
		{name: "coding plan by env alone", env: []string{"BAILIAN_CODING_PLAN_API_KEY=sk-sp-x", "OPENAI_BASE_URL=https://coding-intl.dashscope.aliyuncs.com/v1", "OPENAI_MODEL=qwen3-coder-plus"}, want: sub(PlanCodingPlan)},
		{name: "token plan by endpoint", settings: `{"security":{"auth":{"selectedType":"openai"}},"model":{"name":"m"}}`,
			env: []string{"BAILIAN_TOKEN_PLAN_API_KEY=sk-x", "OPENAI_BASE_URL=https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"}, want: sub(PlanTokenPlan)},
		{name: "qwencloud token plan", settings: `{"modelProviders":{"openai":[{"id":"m","envKey":"QWENCLOUD_TOKEN_PLAN_API_KEY"}]},"security":{"auth":{"selectedType":"openai"}},"model":{"name":"m"}}`,
			dotenv: "QWENCLOUD_TOKEN_PLAN_API_KEY=sk-sp-z\n", want: sub(PlanQwenCloudTokenPlan)},
		{name: "openai key from env", env: []string{"OPENAI_API_KEY=sk-x", "OPENAI_MODEL=gpt", "OPENAI_BASE_URL=https://api.openai.com/v1"}, want: key},
		{name: "anthropic flag without a key", args: []string{"--auth-type", "anthropic"}, want: out},
		{name: "anthropic flag", args: []string{"--auth-type=anthropic"}, env: []string{"ANTHROPIC_API_KEY=sk-ant-x"}, want: key},
		{name: "qwen oauth", settings: `{"security":{"auth":{"selectedType":"qwen-oauth"}}}`, oauth: true, want: sub(PlanQwenOAuth)},
		{name: "qwen oauth signed out", settings: `{"security":{"auth":{"selectedType":"qwen-oauth"}}}`, want: out},
		{name: "vertex", settings: `{"security":{"auth":{"selectedType":"vertex-ai"}}}`, want: ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountCloud, PlanLabel: "Google Vertex AI"}},
		{name: "nothing", want: out},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qh, dir := t.TempDir(), t.TempDir()
			if tc.settings != "" {
				writeFile(t, filepath.Join(qh, "settings.json"), tc.settings)
			}
			if tc.project != "" {
				writeFile(t, filepath.Join(dir, ".qwen", "settings.json"), tc.project)
			}
			if tc.dotenv != "" {
				writeFile(t, filepath.Join(qh, ".env"), tc.dotenv)
			}
			if tc.oauth {
				writeFile(t, filepath.Join(qh, "oauth_creds.json"), `{"access_token":"a","refresh_token":"r"}`)
			}
			env := append([]string{"HOME=" + t.TempDir(), "QWEN_HOME=" + qh}, tc.env...)
			snap, err := Prober{}.Probe(context.Background(), usage.Env{Args: tc.args, Env: env, Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Account == nil || *snap.Account != tc.want {
				t.Fatalf("account = %+v, want %+v", snap.Account, tc.want)
			}
			l := snap.Limits
			if tc.want.Status == ap.AccountAuthenticated && (l == nil || l.Unavailable == nil || l.Unavailable.Reason != ap.UsageUnsupported || l.Unavailable.Message != unsupportedLimitMessage) {
				t.Fatalf("limits = %+v", l)
			}
		})
	}
}
