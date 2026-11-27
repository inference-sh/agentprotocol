package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/harness"
)

// beltHookChoices is what belt asks of each event: a copy of the Hooks map
// in the CLI's internal/beltplugin/source.go, which this module cannot
// import. PreToolUse is for the skill tool only and must not be installed
// where it cannot be filtered (Strict); PostToolUse is for the shell tool,
// where belt commands run.
var beltHookChoices = map[harness.HookEvent]harness.HookSpec{
	harness.SessionStart: {Timeout: 60 * time.Second},
	harness.PromptSubmit: {Timeout: 10 * time.Second},
	harness.PreToolUse:   {Tools: []harness.Tool{harness.ToolSkill}, Timeout: 10 * time.Second, Strict: true},
	harness.PostToolUse:  {Tools: []harness.Tool{harness.ToolShell}, Timeout: 10 * time.Second},
	harness.Stop:         {Timeout: 60 * time.Second, Async: true},
	harness.PreCompact:   {Timeout: 60 * time.Second, Async: true},
	harness.SessionEnd:   {Timeout: 600 * time.Second},
}

// shellCalls is the shell call for agents whose gated tool is not their
// shell (claude's is Edit).
var shellCalls = map[string]harness.ToolCall{
	"claude": {Name: "Bash", Args: `{"command":"rm -rf /tmp/gated-probe-does-not-exist"}`},
}

// probeMatchers installs this suite's hook scripts with belt's hook choices,
// so the agent's hook file carries exactly the tool filters belt's does, and
// asks whether the agent honours them: a shell call fires the post-tool hook
// and not the pre-tool one, and an ordinary call fires the post-tool hook
// only where the filter could not be written (the library's render notes say
// which). It answers, from a live agent, what the generated hook files claim:
// codex's PostToolUse matcher "Bash" catching exec_command, for one.
func (r *TestRunner) probeMatchers() {
	h := r.harness
	if h.DriverKind() == "" || r.server == nil || h.Events.PostToolUse == "" {
		return
	}
	fmt.Println("[probe] matchers (belt's tool filters on a shell call and on another call)")
	shell, ok := shellCalls[h.Name]
	if !ok {
		shell = h.ToolCallGated
	}
	if shell.Name == "" {
		r.skip("matchers:no-shell-call", "matchers: the registry names no shell call for "+h.Name)
		return
	}
	// A command whose output its own text does not contain, so a tool result
	// holding the output proves the call ran: a call the agent refused or
	// asked permission for has no post-tool event to fire.
	shellArgs := strings.ReplaceAll(shell.Args, "rm -rf /tmp/gated-probe-does-not-exist", "echo matcher-probe-$((40+2))")
	src := r.mockSource()
	src.Hooks = beltHookChoices
	_, notes, err := harness.RenderHooks(h.Name, src)
	if err != nil {
		r.fail("matchers.render", "matchers: "+err.Error())
		return
	}
	preInstalled, postFiltered := h.Events.PreToolUse != "", true
	var noteText []string
	for _, n := range notes {
		noteText = append(noteText, fmt.Sprintf("%s: %s", n.Event, n.Detail))
		switch {
		case n.Event == harness.PreToolUse && strings.Contains(n.Detail, "not installed"):
			preInstalled = false
		case n.Event == harness.PostToolUse && strings.Contains(n.Detail, "fires for every tool"):
			postFiltered = false
		}
	}
	res := harness.InstallSource(h.Name, harness.ScopeUser, src)
	if res.Error != nil {
		r.fail("matchers.install", "matchers: "+res.Error.Error())
		return
	}
	// writeHooks copies the hook file into the project for agents that run
	// in a repository, and that copy fires too: it has to carry the filters
	// as well, or an unfiltered project hook answers for the filtered one.
	if h.NeedsGitRepo {
		r.copyHooksToProject(res.HooksPath)
	}
	// The other probes run with the unfiltered hooks.
	defer func() {
		back := harness.InstallSource(h.Name, harness.ScopeUser, r.mockSource())
		if h.NeedsGitRepo && back.Error == nil {
			r.copyHooksToProject(back.HooksPath)
		}
	}()
	plan := fmt.Sprintf("pre-tool installed: %t, post-tool filtered: %t; notes: %s", preInstalled, postFiltered, strings.Join(noteText, "; "))

	pre, post, served := r.matcherTurn("matchers.shell", func() bool { return r.armTool(shell.Name, shellArgs) }, "matcher-probe-42")
	if served {
		if post {
			r.pass("matchers.shell.post-tool", fmt.Sprintf("matchers: %s's %s call fired the post-tool hook (%s)", h.Name, shell.Name, plan))
		} else {
			r.fail("matchers.shell.post-tool", fmt.Sprintf("matchers: %s's %s call did not fire the post-tool hook belt asks for on the shell (%s)", h.Name, shell.Name, plan))
		}
		if pre {
			r.finding("matchers.shell.pre-tool:fired", fmt.Sprintf("matchers: %s's %s call fired the pre-tool hook belt asks for on the skill tool only (%s)", h.Name, shell.Name, plan))
		} else {
			r.pass("matchers.shell.pre-tool", fmt.Sprintf("matchers: %s's %s call did not fire the skill-only pre-tool hook", h.Name, shell.Name))
		}
	}

	// The ordinary call, when it is not a shell call too (codex's is
	// exec_command).
	other := h.ToolCallName
	if o, ok := h.ToolCallByMode[ModeACP]; ok {
		other = o.Name
	}
	if other == "" {
		other = "Read"
	}
	if other == shell.Name {
		return
	}
	// The ordinary call reads README.md or writes test-output.txt; a token
	// in README.md shows a read ran, the written file shows a write did.
	token := "matcher-readme-" + randHex()
	readme := filepath.Join(r.workDir(), "README.md")
	if old, err := os.ReadFile(readme); err == nil {
		defer os.WriteFile(readme, old, 0o644)
	}
	os.WriteFile(readme, []byte(token+"\n"), 0o644)
	os.Remove(filepath.Join(r.workDir(), "test-output.txt"))
	pre, post, served = r.matcherTurn("matchers.other", func() bool { return r.armToolCall(ModeACP) }, token)
	if !served {
		return
	}
	switch {
	case post == !postFiltered:
		r.pass("matchers.other.post-tool", fmt.Sprintf("matchers: %s's %s call fired the post-tool hook: %t, as its filter says (%s)", h.Name, other, post, plan))
	case post:
		r.finding("matchers.other.post-tool:fired", fmt.Sprintf("matchers: %s's %s call fired the post-tool hook filtered to the shell (%s)", h.Name, other, plan))
	default:
		r.fail("matchers.other.post-tool", fmt.Sprintf("matchers: %s's %s call did not fire the post-tool hook, which has no filter here (%s)", h.Name, other, plan))
	}
	if pre {
		r.finding("matchers.other.pre-tool:fired", fmt.Sprintf("matchers: %s's %s call fired the skill-only pre-tool hook (%s)", h.Name, other, plan))
	}
}

// matcherTurn runs one turn with a tool call armed and reports which tool
// hooks fired, and whether the call was served at all.
func (r *TestRunner) matcherTurn(label string, arm func() bool, ranMark string) (pre, post, served bool) {
	r.resetPhase(ModeACP)
	if !arm() {
		r.skip(label+":no-tool", label+": nothing to arm")
		return
	}
	d, ok := r.startProbeTurn(label)
	if !ok {
		return
	}
	// Post-tool hooks run after the result; give them the time the phase
	// checks do.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !r.server.ToolCallServed() {
		time.Sleep(200 * time.Millisecond)
	}
	if r.server.ToolCallServed() {
		for time.Now().Before(deadline) && !r.postToolFired() {
			time.Sleep(200 * time.Millisecond)
		}
	}
	d.Close()
	if !r.server.ToolCallServed() {
		r.skip(label+":not-served", label+": the agent was never offered the call")
		return
	}
	ran := false
	for _, e := range r.entries() {
		for _, res := range toolResults(e.Body) {
			ran = ran || strings.Contains(res.text, ranMark)
		}
	}
	if _, err := os.Stat(filepath.Join(r.workDir(), "test-output.txt")); err == nil {
		ran = true
	}
	if !ran {
		r.skip(label+":not-run", label+": the call was served and the agent did not run it (refused or held for permission), so no post-tool event is due")
		return
	}
	data, _ := os.ReadFile(hookLogPath)
	log := string(data)
	return strings.Contains(log, TagPreTool), strings.Contains(log, TagPostTool), true
}
