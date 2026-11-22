package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSource() PluginSource {
	return PluginSource{
		Name:        "acme",
		HookCommand: func(e HookEvent) string { return "acme hook " + string(e) },
		Hooks: map[HookEvent]HookSpec{
			PromptSubmit: {Timeout: 3 * time.Second},
			PostToolUse:  {Tools: []Tool{ToolShell}},
			Stop:         {Async: true},
		},
	}
}

func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, e := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(e, "")
		os.Unsetenv(e)
	}
	return home
}

// A source's hooks go in beside the user's, a reinstall replaces them, and
// uninstall takes out only the source's: the user's own hook on the same
// event survives all three.
func TestInstallSourceKeepsTheUsersHooks(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "hooks.json")
	os.MkdirAll(filepath.Dir(path), 0755)
	user := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`
	os.WriteFile(path, []byte(user), 0644)

	for i := 0; i < 2; i++ {
		res := InstallSource("codex", ScopeUser, testSource())
		if res.Error != nil {
			t.Fatal(res.Error)
		}
		if res.HooksPath != path {
			t.Fatalf("HooksPath = %s", res.HooksPath)
		}
	}
	stop := readJSON(t, path)["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 2 || !strings.Contains(mustJSON(stop[0]), "say done") || !strings.Contains(mustJSON(stop[1]), "acme hook stop") {
		t.Errorf("Stop after two installs = %s, want the user's hook then one of the source's", mustJSON(stop))
	}
	if !SourceHooksInstalled("codex", ScopeUser, testSource()) {
		t.Error("SourceHooksInstalled = false after install")
	}

	if res := UninstallSource("codex", ScopeUser, testSource()); res.Error != nil {
		t.Fatal(res.Error)
	}
	got := readJSON(t, path)
	if mustJSON(got["hooks"]) != `{"Stop":[{"hooks":[{"command":"say done","type":"command"}]}]}` {
		t.Errorf("after uninstall: %s", mustJSON(got))
	}
	if SourceHooksInstalled("codex", ScopeUser, testSource()) {
		t.Error("SourceHooksInstalled = true after uninstall")
	}
}

// Uninstall leaves no file behind that held nothing but the source's hooks,
// and leaves the agent's own settings file with everything else in it.
func TestUninstallSourceRemovesOnlyWhatInstallMade(t *testing.T) {
	home := tempHome(t)
	InstallSource("codex", ScopeUser, testSource())
	UninstallSource("codex", ScopeUser, testSource())
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("codex hooks.json left behind: %v", err)
	}

	settings := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0755)
	os.WriteFile(settings, []byte(`{"theme":"dark"}`), 0644)
	InstallSource("claude", ScopeUser, testSource())
	UninstallSource("claude", ScopeUser, testSource())
	if got := mustJSON(readJSON(t, settings)); got != `{"theme":"dark"}` {
		t.Errorf("claude settings after uninstall = %s", got)
	}
}

// Files and agents named after the plugin are named after the source.
func TestInstallSourceNamesFilesAfterTheSource(t *testing.T) {
	home := tempHome(t)
	cases := map[string]string{
		"goose": ".agents/plugins/acme/hooks/hooks.json",
		"kiro":  ".kiro/agents/acme.json",
		"grok":  ".grok/hooks/acme.json",
		"pi":    ".pi/agent/extensions/acme.ts",
	}
	for agent, rel := range cases {
		res := InstallSource(agent, ScopeUser, testSource())
		if res.Error != nil {
			t.Errorf("%s: %v", agent, res.Error)
			continue
		}
		if want := filepath.Join(home, rel); res.HooksPath != want {
			t.Errorf("%s: HooksPath = %s, want %s", agent, res.HooksPath, want)
		}
		if _, err := os.Stat(res.HooksPath); err != nil {
			t.Errorf("%s: %v", agent, err)
		}
	}
	if got := KiroActiveAgent(home, t.TempDir()); got != "acme" {
		t.Errorf("kiro default agent = %q, want the source's", got)
	}
	agent := readJSON(t, filepath.Join(home, ".kiro/agents/acme.json"))
	if agent["name"] != "acme" || agent["description"] != kiroAgentDescription("acme") {
		t.Errorf("kiro agent = %v", agent)
	}
	UninstallSource("kiro", ScopeUser, testSource())
	if _, err := os.Stat(filepath.Join(home, ".kiro/agents/acme.json")); !os.IsNotExist(err) {
		t.Error("kiro agent scaffold left behind")
	}
	if got := KiroActiveAgent(home, t.TempDir()); got != "" {
		t.Errorf("kiro default agent after uninstall = %q", got)
	}
}

// A reinstall into kimi's config.toml replaces the source's tables and keeps
// the rest of the agent's config; uninstall leaves the config as it was.
func TestTOMLHooksAreReplacedNotAppended(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".kimi-code", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0755)
	user := "default_model = \"k2\"\n\n[[hooks]]\nevent = \"Stop\"\ncommand = \"say done\"\n"
	os.WriteFile(path, []byte(user), 0644)
	InstallSource("kimi", ScopeUser, testSource())
	InstallSource("kimi", ScopeUser, testSource())
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "acme hook stop"); n != 1 {
		t.Errorf("source's stop hook appears %d times:\n%s", n, data)
	}
	if !strings.Contains(string(data), "say done") || !strings.Contains(string(data), "default_model") {
		t.Errorf("user's config lost:\n%s", data)
	}
	UninstallSource("kimi", ScopeUser, testSource())
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "acme") || strings.TrimSpace(string(data)) != strings.TrimSpace(user) {
		t.Errorf("after uninstall:\n%s", data)
	}
}

// What the agent cannot carry reaches the caller with the install.
func TestInstallSourceReportsNotes(t *testing.T) {
	tempHome(t)
	res := InstallSource("codex", ScopeUser, testSource())
	found := false
	for _, n := range res.Notes {
		if n.Event == Stop && strings.Contains(n.Detail, "runs in line") {
			found = true
		}
	}
	if !found {
		t.Errorf("codex notes = %v, want the dropped async", res.Notes)
	}
}

// A file that is not JSON is the user's; install refuses to overwrite it.
func TestInstallSourceLeavesAnUnreadableFileAlone(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "hooks.json")
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte("{not json"), 0644)
	if res := InstallSource("codex", ScopeUser, testSource()); res.Error == nil {
		t.Error("install over an unparseable file did not fail")
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Errorf("file changed to %s", data)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
