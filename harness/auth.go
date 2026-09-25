package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Auth is how a user signs an agent in with their own account, measured
// against the installed CLI. A host that runs agents on someone's machine
// (often over SSH, with no browser there) reads it to answer: can this user
// log in with their subscription here, how, are they already logged in, and
// which env var points the agent at a different account.
//
// Every entry was measured in a throwaway container on the version in
// MeasuredOn; no login was ever completed, so the logged-in side of a
// StatusCheck is read from the agent's help or source and not observed.
type Auth struct {
	// Methods are the ways to sign in, subscription logins first.
	Methods []AuthMethod

	// Status is a command that reports whether a login exists, or nil when
	// the agent has none.
	Status *StatusCheck

	// AccountDirEnv is the env var that selects which account the agent
	// uses, by relocating the directory its login is stored in. Empty when
	// no env var moves the credentials. It can differ from ConfigDirEnv:
	// opencode keeps config under XDG_CONFIG_HOME and logins under
	// XDG_DATA_HOME.
	AccountDirEnv string

	// CredentialPaths are where a login is stored, relative to $HOME, with
	// AccountDirEnv unset. They are for existence checks only: nothing may
	// read them. A path ending in "/" is a directory.
	CredentialPaths []string

	// AccountDirReplaces is the leading part of every CredentialPaths entry
	// that AccountDirEnv relocates: ".claude" for CLAUDE_CONFIG_DIR,
	// ".local/share" for XDG_DATA_HOME. Empty means the env var stands in for
	// $HOME itself (GEMINI_CLI_HOME, FACTORY_HOME_OVERRIDE).
	//
	// AccountDirValue is what that part becomes when the env var is set, a
	// template on its value: empty means the value itself (a directory);
	// "{{.Value}}/config" or a $HOME-relative ".omp/profiles/{{.Value}}/agent"
	// when the env var is not simply the directory.
	AccountDirReplaces string
	AccountDirValue    string

	// LoginFiles are the CredentialPaths entries that exist only once a
	// login has been saved: files the agent writes at login and not before.
	// A database or settings file every run creates is left out, as are
	// directories. CredentialsPresent stats these and nothing else.
	LoginFiles []string

	// Keyring: the agent stores its login in the OS secret store when one is
	// available (libsecret on Linux, Keychain on macOS), so CredentialPaths
	// may not exist even when the user is logged in.
	Keyring bool

	// MeasuredOn is the CLI version this was measured on, and Source where
	// the facts came from (help text, package source, ACP authMethods, an
	// observed login prompt).
	MeasuredOn string
	Source     string

	// Note is anything the fields above cannot say.
	Note string
}

// AuthKind is what a sign-in method authenticates with.
type AuthKind string

const (
	// AuthSubscriptionOAuth signs in to the user's account with the vendor
	// (Claude Pro/Max, ChatGPT, GitHub Copilot, Google, ...), billed to their
	// plan.
	AuthSubscriptionOAuth AuthKind = "subscription-oauth"
	// AuthSubscriptionToken is a long-lived token minted from a subscription
	// login elsewhere and handed to the agent by env var or stdin.
	AuthSubscriptionToken AuthKind = "subscription-token"
	// AuthDeviceCode is an account login that only runs as a device-code
	// flow: the agent prints a URL and code, the user approves on any
	// device.
	AuthDeviceCode AuthKind = "device-code"
	// AuthAPIKey is a pay-per-use API key.
	AuthAPIKey AuthKind = "api-key"
	// AuthProviderConfig: the agent has no account of its own; it is pointed
	// at a model provider in its config (which may itself be a subscription
	// login, see Subscription).
	AuthProviderConfig AuthKind = "provider-config"
	// AuthCloudSSO is a cloud or enterprise identity: Vertex AI, AWS IAM
	// Identity Center, Bedrock, an enterprise SSO.
	AuthCloudSSO AuthKind = "cloud-sso"
)

// AuthHeadless is how a method completes on a machine with no browser.
type AuthHeadless string

const (
	// HeadlessNone: the method needs a browser on the same machine (a
	// localhost callback), or a TUI with no headless path.
	HeadlessNone AuthHeadless = "none"
	// HeadlessDeviceCode: the agent prints a URL and a code; the user
	// approves on any device and the agent polls until it is done.
	HeadlessDeviceCode AuthHeadless = "device-code"
	// HeadlessPasteToken: the agent prints a URL; the user signs in on any
	// device and pastes the code or token it shows back into the agent.
	HeadlessPasteToken AuthHeadless = "paste-token"
	// HeadlessEnvToken: the credential goes in an env var (or stdin), no
	// interactive login at all.
	HeadlessEnvToken AuthHeadless = "env-token"
	// HeadlessBrowserElsewhere: the agent prints a URL to open on another
	// machine, and the login completes server side without anything coming
	// back to the terminal.
	HeadlessBrowserElsewhere AuthHeadless = "needs-browser-elsewhere"
)

// AuthMethod is one way to sign an agent in.
type AuthMethod struct {
	Kind AuthKind

	// Subscription names the plan this bills to ("Claude Pro/Max",
	// "ChatGPT Plus/Pro"), empty for pay-per-use and cloud identities.
	Subscription string

	Headless AuthHeadless

	// Command runs the login, EnvVars carry the credential instead of one.
	// Either may be empty; an env-token method has EnvVars and usually no
	// Command.
	Command []string
	EnvVars []string

	// KeyEnv are the EnvVars that sign the agent in on their own: one set
	// to a non-empty value is a credential present, which CredentialsPresent
	// reports. Filled only where measured, and never with a name other
	// tools share (GH_TOKEN), since it being set says nothing about this
	// agent.
	KeyEnv []string

	// ACPMethodID is the id the agent returns in ACP initialize's
	// authMethods for this method, when it has one.
	ACPMethodID string

	Note string
}

// Subscriptions reports whether any method signs in with a subscription.
func (a Auth) Subscriptions() bool {
	for _, m := range a.Methods {
		if m.Subscription != "" {
			return true
		}
	}
	return false
}

// HeadlessSubscription returns the first subscription method that completes
// without a browser on this machine, and false when there is none.
func (a Auth) HeadlessSubscription() (AuthMethod, bool) {
	for _, m := range a.Methods {
		if m.Subscription != "" && m.Headless != HeadlessNone && m.Headless != "" {
			return m, true
		}
	}
	return AuthMethod{}, false
}

// StatusCheck is a command that says whether the agent has a login.
//
// Only commands whose output never carries a credential are recorded. The
// logged-out side was measured; the logged-in side is taken from the
// agent's help or source, since no login was ever completed.
type StatusCheck struct {
	Cmd []string

	// LoggedOutExit and LoggedOutContains are what the command returns with
	// no login: its exit code, and a substring of its combined output.
	// LoggedInExit is the exit code with a login, from source or help.
	LoggedOutExit     int
	LoggedOutContains string
	LoggedInExit      int
	// LoggedOutAnyExit: the logged-out marker decides on its own, whatever
	// the exit code (droid's doctor exits 1 offline and 0 online).
	LoggedOutAnyExit bool

	// Providers: the agent keeps one login per model provider and the check
	// asks about one at a time. Cmd then holds "{{.Provider}}", and
	// CheckLogin reports logged in when any listed provider is.
	Providers []string

	// Output describes what it prints in each state.
	Output string

	// Cheap: measured to answer with no network (it gave the same answer
	// with networking disabled), no prompt, no file writes, in well under
	// StatusTimeout. Only a cheap check is safe to run on every enrollment.
	Cheap bool

	// OutputHasSecret: some state prints part of a credential (codex masks
	// an API key to its first 8 and last 5 characters). CheckLogin then
	// returns the state and exit code and drops the output.
	OutputHasSecret bool

	// MinVersion is the earliest version of the agent this command is known
	// to be a status check on. Below it CheckLogin runs nothing: an older pi
	// has no `auth check` and takes the words as a prompt, so the "check"
	// would start a billed model turn. It is the version the command appeared
	// in where the agent's changelog says, else the version it was measured on.
	MinVersion string

	// Note is how it was measured, and any caveat.
	Note string
}

// LoginState is what a status check concluded.
type LoginState string

const (
	LoginUnknown   LoginState = "unknown"
	LoginLoggedIn  LoginState = "logged-in"
	LoginLoggedOut LoginState = "logged-out"
)

// StatusTimeout bounds a status check.
var StatusTimeout = 5 * time.Second

// ErrNoStatusCheck is returned for an agent with no status command.
var ErrNoStatusCheck = errors.New("harness: agent has no auth status command")

// ErrStatusCheckUnsupported is returned when the installed agent is older
// than the status check's MinVersion, or its version cannot be read. Nothing
// was run.
var ErrStatusCheckUnsupported = errors.New("harness: installed agent version does not support its auth status command")

// StatusResult is one run of a StatusCheck.
type StatusResult struct {
	State    LoginState
	ExitCode int
	Output   string
}

// CheckLogin runs the agent's status command, bounded by StatusTimeout,
// with env appended to the current environment (an AccountDirEnv=... entry
// selects the account to ask about). It reports LoginUnknown when the
// output matches neither state, and drops the output of a check marked
// OutputHasSecret.
func CheckLogin(ctx context.Context, name string, env ...string) (StatusResult, error) {
	h, ok := All[name]
	if !ok || h.Auth.Status == nil {
		return StatusResult{State: LoginUnknown}, ErrNoStatusCheck
	}
	return CheckLoginVersion(ctx, name, getVersion(h.Auth.Status.Cmd[0]), env...)
}

// CheckLoginVersion is CheckLogin for a caller that already knows the
// installed version (DetectResult.Version), which saves a --version run.
// It runs nothing and returns ErrStatusCheckUnsupported when version is
// below the check's MinVersion or cannot be read.
func CheckLoginVersion(ctx context.Context, name, version string, env ...string) (StatusResult, error) {
	h, ok := All[name]
	if !ok || h.Auth.Status == nil {
		return StatusResult{State: LoginUnknown}, ErrNoStatusCheck
	}
	s := *h.Auth.Status
	if s.MinVersion != "" && !(VersionRange{From: s.MinVersion}).Contains(version) {
		return StatusResult{State: LoginUnknown}, ErrStatusCheckUnsupported
	}
	if len(s.Providers) == 0 {
		return s.run(ctx, s.Cmd, env)
	}
	// One run per provider, all at once: each is a separate process start
	// (pi's is a Node start per provider, about a second each), and run in
	// turn five of them outlast any enrollment budget. The first logged-in
	// answer wins and cancels the rest; otherwise the result is logged out
	// only if every provider said so.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		r   StatusResult
		err error
	}
	answers := make(chan answer, len(s.Providers))
	for _, p := range s.Providers {
		cmd := make([]string, len(s.Cmd))
		for i, a := range s.Cmd {
			cmd[i] = strings.ReplaceAll(a, "{{.Provider}}", p)
		}
		go func() {
			r, err := s.run(ctx, cmd, env)
			answers <- answer{r, err}
		}()
	}
	var last StatusResult
	var firstErr error
	allOut := true
	for range s.Providers {
		a := <-answers
		if a.err == nil && a.r.State == LoginLoggedIn {
			return a.r, nil
		}
		if a.err != nil {
			if firstErr == nil {
				firstErr = a.err
			}
			allOut = false
			continue
		}
		allOut = allOut && a.r.State == LoginLoggedOut
		last = a.r
	}
	if firstErr != nil {
		return StatusResult{State: LoginUnknown}, firstErr
	}
	if !allOut {
		last.State = LoginUnknown
	}
	return last, nil
}

func (s StatusCheck) run(ctx context.Context, args []string, env []string) (StatusResult, error) {
	ctx, cancel := context.WithTimeout(ctx, StatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 500 * time.Millisecond
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	res := StatusResult{State: LoginUnknown, Output: strings.TrimSpace(buf.String())}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr) && ctx.Err() == nil:
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, err
	}
	res.State = s.classify(res.ExitCode, res.Output)
	if s.OutputHasSecret {
		res.Output = ""
	}
	return res, nil
}

func (s StatusCheck) classify(code int, out string) LoginState {
	hasMarker := strings.Contains(out, s.LoggedOutContains)
	switch {
	case hasMarker && (code == s.LoggedOutExit || s.LoggedOutAnyExit):
		return LoginLoggedOut
	case code == s.LoggedInExit && !hasMarker:
		return LoginLoggedIn
	}
	return LoginUnknown
}
