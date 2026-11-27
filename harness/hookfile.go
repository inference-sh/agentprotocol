package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// sourceName stands in registry paths for the name of the source whose hooks
// are written there: kiro's agent file and goose's plugin directory are named
// after the plugin, not after the agent.
const sourceName = "{{.Name}}"

// HookPath is where a source's hook file goes for this agent, relative to the
// home directory at user scope and to the project at project scope.
func (h Harness) HookPath(source string) string {
	return strings.ReplaceAll(filepath.Join(h.HookConfigDir, hookFileName(h)), sourceName, source)
}

func hookFileName(h Harness) string {
	if h.HookFileName != "" {
		return h.HookFileName
	}
	switch h.HookFormat {
	case TSExtension, TSPlugin:
		return sourceName + ".ts"
	case TOML:
		return "config.toml"
	case YAML:
		return "config.yaml"
	default:
		return sourceName + ".json"
	}
}

// needsMerge reports whether the hook file is the agent's own settings file,
// shared with everything else the agent keeps there.
func needsMerge(h Harness) bool {
	return h.HookWrapper != ""
}

// settingsFile reports whether the hooks share a settings file the agent owns
// (claude, gemini, qwen), which stays when the last hook goes. Every other
// format's file holds hooks alone.
func settingsFile(h Harness) bool {
	return h.HookFormat == JSONNested && needsMerge(h) && !h.HookNoEnvelope
}

// hookTarget resolves where a source's hooks go at a scope: the root the path
// is under and the file itself.
func hookTarget(h Harness, scope InstallScope, src PluginSource) (root, path string) {
	switch scope {
	case ScopeProject:
		root, _ = os.Getwd()
	default:
		root, _ = os.UserHomeDir()
	}
	return root, filepath.Join(root, h.HookPath(src.Name))
}

// ownership says which hook commands in a file are this source's: the exact
// commands it renders for every event, as written in each format.
func (src PluginSource) ownership(h Harness) func(string) bool {
	cmds := map[string]bool{}
	if src.HookCommand != nil {
		for _, e := range HookEvents() {
			c := src.HookCommand(e)
			cmds[c] = true
			cmds[declare(h, c)] = true
		}
	}
	return func(cmd string) bool {
		return cmds[cmd] || src.Owns != nil && src.Owns(cmd)
	}
}

// InstallSource writes a source's hooks into an agent's hook file at a scope,
// keeping every hook in that file that is not the source's: a reinstall
// replaces the source's own entries instead of adding a second copy.
func InstallSource(name string, scope InstallScope, src PluginSource) InstallResult {
	h, ok := All[name]
	if !ok {
		return InstallResult{Harness: name, Error: fmt.Errorf("unknown harness: %s", name)}
	}
	if src.HookCommand == nil {
		return InstallResult{Harness: name, Error: fmt.Errorf("plugin source %q has no hook command", src.Name)}
	}
	root, path := hookTarget(h, scope, src)
	plan, notes := planHooks(h, src)
	content, err := generateHookConfig(name, h, plan, src)
	result := InstallResult{Harness: name, Scope: scope, HooksPath: path, Notes: notes}
	if err != nil {
		result.Error = err
		return result
	}
	owned := src.ownership(h)

	switch h.HookFormat {
	case JSONNested, JSONFlat, JSONCopilot:
		result.Merged = h.HookFormat == JSONNested && needsMerge(h)
		result.Created = !result.Merged
		err = writeJSONHooks(path, content, h, owned)
	case JSONKiro:
		result.Merged = true
		err = mergeKiroAgentHooks(path, content, owned)
		if err == nil {
			// Older installs merged into a kiro_default.json override, which
			// the V1 engine never ran. Removing the source's hooks from it
			// keeps both engines from running them twice.
			err = removeKiroHooks(filepath.Join(root, h.HookConfigDir, KiroDefaultAgentName+".json"), src.Name, owned)
		}
		if err == nil {
			// A user-chosen default agent is left alone and reported.
			result.Inactive, err = kiroSelectAgent(root, src.Name)
		}
	case TOML:
		result.Merged = true
		err = writeTOMLHooks(path, content, src.Name, owned)
	case YAML:
		result.Merged = true
		err = writeYAMLHooks(path, content, owned)
	case TSExtension, TSPlugin:
		err = writeFile(path, content)
		result.Created = true
	default:
		err = fmt.Errorf("hook format not supported for install: %d", h.HookFormat)
	}
	result.Error = err
	return result
}

// UninstallSource removes a source's hooks from an agent's hook file at a
// scope and leaves every other hook there. A file left holding nothing but
// what the source's install scaffolded is removed.
func UninstallSource(name string, scope InstallScope, src PluginSource) InstallResult {
	h, ok := All[name]
	if !ok {
		return InstallResult{Harness: name, Error: fmt.Errorf("unknown harness: %s", name)}
	}
	root, path := hookTarget(h, scope, src)
	result := InstallResult{Harness: name, Scope: scope, HooksPath: path}
	owned := src.ownership(h)
	switch h.HookFormat {
	case JSONKiro:
		result.Merged = true
		result.Error = removeKiroHooks(path, src.Name, owned)
		if err := removeKiroHooks(filepath.Join(root, h.HookConfigDir, KiroDefaultAgentName+".json"), src.Name, owned); result.Error == nil {
			result.Error = err
		}
		if err := kiroDeselectAgent(root, src.Name); result.Error == nil {
			result.Error = err
		}
	case JSONNested, JSONFlat, JSONCopilot:
		result.Merged = needsMerge(h)
		result.Error = removeJSONHooks(path, h, owned)
	case TOML:
		result.Error = removeTOMLHooks(path, src.Name, owned)
	case YAML:
		result.Error = removeYAMLHooks(path, owned)
	default:
		result.Error = os.Remove(path)
		if os.IsNotExist(result.Error) {
			result.Error = nil
		}
	}
	return result
}

// SourceHooksInstalled reports whether an agent's hook file at a scope holds
// any of a source's hooks.
func SourceHooksInstalled(name string, scope InstallScope, src PluginSource) bool {
	h, ok := All[name]
	if !ok {
		return false
	}
	_, path := hookTarget(h, scope, src)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, e := range HookEvents() {
		c := src.HookCommand(e)
		if strings.Contains(string(data), c) || strings.Contains(string(data), jsonEscape(c)) {
			return true
		}
	}
	return false
}

// hookLists is the event → entries object of a JSON hook file: the file
// itself for droid's envelope-less hooks.json, its "hooks" otherwise.
func hookLists(obj map[string]any, h Harness) map[string]any {
	if h.HookNoEnvelope {
		return obj
	}
	m, _ := obj["hooks"].(map[string]any)
	return m
}

// entryOwned reports whether a hook entry is the source's. A nested entry
// (matcher plus hooks) has its inner hooks filtered instead, so a user's hook
// sharing a matcher group with the source's survives; it is owned when none
// are left.
func entryOwned(entry any, owned func(string) bool) (keep any, drop bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return entry, false
	}
	if inner, ok := m["hooks"].([]any); ok {
		var kept []any
		for _, i := range inner {
			if _, drop := entryOwned(i, owned); !drop {
				kept = append(kept, i)
			}
		}
		if len(kept) == 0 {
			return nil, true
		}
		if len(kept) != len(inner) {
			m["hooks"] = kept
		}
		return m, false
	}
	for _, k := range []string{"command", "bash"} {
		if c, _ := m[k].(string); c != "" && owned(c) {
			return nil, true
		}
	}
	return m, false
}

// stripOwned drops the source's entries from every event and drops events
// left empty.
func stripOwned(lists map[string]any, owned func(string) bool) {
	for event, v := range lists {
		arr, _ := v.([]any)
		var kept []any
		for _, e := range arr {
			if k, drop := entryOwned(e, owned); !drop {
				kept = append(kept, k)
			}
		}
		if len(kept) == 0 {
			delete(lists, event)
		} else {
			lists[event] = kept
		}
	}
}

// onlyScaffold reports whether a hooks-only file has nothing left that the
// source's install did not write: no hooks, and no keys beyond the format's
// fixed envelope.
func onlyScaffold(obj map[string]any, h Harness) bool {
	if len(hookLists(obj, h)) > 0 {
		return false
	}
	if h.HookNoEnvelope {
		return true
	}
	for k := range obj {
		if k != "hooks" && k != "version" {
			return false
		}
	}
	return true
}

// writeJSONHooks writes rendered JSON hooks into path. A missing file, or a
// hooks-only file holding nothing but the source's own hooks, gets the
// rendered config as is; otherwise the source's entries are replaced event by
// event and everything else in the file is kept. A file that is not JSON is
// the user's and is left alone.
func writeJSONHooks(path, content string, h Harness, owned func(string) bool) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return writeFile(path, content)
	}
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return fmt.Errorf("%s is not a JSON object; left as is", path)
	}
	lists := hookLists(obj, h)
	if lists == nil {
		lists = map[string]any{}
		obj["hooks"] = lists
	}
	stripOwned(lists, owned)
	if !settingsFile(h) && onlyScaffold(obj, h) {
		return writeFile(path, content)
	}

	var rendered map[string]any
	if err := json.Unmarshal([]byte(content), &rendered); err != nil {
		return fmt.Errorf("rendered hooks are not JSON: %w", err)
	}
	for event, v := range hookLists(rendered, h) {
		existing, _ := lists[event].([]any)
		lists[event] = append(existing, v.([]any)...)
	}
	return writeJSONObject(path, obj, 0644)
}

// removeJSONHooks drops the source's entries from a JSON hook file. An
// unreadable or unparseable file is left exactly as it is: it is the user's.
func removeJSONHooks(path string, h Harness, owned func(string) bool) error {
	obj, err := readJSONObject(path)
	if err != nil {
		return nil
	}
	lists := hookLists(obj, h)
	stripOwned(lists, owned)
	if !h.HookNoEnvelope && lists != nil && len(lists) == 0 {
		delete(obj, "hooks")
	}
	if settingsFile(h) {
		if len(obj) == 0 {
			return os.Remove(path)
		}
		return writeJSONObject(path, obj, 0644)
	}
	if onlyScaffold(obj, h) {
		return os.Remove(path)
	}
	return writeJSONObject(path, obj, 0644)
}

// tomlMarker is the comment an install writes above a source's TOML hooks.
func tomlMarker(source string) string { return "# " + source + " hooks" }

// stripTOMLHooks removes the source's [[hooks]] tables and its marker. A
// table runs from its header to the next header; it is the source's when its
// command is.
func stripTOMLHooks(text, source string, owned func(string) bool) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == tomlMarker(source) {
			continue
		}
		if strings.TrimSpace(line) != "[[hooks]]" {
			out = append(out, line)
			continue
		}
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "[") && strings.TrimSpace(lines[end]) != tomlMarker(source) {
			end++
		}
		block := lines[i:end]
		mine := false
		for _, l := range block {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "command = "); ok {
				var cmd string
				if json.Unmarshal([]byte(v), &cmd) == nil && owned(cmd) {
					mine = true
				}
			}
		}
		if !mine {
			out = append(out, block...)
		}
		i = end - 1
	}
	return strings.Join(out, "\n")
}

func writeTOMLHooks(path, content, source string, owned func(string) bool) error {
	os.MkdirAll(filepath.Dir(path), 0755)
	existing, _ := os.ReadFile(path)
	kept := stripTOMLHooks(string(existing), source, owned)
	if strings.TrimSpace(kept) == "" {
		kept = ""
	}
	return os.WriteFile(path, []byte(kept+"\n"+tomlMarker(source)+"\n"+content), 0644)
}

func removeTOMLHooks(path, source string, owned func(string) bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	kept := stripTOMLHooks(string(data), source, owned)
	if strings.TrimSpace(kept) == "" {
		return os.Remove(path)
	}
	return os.WriteFile(path, []byte(kept), 0644)
}

// YAML hooks are matched as the block an install wrote, whole: hermes keeps
// its whole config in this file and nothing here parses YAML, so a block that
// was edited since is left for the user.
// writeYAMLHooks merges a source's hooks into a YAML config (hermes keeps
// them in its whole config.yaml). The source's entries are dropped from every
// event first, then the new ones added: appending the generated text, as this
// did, wrote a second top-level hooks key whenever the user's config had one
// or the source's hooks had changed since the last install, and an event the
// source no longer asks for kept its old entry.
func writeYAMLHooks(path, content string, owned func(string) bool) error {
	os.MkdirAll(filepath.Dir(path), 0755)
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return writeFile(path, content)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var add yaml.Node
	if err := yaml.Unmarshal([]byte(content), &add); err != nil {
		return err
	}
	root := yamlRoot(&doc)
	if root == nil {
		return fmt.Errorf("%s: not a YAML mapping", path)
	}
	hooks := yamlKey(root, "hooks", true)
	dropOwnedYAML(hooks, owned)
	if addHooks := yamlKey(yamlRoot(&add), "hooks", false); addHooks != nil {
		for i := 0; i+1 < len(addHooks.Content); i += 2 {
			event, entries := addHooks.Content[i], addHooks.Content[i+1]
			list := yamlKey(hooks, event.Value, false)
			if list == nil || list.Kind != yaml.SequenceNode {
				hooks.Content = append(hooks.Content, event, entries)
				continue
			}
			list.Content = append(list.Content, entries.Content...)
		}
	}
	return writeYAMLDoc(path, &doc)
}

// removeYAMLHooks drops a source's entries from every event of a YAML config,
// and the file when nothing else is left in it.
func removeYAMLHooks(path string, owned func(string) bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	root := yamlRoot(&doc)
	if root == nil {
		return nil
	}
	if hooks := yamlKey(root, "hooks", false); hooks != nil {
		dropOwnedYAML(hooks, owned)
		if len(hooks.Content) == 0 {
			yamlDelete(root, "hooks")
		}
	}
	if len(root.Content) == 0 {
		return os.Remove(path)
	}
	return writeYAMLDoc(path, &doc)
}

// yamlRoot is a document's top-level mapping, or nil.
func yamlRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	return doc
}

// yamlKey is the value under key in a mapping; with create it adds an empty
// mapping there when the key is missing or holds nothing (hooks: {} or ~).
func yamlKey(m *yaml.Node, key string, create bool) *yaml.Node {
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if create && v.Kind != yaml.MappingNode {
				*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return v
		}
	}
	if !create {
		return nil
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func yamlDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// dropOwnedYAML removes the source's entries ({command: ...}) from every
// event list under hooks, and the events left empty.
func dropOwnedYAML(hooks *yaml.Node, owned func(string) bool) {
	var kept []*yaml.Node
	for i := 0; i+1 < len(hooks.Content); i += 2 {
		event, list := hooks.Content[i], hooks.Content[i+1]
		if list.Kind == yaml.SequenceNode {
			var entries []*yaml.Node
			for _, e := range list.Content {
				if cmd := yamlKey(e, "command", false); e.Kind == yaml.MappingNode && cmd != nil && owned(cmd.Value) {
					continue
				}
				entries = append(entries, e)
			}
			if len(entries) == 0 {
				continue
			}
			list.Content = entries
		}
		kept = append(kept, event, list)
	}
	hooks.Content = kept
}

func writeYAMLDoc(path string, doc *yaml.Node) error {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	enc.Close()
	return os.WriteFile(path, b.Bytes(), 0644)
}

// kiroAgentDescription marks a kiro agent file a source's install created.
func kiroAgentDescription(source string) string { return "Default agent with " + source + " hooks" }

// isKiroScaffold reports whether obj holds nothing beyond what generateJSONKiro writes.
func isKiroScaffold(obj map[string]any) bool {
	for k := range obj {
		switch k {
		case "name", "description", "tools", "includeMcpJson":
		default:
			return false
		}
	}
	return true
}

// withoutOwned drops the source's entries from a kiro hook list.
func withoutOwned(list any, owned func(string) bool) []any {
	arr, _ := list.([]any)
	var kept []any
	for _, e := range arr {
		m, _ := e.(map[string]any)
		cmd, _ := m["command"].(string)
		if owned(cmd) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// mergeKiroAgentHooks adds a source's hooks to a kiro agent config, keeping
// the user's prompt, tools, and their own hooks; the source's entries are
// replaced, not duplicated.
func mergeKiroAgentHooks(path, newContent string, owned func(string) bool) error {
	obj, err := readJSONObject(path)
	if err != nil {
		return writeFile(path, newContent)
	}
	var newObj map[string]any
	json.Unmarshal([]byte(newContent), &newObj)
	newHooks, _ := newObj["hooks"].(map[string]any)

	hooks, _ := obj["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	// The source's entries go from every event, not only the ones it writes
	// now: an event it no longer asks for (belt's PreToolUse, which kiro
	// cannot filter to the skill tool) kept its old entry and ran on every
	// tool.
	for k, v := range hooks {
		if kept := withoutOwned(v, owned); len(kept) == 0 {
			delete(hooks, k)
		} else {
			hooks[k] = kept
		}
	}
	for k, v := range newHooks {
		hooks[k] = append(withoutOwned(hooks[k], owned), v.([]any)...)
	}
	obj["hooks"] = hooks
	return writeJSONObject(path, obj, 0644)
}

// removeKiroHooks drops a source's entries from a kiro agent config, and the
// file with them when only the source's scaffold is left.
func removeKiroHooks(path, source string, owned func(string) bool) error {
	obj, err := readJSONObject(path)
	if err != nil {
		return nil
	}
	hooks, _ := obj["hooks"].(map[string]any)
	for k, v := range hooks {
		kept := withoutOwned(v, owned)
		if len(kept) == 0 {
			delete(hooks, k)
		} else {
			hooks[k] = kept
		}
	}
	if len(hooks) == 0 {
		delete(obj, "hooks")
	}
	if obj["description"] == kiroAgentDescription(source) && isKiroScaffold(obj) {
		return os.Remove(path)
	}
	return writeJSONObject(path, obj, 0644)
}
