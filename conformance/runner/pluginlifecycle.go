package runner

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/harness"
)

// beltPluginSource is belt as the released CLI installs it: through the
// agent's plugin manager from the belt-sh/skills marketplace where the agent
// has one, as hook commands in the agent's hook file where it does not. The
// probe reads state with it; it never installs with it.
var beltPluginSource = harness.PluginSource{
	Name:            "belt",
	Marketplace:     "belt-sh/skills",
	MarketplaceName: "belt-sh-skills",
	HookCommand:     BeltSource.HookCommand,
	Owns:            BeltSource.Owns,
}

// foreignSource is a hook somebody else installed before belt. Install must
// add belt beside it and uninstall must leave it.
var foreignSource = harness.PluginSource{
	Name:        "foreign-probe",
	HookCommand: func(e harness.HookEvent) string { return "echo foreign-probe " + string(e) },
}

// probePluginLifecycle drives the released `belt plugin install`, `status`
// and `uninstall` against the real agent, in a home of its own, and judges
// each step from the agent's state on disk and its own plugin listing
// (harness.PluginState), never from what belt prints:
//
//   - install: belt is installed, and a hook another tool put there first is
//     still there;
//   - a second install leaves one copy of belt's hooks, not two;
//   - status says what the agent's state says;
//   - uninstall: belt is gone, the other hook is kept, and no file outside the
//     agent's plugin caches still runs `belt plugin hook`.
//
// Whether the installed hooks fire needs a model turn; --hooks belt runs
// cover that.
func (r *TestRunner) probePluginLifecycle() {
	name := r.harness.Name
	fmt.Println("[probe] plugin (belt plugin install, status, uninstall)")
	belt, err := exec.LookPath("belt")
	if err != nil {
		r.skip("plugin:no-belt", "plugin: no belt binary on PATH")
		return
	}
	home, err := os.MkdirTemp("", "plugin-"+name+"-")
	if err != nil {
		r.fail("plugin.setup", "plugin: "+err.Error())
		return
	}
	defer os.RemoveAll(home)
	// The library reads $HOME; so do belt and the agent's CLI.
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", oldHome)

	h := r.harness
	hookFile := ""
	if p := h.HookPath(beltPluginSource.Name); p != "" {
		hookFile = filepath.Join(home, p)
	}
	foreign := false
	if hookFile != "" {
		if res := harness.InstallSource(name, harness.ScopeUser, foreignSource); res.Error == nil {
			foreign = harness.SourceHooksInstalled(name, harness.ScopeUser, foreignSource)
		}
	}

	runBelt := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, belt, args...)
		cmd.Env = append(os.Environ(), "HOME="+home, "INFSH_NO_AUTOUPDATE=1", "TERM=dumb")
		cmd.Dir = home
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := runBelt("plugin", "install", name, "--no-input")
	if err != nil {
		r.fail("plugin.install", fmt.Sprintf("plugin: belt plugin install %s: %v: %s", name, err, tail(out, 300)))
		return
	}
	st := harness.PluginState(name, harness.ScopeUser, beltPluginSource)
	if !st.Installed {
		r.fail("plugin.install", fmt.Sprintf("plugin: belt plugin install %s exited 0 and %s has no belt installed: %s", name, name, tail(out, 300)))
		return
	}
	msg := fmt.Sprintf("plugin: belt plugin install put belt into %s (%s, version %q)", name, st.Via, st.Version)
	if st.Via == "marketplace" {
		r.pass("plugin.install:marketplace", msg)
	} else {
		r.pass("plugin.install:files", msg)
	}
	if foreign {
		r.reportForeignKept("plugin.install.foreign-kept", "install")
	}
	r.reportStatus("plugin.status.installed", runBelt, true)

	before := countIn(hookFile, "belt plugin hook")
	if _, err := runBelt("plugin", "install", name, "--no-input"); err != nil {
		r.fail("plugin.reinstall", fmt.Sprintf("plugin: a second belt plugin install %s failed: %v", name, err))
	} else if after := countIn(hookFile, "belt plugin hook"); after != before {
		r.finding("plugin.reinstall:duplicated", fmt.Sprintf("plugin: a second install changed belt's hook commands in %s from %d to %d", h.HookPath(beltPluginSource.Name), before, after))
	} else if !harness.PluginState(name, harness.ScopeUser, beltPluginSource).Installed {
		r.fail("plugin.reinstall", "plugin: belt is not installed after a second install")
	} else {
		r.pass("plugin.reinstall", fmt.Sprintf("plugin: a second install left belt installed once (%d hook command(s) in the hook file)", after))
	}

	out, err = runBelt("plugin", "uninstall", name, "--yes", "--no-input")
	if err != nil {
		r.fail("plugin.uninstall", fmt.Sprintf("plugin: belt plugin uninstall %s: %v: %s", name, err, tail(out, 300)))
		return
	}
	if st := harness.PluginState(name, harness.ScopeUser, beltPluginSource); st.Installed {
		r.fail("plugin.uninstall", fmt.Sprintf("plugin: belt plugin uninstall %s exited 0 and belt is still installed (%s)", name, st.Via))
	} else {
		r.pass("plugin.uninstall", fmt.Sprintf("plugin: belt plugin uninstall removed belt from %s", name))
	}
	if foreign {
		r.reportForeignKept("plugin.uninstall.foreign-kept", "uninstall")
	}
	r.reportStatus("plugin.status.removed", runBelt, false)
	if left := filesRunning(home, "belt plugin hook"); len(left) > 0 {
		r.finding("plugin.uninstall.leftovers", fmt.Sprintf("plugin: after uninstall these files still run belt plugin hook: %s", strings.Join(left, ", ")))
	} else {
		r.pass("plugin.uninstall.leftovers", "plugin: after uninstall no file outside the plugin caches runs belt plugin hook")
	}
}

// reportForeignKept checks the other tool's hook survived a belt step.
func (r *TestRunner) reportForeignKept(label, step string) {
	if harness.SourceHooksInstalled(r.harness.Name, harness.ScopeUser, foreignSource) {
		r.pass(label, fmt.Sprintf("plugin: the hook installed before belt is still there after %s", step))
	} else {
		r.fail(label, fmt.Sprintf("plugin: belt plugin %s removed a hook it did not install", step))
	}
}

// reportStatus checks belt plugin status says what the agent's state says.
// Status prints one line per agent, starting with ✓ when belt is installed.
func (r *TestRunner) reportStatus(label string, runBelt func(...string) (string, error), want bool) {
	display := r.harness.DisplayName
	if display == "" {
		return
	}
	out, err := runBelt("plugin", "status", "--no-input")
	if err != nil {
		r.fail(label, fmt.Sprintf("plugin: belt plugin status: %v", err))
		return
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, display) {
			line = strings.TrimSpace(l)
			break
		}
	}
	if line == "" {
		r.finding(label+":not-listed", fmt.Sprintf("plugin: belt plugin status does not list %s", display))
		return
	}
	if claims := strings.HasPrefix(line, "✓"); claims != want {
		r.fail(label, fmt.Sprintf("plugin: belt plugin status says %q, and the agent's state says installed=%t", line, want))
		return
	}
	r.pass(label, fmt.Sprintf("plugin: belt plugin status agrees with the agent: %s", line))
}

func countIn(path, s string) int {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return bytes.Count(data, []byte(s))
}

// filesRunning lists files under home that contain s, relative to home,
// leaving out the copies of the plugin an agent's plugin manager keeps
// (claude's plugins/cache and plugins/marketplaces, codex's
// .tmp/marketplaces), where belt's own docs and hook manifests name the
// command without anything running it. The marketplace stays registered
// after uninstall by design.
func filesRunning(home, s string) []string {
	var out []string
	filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		if d.IsDir() {
			if strings.Contains(rel, "plugins/cache") || d.Name() == "marketplaces" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil
		}
		if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, []byte(s)) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return strings.Join(strings.Fields(s), " ")
}
