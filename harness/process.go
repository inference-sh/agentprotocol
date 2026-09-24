package harness

import (
	"path/filepath"
	"strings"
)

// processNames are the names an agent's running processes show besides its
// Binary: helper and worker processes, process titles, and the directory an
// npm or bun package keeps its script in. Measured by recording every
// process of each agent while a harness-test session was live
// (2026-09, current release of each). hermes was not observed and falls back
// to its Binary alone.
var processNames = map[string][]string{
	"grok": {"grok-linux-x86_64"},
	"kilo": {".kilo"},
	"kimi": {"kimi-code"},
	"kiro": {"kiro-cli-chat"},
	"qwen": {"qwen-code"},
}

// ProcessNames lists the names a running process of this agent shows: its
// Binary and any measured extras. Use Runs to match a process against them.
func (h Harness) ProcessNames() []string {
	names := []string{h.Binary}
	return append(names, processNames[h.Name]...)
}

// interpreters run an agent's script as their first argument.
var interpreters = map[string]bool{"node": true, "bun": true, "deno": true, "python": true, "python3": true}

// Runs reports whether a process with this argv and executable path is one
// of the agent's. A process is named by its first argument (its title, which
// agents like Claude Code set), or, when that is a script interpreter, by the
// script it runs; a name matches the base name or a directory in that path
// (npm keeps cursor-agent's script in .../cursor-agent/versions/...). The
// executable path is not a name: Claude Code runs its search helper as a
// child of its own binary with argv[0] "ugrep", which is not a session.
func (h Harness) Runs(argv []string, exe string) bool {
	if len(argv) == 0 {
		return false
	}
	paths := []string{argv[0]}
	if interpreters[filepath.Base(argv[0])] || interpreters[filepath.Base(exe)] {
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				paths = append(paths, a)
				break
			}
		}
	}
	for _, name := range h.ProcessNames() {
		if name == "" {
			continue
		}
		for _, p := range paths {
			if strings.TrimSuffix(filepath.Base(p), ".exe") == name || strings.Contains(p, "/"+name+"/") {
				return true
			}
		}
	}
	return false
}
