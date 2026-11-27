package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFilesRunning(t *testing.T) {
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write(".claude/plugins/cache/belt-sh-skills/belt/1.1.4/hooks/claude.json", "belt plugin hook stop")
	write(".claude/plugins/marketplaces/belt-sh-skills/README.md", "belt plugin hook")
	write(".codex/.tmp/marketplaces/belt-sh-skills/README.md", "belt plugin hook")
	write(".config/opencode/plugins/belt.js", "spawn('belt plugin hook prompt')")
	write(".config/opencode/opencode.json", "{}")
	got := filesRunning(home, "belt plugin hook")
	want := []string{".config/opencode/plugins/belt.js"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
