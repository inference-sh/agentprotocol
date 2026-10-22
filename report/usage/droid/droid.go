// Package droid reads Factory Droid's account and plan limits. Droid reports
// neither over ACP, so Prober reads them the way the CLI does: from
// FACTORY_API_KEY or the login droid saved, through Factory's API
// (/api/cli/whoami for the organization and region, /api/billing/limits for
// the five-hour, weekly and monthly windows). A Factory API key belongs to an
// organization and draws on its plan, so it has the same limits.
package droid

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
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

// Factory's API, global and EU. FACTORY_API_BASE_URL and
// FACTORY_API_BASE_URL_EU in the agent's environment move them, as they do
// for the CLI.
const (
	DefaultAPI   = "https://api.factory.ai"
	DefaultAPIEU = "https://api.eu.factory.ai"
	whoamiPath   = "/api/cli/whoami"
	limitsPath   = "/api/billing/limits"
)

// The credential files under ~/.factory. auth.v2.file is the login sealed
// with the key in auth.v2.key; auth.v2.keyring and auth.v2.loginkeychain are
// sealed with a key held in the system keyring.
const (
	keyfileCreds  = "auth.v2.file"
	keyfileKey    = "auth.v2.key"
	keyringCreds  = "auth.v2.keyring"
	keychainCreds = "auth.v2.loginkeychain"
)

// ProbeTimeout bounds a probe: two API calls.
const ProbeTimeout = 20 * time.Second

// Prober reads the account and the plan's windows. It never starts the CLI
// and never calls a model. It does not refresh an expired login: Factory's
// refresh tokens are single-use, so a refresh the CLI did not save would sign
// the CLI out.
type Prober struct {
	// Client makes the requests; nil uses http.DefaultClient.
	Client *http.Client
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	now := time.Now()

	acct := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription}
	snap := ap.UsageSnapshot{Account: &acct}
	c := api{client: p.Client, global: apiBase(env, "FACTORY_API_BASE_URL", DefaultAPI)}
	if key := strings.TrimSpace(env.Lookup("FACTORY_API_KEY")); key != "" {
		acct.Type = ap.AccountAPIKey
		c.token = key
	} else {
		creds, err := ReadCredentials(env)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return ap.UsageSnapshot{Account: &ap.AgentAccount{Status: ap.AccountUnauthenticated}}, nil
		case errors.Is(err, errKeyring):
			snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
			return snap, nil
		case err != nil:
			return ap.UsageSnapshot{}, err
		}
		claims := tokenClaims(creds.AccessToken)
		acct.Email, acct.Workspace = claims.Email, claims.orgID()
		if claims.Exp != 0 && !time.Unix(int64(claims.Exp), 0).After(now) {
			snap.Limits = usage.Unavailable(ap.UsageProbeFailed, "droid: the saved login has expired; run droid to refresh it", now)
			return snap, nil
		}
		c.token, c.org = creds.AccessToken, strings.TrimSpace(creds.ActiveOrganizationID)
	}

	var who Whoami
	if err := c.get(ctx, c.global, whoamiPath, &who); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	if who.OrgID != "" {
		acct.Workspace = who.OrgID
	}
	if who.IsEnterprise {
		acct.Plan, acct.PlanLabel = "enterprise", "Enterprise"
	}
	base := c.global
	if who.Region == "eu" {
		base = apiBase(env, "FACTORY_API_BASE_URL_EU", DefaultAPIEU)
	}
	var limits BillingLimits
	if err := c.get(ctx, base, limitsPath, &limits); err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	ws := Windows(limits, now)
	if len(ws) == 0 {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "the organization's plan has no usage windows", now)
		return snap, nil
	}
	snap.Limits = usage.Limits(ws, now)
	return snap, nil
}

// Windows maps the plan's pools (Standard, and Droid Core when the plan has
// it) and each pool's five-hour, weekly and monthly windows. Factory leaves a
// lapsed window at its last reading until the next request opens a new one;
// like the CLI, a window whose end has passed reads as unused.
func Windows(r BillingLimits, now time.Time) []ap.UsageWindow {
	if r.UsesTokenRateLimitsBilling != nil && !*r.UsesTokenRateLimitsBilling {
		return nil
	}
	var ws []ap.UsageWindow
	for _, pool := range []struct {
		p           *Pool
		idPrefix    string
		labelPrefix string
	}{
		{r.Limits.Standard, "", ""},
		{r.Limits.Core, "core_", "Droid Core "},
	} {
		if pool.p == nil {
			continue
		}
		for _, def := range []struct {
			b       *Bucket
			id      string
			label   string
			kind    ap.UsageWindowKind
			minutes int
		}{
			{pool.p.FiveHour, "five_hour", "5-hour", ap.UsageWindowSession, 5 * 60},
			{pool.p.Weekly, "weekly", "Weekly", ap.UsageWindowWeekly, 7 * 24 * 60},
			{pool.p.Monthly, "monthly", "Monthly", ap.UsageWindowMonthly, 0},
		} {
			if def.b == nil {
				continue
			}
			w := ap.UsageWindow{ID: pool.idPrefix + def.id, Kind: def.kind, Label: pool.labelPrefix + def.label, DurationMinutes: def.minutes}
			if end, err := time.Parse(time.RFC3339, def.b.WindowEnd); err == nil && !end.Before(now) {
				w.UsedPercent, w.ResetsAt = def.b.UsedPercent, end.UTC()
			}
			ws = append(ws, w)
		}
	}
	return ws
}

// Factory's API shapes, the fields a probe reads.
type (
	Whoami struct {
		UserID       string `json:"userId"`
		OrgID        string `json:"orgId"`
		Region       string `json:"region,omitempty"` // "global" or "eu"
		IsEnterprise bool   `json:"isEnterprise,omitempty"`
	}
	BillingLimits struct {
		Limits struct {
			Standard *Pool `json:"standard,omitempty"`
			Core     *Pool `json:"core,omitempty"`
		} `json:"limits"`
		UsesTokenRateLimitsBilling *bool  `json:"usesTokenRateLimitsBilling,omitempty"`
		ExtraUsageBalanceCents     int64  `json:"extraUsageBalanceCents,omitempty"`
		OveragePreference          string `json:"overagePreference,omitempty"`
	}
	Pool struct {
		FiveHour *Bucket `json:"fiveHour,omitempty"`
		Weekly   *Bucket `json:"weekly,omitempty"`
		Monthly  *Bucket `json:"monthly,omitempty"`
	}
	Bucket struct {
		UsedPercent float64 `json:"usedPercent"`
		WindowEnd   string  `json:"windowEnd,omitempty"` // RFC 3339
	}
)

// Credentials is the login droid saved, decrypted.
type Credentials struct {
	AccessToken          string `json:"access_token"`
	RefreshToken         string `json:"refresh_token"`
	ActiveOrganizationID string `json:"active_organization_id,omitempty"`
}

var errKeyring = errors.New("droid: the login is in the system keyring")

// ReadCredentials is the login droid saved, as the CLI finds it: in the
// keyring-sealed file unless FACTORY_DISABLE_KEYRING is set, else in
// auth.v2.file, sealed with auth.v2.key. A login sealed with a keyring key is
// out of reach and reads as errKeyring. Without a login, or with one the CLI
// could not open either, the error is os.ErrNotExist.
func ReadCredentials(env usage.Env) (Credentials, error) {
	dir := factoryHome(env)
	if !truthy(env.Lookup("FACTORY_DISABLE_KEYRING")) {
		for _, name := range []string{keyringCreds, keychainCreds} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return Credentials{}, errKeyring
			}
		}
	}
	sealed, err := os.ReadFile(filepath.Join(dir, keyfileCreds))
	if err != nil {
		return Credentials{}, err
	}
	rawKey, err := os.ReadFile(filepath.Join(dir, keyfileKey))
	if err != nil {
		return Credentials{}, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(rawKey)))
	if err != nil || len(key) != 32 {
		return Credentials{}, os.ErrNotExist
	}
	plain, err := open(string(sealed), key)
	if err != nil {
		return Credentials{}, os.ErrNotExist
	}
	var c Credentials
	if err := json.Unmarshal(plain, &c); err != nil || c.AccessToken == "" {
		return Credentials{}, os.ErrNotExist
	}
	return c, nil
}

// open decrypts the CLI's sealed format: base64 of a 16-byte IV, the
// 16-byte GCM tag and the ciphertext, joined by colons, under AES-256-GCM.
func open(sealed string, key []byte) ([]byte, error) {
	parts := strings.Split(strings.TrimSpace(sealed), ":")
	if len(parts) != 3 {
		return nil, errors.New("droid: not a sealed credential")
	}
	var b [3][]byte
	for i, p := range parts {
		v, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return nil, err
		}
		b[i] = v
	}
	iv, tag, ct := b[0], b[1], b[2]
	if len(iv) != 16 || len(tag) != 16 {
		return nil, errors.New("droid: bad IV or tag length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, append(ct, tag...), nil)
}

// claims is the WorkOS access token's payload, the fields a probe reads.
type claims struct {
	Email         string  `json:"email"`
	OrgID         string  `json:"org_id"`
	ExternalOrgID string  `json:"external_org_id"`
	Exp           float64 `json:"exp"`
}

func (c claims) orgID() string {
	if c.ExternalOrgID != "" {
		return c.ExternalOrgID
	}
	return c.OrgID
}

func tokenClaims(jwt string) claims {
	var c claims
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return c
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err == nil {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

type api struct {
	client *http.Client
	global string
	token  string
	org    string
}

func (a api) get(ctx context.Context, base, path string, out any) error {
	h := map[string]string{"Authorization": "Bearer " + a.token, "X-Factory-Client": "cli"}
	if path == whoamiPath {
		h["X-Factory-Whoami-Extended"] = "true"
	}
	if a.org != "" {
		h["X-Factory-Org-Id"] = a.org
	}
	if err := webjson.Get(ctx, a.client, base+path, h, out); err != nil {
		return fmt.Errorf("droid: %s: %w", path, err)
	}
	return nil
}

// factoryHome is the CLI's .factory directory: under FACTORY_HOME_OVERRIDE
// when it is set, else under HOME.
func factoryHome(env usage.Env) string {
	if base := env.Lookup("FACTORY_HOME_OVERRIDE"); base != "" {
		return filepath.Join(base, ".factory")
	}
	return filepath.Join(env.Home(), ".factory")
}

func apiBase(env usage.Env, key, def string) string {
	if e := env.Lookup(key); e != "" {
		return strings.TrimRight(e, "/")
	}
	return def
}

func truthy(v string) bool {
	v = strings.ToLower(v)
	return v == "1" || v == "true"
}
