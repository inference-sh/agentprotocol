package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// PluginManager is an agent's own plugin system: the commands that add a
// marketplace and install or remove a plugin from it, and where the agent
// records what is installed. Argument templates take {{.Marketplace}} (the
// source's Marketplace, what the agent fetches) and {{.Plugin}}
// (name@marketplace-name, the key the agent registers it under).
type PluginManager struct {
	AddMarketplace []string
	Install        []string
	Uninstall      []string

	// ConfigDir is the agent's config dir under $HOME, which ConfigDirEnv
	// relocates; the files below are relative to it.
	ConfigDir string
	// InstalledFile lists installed plugins: {"plugins": {"<plugin>":
	// [{"installPath", "version", "scope"}]}}. Its installPath sits beside
	// the agent's cached copies of older versions.
	InstalledFile string
	// SettingsFile holds enabledPlugins, {"<plugin>": true|false}, at user
	// scope under ConfigDir and at project scope under the project's
	// ConfigDir. A plugin listed false is disabled.
	SettingsFile string
	// ListJSON is a command that prints {"installed": [{"pluginId",
	// "version", "installed", "enabled"}]}, for an agent whose installed
	// state is read through the agent rather than from a file.
	ListJSON []string
}

// PluginRegister is what an agent needs, beyond the hook file, to load a
// plugin it reads from files: a package manifest written beside the hook
// file ({{.Name}} is the source's name) and commands run after it, each only
// when its program is on PATH. Templates take {{.HookDir}} and {{.HooksPath}}.
type PluginRegister struct {
	PackageJSON string
	Cmds        [][]string
}

// Step is one command an install ran.
type Step struct {
	Argv   []string
	Output string
	Err    error
}

// runCommand and lookPath are how this package runs an agent's own CLI;
// tests replace them.
var (
	runCommand = func(argv []string) (string, error) {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return string(out), err
	}
	lookPath = exec.LookPath
)

func (src PluginSource) pluginKey() string { return src.Name + "@" + src.MarketplaceName }

func (src PluginSource) expand(argv []string, vars map[string]string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		a = strings.ReplaceAll(a, "{{.Marketplace}}", src.Marketplace)
		a = strings.ReplaceAll(a, "{{.Plugin}}", src.pluginKey())
		a = strings.ReplaceAll(a, sourceName, src.Name)
		for k, v := range vars {
			a = strings.ReplaceAll(a, "{{."+k+"}}", v)
		}
		out[i] = a
	}
	return out
}

func (r *InstallResult) run(argv []string) error {
	out, err := runCommand(argv)
	r.Steps = append(r.Steps, Step{Argv: argv, Output: out, Err: err})
	return err
}

// viaMarketplace reports whether a source installs into this agent through
// the agent's own plugin manager: the agent has one, the source names a
// marketplace, the install is user-wide, and the agent's CLI is on PATH.
func viaMarketplace(h Harness, scope InstallScope, src PluginSource) bool {
	if h.Plugins == nil || len(h.Plugins.Install) == 0 || src.Marketplace == "" || scope != ScopeUser {
		return false
	}
	_, err := lookPath(h.Binary)
	return err == nil
}

// InstallPlugin installs a source into an agent. Through the agent's plugin
// manager when it has one (viaMarketplace), with the source's own hook file
// then removed so its hooks do not run twice; otherwise by copying the
// source's skills into the agent's skills dir, writing its hook file
// (InstallSource) and registering the plugin where the agent needs that.
// Nothing is printed: every command run is in Steps.
func InstallPlugin(name string, scope InstallScope, src PluginSource) InstallResult {
	h, ok := All[name]
	if !ok {
		return InstallResult{Harness: name, Error: fmt.Errorf("unknown harness: %s", name)}
	}
	if viaMarketplace(h, scope, src) {
		result := InstallResult{Harness: name, Scope: scope, Via: "marketplace"}
		if len(h.Plugins.AddMarketplace) > 0 {
			// Not fatal: the marketplace may already be added.
			result.run(src.expand(h.Plugins.AddMarketplace, nil))
		}
		if err := result.run(src.expand(h.Plugins.Install, nil)); err != nil {
			result.Error = fmt.Errorf("%s plugin install: %w", name, err)
			return result
		}
		if src.HookCommand != nil {
			if files := UninstallSource(name, scope, src); files.Error != nil {
				result.Error = files.Error
			}
		}
		return result
	}

	var errs []error
	if src.Skills != nil && h.SkillsDir != "" {
		for _, skill := range sourceSkills(src.Skills) {
			sub, _ := fs.Sub(src.Skills, skill)
			if err := InstallSkill(name, scope, sub, skill); err != nil {
				errs = append(errs, err)
			}
		}
	}
	result := InstallResult{Harness: name, Scope: scope, Via: "files"}
	if src.HookCommand != nil {
		files := InstallSource(name, scope, src)
		files.Via, files.Steps = result.Via, result.Steps
		result = files
		if result.Error != nil {
			errs = append(errs, result.Error)
		}
	}
	if result.Error == nil && h.PluginRegister != nil && scope == ScopeUser && src.HookCommand != nil {
		errs = append(errs, registerPlugin(&result, h, src)...)
	}
	result.Error = errors.Join(errs...)
	return result
}

func registerPlugin(result *InstallResult, h Harness, src PluginSource) []error {
	reg := h.PluginRegister
	dir := filepath.Dir(result.HooksPath)
	vars := map[string]string{"HookDir": dir, "HooksPath": result.HooksPath}
	var errs []error
	if reg.PackageJSON != "" {
		if err := writeFile(filepath.Join(dir, "package.json"), strings.ReplaceAll(reg.PackageJSON, sourceName, src.Name)); err != nil {
			return []error{err}
		}
	}
	for _, argv := range reg.Cmds {
		if _, err := lookPath(argv[0]); err != nil {
			continue
		}
		if err := result.run(src.expand(argv, vars)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", strings.Join(argv[:2], " "), err))
		}
	}
	return errs
}

// UninstallPlugin removes a source from an agent: through the agent's plugin
// manager when the source is installed there, and its hook file, its skills
// and the package manifest its registration wrote, whichever exist.
func UninstallPlugin(name string, scope InstallScope, src PluginSource) InstallResult {
	h, ok := All[name]
	if !ok {
		return InstallResult{Harness: name, Error: fmt.Errorf("unknown harness: %s", name)}
	}
	result := InstallResult{Harness: name, Scope: scope}
	var errs []error
	if viaMarketplace(h, scope, src) && len(h.Plugins.Uninstall) > 0 {
		if st := marketplaceState(h, src); st.Installed {
			result.Via = "marketplace"
			if err := result.run(src.expand(h.Plugins.Uninstall, nil)); err != nil {
				errs = append(errs, fmt.Errorf("%s plugin uninstall: %w", name, err))
			}
		}
	}
	if src.HookCommand != nil {
		files := UninstallSource(name, scope, src)
		result.HooksPath, result.Merged = files.HooksPath, files.Merged
		if files.Error != nil {
			errs = append(errs, files.Error)
		}
		if h.PluginRegister != nil && h.PluginRegister.PackageJSON != "" {
			removeOwnManifest(filepath.Join(filepath.Dir(files.HooksPath), "package.json"), h, src)
		}
	}
	if src.Skills != nil && h.SkillsDir != "" {
		for _, skill := range sourceSkills(src.Skills) {
			if err := RemoveSkill(name, scope, skill); err != nil {
				errs = append(errs, err)
			}
		}
	}
	result.Error = errors.Join(errs...)
	return result
}

// removeOwnManifest removes the package manifest registration wrote, and
// only that: a package.json naming anything else is the user's.
func removeOwnManifest(path string, h Harness, src PluginSource) {
	want := map[string]any{}
	if json.Unmarshal([]byte(strings.ReplaceAll(h.PluginRegister.PackageJSON, sourceName, src.Name)), &want) != nil {
		return
	}
	obj, err := readJSONObject(path)
	if err != nil || obj["name"] != want["name"] {
		return
	}
	os.Remove(path)
}

// State is what an agent has of a source installed.
type State struct {
	Installed bool
	Enabled   bool
	Via       string // "marketplace" or "files"
	Version   string
	// Stale lists cached copies of the plugin beside the installed one,
	// which the agent no longer loads.
	Stale     []string
	HooksFile bool     // the source's hooks are in the agent's hook file
	Skills    []string // the source's skills present in the agent's skills dir
}

// PluginState reports what an agent has of a source installed, at a scope.
func PluginState(name string, scope InstallScope, src PluginSource) State {
	h, ok := All[name]
	if !ok {
		return State{}
	}
	var st State
	if h.Plugins != nil && src.Marketplace != "" && scope == ScopeUser {
		st = marketplaceState(h, src)
	}
	if src.HookCommand != nil {
		st.HooksFile = SourceHooksInstalled(name, scope, src)
	}
	if src.Skills != nil && h.SkillsDir != "" {
		have, _ := ListSkills(name, scope)
		for _, s := range sourceSkills(src.Skills) {
			if containsString(have, s) {
				st.Skills = append(st.Skills, s)
			}
		}
	}
	if !st.Installed && (st.HooksFile || len(st.Skills) > 0) {
		st.Installed, st.Enabled, st.Via = true, st.HooksFile, "files"
	}
	return st
}

func marketplaceState(h Harness, src PluginSource) State {
	pm := h.Plugins
	key := src.pluginKey()
	var st State
	if pm.InstalledFile != "" {
		dir := configDir(h, pm.ConfigDir)
		var installed struct {
			Plugins map[string][]struct {
				InstallPath string `json:"installPath"`
				Version     string `json:"version"`
			} `json:"plugins"`
		}
		if data, err := os.ReadFile(filepath.Join(dir, pm.InstalledFile)); err == nil && json.Unmarshal(data, &installed) == nil {
			if entries := installed.Plugins[key]; len(entries) > 0 {
				st.Installed, st.Via, st.Version = true, "marketplace", entries[0].Version
				st.Stale = staleCopies(entries[0].InstallPath)
			}
		}
		if st.Installed && pm.SettingsFile != "" {
			cwd, _ := os.Getwd()
			st.Enabled = pluginEnabled(filepath.Join(dir, pm.SettingsFile), key) ||
				cwd != "" && pluginEnabled(filepath.Join(cwd, pm.ConfigDir, pm.SettingsFile), key)
		}
		return st
	}
	if len(pm.ListJSON) > 0 {
		if _, err := lookPath(pm.ListJSON[0]); err != nil {
			return st
		}
		out, err := runCommand(src.expand(pm.ListJSON, nil))
		if err != nil {
			return st
		}
		var list struct {
			Installed []struct {
				PluginID  string `json:"pluginId"`
				Version   string `json:"version"`
				Installed bool   `json:"installed"`
				Enabled   bool   `json:"enabled"`
			} `json:"installed"`
		}
		if json.Unmarshal([]byte(out), &list) != nil {
			return st
		}
		for _, p := range list.Installed {
			if p.PluginID == key && p.Installed {
				st.Installed, st.Enabled, st.Via, st.Version = true, p.Enabled, "marketplace", p.Version
			}
		}
	}
	return st
}

// pluginEnabled reads enabledPlugins in one settings file. A plugin listed
// false is disabled; one not listed is not enabled by this file.
func pluginEnabled(path, key string) bool {
	var settings struct {
		EnabledPlugins map[string]any `json:"enabledPlugins"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &settings) != nil {
		return false
	}
	v, ok := settings.EnabledPlugins[key]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return !isBool || b
}

// staleCopies lists the directories beside a plugin's installed copy.
func staleCopies(installPath string) []string {
	if installPath == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Dir(installPath))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != filepath.Base(installPath) {
			out = append(out, filepath.Join(filepath.Dir(installPath), e.Name()))
		}
	}
	return out
}

// configDir is an agent's config dir: ConfigDirEnv when set, else rel under
// $HOME.
func configDir(h Harness, rel string) string {
	if h.ConfigDirEnv != "" && h.ConfigDirEnv != "XDG_CONFIG_HOME" {
		if dir := os.Getenv(h.ConfigDirEnv); dir != "" {
			return dir
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, rel)
}

// sourceSkills lists the skill directories at the root of a source's skills.
func sourceSkills(fsys fs.FS) []string {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(fsys, e.Name()+"/"+skillFile); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// PluginTree is a source rendered for every agent, for a repository that
// ships the files instead of having them installed.
type PluginTree struct {
	// Files maps "<agent>/<file>" to its content: each agent's hook file as
	// InstallSource writes it, and for an agent with a plugin manager the
	// plugin package's hooks file, "<agent>/hooks/hooks.json", which is the
	// hooks alone, without the agent's settings around them.
	Files map[string]string
	// Notes is, per agent, what the source asked for that its hooks cannot
	// carry.
	Notes map[string][]HookNote
}

// RenderPluginTree renders a source's hooks for every agent.
func RenderPluginTree(src PluginSource) (PluginTree, error) {
	if src.HookCommand == nil {
		return PluginTree{}, fmt.Errorf("plugin source %q has no hook command", src.Name)
	}
	tree := PluginTree{Files: map[string]string{}, Notes: map[string][]HookNote{}}
	for name, h := range All {
		plan, notes := planHooks(h, src)
		content, err := generateHookConfig(name, h, plan, src)
		if err != nil {
			return PluginTree{}, fmt.Errorf("%s: %w", name, err)
		}
		tree.Files[name+"/"+filepath.Base(h.HookPath(src.Name))] = content
		if len(notes) > 0 {
			tree.Notes[name] = notes
		}
		if h.Plugins != nil && h.HookFormat == JSONNested {
			bare := h
			bare.HookWrapper, bare.HookNoEnvelope = "", false
			pkg, err := generateHookConfig(name, bare, plan, src)
			if err != nil {
				return PluginTree{}, fmt.Errorf("%s plugin: %w", name, err)
			}
			tree.Files[name+"/hooks/hooks.json"] = pkg
		}
	}
	return tree, nil
}
