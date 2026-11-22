package runner

import (
	"strings"

	"github.com/inference-sh/agentprotocol/harness"
)

// beltSourceName is what belt installs under, and so what its files are
// named after: belt.json, .agents/plugins/belt, kiro's belt agent.
const beltSourceName = "belt"

// BeltSource is belt's hooks as the suite installs them for --hooks belt:
// the released CLI's command, under belt's name. Owns also takes the
// commands older belt versions wrote.
var BeltSource = harness.PluginSource{
	Name:        beltSourceName,
	HookCommand: func(e harness.HookEvent) string { return "belt plugin hook " + string(e) },
	Owns:        func(cmd string) bool { return strings.Contains(cmd, "belt plugin hook") },
}

// mockSource is the suite's own hooks, shaped exactly like belt's and
// installed under its name, running a script whose output the checks read.
func (r *TestRunner) mockSource() harness.PluginSource {
	return harness.PluginSource{Name: beltSourceName, HookCommand: r.mockHookCommand}
}

// hookPath is the hook file relative to HOME.
func (r *TestRunner) hookPath() string { return r.harness.HookPath(beltSourceName) }
