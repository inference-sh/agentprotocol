package harness

import "testing"

// TestRuns checks each agent's process signature against the processes
// recorded while its session was live in the harness-test container.
func TestRuns(t *testing.T) {
	cases := []struct {
		agent string
		argv  []string
		exe   string
	}{
		{"claude", []string{"claude", "-p"}, "/root/.local/share/claude/versions/2.1/claude.exe"},
		{"codex", []string{"/h/.npm-global/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex", "exec"}, "/x/codex"},
		{"copilot", []string{"/h/.npm-global/lib/node_modules/@github/copilot/node_modules/@github/copilot-linux-x64/copilot", "--acp"}, "/x/copilot"},
		{"cursor", []string{"/h/.local/share/cursor-agent/versions/2026.09.23-86fc751/node", "/h/.local/share/cursor-agent/versions/2026.09.23-86fc751/index.js", "worker"}, "/usr/local/bin/node"},
		{"droid", []string{"droid", "exec"}, "/x/droid"},
		{"gemini", []string{"/usr/local/bin/node", "--max-old-space-size=128800", "/h/.npm-global/bin/gemini"}, "/usr/local/bin/node"},
		{"goose", []string{"goose", "acp"}, "/h/.local/bin/goose"},
		{"grok", []string{"grok", "agent", "stdio"}, "/h/.grok/bin/grok-linux-x86_64"},
		{"kilo", []string{"/h/.npm-global/lib/node_modules/@kilocode/cli/bin/.kilo", "acp"}, "/x/.kilo"},
		{"kimi", []string{"kimi-code"}, "/usr/local/bin/node"},
		{"kiro", []string{"/h/.local/bin/kiro-cli-chat", "acp"}, "/x/kiro-cli-chat"},
		{"omp", []string{"bun", "/h/.npm-global/bin/omp", "acp"}, "/x/bun"},
		{"opencode", []string{"opencode", "acp"}, "/x/opencode.exe"},
		{"pi", []string{"pi"}, "/usr/local/bin/node"},
		{"qwen", []string{"/usr/local/bin/node", "--expose-gc", "/h/.npm-global/lib/node_modules/@qwen-code/qwen-code/cli.js"}, "/usr/local/bin/node"},
	}
	for _, c := range cases {
		h, ok := All[c.agent]
		if !ok {
			t.Fatalf("no harness %s", c.agent)
		}
		if !h.Runs(c.argv, c.exe) {
			t.Errorf("%s does not recognise its own process %v (%s)", c.agent, c.argv, c.exe)
		}
		for _, other := range []string{"claude", "gemini", "goose", "kimi", "pi"} {
			if other != c.agent && All[other].Runs(c.argv, c.exe) {
				t.Errorf("%s claims %s's process %v", other, c.agent, c.argv)
			}
		}
	}
	// Claude Code's search helper runs from Claude's own binary under
	// another name; it is not a session.
	if All["claude"].Runs([]string{"ugrep", "-G", "--ignore-files"}, "/home/u/.local/share/claude/versions/2.1.280") {
		t.Error("claude claims its ugrep helper")
	}
	// A node process running something else is nobody's agent.
	for name, h := range All {
		if h.Runs([]string{"node", "/h/.npm-global/lib/node_modules/typescript-language-server/lib/cli.mjs", "--stdio"}, "/usr/local/bin/node") {
			t.Errorf("%s claims a language server", name)
		}
	}
}
