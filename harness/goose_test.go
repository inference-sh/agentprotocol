package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// goose discovers hooks and skills under .agents/plugins/<name>/ by directory
// convention. A plugin.json buys nothing, and a wrong one is destructive:
// measured on goose 1.51, a component path that does not start with "./", or
// a file that is not valid JSON, makes that component vanish from
// "goose skills list" with no error, no warning and a zero exit. Hooks keep
// firing either way, so a suite watching hooks would stay green while belt's
// skills disappeared.
//
// The registry used to carry a PluginManifest field for this, documented as
// "write plugin.json here on install", which nothing implemented. Writing it
// was the obvious fix and the wrong one.
func TestGooseInstallWritesNoPluginManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if res := Install("goose", ScopeUser); res.Error != nil {
		t.Fatalf("install: %+v", res)
	}

	manifest := filepath.Join(home, GoosePluginDir, "plugin.json")
	if _, err := os.Stat(manifest); err == nil {
		t.Errorf("install wrote %s — a malformed manifest silently removes belt's skills from goose, and a correct one adds nothing convention does not already give", manifest)
	}
}

// belt removes what belt wrote. Uninstall used to delete a plugin.json it
// never created, which is the rule removeMergedHooks states a few lines below
// it: an unparseable file is left as it is, because it is the user's.
func TestGooseUninstallLeavesAManifestItDidNotWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if res := Install("goose", ScopeUser); res.Error != nil {
		t.Fatalf("install: %+v", res)
	}
	manifest := filepath.Join(home, GoosePluginDir, "plugin.json")
	os.MkdirAll(filepath.Dir(manifest), 0755)
	mine := []byte(`{"name":"mine","skills":"./skills"}`)
	os.WriteFile(manifest, mine, 0600)

	Uninstall("goose", ScopeUser)

	got, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("uninstall deleted a plugin.json belt never wrote: %v", err)
	}
	if string(got) != string(mine) {
		t.Errorf("uninstall rewrote the user's manifest: %s", got)
	}
}

// goose's hooks must land under the plugin directory, since that is what the
// convention-based discovery walks.
func TestGooseHooksLandInThePluginDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	res := Install("goose", ScopeUser)
	if res.Error != nil {
		t.Fatalf("install: %+v", res)
	}
	want := filepath.Join(home, GoosePluginDir, "hooks")
	if got := filepath.Dir(res.HooksPath); got != want {
		t.Errorf("hooks at %s, want them under %s", got, want)
	}
}
