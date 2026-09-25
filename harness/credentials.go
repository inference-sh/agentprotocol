package harness

import (
	"os"
	"path/filepath"
	"strings"
)

// CredentialsPresent reports whether a credential the agent would use is
// there: an AuthMethod.KeyEnv variable set in env, or one of its login files
// (Auth.LoginFiles) for the account env selects. It returns where it found
// it: "$NAME" for a variable, else the path. Nothing is opened or read, and
// no variable's value is returned.
//
// env is read like CheckLogin's: entries appended to the current
// environment, the last one for a name winning. HOME and Auth.AccountDirEnv
// (CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_DATA_HOME, OMP_PROFILE, ...) are
// honoured, so the answer is about the account the agent would use with
// that environment.
//
// It is a weaker signal than a status check. A file that exists may hold an
// expired or revoked token, and one that does not exist says nothing when
// the agent keeps its login in the OS keyring (Auth.Keyring) or takes it
// from an env var not in KeyEnv. A KeyEnv variable that is set may hold a
// bad key. With no KeyEnv set it returns false for an agent with no
// LoginFiles (copilot, kiro and omp keep their logins in a directory,
// database or keyring whose existence proves nothing). Use a Cheap status check first
// where the agent has one.
func CredentialsPresent(name string, env ...string) (bool, string) {
	h, ok := All[name]
	if !ok {
		return false, ""
	}
	a := h.Auth
	for _, m := range a.Methods {
		for _, k := range m.KeyEnv {
			if lookupEnv(env, k) != "" {
				return true, "$" + k
			}
		}
	}
	home := lookupEnv(env, "HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return false, ""
	}
	for _, rel := range a.LoginFiles {
		p := credentialPath(a, rel, home, env)
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return true, p
		}
	}
	return false, ""
}

// credentialPath resolves a $HOME-relative credential path for the account
// the env selects.
func credentialPath(a Auth, rel, home string, env []string) string {
	if a.AccountDirEnv == "" {
		return filepath.Join(home, rel)
	}
	val := lookupEnv(env, a.AccountDirEnv)
	if val == "" {
		return filepath.Join(home, rel)
	}
	base := val
	if a.AccountDirValue != "" {
		base = strings.ReplaceAll(a.AccountDirValue, "{{.Value}}", val)
	}
	if !filepath.IsAbs(base) {
		if a.AccountDirValue == "" || strings.HasPrefix(a.AccountDirValue, "{{.Value}}") {
			// A directory env var with a relative value: XDG and goose ignore
			// it, and guessing a working directory would stat the wrong file.
			return filepath.Join(home, rel)
		}
		base = filepath.Join(home, base)
	}
	if a.AccountDirReplaces == "" {
		return filepath.Join(base, rel)
	}
	prefix := a.AccountDirReplaces + "/"
	if !strings.HasPrefix(rel, prefix) {
		return filepath.Join(home, rel)
	}
	return filepath.Join(base, strings.TrimPrefix(rel, prefix))
}

// lookupEnv returns name's value from env (the last entry wins), else from
// the process environment.
func lookupEnv(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], name+"="); ok {
			return v
		}
	}
	return os.Getenv(name)
}
