package runner

import (
	"context"
	"fmt"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/harness"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// usageSettle is how long the probe waits after a turn for usage the agent
// reports once the turn is over: pi's context fill is read when the turn
// settles, codex's account and limits are read beside the session.
const usageSettle = 5 * time.Second

// probeUsage asks what the agent reports about usage, through its session
// driver and outside a session.
//
// In a session: one turn against the mock, whose every answer reports 10
// prompt and 5 completion tokens, and then whether the driver reported the
// turn's usage once, the context window's fill, the account, and rate
// limits. Outside one: the registry's prober, which reads the account and
// limits without a model call. The mock signs every agent in with an API key,
// so a plan's rate limits are not expected; an account read as anything but
// an API key, or limits that are not "unsupported", would be a mapping bug.
func (r *TestRunner) probeUsage() {
	if r.harness.DriverKind() == "" {
		return
	}
	fmt.Println("[probe] usage (tokens, context window, account, limits)")
	r.armToolCall(ModeACP)
	d, ok := r.startProbeTurn("usage")
	if !ok {
		return
	}
	deadline := time.Now().Add(usageSettle)
	for time.Now().Before(deadline) {
		u := d.Usage()
		if len(u.Turns) > 0 && len(u.Context) > 0 && (r.harness.Usage == nil || len(u.Accounts) > 0) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	u := d.Usage()
	d.Close()
	name := r.harness.Name

	switch {
	case len(u.Turns) == 0:
		r.finding("usage.turn:not-reported", fmt.Sprintf("usage: %s's driver reported no usage for the turn", name))
	case len(u.Turns) > 1:
		r.fail("usage.turn", fmt.Sprintf("usage: %s's driver reported the turn's usage %d times; it is once per turn", name, len(u.Turns)))
	default:
		t := u.Turns[0]
		switch {
		case t.TotalTokens <= 0 || t.PromptTokens <= 0 || t.CompletionTokens <= 0:
			r.fail("usage.turn", fmt.Sprintf("usage: %s's turn usage is empty: %+v", name, t))
		case t.CachedInputTokens+t.CacheCreationTokens > t.PromptTokens || t.ReasoningTokens > t.CompletionTokens || t.TotalTokens != t.PromptTokens+t.CompletionTokens:
			r.fail("usage.turn", fmt.Sprintf("usage: %s's turn usage breaks its own sums: %+v", name, t))
		default:
			r.pass("usage.turn", fmt.Sprintf("usage: %s reported the turn once: %d prompt, %d completion tokens", name, t.PromptTokens, t.CompletionTokens))
		}
	}

	if len(u.Context) == 0 {
		r.finding("usage.context:not-reported", fmt.Sprintf("usage: %s reported no context window fill", name))
	} else {
		c := u.Context[len(u.Context)-1]
		switch {
		case c.UsedTokens <= 0:
			r.fail("usage.context", fmt.Sprintf("usage: %s reported an empty context fill: %+v", name, c))
		case c.MaxTokens <= 0:
			r.pass("usage.context:no-window", fmt.Sprintf("usage: %s reported %d tokens in context and no window size", name, c.UsedTokens))
		case c.UsedTokens > c.MaxTokens:
			r.fail("usage.context", fmt.Sprintf("usage: %s reported more in context than its window holds: %+v", name, c))
		default:
			r.pass("usage.context:with-window", fmt.Sprintf("usage: %s reported %d of %d tokens in context", name, c.UsedTokens, c.MaxTokens))
		}
	}

	if len(u.Accounts) == 0 {
		r.finding("usage.account:not-reported", fmt.Sprintf("usage: %s's driver reported no account", name))
	} else {
		switch a := u.Accounts[len(u.Accounts)-1]; accountAnswer(a) {
		case answerAPIKey:
			r.pass("usage.account:api_key", r.accountLine("in a session", a))
		case answerSubscription:
			r.pass("usage.account:subscription", r.accountLine("in a session", a))
		case answerCloud:
			r.pass("usage.account:cloud", r.accountLine("in a session", a))
		case answerSignedIn:
			r.pass("usage.account:signed-in", r.accountLine("in a session", a))
		case answerSignedOut:
			r.fail("usage.account", r.accountLine("in a session", a))
		default:
			r.finding("usage.account:unknown", r.accountLine("in a session", a))
		}
	}
	if len(u.Limits) == 0 {
		r.finding("usage.limits:not-reported", fmt.Sprintf("usage: %s pushed no rate limits", name))
	} else {
		r.pass("usage.limits", fmt.Sprintf("usage: %s pushed %d rate-limit update(s)", name, len(u.Limits)))
	}

	r.probeUsageOutsideSession()
}

// accountAnswer classes an account. The mock signs every agent in with a key,
// so signed out is a mapping bug.
type accountClass int

const (
	answerUnknown accountClass = iota
	answerSignedOut
	answerSignedIn
	answerAPIKey
	answerSubscription
	answerCloud
)

func accountAnswer(a ap.AgentAccount) accountClass {
	switch {
	case a.Status == ap.AccountUnauthenticated:
		return answerSignedOut
	case a.Status != ap.AccountAuthenticated:
		return answerUnknown
	case a.Type == ap.AccountAPIKey:
		return answerAPIKey
	case a.Type == ap.AccountSubscription:
		return answerSubscription
	case a.Type == ap.AccountCloud:
		return answerCloud
	}
	return answerSignedIn
}

func (r *TestRunner) accountLine(where string, a ap.AgentAccount) string {
	return fmt.Sprintf("usage: %s %s reports %s, %s", r.harness.Name, where, a.Status, orNone(string(a.Type)))
}

// probeUsageOutsideSession runs the registry's prober, which must answer
// without a model call.
func (r *TestRunner) probeUsageOutsideSession() {
	name := r.harness.Name
	if r.harness.Usage == nil {
		r.skip("usage.probe:none", fmt.Sprintf("usage: %s has no prober in the registry; its login is the status check", name))
		return
	}
	served := r.server.AnswersServed()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	snap, err := r.harness.Usage.Probe(ctx, r.usageEnv())
	if err != nil {
		r.fail("usage.probe", fmt.Sprintf("usage: %s's prober failed: %v", name, err))
		return
	}
	if n := r.server.AnswersServed() - served; n > 0 {
		r.fail("usage.probe.no-model-call", fmt.Sprintf("usage: %s's prober made %d model call(s); it must read the account without one", name, n))
	} else {
		r.pass("usage.probe.no-model-call", fmt.Sprintf("usage: %s's prober made no model call", name))
	}
	if snap.Account == nil {
		r.fail("usage.probe.account", fmt.Sprintf("usage: %s's prober returned no account", name))
	} else {
		switch a := *snap.Account; accountAnswer(a) {
		case answerAPIKey:
			r.pass("usage.probe.account:api_key", r.accountLine("outside a session", a))
		case answerSubscription:
			r.pass("usage.probe.account:subscription", r.accountLine("outside a session", a))
		case answerCloud:
			r.pass("usage.probe.account:cloud", r.accountLine("outside a session", a))
		case answerSignedIn:
			r.pass("usage.probe.account:signed-in", r.accountLine("outside a session", a))
		case answerSignedOut:
			r.fail("usage.probe.account", r.accountLine("outside a session", a))
		default:
			r.finding("usage.probe.account:unknown", r.accountLine("outside a session", a))
		}
	}
	switch l := snap.Limits; {
	case l == nil:
		r.fail("usage.probe.limits", fmt.Sprintf("usage: %s's prober returned no limits and no reason", name))
	case l.Unavailable != nil && l.Unavailable.Reason == ap.UsageUnsupported:
		r.pass("usage.probe.limits:unsupported", fmt.Sprintf("usage: %s's limits do not apply: %s", name, l.Unavailable.Message))
	case l.Unavailable != nil:
		r.fail("usage.probe.limits", fmt.Sprintf("usage: %s's limits could not be read: %s", name, l.Unavailable.Message))
	default:
		r.pass("usage.probe.limits:windows", fmt.Sprintf("usage: %s's prober read %d window(s)", name, len(l.Windows)))
	}
}

// usageEnv runs the prober the way this suite's sessions run the agent
// (sessionBackend), so it reads the account they use.
func (r *TestRunner) usageEnv() usage.Env {
	dir := r.workDir()
	env := usage.Env{Binary: r.harness.Binary, Dir: dir, Env: r.envIn(dir)}
	if r.harness.DriverKind() == harness.DriverCodex {
		env.Args = r.codexProviderArgs()
	}
	return env
}
