package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// stubCommands replaces the agent CLIs: every binary in onPath is found, and
// each command is recorded and answered by reply.
func stubCommands(t *testing.T, onPath []string, reply func(argv []string) (string, error)) *[][]string {
	t.Helper()
	var ran [][]string
	oldRun, oldLook := runCommand, lookPath
	t.Cleanup(func() { runCommand, lookPath = oldRun, oldLook })
	runCommand = func(argv []string) (string, error) {
		ran = append(ran, argv)
		if reply != nil {
			return reply(argv)
		}
		return "", nil
	}
	lookPath = func(bin string) (string, error) {
		for _, b := range onPath {
			if b == bin {
				return "/usr/bin/" + bin, nil
			}
		}
		return "", errors.New("not found")
	}
	return &ran
}

func pluginSource() PluginSource {
	src := testSource()
	src.Marketplace = "acme/plugins"
	src.MarketplaceName = "acme-plugins"
	src.Skills = fstest.MapFS{
		"greet/SKILL.md":       {Data: []byte("---\nname: greet\n---\nSay hi.\n")},
		"greet/scripts/run.sh": {Data: []byte("echo hi\n")},
		"notes.txt":            {Data: []byte("not a skill")},
	}
	return src
}

// Through a plugin manager: add the marketplace, install, and take the
// source's own hooks out of the hook file so they do not run twice, leaving
// the user's.
func TestInstallPluginThroughTheMarketplace(t *testing.T) {
	home := tempHome(t)
	ran := stubCommands(t, []string{"claude"}, func(argv []string) (string, error) {
		if argv[2] == "marketplace" {
			return "already added", errors.New("exit 1")
		}
		return "", nil
	})
	settings := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0755)
	os.WriteFile(settings, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"},{"type":"command","command":"acme hook stop"}]}]}}`), 0644)

	res := InstallPlugin("claude", ScopeUser, pluginSource())
	if res.Error != nil || res.Via != "marketplace" {
		t.Fatalf("result = %+v", res)
	}
	want := [][]string{
		{"claude", "plugin", "marketplace", "add", "acme/plugins"},
		{"claude", "plugin", "install", "acme@acme-plugins"},
	}
	if !reflect.DeepEqual(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
	if got := mustJSON(readJSON(t, settings)); got != `{"hooks":{"Stop":[{"hooks":[{"command":"say done","type":"command"}]}]}}` {
		t.Errorf("settings after a marketplace install = %s", got)
	}
}

// Without a plugin manager the plugin is files: skills copied, hooks written,
// and the agent's registration run where its program is on PATH.
func TestInstallPluginAsFiles(t *testing.T) {
	home := tempHome(t)
	ran := stubCommands(t, []string{"npm"}, nil)
	res := InstallPlugin("opencode", ScopeUser, pluginSource())
	if res.Error != nil || res.Via != "files" {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/opencode/skills/greet/scripts/run.sh")); err != nil {
		t.Errorf("skill not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/opencode/skills/notes.txt")); !os.IsNotExist(err) {
		t.Error("a file at the skills root was installed as a skill")
	}
	dir := filepath.Join(home, ".config/opencode/plugins")
	if res.HooksPath != filepath.Join(dir, "acme.ts") {
		t.Errorf("HooksPath = %s", res.HooksPath)
	}
	pkg := readJSON(t, filepath.Join(dir, "package.json"))
	if pkg["name"] != "acme-opencode" {
		t.Errorf("package.json = %v", pkg)
	}
	want := [][]string{{"npm", "install", "--ignore-scripts", "--prefix", dir}}
	if !reflect.DeepEqual(*ran, want) {
		t.Errorf("ran %v, want %v (opencode is not on PATH)", *ran, want)
	}

	st := PluginState("opencode", ScopeUser, pluginSource())
	if !st.Installed || !st.Enabled || st.Via != "files" || !st.HooksFile || !reflect.DeepEqual(st.Skills, []string{"greet"}) {
		t.Errorf("state = %+v", st)
	}

	if res := UninstallPlugin("opencode", ScopeUser, pluginSource()); res.Error != nil {
		t.Fatal(res.Error)
	}
	for _, p := range []string{"plugins/acme.ts", "plugins/package.json", "skills/greet"} {
		if _, err := os.Stat(filepath.Join(home, ".config/opencode", p)); !os.IsNotExist(err) {
			t.Errorf("%s left after uninstall", p)
		}
	}
	if st := PluginState("opencode", ScopeUser, pluginSource()); st.Installed {
		t.Errorf("state after uninstall = %+v", st)
	}
}

// Claude's installed state is read from its own files: installed_plugins.json
// for the version and the cached copy, enabledPlugins for whether it loads.
func TestPluginStateReadsClaudesFiles(t *testing.T) {
	home := tempHome(t)
	stubCommands(t, nil, nil)
	cache := filepath.Join(home, ".claude/plugins/cache/acme-plugins/acme")
	os.MkdirAll(filepath.Join(cache, "abc123"), 0755)
	os.MkdirAll(filepath.Join(cache, "old999"), 0755)
	installed, _ := json.Marshal(map[string]any{"plugins": map[string]any{
		"acme@acme-plugins": []map[string]any{{"installPath": filepath.Join(cache, "abc123"), "version": "1.2.0", "scope": "user"}},
	}})
	os.WriteFile(filepath.Join(home, ".claude/plugins/installed_plugins.json"), installed, 0644)

	st := PluginState("claude", ScopeUser, pluginSource())
	if !st.Installed || st.Enabled || st.Version != "1.2.0" || st.Via != "marketplace" {
		t.Errorf("state = %+v, want installed, not enabled", st)
	}
	if !reflect.DeepEqual(st.Stale, []string{filepath.Join(cache, "old999")}) {
		t.Errorf("stale = %v", st.Stale)
	}
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte(`{"enabledPlugins":{"acme@acme-plugins":true}}`), 0644)
	if st := PluginState("claude", ScopeUser, pluginSource()); !st.Enabled {
		t.Error("enabledPlugins true not read as enabled")
	}
	os.WriteFile(filepath.Join(home, ".claude/settings.json"), []byte(`{"enabledPlugins":{"acme@acme-plugins":false}}`), 0644)
	if st := PluginState("claude", ScopeUser, pluginSource()); st.Enabled {
		t.Error("enabledPlugins false read as enabled")
	}

	// CLAUDE_CONFIG_DIR moves all of it.
	moved := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", moved)
	if st := PluginState("claude", ScopeUser, pluginSource()); st.Installed {
		t.Error("state read from ~/.claude with CLAUDE_CONFIG_DIR set")
	}
}

// Codex is asked: plugin list --json.
func TestPluginStateAsksCodex(t *testing.T) {
	tempHome(t)
	stubCommands(t, []string{"codex"}, func(argv []string) (string, error) {
		return `{"installed":[{"pluginId":"acme@acme-plugins","version":"0.3.0","installed":true,"enabled":false}]}`, nil
	})
	st := PluginState("codex", ScopeUser, pluginSource())
	if !st.Installed || st.Enabled || st.Version != "0.3.0" {
		t.Errorf("state = %+v", st)
	}
}

// Uninstall goes through the plugin manager only for a plugin installed
// there.
func TestUninstallPluginThroughTheMarketplace(t *testing.T) {
	tempHome(t)
	listed := `{"installed":[{"pluginId":"acme@acme-plugins","installed":true,"enabled":true}]}`
	ran := stubCommands(t, []string{"codex"}, func(argv []string) (string, error) {
		if argv[2] == "list" {
			return listed, nil
		}
		return "", nil
	})
	if res := UninstallPlugin("codex", ScopeUser, pluginSource()); res.Error != nil || res.Via != "marketplace" {
		t.Fatalf("result = %+v", res)
	}
	if last := (*ran)[len(*ran)-1]; !reflect.DeepEqual(last, []string{"codex", "plugin", "remove", "acme@acme-plugins"}) {
		t.Errorf("last command %v", last)
	}
	listed = `{"installed":[]}`
	*ran = nil
	UninstallPlugin("codex", ScopeUser, pluginSource())
	for _, argv := range *ran {
		if argv[2] == "remove" {
			t.Error("removed a plugin codex does not have")
		}
	}
}

func TestSkills(t *testing.T) {
	home := tempHome(t)
	skill := fstest.MapFS{"SKILL.md": {Data: []byte("v1")}}
	if err := InstallSkill("claude", ScopeUser, skill, "greet"); err != nil {
		t.Fatal(err)
	}
	if err := InstallSkill("claude", ScopeUser, fstest.MapFS{"SKILL.md": {Data: []byte("v2")}}, "greet"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(home, ".claude/skills/greet/SKILL.md")); string(data) != "v2" {
		t.Errorf("reinstall left %q", data)
	}
	if got, _ := ListSkills("claude", ScopeUser); !reflect.DeepEqual(got, []string{"greet"}) {
		t.Errorf("ListSkills = %v", got)
	}
	for _, bad := range []string{"", "..", "../x", "a/b", `a\b`} {
		if err := InstallSkill("claude", ScopeUser, skill, bad); err == nil {
			t.Errorf("skill name %q accepted", bad)
		}
	}
	if err := InstallSkill("claude", ScopeUser, fstest.MapFS{"skill.md": {Data: []byte("x")}}, "lower"); err == nil {
		t.Error("a skill without SKILL.md installed")
	}
	if err := RemoveSkill("claude", ScopeUser, "greet"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ListSkills("claude", ScopeUser); len(got) != 0 {
		t.Errorf("ListSkills after remove = %v", got)
	}

	// A linked skill is removed as a link.
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("mine"), 0644)
	os.Symlink(target, filepath.Join(home, ".claude/skills/linked"))
	if got, _ := ListSkills("claude", ScopeUser); !reflect.DeepEqual(got, []string{"linked"}) {
		t.Errorf("ListSkills with a link = %v", got)
	}
	RemoveSkill("claude", ScopeUser, "linked")
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Error("removing a linked skill removed what it pointed to")
	}

	// ConfigDirEnv moves the skills dir with the config dir, except where
	// the skills live outside it: codex reads .agents/skills.
	moved := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", moved)
	if got := SkillsPath("claude", ScopeUser); got != filepath.Join(moved, "skills") {
		t.Errorf("claude skills with CLAUDE_CONFIG_DIR = %s", got)
	}
	t.Setenv("CODEX_HOME", moved)
	if got := SkillsPath("codex", ScopeUser); got != filepath.Join(home, ".agents/skills") {
		t.Errorf("codex skills with CODEX_HOME = %s", got)
	}
	if got := SkillsPath("gemini", ScopeProject); got != "" {
		t.Errorf("gemini project skills = %q, want unknown", got)
	}
}

// The tree a skills repository ships: every agent's hook file, and the
// plugin package's hooks for agents with a plugin manager. belt's Claude
// package is the hooks.json belt-sh/skills ships.
func TestRenderPluginTree(t *testing.T) {
	tree, err := RenderPluginTree(beltSource())
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range All {
		if _, ok := tree.Files[name+"/"+filepath.Base(h.HookPath("belt"))]; !ok {
			t.Errorf("no hook file for %s", name)
		}
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(tree.Files["claude/hooks/hooks.json"]), &got); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile("testdata/belt-claude-hooks.json")
	json.Unmarshal(data, &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claude plugin hooks = %s", tree.Files["claude/hooks/hooks.json"])
	}
	if strings.Contains(tree.Files["claude/hooks/hooks.json"], "permissions") {
		t.Error("the plugin package carries claude's settings wrapper")
	}
	if _, ok := tree.Files["codex/hooks/hooks.json"]; !ok {
		t.Error("no codex plugin hooks")
	}
	if len(tree.Notes["codex"]) == 0 {
		t.Error("codex notes missing")
	}
}

// kilo loads a plugin by the id its default export names.
func TestKiloPluginIsNamedAfterTheSource(t *testing.T) {
	got, _, err := RenderHooks("kilo", testSource())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `id: "acme"`) {
		t.Errorf("kilo plugin export:\n%s", got)
	}
}
