package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolate points HOME at a temp dir and clears every account selector, so
// nothing on the machine running the test is found.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, h := range All {
		if e := h.Auth.AccountDirEnv; e != "" {
			t.Setenv(e, "")
		}
	}
	return home
}

// touch creates path with mode 000: CredentialsPresent must find it without
// being able to read it.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o000); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsPresentDefaultPaths(t *testing.T) {
	for name, h := range All {
		for _, f := range h.Auth.LoginFiles {
			t.Run(name+"/"+f, func(t *testing.T) {
				home := isolate(t)
				if ok, _ := CredentialsPresent(name); ok {
					t.Fatal("present in an empty home")
				}
				touch(t, filepath.Join(home, f))
				ok, p := CredentialsPresent(name)
				if !ok || p != filepath.Join(home, f) {
					t.Fatalf("got %v %q", ok, p)
				}
			})
		}
	}
}

func TestCredentialsPresentFollowsAccountEnv(t *testing.T) {
	for _, c := range []struct {
		name, env, val string
		file           string // relative to the temp dir; {{home}} marks $HOME
	}{
		{"claude", "CLAUDE_CONFIG_DIR", "{{tmp}}/work", "work/.credentials.json"},
		{"codex", "CODEX_HOME", "{{tmp}}/cx", "cx/auth.json"},
		{"cursor", "XDG_CONFIG_HOME", "{{tmp}}/xc", "xc/cursor/auth.json"},
		{"gemini", "GEMINI_CLI_HOME", "{{tmp}}/g", "g/.gemini/oauth_creds.json"},
		{"droid", "FACTORY_HOME_OVERRIDE", "{{tmp}}/d", "d/.factory/auth.v2.file"},
		{"opencode", "XDG_DATA_HOME", "{{tmp}}/xd", "xd/opencode/auth.json"},
		{"kilo", "XDG_DATA_HOME", "{{tmp}}/xd", "xd/kilo/auth.json"},
		{"pi", "PI_CODING_AGENT_DIR", "{{tmp}}/p", "p/auth.json"},
		{"goose", "GOOSE_PATH_ROOT", "{{tmp}}/gr", "gr/config/secrets.yaml"},
		{"kimi", "KIMI_CODE_HOME", "{{tmp}}/k", "k/credentials/kimi-code.json"},
		{"hermes", "HERMES_HOME", "{{tmp}}/hp", "hp/auth.json"},
		{"grok", "GROK_HOME", "{{tmp}}/gk", "gk/auth.json"},
		{"qwen", "QWEN_HOME", "{{tmp}}/q", "q/oauth_creds.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := isolate(t)
			tmp := t.TempDir()
			env := c.env + "=" + strings.ReplaceAll(c.val, "{{tmp}}", tmp)
			// The default-location login belongs to another account.
			for _, f := range All[c.name].Auth.LoginFiles {
				touch(t, filepath.Join(home, f))
			}
			if ok, p := CredentialsPresent(c.name, env); ok {
				t.Fatalf("found the default account's %s for %s", p, env)
			}
			want := filepath.Join(tmp, c.file)
			touch(t, want)
			if ok, p := CredentialsPresent(c.name, env); !ok || p != want {
				t.Fatalf("%s: got %v %q, want %q", env, ok, p, want)
			}
			// From the process environment too.
			t.Setenv(c.env, strings.ReplaceAll(c.val, "{{tmp}}", tmp))
			if ok, p := CredentialsPresent(c.name); !ok || p != want {
				t.Fatalf("process env: got %v %q", ok, p)
			}
		})
	}
}

// OMP_PROFILE names a profile under ~/.omp/profiles, not a directory; omp
// has no LoginFiles, so the path is checked through credentialPath.
func TestCredentialPathProfileName(t *testing.T) {
	home := "/home/u"
	got := credentialPath(All["omp"].Auth, ".omp/agent/agent.db", home, []string{"OMP_PROFILE=work"})
	if want := "/home/u/.omp/profiles/work/agent/agent.db"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A relative value for a directory env var is ignored, as XDG and goose do.
func TestCredentialsPresentRelativeAccountDir(t *testing.T) {
	home := isolate(t)
	touch(t, filepath.Join(home, ".local/share/opencode/auth.json"))
	if ok, _ := CredentialsPresent("opencode", "XDG_DATA_HOME=relative/dir"); !ok {
		t.Error("relative XDG_DATA_HOME should fall back to the default")
	}
}

func TestCredentialsPresentLastEnvWins(t *testing.T) {
	isolate(t)
	a, b := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(b, ".credentials.json"))
	if ok, _ := CredentialsPresent("claude", "CLAUDE_CONFIG_DIR="+a, "CLAUDE_CONFIG_DIR="+b); !ok {
		t.Error("the last CLAUDE_CONFIG_DIR entry should win")
	}
}

func TestCredentialsPresentIgnoresDirectoriesAndUnknown(t *testing.T) {
	home := isolate(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude/.credentials.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := CredentialsPresent("claude"); ok {
		t.Error("a directory is not a login file")
	}
	if err := os.MkdirAll(filepath.Join(home, ".copilot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := CredentialsPresent("copilot"); ok {
		t.Error("copilot has no login file; its directory proves nothing")
	}
	if ok, _ := CredentialsPresent("zz-not-registered"); ok {
		t.Error("unknown agent")
	}
}

func TestCredentialsPresentKeyEnv(t *testing.T) {
	isolate(t)
	t.Setenv("CURSOR_API_KEY", "")
	if ok, _ := CredentialsPresent("cursor"); ok {
		t.Error("no key and no login file")
	}
	if ok, _ := CredentialsPresent("cursor", "CURSOR_API_KEY="); ok {
		t.Error("an empty key is no credential")
	}
	ok, where := CredentialsPresent("cursor", "CURSOR_API_KEY=k")
	if !ok || where != "$CURSOR_API_KEY" {
		t.Errorf("got %v %q, want the key named, never its value", ok, where)
	}
}

func TestKeyEnvIsInEnvVars(t *testing.T) {
	for name, h := range All {
		for _, m := range h.Auth.Methods {
			for _, k := range m.KeyEnv {
				if !slices.Contains(m.EnvVars, k) {
					t.Errorf("%s: KeyEnv %s is not one of the method's EnvVars", name, k)
				}
			}
		}
	}
}

func TestLoginFilesAreCredentialPaths(t *testing.T) {
	for name, h := range All {
		a := h.Auth
		for _, f := range a.LoginFiles {
			found := false
			for _, p := range a.CredentialPaths {
				found = found || p == f
			}
			if !found || strings.HasSuffix(f, "/") {
				t.Errorf("%s: LoginFiles %q must be a file in CredentialPaths", name, f)
			}
		}
		if a.AccountDirEnv == "" {
			if a.AccountDirReplaces != "" || a.AccountDirValue != "" {
				t.Errorf("%s: AccountDir mapping without AccountDirEnv", name)
			}
			continue
		}
		for _, p := range a.CredentialPaths {
			if a.AccountDirReplaces != "" && !strings.HasPrefix(p, a.AccountDirReplaces+"/") {
				t.Errorf("%s: %q is not under AccountDirReplaces %q", name, p, a.AccountDirReplaces)
			}
		}
	}
}
