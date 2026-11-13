package harness

import "testing"

func TestCompatibilityKimiStop(t *testing.T) {
	for _, c := range []struct {
		version string
		state   FeatureState
		within  VersionRange
	}{
		{"2.5.3", FeatureWorks, VersionRange{Before: "2.6.0"}},
		{"2.6.0", FeatureMissing, VersionRange{From: "2.6.0"}},
		{"kimi 2.6.1", FeatureMissing, VersionRange{From: "2.6.0"}},
	} {
		got := Compatibility("kimi", c.version)
		for _, mode := range []Mode{ModeHeadless, ModeInteractive, ModeACP} {
			s, ok := got.Feature("hook:stop", mode)
			if !ok || s.State != c.state || s.Versions != c.within {
				t.Errorf("kimi %s %s hook:stop = %+v, %v; want %s within %s", c.version, mode, s, ok, c.state, c.within)
			}
			if c.state == FeatureMissing && s.Reason == "" {
				t.Errorf("kimi %s %s hook:stop missing with no reason", c.version, mode)
			}
		}
		if s, _ := got.Feature("hook:user-prompt-submit", ModeACP); s.State != FeatureWorks {
			t.Errorf("kimi %s prompt hook = %+v, want works", c.version, s)
		}
	}
}

func TestCompatibilityDirect(t *testing.T) {
	if s, _ := Compatibility("kimi", "2.2.0").Feature(FeatureDirect, ""); s.State != FeatureWorks {
		t.Errorf("kimi 2.2.0 direct = %+v, want works", s)
	}
	if s, _ := Compatibility("kimi", "2.6.1").Feature(FeatureDirect, ""); s.State != FeaturePartial || s.Reason == "" {
		t.Errorf("kimi 2.6.1 direct = %+v, want partial with a reason", s)
	}
	if !All["kimi"].Direct.PureAt("2.5.3") || All["kimi"].Direct.PureAt("2.6.0") {
		t.Error("kimi PureAt: want pure before 2.6.0 only")
	}
	// droid's row is partial on every version.
	if s, _ := Compatibility("droid", "1.10.0").Feature(FeatureDirect, ""); s.State != FeaturePartial || s.Versions != (VersionRange{}) {
		t.Errorf("droid direct = %+v, want partial on every version", s)
	}
	if _, ok := Compatibility("windsurf", "").Feature(FeatureDirect, ""); ok {
		t.Error("windsurf lists direct, but has no direct row")
	}
}

func TestCompatibilityFeaturesAndChecks(t *testing.T) {
	old := Compatibility("codex", "0.98.0")
	if s, _ := old.Feature(FeatureTurnSteer, ""); s.State != FeatureMissing || s.Versions != (VersionRange{Before: "0.99.0"}) {
		t.Errorf("codex 0.98.0 turn-steer = %+v", s)
	}
	if s, _ := Compatibility("codex", "0.169.0").Feature(FeatureTurnSteer, ""); s.State != FeatureWorks {
		t.Errorf("codex 0.169.0 turn-steer = %+v", s)
	}
	// An unranged event issue holds on every version.
	if s, _ := Compatibility("codex", "0.169.0").Feature("hook:pre-compact", ModeHeadless); s.State != FeatureMissing {
		t.Errorf("codex headless pre-compact = %+v, want missing", s)
	}
	// droid's ACP prompt hook fires from 0.236.0.
	if s, _ := Compatibility("droid", "0.235.0").Feature("hook:user-prompt-submit", ModeACP); s.State != FeatureMissing {
		t.Errorf("droid 0.235.0 acp prompt hook = %+v, want missing", s)
	}
	if s, _ := Compatibility("droid", "1.10.0").Feature("hook:user-prompt-submit", ModeACP); s.State != FeatureWorks {
		t.Errorf("droid 1.10.0 acp prompt hook = %+v, want works", s)
	}
}

func TestCompatibilityUnknown(t *testing.T) {
	c := Compatibility("zz-not-registered", "1.0")
	if c.Support.Level != SupportUnknown || len(c.Features) != 0 {
		t.Errorf("unknown agent = %+v", c)
	}
}

// Every IssueVersions key names a KnownIssues entry, and every range reads.
func TestIssueVersionsKeys(t *testing.T) {
	for name, h := range All {
		for key, r := range h.IssueVersions {
			if _, ok := h.KnownIssues[key]; !ok {
				t.Errorf("%s: IssueVersions[%q] has no KnownIssues entry", name, key)
			}
			for _, v := range []string{r.From, r.Before} {
				if v != "" && ParseVersion(v) != v {
					t.Errorf("%s: IssueVersions[%q] bound %q is not a dotted version", name, key, v)
				}
			}
		}
		if d := h.Direct; d != nil && d.Partial == "" && d.PartialVersions != (VersionRange{}) {
			t.Errorf("%s: PartialVersions set on a pure direct row", name)
		}
	}
}

func TestKnownIssue(t *testing.T) {
	kimi := All["kimi"]
	if _, ok := kimi.KnownIssue("acp:event:STOP", "2.5.3"); ok {
		t.Error("kimi 2.5.3: Stop issue applied")
	}
	if _, ok := kimi.EventKnownMissing("acp", "STOP", "2.6.1"); !ok {
		t.Error("kimi 2.6.1: Stop issue not applied")
	}
	// Unranged entries hold even when the version cannot be read.
	if _, ok := All["gemini"].KnownIssue("acp:event:PRE_TOOL", ""); !ok {
		t.Error("gemini: unranged issue not applied to an unread version")
	}
}

func TestHistorySeen(t *testing.T) {
	h, ok := AgentHistory("kimi")
	if !ok {
		t.Fatal("no kimi history")
	}
	check := hookCheck(ModeACP, "STOP")
	if s, ok := h.Seen(check, "2.5.3"); !ok || s.Outcome != "pass" || s.Version != "2.5.3" {
		t.Errorf("kimi 2.5.3 %s = %+v, %v", check, s, ok)
	}
	if s, ok := h.Seen(check, "2.6.1"); !ok || s.Outcome != "skip:known-missing" || s.Message == "" {
		t.Errorf("kimi 2.6.1 %s = %+v, %v", check, s, ok)
	}
	// A version between two the suite ran is answered by the older one.
	if s, ok := h.Seen(check, "2.5.9"); !ok || s.Version != "2.5.3" {
		t.Errorf("kimi 2.5.9 %s = %+v, %v", check, s, ok)
	}
	if _, ok := h.Seen(check, "0.1.0"); ok {
		t.Error("kimi 0.1.0: seen on a version older than any run")
	}
	if s, _ := Compatibility("kimi", "2.6.1").Feature("hook:stop", ModeACP); s.Seen == nil || s.Seen.Outcome != "skip:known-missing" {
		t.Errorf("Compatibility kimi 2.6.1 hook:stop seen = %+v", s.Seen)
	}
}

func TestHistoryFiles(t *testing.T) {
	entries, err := history.ReadDir("history")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()[:len(e.Name())-len(".json")]
		if _, ok := All[name]; !ok {
			t.Errorf("history/%s names no registry agent", e.Name())
		}
		if h, ok := AgentHistory(name); !ok || h.Agent != name || len(h.Versions) == 0 {
			t.Errorf("history/%s does not read as %s's history", e.Name(), name)
		}
	}
}

// droid 1.9.0 fires the prompt hook in plain exec; 1.8.0 did not.
func TestDroidHeadlessPrompt(t *testing.T) {
	if s, _ := Compatibility("droid", "1.8.0").Feature("hook:user-prompt-submit", ModeHeadless); s.State != FeatureMissing {
		t.Errorf("droid 1.8.0 headless prompt hook = %+v", s)
	}
	if s, _ := Compatibility("droid", "1.9.0").Feature("hook:user-prompt-submit", ModeHeadless); s.State != FeatureWorks || s.Seen == nil || s.Seen.Outcome != "pass" {
		t.Errorf("droid 1.9.0 headless prompt hook = %+v", s)
	}
}
