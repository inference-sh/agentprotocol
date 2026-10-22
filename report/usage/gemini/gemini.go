// Package gemini reads Gemini CLI's account and plan limits. The CLI reports
// neither over ACP, so Prober reads them the way the CLI's own /stats does:
// from the login gemini saved, through Google's Code Assist API
// (loadCodeAssist for the tier, retrieveUserQuota for the per-model daily
// quota). Signed in with an API key, through Vertex AI or a gateway, there is
// no plan to read.
package gemini

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

	"github.com/inference-sh/agentprotocol/internal/webjson"
	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// Auth types, as settings.json security.auth.selectedType and ACP name them.
const (
	AuthOAuth   = "oauth-personal"
	AuthAPIKey  = "gemini-api-key"
	AuthVertex  = "vertex-ai"
	AuthADC     = "compute-default-credentials"
	AuthShell   = "cloud-shell"
	AuthGateway = "gateway"
)

// The Code Assist API. CODE_ASSIST_ENDPOINT in the agent's environment moves
// it, as it does for the CLI.
const (
	DefaultEndpoint = "https://cloudcode-pa.googleapis.com"
	apiVersion      = "v1internal"
)

// ProbeTimeout bounds a probe: two API calls.
const ProbeTimeout = 20 * time.Second

// Prober reads the account and the daily quota from the login gemini saved.
// It never starts the CLI, never calls a model and never refreshes the
// token: an expired one reads as probe_failed until gemini refreshes it on
// its next run.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	home := geminiHome(env)
	now := time.Now()

	switch auth := AuthType(env, home); auth {
	case AuthAPIKey, AuthGateway:
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "plan quota applies only to a Google login", now),
		}, nil
	case AuthVertex, AuthADC, AuthShell:
		return ap.UsageSnapshot{
			Account: &ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountCloud, PlanLabel: "Google Vertex AI"},
			Limits:  usage.Unavailable(ap.UsageUnsupported, "plan quota applies only to a Google login", now),
		}, nil
	case "":
		if _, err := os.Stat(filepath.Join(home, "oauth_creds.json")); err != nil {
			return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
		}
	}

	creds, err := readCreds(home)
	if errors.Is(err, os.ErrNotExist) {
		return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
	}
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: activeEmail(home)}
	snap := ap.UsageSnapshot{Account: &acct}
	if creds.AccessToken == "" || (creds.ExpiryDate != 0 && !time.UnixMilli(creds.ExpiryDate).After(now.Add(time.Minute))) {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "the saved login has expired; gemini refreshes it on its next run", now)
		return snap, nil
	}
	c := api{client: p.Client, base: endpoint(env), token: creds.AccessToken}
	var load LoadCodeAssistResponse
	err = c.call(ctx, "loadCodeAssist", LoadCodeAssistRequest{
		CloudaicompanionProject: env.Lookup("GOOGLE_CLOUD_PROJECT"),
		Metadata:                clientMetadata{IDEType: "IDE_UNSPECIFIED", Platform: "PLATFORM_UNSPECIFIED", PluginType: "GEMINI", DuetProject: env.Lookup("GOOGLE_CLOUD_PROJECT")},
	}, &load)
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	acct.Plan, acct.PlanLabel = Plan(load)
	project := load.CloudaicompanionProject
	if project == "" {
		project = env.Lookup("GOOGLE_CLOUD_PROJECT")
	}
	if project == "" {
		// Not onboarded yet: the CLI onboards on its first run, a probe does not.
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "no Code Assist project yet; run gemini once", now)
		return snap, nil
	}
	acct.Workspace = env.Lookup("GOOGLE_CLOUD_PROJECT")
	var quota RetrieveUserQuotaResponse
	if err := c.call(ctx, "retrieveUserQuota", RetrieveUserQuotaRequest{Project: project}, &quota); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	snap.Limits = usage.Limits(Windows(quota), now)
	return snap, nil
}

// AuthType is the login gemini runs with, as the CLI picks it: the saved
// selectedType (the workspace's settings over the user's), else the
// environment. Empty when neither names one.
func AuthType(env usage.Env, home string) string {
	var paths []string
	if env.Dir != "" {
		paths = append(paths, filepath.Join(env.Dir, ".gemini", "settings.json"))
	}
	paths = append(paths, filepath.Join(home, "settings.json"))
	for _, p := range paths {
		if t := selectedType(p); t != "" {
			return t
		}
	}
	switch {
	case env.Lookup("GOOGLE_GENAI_USE_GCA") == "true":
		return AuthOAuth
	case env.Lookup("GOOGLE_GENAI_USE_VERTEXAI") == "true":
		return AuthVertex
	case env.Lookup("GOOGLE_GEMINI_BASE_URL") != "":
		return AuthGateway
	case env.Lookup("GEMINI_API_KEY") != "":
		return AuthAPIKey
	case env.Lookup("CLOUD_SHELL") == "true":
		return AuthShell
	case env.Lookup("GEMINI_CLI_USE_COMPUTE_ADC") == "true":
		return AuthADC
	}
	return ""
}

func selectedType(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
		// Before gemini 0.3 the setting was top-level.
		SelectedAuthType string `json:"selectedAuthType"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	if s.Security.Auth.SelectedType != "" {
		return s.Security.Auth.SelectedType
	}
	return s.SelectedAuthType
}

// Plan is the tier's id and display name: the paid tier when there is one,
// else the current one. Google names the tiers; "free-tier", "legacy-tier"
// and "standard-tier" are the ids the CLI knows.
func Plan(r LoadCodeAssistResponse) (id, label string) {
	t := r.PaidTier
	if t == nil {
		t = r.CurrentTier
	}
	if t == nil {
		return "", ""
	}
	return t.ID, t.Name
}

// Windows maps the quota's buckets, one per model and token type. Each is a
// daily allowance; used is what the remaining fraction leaves.
func Windows(r RetrieveUserQuotaResponse) []ap.UsageWindow {
	ws := make([]ap.UsageWindow, 0, len(r.Buckets))
	for _, b := range r.Buckets {
		if b.ModelID == "" || b.RemainingFraction == nil {
			continue
		}
		id := b.ModelID
		if b.TokenType != "" && b.TokenType != "REQUESTS" {
			id += ":" + strings.ToLower(b.TokenType)
		}
		w := ap.UsageWindow{
			ID: id, Kind: ap.UsageWindowOther, Label: b.ModelID + " daily",
			UsedPercent:     (1 - *b.RemainingFraction) * 100,
			DurationMinutes: 24 * 60,
		}
		if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
			w.ResetsAt = t.UTC()
		}
		ws = append(ws, w)
	}
	return ws
}

// The Code Assist API's shapes (gemini-cli packages/core/src/code_assist/
// types.ts), the fields a probe reads.
type (
	LoadCodeAssistRequest struct {
		CloudaicompanionProject string         `json:"cloudaicompanionProject,omitempty"`
		Metadata                clientMetadata `json:"metadata"`
	}
	clientMetadata struct {
		IDEType     string `json:"ideType"`
		Platform    string `json:"platform"`
		PluginType  string `json:"pluginType"`
		DuetProject string `json:"duetProject,omitempty"`
	}
	LoadCodeAssistResponse struct {
		CurrentTier             *Tier  `json:"currentTier,omitempty"`
		PaidTier                *Tier  `json:"paidTier,omitempty"`
		CloudaicompanionProject string `json:"cloudaicompanionProject,omitempty"`
	}
	Tier struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	RetrieveUserQuotaRequest struct {
		Project string `json:"project"`
	}
	RetrieveUserQuotaResponse struct {
		Buckets []Bucket `json:"buckets"`
	}
	Bucket struct {
		ModelID           string   `json:"modelId"`
		TokenType         string   `json:"tokenType,omitempty"`
		RemainingFraction *float64 `json:"remainingFraction,omitempty"`
		RemainingAmount   string   `json:"remainingAmount,omitempty"`
		ResetTime         string   `json:"resetTime,omitempty"`
	}
)

// oauthCreds is oauth_creds.json, google-auth-library's saved credentials.
type oauthCreds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiryDate   int64  `json:"expiry_date"` // ms since the epoch
}

func readCreds(home string) (oauthCreds, error) {
	raw, err := os.ReadFile(filepath.Join(home, "oauth_creds.json"))
	if err != nil {
		return oauthCreds{}, err
	}
	var c oauthCreds
	if err := json.Unmarshal(raw, &c); err != nil {
		return oauthCreds{}, fmt.Errorf("gemini: oauth_creds.json: %w", err)
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return oauthCreds{}, os.ErrNotExist
	}
	return c, nil
}

// activeEmail is the account google_accounts.json names active.
func activeEmail(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, "google_accounts.json"))
	if err != nil {
		return ""
	}
	var a struct {
		Active string `json:"active"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Active
}

type api struct {
	client *http.Client
	base   string
	token  string
}

func (a api) call(ctx context.Context, method string, body, out any) error {
	if err := webjson.Post(ctx, a.client, a.base+"/"+apiVersion+":"+method, map[string]string{"Authorization": "Bearer " + a.token}, body, out); err != nil {
		return fmt.Errorf("gemini: %s: %w", method, err)
	}
	return nil
}

// geminiHome is the CLI's .gemini directory: under GEMINI_CLI_HOME when it is
// set, else under HOME.
func geminiHome(env usage.Env) string {
	if base := env.Lookup("GEMINI_CLI_HOME"); base != "" {
		return filepath.Join(base, ".gemini")
	}
	return filepath.Join(env.Home(), ".gemini")
}

func endpoint(env usage.Env) string {
	if e := env.Lookup("CODE_ASSIST_ENDPOINT"); e != "" {
		return strings.TrimRight(e, "/")
	}
	return DefaultEndpoint
}
