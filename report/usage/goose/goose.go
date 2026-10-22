// Package goose reads goose's account and plan limits. goose runs on the
// provider GOOSE_PROVIDER names (the environment over config.yaml); Prober
// reads the login of that provider. The chatgpt_codex provider's ChatGPT
// login is read through ChatGPT's usage API (the shared reader in the codex
// package). goose has no Claude login of its own, and API keys sit in the
// keyring, so other providers report the account without limits.
package goose

import (
	"bufio"
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
	"github.com/inference-sh/agentprotocol/report/usage/codex"
)

// ProviderChatGPT is the provider that signs in with a ChatGPT login.
const ProviderChatGPT = "chatgpt_codex"

// ProbeTimeout bounds a probe: one API call.
const ProbeTimeout = 10 * time.Second

// Prober reads the account and limits from goose's configuration and the
// login it saved. It never starts goose, never calls a model and never
// refreshes a token: an expired one reads as probe_failed until goose
// refreshes it, since a refresh rotates the refresh token goose holds.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
	// ChatGPTBase replaces ChatGPT's API root, for tests.
	ChatGPTBase string
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	dir := configDir(env)
	now := time.Now()
	provider := Provider(env, dir)
	switch {
	case provider == "":
		return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
	case provider == ProviderChatGPT:
		tok, err := readTokens(filepath.Join(dir, ProviderChatGPT, "tokens.json"))
		if errors.Is(err, os.ErrNotExist) {
			return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
		}
		if err != nil {
			return ap.UsageSnapshot{}, err
		}
		return codex.WhamSnapshot(ctx, p.Client, p.ChatGPTBase, tok.AccessToken, tok.AccountID, isoTime(tok.ExpiresAt), now), nil
	case delegates(provider):
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountUnknown},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "goose's "+provider+" provider runs another agent on that agent's own login; probe that agent", now),
		}, nil
	}
	if label, ok := subscriptions[provider]; ok {
		if provider == "xai_oauth" || provider == "gemini_oauth" {
			if _, err := os.Stat(filepath.Join(dir, provider, "tokens.json")); err != nil {
				return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
			}
		}
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, PlanLabel: label},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "no quota read for goose's "+provider+" login", now),
		}, nil
	}
	// An API-key provider: the key is in the keyring (or secrets.yaml), so
	// configuring the provider is taken as signed in.
	return ap.UsageSnapshot{
		Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
		Limits:  usage.Unavailable(ap.UsageUnsupported, "plan limits do not apply to the "+provider+" provider", now),
	}, nil
}

// subscriptions are the other providers goose signs in to with a
// subscription login. xai_oauth and gemini_oauth keep their tokens in
// <provider>/tokens.json under the config directory; the others keep theirs
// in the keyring.
var subscriptions = map[string]string{
	"github_copilot": "GitHub Copilot",
	"xai_oauth":      "SuperGrok",
	"gemini_oauth":   "Google Gemini",
	"kimi_code":      "Kimi Code",
}

// delegates reports whether a provider runs another agent's CLI, which
// signs in on its own.
func delegates(provider string) bool {
	switch provider {
	case "claude-code", "codex", "cursor-agent", "gemini-cli":
		return true
	}
	return strings.HasSuffix(provider, "-acp")
}

// Provider is the provider goose runs on: --provider in the arguments, else
// GOOSE_PROVIDER from the environment, else from config.yaml.
func Provider(env usage.Env, dir string) string {
	for i, a := range env.Args {
		if a == "--provider" && i+1 < len(env.Args) {
			return env.Args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--provider="); ok {
			return v
		}
	}
	if v := env.Lookup("GOOSE_PROVIDER"); v != "" {
		return v
	}
	return topLevel(filepath.Join(dir, "config.yaml"), "GOOSE_PROVIDER")
}

// topLevel is a top-level scalar of a YAML file, read with a line scan.
func topLevel(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), key+":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				return v[1 : end+1]
			}
		}
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// tokens is chatgpt_codex/tokens.json (crates/goose/src/providers/
// chatgpt_codex.rs TokenData).
type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	AccountID    string `json:"account_id"`
}

func readTokens(path string) (tokens, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return tokens{}, err
	}
	var t tokens
	if err := json.Unmarshal(raw, &t); err != nil {
		return tokens{}, fmt.Errorf("goose: %s: %w", filepath.Base(filepath.Dir(path))+"/tokens.json", err)
	}
	if t.AccessToken == "" {
		return tokens{}, os.ErrNotExist
	}
	return t, nil
}

func isoTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// configDir is goose's config directory: GOOSE_PATH_ROOT/config when that
// is an absolute path, else XDG_CONFIG_HOME/goose, else ~/.config/goose.
func configDir(env usage.Env) string {
	if root := env.Lookup("GOOSE_PATH_ROOT"); filepath.IsAbs(root) {
		return filepath.Join(root, "config")
	}
	if x := env.Lookup("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "goose")
	}
	return filepath.Join(env.Home(), ".config", "goose")
}
