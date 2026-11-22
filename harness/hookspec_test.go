package harness

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// beltSource is belt's plugin as belt-sh/skills shipped it for Claude Code
// (hooks/hooks.json at 9fe11ca), written as a source.
func beltSource() PluginSource {
	return PluginSource{
		Name:        "belt",
		HookCommand: beltCmd,
		Hooks: map[HookEvent]HookSpec{
			SessionStart: {Timeout: 60 * time.Second},
			PromptSubmit: {Timeout: 10 * time.Second},
			PreToolUse:   {Tools: []Tool{ToolSkill}, Timeout: 10 * time.Second},
			PostToolUse:  {Tools: []Tool{ToolShell}, Timeout: 10 * time.Second},
			Stop:         {Timeout: 60 * time.Second, Async: true},
			PreCompact:   {Timeout: 60 * time.Second, Async: true},
			SessionEnd:   {Timeout: 600 * time.Second},
		},
	}
}

// The Claude plugin's hooks.json was written by hand and drifted from this
// generator: no SessionEnd, PreToolUse on every tool, one timeout for all.
// Generated from the same source, the two are now the same document.
func TestRenderHooksReproducesTheShippedClaudePlugin(t *testing.T) {
	got, notes, err := RenderHooks("claude", beltSource())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("claude can carry all of belt's source, got notes %v", notes)
	}
	want, err := os.ReadFile("testdata/belt-claude-hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var g, w map[string]any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("generated config is not JSON: %v\n%s", err, got)
	}
	json.Unmarshal(want, &w)
	if !reflect.DeepEqual(g["hooks"], w["hooks"]) {
		gh, _ := json.MarshalIndent(g["hooks"], "", "  ")
		t.Errorf("generated hooks differ from the shipped plugin:\n%s", gh)
	}
}

// What an agent cannot carry is written as the nearest thing that still
// works, and said: a hook that fires more often, or that the agent waits for.
func TestRenderHooksNotesWhatTheAgentCannotCarry(t *testing.T) {
	cases := []struct {
		agent string
		event HookEvent
		want  string
	}{
		{"codex", PreToolUse, "no skill tool name"},
		{"codex", Stop, "runs in line"},
		{"kiro", PostToolUse, "no tool matcher"},
		{"codex", SessionEnd, "no such hook"},
	}
	for _, c := range cases {
		_, notes, err := RenderHooks(c.agent, beltSource())
		if err != nil {
			t.Fatalf("%s: %v", c.agent, err)
		}
		found := false
		for _, n := range notes {
			if n.Event == c.event && strings.Contains(n.Detail, c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s %s: no note containing %q in %v", c.agent, c.event, c.want, notes)
		}
	}
}

// A tool the agent has no name for widens the hook to every tool rather than
// matching a name the agent never sends, which is how codex's shipped
// "exec_command" matcher fired for nothing.
func TestRenderHooksMatchesOnlyToolNamesTheAgentUses(t *testing.T) {
	got, _, _ := RenderHooks("codex", beltSource())
	var obj map[string]map[string][]map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("codex config is not JSON: %v\n%s", err, got)
	}
	pre := obj["hooks"]["PreToolUse"]
	if len(pre) != 1 || pre[0]["matcher"] != "*" {
		t.Errorf("codex PreToolUse = %v, want one group matching every tool", pre)
	}
	post := obj["hooks"]["PostToolUse"]
	if len(post) != 1 || post[0]["matcher"] != "Bash" {
		t.Errorf("codex PostToolUse = %v, want the Bash matcher", post)
	}
	if strings.Contains(got, `"async"`) {
		t.Error("codex config carries async, which codex does not take")
	}
}

func TestRenderHooksWritesOneTOMLEntryPerTool(t *testing.T) {
	src := beltSource()
	src.Hooks = map[HookEvent]HookSpec{PostToolUse: {Tools: []Tool{ToolShell, ToolSkill}, Timeout: 1500 * time.Millisecond}}
	h := All["kimi"]
	h.ToolNames = map[Tool]string{ToolShell: "Shell", ToolSkill: "Skill"}
	plan, notes := planHooks(h, src)
	got, _ := generateHookConfig("kimi", h, plan, src.HookCommand)
	if len(notes) != 0 || strings.Count(got, "[[hooks]]") != 2 || !strings.Contains(got, `matcher = "Shell"`) || !strings.Contains(got, "timeout = 2") {
		t.Errorf("kimi config with two tools, notes %v:\n%s", notes, got)
	}
}

// Every agent renders belt's source to a config its format parses.
func TestRenderHooksEveryAgent(t *testing.T) {
	for name, h := range All {
		got, _, err := RenderHooks(name, beltSource())
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		switch h.HookFormat {
		case JSONNested, JSONFlat, JSONCopilot, JSONKiro:
			var obj map[string]any
			if err := json.Unmarshal([]byte(got), &obj); err != nil {
				t.Errorf("%s: not JSON: %v\n%s", name, err, got)
			}
		}
	}
}

func TestRenderHooksNeedsACommand(t *testing.T) {
	if _, _, err := RenderHooks("claude", PluginSource{Name: "x"}); err == nil {
		t.Error("a source without a hook command rendered")
	}
}
