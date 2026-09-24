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
// of the agent's. It matches a name against the executable's base name, the
// first argument's, or, under a script interpreter, the script's; or a
// directory named for it in either path (npm keeps cursor-agent's script in
// .../cursor-agent/versions/...).
func (h Harness) Runs(argv []string, exe string) bool {
	var cands, paths []string
	add := func(p string) {
		if p == "" {
			return
		}
		paths = append(paths, p)
		cands = append(cands, strings.TrimSuffix(filepath.Base(p), ".exe"))
	}
	add(exe)
	if len(argv) > 0 {
		add(argv[0])
		base := filepath.Base(argv[0])
		if interpreters[base] || interpreters[filepath.Base(exe)] {
			for _, a := range argv[1:] {
				if !strings.HasPrefix(a, "-") {
					add(a)
					break
				}
			}
		}
	}
	for _, name := range h.ProcessNames() {
		if name == "" {
			continue
		}
		for _, c := range cands {
			if c == name {
				return true
			}
		}
		for _, p := range paths {
			if strings.Contains(p, "/"+name+"/") {
				return true
			}
		}
	}
	return false
}
