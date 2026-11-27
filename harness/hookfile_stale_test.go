package harness

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Reinstalling a source that asks for fewer events leaves none of its old
// entries, in every format; kiro's and hermes's kept them.
func TestReinstallDropsEventsNoLongerAsked(t *testing.T) {
	names := make([]string, 0, len(All))
	for n := range All {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			src := PluginSource{Name: "belt", HookCommand: func(e HookEvent) string { return "stale-probe " + string(e) }}
			if r := InstallSource(name, ScopeUser, src); r.Error != nil || r.HooksPath == "" {
				t.Skip("no hook file")
			}
			src.Hooks = map[HookEvent]HookSpec{Stop: {Timeout: 60 * time.Second}}
			r := InstallSource(name, ScopeUser, src)
			if r.Error != nil {
				t.Fatal(r.Error)
			}
			data, _ := os.ReadFile(r.HooksPath)
			for _, e := range []HookEvent{SessionStart, PromptSubmit, PreToolUse, PostToolUse} {
				if strings.Contains(string(data), "stale-probe "+string(e)) {
					t.Errorf("%s still has the %s entry after a reinstall without it:\n%s", filepath.Base(r.HooksPath), e, data)
				}
			}
		})
	}
}

// hermes's hooks live in its whole config.yaml: install merges into the
// user's hooks key instead of writing a second one, keeps their own keys and
// hooks, and uninstall takes only the source's entries.
func TestHermesYAMLMerge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".hermes", "config.yaml")
	os.MkdirAll(filepath.Dir(path), 0o755)
	user := "model: some-model\n# the user's comment\nhooks:\n  on_session_start:\n    - command: user-own-hook\n      timeout: 5\n"
	os.WriteFile(path, []byte(user), 0o644)

	src := PluginSource{Name: "belt", HookCommand: func(e HookEvent) string { return "belt plugin hook " + string(e) }}
	for i := 0; i < 2; i++ {
		if r := InstallSource("hermes", ScopeUser, src); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("config.yaml is not valid YAML after install: %v\n%s", err, data)
	}
	if strings.Count(string(data), "\nhooks:") != 1 {
		t.Errorf("want one hooks key:\n%s", data)
	}
	for _, want := range []string{"model: some-model", "the user's comment", "user-own-hook"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("install lost %q:\n%s", want, data)
		}
	}
	if n := strings.Count(string(data), "belt plugin hook stop"); n != 1 {
		t.Errorf("stop hook installed %d times after two installs:\n%s", n, data)
	}

	if r := UninstallSource("hermes", ScopeUser, src); r.Error != nil {
		t.Fatal(r.Error)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "belt plugin hook") {
		t.Errorf("uninstall left belt's hooks:\n%s", data)
	}
	for _, want := range []string{"model: some-model", "user-own-hook"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("uninstall lost %q:\n%s", want, data)
		}
	}
}
