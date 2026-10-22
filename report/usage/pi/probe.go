package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
	"github.com/inference-sh/agentprotocol/report/usage/claude"
	"github.com/inference-sh/agentprotocol/report/usage/codex"
	"github.com/inference-sh/agentprotocol/report/usage/kimi"
)

// Provider IDs, as auth.json and settings.json name them.
const (
	ProviderAnthropic = "anthropic"
	ProviderCodex     = "openai-codex"
	ProviderOpenAI    = "openai"
	ProviderKimi      = "kimi-coding"
	ProviderCopilot   = "github-copilot"
	ProviderXAI       = "xai"
)

// ProbeTimeout bounds a probe: at most two API calls.
const ProbeTimeout = 15 * time.Second

// Prober reads the account and plan limits of the provider pi's sessions
// use, from the login pi saved in auth.json: a Claude login through
// claude.ai's usage API, a ChatGPT (Codex) login through ChatGPT's, a Kimi
// Code login through Kimi's. It never
// starts pi, never calls a model and never refreshes a token: an expired one
// reads as probe_failed until pi refreshes it, since a refresh rotates the
// refresh token pi holds.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
	// AnthropicBase and ChatGPTBase replace claude.ai's and ChatGPT's API
	// roots, for tests.
	AnthropicBase string
	ChatGPTBase   string
	// KimiBase replaces Kimi Code's API root, for tests.
	KimiBase string
}

// Credential is an auth.json entry: an OAuth login (access, refresh,
// expires in ms) or an API key.
type Credential struct {
	Type      string `json:"type"` // oauth or api_key
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId"`
	Key       string `json:"key"`
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	dir := agentDir(env)
	now := time.Now()
	creds, err := readAuth(dir)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	provider := Provider(env, dir)
	if provider == "" && len(creds) == 1 {
		for id := range creds {
			provider = id
		}
	}
	if provider == "" {
		if len(creds) == 0 {
			return usage.NoSavedLogin(env, now), nil
		}
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "no default provider is set (defaultProvider in settings.json)", now),
		}, nil
	}

	c, ok := creds[provider]
	if !ok || (c.Type == "oauth" && c.Access == "") {
		return p.envLogin(ctx, env, provider, now), nil
	}
	if c.Type != "oauth" {
		return apiKey(provider, now), nil
	}
	var expires time.Time
	if c.Expires > 0 {
		expires = time.UnixMilli(c.Expires)
	}
	switch provider {
	case ProviderAnthropic:
		return claude.OAuthSnapshot(ctx, p.Client, p.AnthropicBase, c.Access, expires, now), nil
	case ProviderCodex:
		return codex.WhamSnapshot(ctx, p.Client, p.ChatGPTBase, c.Access, c.AccountID, expires, now), nil
	case ProviderKimi:
		return kimi.Snapshot(ctx, p.Client, p.KimiBase, c.Access, expires, now), nil
	}
	return ap.UsageSnapshot{
		Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, PlanLabel: subscriptionLabel(provider)},
		Limits:  usage.Unavailable(ap.UsageUnsupported, "no quota read for pi's "+provider+" login", now),
	}, nil
}

// envLogin is the account when auth.json holds nothing for the provider: a
// Claude token in ANTHROPIC_OAUTH_TOKEN (a claude setup-token) is a Claude
// login, the provider's API key variable an API key.
func (p Prober) envLogin(ctx context.Context, env usage.Env, provider string, now time.Time) ap.UsageSnapshot {
	if t := env.Lookup("ANTHROPIC_OAUTH_TOKEN"); provider == ProviderAnthropic && t != "" {
		return claude.OAuthSnapshot(ctx, p.Client, p.AnthropicBase, t, time.Time{}, now)
	}
	for _, k := range envKeys(provider) {
		if env.Lookup(k) != "" {
			return apiKey(provider, now)
		}
	}
	return usage.NoSavedLogin(env, now)
}

func envKeys(provider string) []string {
	switch provider {
	case ProviderAnthropic:
		return []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}
	case ProviderCopilot:
		return []string{"COPILOT_GITHUB_TOKEN"}
	case "google":
		return []string{"GEMINI_API_KEY"}
	case ProviderCodex:
		return nil
	}
	return []string{strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_API_KEY"}
}

func apiKey(provider string, now time.Time) ap.UsageSnapshot {
	return ap.UsageSnapshot{
		Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
		Limits:  usage.Unavailable(ap.UsageUnsupported, "plan limits do not apply to an API key ("+provider+")", now),
	}
}

func subscriptionLabel(provider string) string {
	switch provider {
	case ProviderOpenAI:
		return "ChatGPT"
	case ProviderKimi:
		return "Kimi Code"
	case ProviderCopilot:
		return "GitHub Copilot"
	case ProviderXAI:
		return "SuperGrok"
	}
	return ""
}

// Provider is the provider pi's sessions run on: --provider, or the
// provider of a --model given as provider/id, else defaultProvider from the
// project's .pi/settings.json over the user's. Empty when none names one.
func Provider(env usage.Env, dir string) string {
	var model string
	for i := 0; i < len(env.Args); i++ {
		a := env.Args[i]
		switch {
		case a == "--provider" && i+1 < len(env.Args):
			return env.Args[i+1]
		case strings.HasPrefix(a, "--provider="):
			return strings.TrimPrefix(a, "--provider=")
		case a == "--model" && i+1 < len(env.Args):
			model = env.Args[i+1]
			i++
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
		}
	}
	if prov, _, ok := strings.Cut(model, "/"); ok && prov != "" {
		return prov
	}
	var paths []string
	if env.Dir != "" {
		paths = append(paths, filepath.Join(env.Dir, ".pi", "settings.json"))
	}
	paths = append(paths, filepath.Join(dir, "settings.json"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s struct {
			DefaultProvider string `json:"defaultProvider"`
		}
		if json.Unmarshal(raw, &s) == nil && s.DefaultProvider != "" {
			return s.DefaultProvider
		}
	}
	return ""
}

func readAuth(dir string) (map[string]Credential, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var creds map[string]Credential
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("pi: auth.json: %w", err)
	}
	return creds, nil
}

// agentDir is PI_CODING_AGENT_DIR, else ~/.pi/agent.
func agentDir(env usage.Env) string {
	if d := env.Lookup("PI_CODING_AGENT_DIR"); d != "" {
		if rest, ok := strings.CutPrefix(d, "~/"); ok {
			return filepath.Join(env.Home(), rest)
		}
		return d
	}
	return filepath.Join(env.Home(), ".pi", "agent")
}
