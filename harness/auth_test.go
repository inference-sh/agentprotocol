package harness

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var authKinds = map[AuthKind]bool{
	AuthSubscriptionOAuth: true, AuthSubscriptionToken: true, AuthDeviceCode: true,
	AuthAPIKey: true, AuthProviderConfig: true, AuthCloudSSO: true,
}

var authHeadless = map[AuthHeadless]bool{
	HeadlessNone: true, HeadlessDeviceCode: true, HeadlessPasteToken: true,
	HeadlessEnvToken: true, HeadlessBrowserElsewhere: true,
}

// Every CLI agent carries a measured Auth; windsurf is IDE-only and has none.
func TestAuthRecordedForEveryCLIAgent(t *testing.T) {
	for name, h := range All {
		if name == "windsurf" {
			if len(h.Auth.Methods) > 0 {
				t.Errorf("windsurf: IDE-only, expected no Auth")
			}
			continue
		}
		a := h.Auth
		if len(a.Methods) == 0 {
			t.Errorf("%s: no Auth.Methods", name)
		}
		if a.MeasuredOn == "" || a.Source == "" {
			t.Errorf("%s: Auth needs MeasuredOn and Source", name)
		}
	}
}

func TestAuthWellFormed(t *testing.T) {
	for name, h := range All {
		a := h.Auth
		for i, m := range a.Methods {
			if !authKinds[m.Kind] {
				t.Errorf("%s: method %d: unknown Kind %q", name, i, m.Kind)
			}
			if !authHeadless[m.Headless] {
				t.Errorf("%s: method %d: unknown Headless %q", name, i, m.Headless)
			}
			if len(m.Command) == 0 && len(m.EnvVars) == 0 && m.Note == "" {
				t.Errorf("%s: method %d: no Command, EnvVars or Note says how to use it", name, i)
			}
			if len(m.Command) > 0 && m.Command[0] != h.Binary {
				t.Errorf("%s: method %d: Command runs %q, binary is %q", name, i, m.Command[0], h.Binary)
			}
			if m.Headless == HeadlessEnvToken && len(m.EnvVars) == 0 && m.Kind != AuthProviderConfig && m.Kind != AuthCloudSSO {
				t.Errorf("%s: method %d: env-token with no EnvVars", name, i)
			}
			if m.Kind == AuthSubscriptionOAuth || m.Kind == AuthSubscriptionToken {
				if m.Subscription == "" {
					t.Errorf("%s: method %d: %s without Subscription", name, i, m.Kind)
				}
			}
			if m.Kind == AuthAPIKey && m.Subscription != "" {
				t.Errorf("%s: method %d: api-key names a Subscription", name, i)
			}
			for _, v := range m.EnvVars {
				if v == "" || strings.ToUpper(v) != v || strings.ContainsAny(v, " =$") {
					t.Errorf("%s: method %d: bad env var name %q", name, i, v)
				}
			}
		}
		for _, p := range a.CredentialPaths {
			if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "~") || strings.Contains(p, "..") {
				t.Errorf("%s: CredentialPaths %q must be relative to $HOME", name, p)
			}
		}
		if a.AccountDirEnv != "" && strings.ToUpper(a.AccountDirEnv) != a.AccountDirEnv {
			t.Errorf("%s: AccountDirEnv %q is not an env var name", name, a.AccountDirEnv)
		}
		if s := a.Status; s != nil {
			if len(s.Cmd) == 0 || s.Cmd[0] != h.Binary {
				t.Errorf("%s: Status.Cmd %v must run %q", name, s.Cmd, h.Binary)
			}
			if s.LoggedOutContains == "" || s.Output == "" {
				t.Errorf("%s: Status needs LoggedOutContains and Output", name)
			}
			if s.Cheap && s.OutputHasSecret {
				t.Errorf("%s: a check whose output can carry a secret is not Cheap", name)
			}
			hasPlaceholder := strings.Contains(strings.Join(s.Cmd, " "), "{{.Provider}}")
			if hasPlaceholder != (len(s.Providers) > 0) {
				t.Errorf("%s: Status.Cmd has {{.Provider}} exactly when Providers is set", name)
			}
			if s.Cheap && s.Note == "" {
				t.Errorf("%s: a Cheap check needs a Note saying how it was measured", name)
			}
		}
	}
}

// APIKeyEnvVar is the mock's key and must keep working alongside Auth.
func TestAPIKeyEnvVarUnchanged(t *testing.T) {
	for name, want := range map[string]string{
		"claude": "ANTHROPIC_API_KEY", "codex": "OPENAI_API_KEY", "cursor": "CURSOR_API_KEY",
	} {
		if got := All[name].APIKeyEnvVar; got != want {
			t.Errorf("%s: APIKeyEnvVar = %q, want %q", name, got, want)
		}
	}
}

func TestHeadlessSubscription(t *testing.T) {
	m, ok := All["codex"].Auth.HeadlessSubscription()
	if !ok || m.Headless != HeadlessDeviceCode {
		t.Errorf("codex: HeadlessSubscription = %+v, %v; want the device-code login", m, ok)
	}
	if _, ok := All["hermes"].Auth.HeadlessSubscription(); ok && !All["hermes"].Auth.Subscriptions() {
		t.Error("hermes: headless subscription without any subscription")
	}
}

func TestStatusClassify(t *testing.T) {
	codex := *All["codex"].Auth.Status
	cursor := *All["cursor"].Auth.Status
	for _, c := range []struct {
		s    StatusCheck
		code int
		out  string
		want LoginState
	}{
		{codex, 1, "Not logged in", LoginLoggedOut},
		{codex, 0, "Logged in using ChatGPT", LoginLoggedIn},
		{codex, 2, "error: config", LoginUnknown},
		{cursor, 0, `{"status": "unauthenticated", "isAuthenticated": false}`, LoginLoggedOut},
		{cursor, 0, `{"status": "authenticated", "isAuthenticated": true}`, LoginLoggedIn},
		{cursor, 1, `boom`, LoginUnknown},
		{*All["droid"].Auth.Status, 1, `"detail": "no usable credentials found (not logged in)"`, LoginLoggedOut},
	} {
		if got := c.s.classify(c.code, c.out); got != c.want {
			t.Errorf("%v exit %d %q: got %s, want %s", c.s.Cmd, c.code, c.out, got, c.want)
		}
	}
}

func TestCheckLoginWithoutStatus(t *testing.T) {
	if _, err := CheckLogin(context.Background(), "copilot"); !errors.Is(err, ErrNoStatusCheck) {
		t.Errorf("copilot: err = %v, want ErrNoStatusCheck", err)
	}
	if _, err := CheckLogin(context.Background(), "no-such-agent"); !errors.Is(err, ErrNoStatusCheck) {
		t.Errorf("unknown agent: err = %v, want ErrNoStatusCheck", err)
	}
}
