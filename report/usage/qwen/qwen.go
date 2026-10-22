// Package qwen reads Qwen Code's account. qwen exposes no quota read for any
// of its logins (the Coding Plan's quota shows only in Alibaba Cloud's
// console), so Prober reports the account, from the auth type qwen runs with
// and the key and endpoint of its model, and always reports limits as
// unsupported. It reads files only; it never starts qwen or calls a model.
package qwen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// Auth types, as settings.json security.auth.selectedType and --auth-type
// name them.
const (
	AuthQwenOAuth       = "qwen-oauth"
	AuthOpenAI          = "openai"
	AuthOpenAIResponses = "openai-responses"
	AuthAnthropic       = "anthropic"
	AuthGemini          = "gemini"
	AuthVertex          = "vertex-ai"
)

// Plans: the subscriptions a key can belong to, by the variable qwen keeps
// it in and the endpoint it calls.
const (
	PlanCodingPlan          = "coding_plan"
	PlanTokenPlan           = "token_plan"
	PlanQwenCloudTokenPlan  = "qwencloud_token_plan"
	PlanQwenOAuth           = "qwen_oauth"
	codingPlanKey           = "BAILIAN_CODING_PLAN_API_KEY"
	tokenPlanKey            = "BAILIAN_TOKEN_PLAN_API_KEY"
	qwenCloudTokenPlanKey   = "QWENCLOUD_TOKEN_PLAN_API_KEY"
	unsupportedLimitMessage = "qwen exposes no quota read"
)

var planKeys = map[string]string{
	PlanCodingPlan:         codingPlanKey,
	PlanTokenPlan:          tokenPlanKey,
	PlanQwenCloudTokenPlan: qwenCloudTokenPlanKey,
}

var planLabels = map[string]string{
	PlanCodingPlan:         "Alibaba Cloud Coding Plan",
	PlanTokenPlan:          "Alibaba Cloud Token Plan",
	PlanQwenCloudTokenPlan: "QwenCloud Token Plan",
	PlanQwenOAuth:          "Qwen OAuth",
}

// Prober reads the account from qwen's settings, environment and saved
// login.
type Prober struct{}

func (Prober) Probe(_ context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	home := qwenHome(env)
	now := time.Now()
	s, err := readSettings(env, home)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	dotenv := readDotenv(filepath.Join(home, ".env"))
	lookup := func(k string) string {
		if v := env.Lookup(k); v != "" {
			return v
		}
		if v := s.Env[k]; v != "" {
			return v
		}
		return dotenv[k]
	}
	acct := Account(AuthType(env, s, lookup), s, lookup, home)
	snap := ap.UsageSnapshot{Account: &acct}
	if acct.Status == ap.AccountAuthenticated {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, unsupportedLimitMessage, now)
	}
	return snap, nil
}

// Account is the account an auth type runs as. An OpenAI-, Anthropic- or
// Gemini-compatible key is a plan's when qwen keeps it in that plan's
// variable or it calls that plan's endpoint, and an API key otherwise.
func Account(auth string, s Settings, lookup func(string) string, home string) ap.AgentAccount {
	signedIn := ap.AgentAccount{Status: ap.AccountAuthenticated}
	switch auth {
	case "":
		// A plan's key in its own variable works with no auth type saved.
		for _, plan := range []string{PlanCodingPlan, PlanTokenPlan, PlanQwenCloudTokenPlan} {
			if lookup(planKeys[plan]) != "" {
				return subscription(plan)
			}
		}
		return ap.AgentAccount{Status: ap.AccountUnauthenticated}
	case AuthQwenOAuth:
		if _, err := os.Stat(filepath.Join(home, "oauth_creds.json")); err != nil {
			return ap.AgentAccount{Status: ap.AccountUnauthenticated}
		}
		return subscription(PlanQwenOAuth)
	case AuthVertex:
		signedIn.Type, signedIn.PlanLabel = ap.AccountCloud, "Google Vertex AI"
		return signedIn
	}
	envKey, baseURL := s.modelProvider(auth)
	if baseURL == "" {
		baseURL = s.Model.BaseURL
	}
	if baseURL == "" {
		baseURL = s.Security.Auth.BaseURL
	}
	if baseURL == "" {
		baseURL = lookup(baseURLVar(auth))
	}
	key := ""
	if envKey != "" {
		key = lookup(envKey)
	}
	if key == "" {
		key = s.Security.Auth.APIKey
	}
	if key == "" {
		key = lookup(defaultKeyVar(auth))
	}
	plan := Plan(envKey, baseURL)
	if key == "" && plan != "" {
		key = lookup(planKeys[plan])
	}
	if key == "" {
		return ap.AgentAccount{Status: ap.AccountUnauthenticated}
	}
	if plan != "" {
		return subscription(plan)
	}
	signedIn.Type = ap.AccountAPIKey
	return signedIn
}

func subscription(plan string) ap.AgentAccount {
	return ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: plan, PlanLabel: planLabels[plan]}
}

// Plan is the subscription a key belongs to, by the variable it is kept in
// or, failing that, the endpoint it calls; empty for a pay-as-you-go key.
func Plan(envKey, baseURL string) string {
	switch envKey {
	case codingPlanKey:
		return PlanCodingPlan
	case tokenPlanKey:
		return PlanTokenPlan
	case qwenCloudTokenPlanKey:
		return PlanQwenCloudTokenPlan
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	switch host := strings.ToLower(u.Hostname()); {
	case host == "coding.dashscope.aliyuncs.com" || host == "coding-intl.dashscope.aliyuncs.com":
		return PlanCodingPlan
	case strings.HasPrefix(host, "token-plan.") && strings.HasSuffix(host, ".maas.aliyuncs.com"):
		return PlanTokenPlan
	case host == "token-plan.maas.qwencloudapi.com" || host == "token-plan.maas.qianwenaiapi.com":
		return PlanQwenCloudTokenPlan
	}
	return ""
}

func baseURLVar(auth string) string {
	switch auth {
	case AuthAnthropic:
		return "ANTHROPIC_BASE_URL"
	case AuthGemini:
		return "GEMINI_BASE_URL"
	}
	return "OPENAI_BASE_URL"
}

func defaultKeyVar(auth string) string {
	switch auth {
	case AuthAnthropic:
		return "ANTHROPIC_API_KEY"
	case AuthGemini:
		return "GEMINI_API_KEY"
	}
	return "OPENAI_API_KEY"
}

// AuthType is the auth type qwen runs with: --auth-type, else the saved
// selectedType (the workspace's settings over the user's), else what the
// environment implies (qwen's getAuthTypeFromEnv).
func AuthType(env usage.Env, s Settings, lookup func(string) string) string {
	for i, a := range env.Args {
		if a == "--auth-type" && i+1 < len(env.Args) {
			return env.Args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--auth-type="); ok {
			return v
		}
	}
	if s.Security.Auth.SelectedType != "" {
		return s.Security.Auth.SelectedType
	}
	switch {
	case lookup("QWEN_OAUTH") != "":
		return AuthQwenOAuth
	case lookup("OPENAI_API_KEY") != "" && (lookup("OPENAI_MODEL") != "" || lookup("QWEN_MODEL") != "") && lookup("OPENAI_BASE_URL") != "":
		return AuthOpenAI
	case lookup("GEMINI_API_KEY") != "" && lookup("GEMINI_MODEL") != "":
		return AuthGemini
	case lookup("GOOGLE_MODEL") != "" && (lookup("GOOGLE_API_KEY") != "" || lookup("GOOGLE_CLOUD_PROJECT") != ""):
		return AuthVertex
	case lookup("ANTHROPIC_API_KEY") != "" && lookup("ANTHROPIC_MODEL") != "" && lookup("ANTHROPIC_BASE_URL") != "":
		return AuthAnthropic
	}
	return ""
}

// Settings is settings.json, the fields a probe reads.
type Settings struct {
	Security struct {
		Auth struct {
			SelectedType string `json:"selectedType"`
			APIKey       string `json:"apiKey"`
			BaseURL      string `json:"baseUrl"`
		} `json:"auth"`
	} `json:"security"`
	Model struct {
		Name    string `json:"name"`
		BaseURL string `json:"baseUrl"`
	} `json:"model"`
	ModelProviders map[string][]ModelProvider `json:"modelProviders"`
	Env            map[string]string          `json:"env"`
}

// ModelProvider is a modelProviders entry: a model, the endpoint it is
// called at and the variable holding its key.
type ModelProvider struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseUrl"`
	EnvKey  string `json:"envKey"`
}

// modelProvider is the entry of the model in use under the auth type, else
// the auth type's only entry.
func (s Settings) modelProvider(auth string) (envKey, baseURL string) {
	entries := s.ModelProviders[auth]
	for _, e := range entries {
		if e.ID == s.Model.Name {
			return e.EnvKey, e.BaseURL
		}
	}
	if len(entries) == 1 {
		return entries[0].EnvKey, entries[0].BaseURL
	}
	return "", ""
}

// readSettings merges the user's settings.json and the workspace's
// .qwen/settings.json, the workspace's winning field by field.
func readSettings(env usage.Env, home string) (Settings, error) {
	paths := []string{filepath.Join(home, "settings.json")}
	if env.Dir != "" {
		paths = append(paths, filepath.Join(env.Dir, ".qwen", "settings.json"))
	}
	var s Settings
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// Unmarshalling into the same value keeps the fields the later
		// file leaves out.
		if err := json.Unmarshal(raw, &s); err != nil {
			return Settings{}, fmt.Errorf("qwen: %s: %w", p, err)
		}
	}
	return s, nil
}

// readDotenv reads KEY=VALUE lines of qwen's .env.
func readDotenv(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// qwenHome is QWEN_HOME, which replaces ~/.qwen, else ~/.qwen.
func qwenHome(env usage.Env) string {
	if h := env.Lookup("QWEN_HOME"); h != "" {
		return h
	}
	return filepath.Join(env.Home(), ".qwen")
}
