package history

import (
	"strings"
	"sync"
	"testing"
)

var runnerPatterns = sync.OnceValue(func() []Pattern {
	p, err := Patterns("../runner")
	if err != nil {
		panic(err)
	}
	return p
})

func log(version, stop string) string {
	return `2026-11-12T04:04:57.0Z === kimi (acp, mock hooks) ===
=== kimi ===
  ✓ kimi installed
  → version: ` + version + `
  ✓ KIMI_CODE_BASE_URL=http://127.0.0.1:43471/coding/v1
  ✓ hooks configured (code: HOOK-KIMI-1794456297355)
  ` + stop + `
=== kimi: 3 passed, 0 failed, 0 skipped (8s) ===
  ✓ outside any section
`
}

func TestParse(t *testing.T) {
	s := Parse(strings.NewReader(log("kimi 2.5.3", "✓ acp: stop hook fired")), runnerPatterns())
	if len(s) != 1 || s[0].Agent != "kimi" || s[0].Variant != "acp, mock hooks" || s[0].Version != "2.5.3" || len(s[0].Checks) != 4 {
		t.Fatalf("Parse = %+v", s)
	}
	for i, want := range []string{
		"[acp, mock hooks] binary",
		"[acp, mock hooks] env.KIMI_CODE_BASE_URL",
		"[acp, mock hooks] hooks.install",
		"[acp, mock hooks] acp/hook.stop",
	} {
		if got := s[0].Checks[i].Key; got != want {
			t.Errorf("key %d = %q, want %q", i, got, want)
		}
	}
	if got := Key(runnerPatterns(), "acp, belt hooks", "acp: belt post-tool hook did not fire"); got != "[acp, belt hooks] acp/belt-hook.post-tool" {
		t.Errorf("belt hook key = %q", got)
	}
	if got := Key(nil, "acp, mock hooks", "KIMI_CODE_BASE_URL=http://127.0.0.1:43471/coding/v1"); got != "[acp, mock hooks] ~KIMI_CODE_BASE_URL=URL/coding/vN" {
		t.Errorf("unmatched key = %q", got)
	}
}

func run(date, version, stop string) Run {
	return Run{Date: date, Source: date, Sections: Parse(strings.NewReader(log(version, stop)), runnerPatterns())}
}

func TestFoldAndChanges(t *testing.T) {
	const fired, skipped = "✓ acp: stop hook fired", "○ acp: stop hook not fired — known"
	runs := []Run{
		run("2026-10-01", "2.5.0", fired),
		run("2026-10-02", "2.5.3", fired),
		run("2026-10-03", "2.5.3", fired),
		run("2026-10-04", "2.5.3", "✗ acp: stop hook fired"), // one flaky run of three
		run("2026-11-12", "2.6.0", skipped),
		run("2026-11-13", "2.6.1", skipped),
	}
	a := Fold(runs)["kimi"]
	if len(a.Versions) != 4 || a.Versions[1].Version != "2.5.3" || a.Versions[1].Runs != 3 {
		t.Fatalf("versions = %+v", a.Versions)
	}
	ch := Changes(a)
	if len(ch) != 1 {
		t.Fatalf("changes = %+v", ch)
	}
	if c := ch[0]; c.Check != "[acp, mock hooks] acp/hook.stop" || c.Was != Pass || c.Now != "skip:known-missing" || c.After != "2.5.3" || c.From != "2.6.0" {
		t.Errorf("change = %+v", c)
	}
}

func TestSettle(t *testing.T) {
	if o := settle(map[Outcome]int{Pass: 2, Fail: 1}); o != Pass {
		t.Errorf("2 of 3 = %s", o)
	}
	if o := settle(map[Outcome]int{Pass: 1, Fail: 1}); o != Mixed {
		t.Errorf("1 of 2 = %s", o)
	}
}

func TestMeasuredAndProbeRuns(t *testing.T) {
	s := Parse(strings.NewReader(`=== kimi/direct: --harness kimi --mode acp --probe direct ===
=== kimi ===
  ✓ kimi 2.6.1 installed
  ○ acp: post-tool hook not checked — the agent was never offered the tool call
  ○ acp: stop hook not fired — kimi 2.6 skips it
  ● direct: the harness offers 25 tools of its own
=== kimi: 0 passed, 0 failed, 2 skipped, 1 found (1s) ===
`), runnerPatterns())
	if len(s) != 1 || s[0].Variant != "direct" || s[0].Version != "2.6.1" || len(s[0].Checks) != 4 {
		t.Fatalf("Parse = %+v", s)
	}
	if c := s[0].Checks[1]; c.Measured() || c.Answer != "tool-not-offered" {
		t.Errorf("tool-not-offered skip = %+v, measured %v", c, c.Measured())
	}
	if c := s[0].Checks[2]; !c.Measured() || c.Answer != "known-missing" {
		t.Errorf("known-missing skip = %+v, measured %v", c, c.Measured())
	}
}
