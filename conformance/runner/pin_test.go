package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/harness"
)

func TestPinnedInstallCmd(t *testing.T) {
	cases := []struct {
		agent, version, want string
	}{
		{"claude", "2.1.281", "npm install -g @anthropic-ai/claude-code@2.1.281"},
		{"pi", "0.87.1", "npm install -g --ignore-scripts @earendil-works/pi-coding-agent@0.87.1"},
		{"hermes", "0.14.0", "pip install --break-system-packages hermes-agent[acp,mcp]==0.14.0"},
		{"grok", "1.0.34", "bash -s 1.0.34"},
		{"kiro", "2.23.1", "stable/2.23.1/kirocli-"},
		{"goose", "1.51.0", "/releases/download/v1.51.0/goose-"},
		{"cursor", "2026.05.16-0338208", "/2026.05.16-0338208/g' | bash"},
	}
	for _, c := range cases {
		cmd, err := PinnedInstallCmd(harness.All[c.agent], c.version)
		if err != nil {
			t.Errorf("%s %s: %v", c.agent, c.version, err)
			continue
		}
		if got := strings.Join(cmd, " "); !strings.Contains(got, c.want) {
			t.Errorf("%s %s: %q does not contain %q", c.agent, c.version, got, c.want)
		}
	}
}

func TestPinnedInstallCmdRefuses(t *testing.T) {
	for _, c := range []struct{ agent, version string }{
		{"cursor", "2026.05.16"},        // a build id has the commit
		{"claude", "2.1.281; rm -rf ~"}, // not a version
		{"windsurf", "1.0.0"},           // no install command
	} {
		if cmd, err := PinnedInstallCmd(harness.All[c.agent], c.version); err == nil {
			t.Errorf("%s %s: got %q, want an error", c.agent, c.version, cmd)
		}
	}
	h := harness.Harness{Name: "x", InstallCmd: []string{"sh", "-c", "curl -fsSL https://example.com/install | bash"}}
	if _, err := PinnedInstallCmd(h, "1.0.0"); err == nil || !strings.Contains(err.Error(), "no way to select a version") {
		t.Errorf("unknown installer: %v", err)
	}
}

func TestSameVersion(t *testing.T) {
	for _, c := range []struct {
		printed, want string
		same          bool
	}{
		{"Hermes Agent v0.14.0 (2026.5.1)", "0.14.0", true},
		{"2026.05.16-0338208", "2026.05.16-0338208", true},
		{"2.1.281 (Claude Code)", "2.1.281", true},
		{"1.2", "1.2.0", true},
		{"0.19.0", "0.14.0", false},
		{"", "0.14.0", false},
	} {
		if got := sameVersion(c.printed, c.want); got != c.same {
			t.Errorf("sameVersion(%q, %q) = %v", c.printed, c.want, got)
		}
	}
}

func TestPinnableVersion(t *testing.T) {
	for _, c := range []struct{ agent, printed, want string }{
		{"droid", "1.5.0", "1.5.0"},
		{"claude", "2.1.318 (Claude Code)", "2.1.318"},
		{"codex", "codex-cli 0.167.0", "0.167.0"},
		{"cursor", "2026.05.16-0338208", "2026.05.16-0338208"},
		{"droid", "", ""},
		{"cursor", "2026.05.16", ""},
	} {
		got, ok := PinnableVersion(harness.All[c.agent], c.printed)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s %q: got %q %v, want %q", c.agent, c.printed, got, ok, c.want)
		}
	}
}

func TestSettledVersion(t *testing.T) {
	now := time.Date(2026, 11, 13, 4, 3, 0, 0, time.UTC)
	at := func(hm string) time.Time {
		t, _ := time.Parse("2006-01-02 15:04", hm)
		return t
	}
	times := map[string]time.Time{
		"0.167.0":           at("2026-11-08 10:00"),
		"0.167.0-linux-x64": at("2026-11-08 10:04"),
		"0.168.0-alpha.2":   at("2026-11-12 20:00"),
		"0.168.0":           at("2026-11-13 04:00"),
	}
	for _, c := range []struct {
		name   string
		latest string
		now    time.Time
		want   string
		ok     bool
	}{
		{"latest too new", "0.168.0", now, "0.167.0", true},
		{"latest settled", "0.168.0", now.Add(2 * time.Hour), "0.168.0", true},
		{"unknown latest", "9.9.9", now, "0.167.0", true},
		{"nothing old enough", "0.168.0", at("2026-11-08 10:30"), "", false},
	} {
		got, ok := SettledVersion(times, c.latest, c.now, time.Hour)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
