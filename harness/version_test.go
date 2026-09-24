package harness

import "testing"

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.282 (Claude Code)":            "2.1.282",
		"claude 2.1.282 (Claude Code)":     "2.1.282",
		"codex-cli 0.156.1":                "0.156.1",
		"2026.09.23-86fc751":               "2026.09.23",
		"Hermes Agent v0.19.0 (2026.7.20)": "0.19.0",
		"omp/18.3.0":                       "18.3.0",
		"GitHub Copilot CLI 1.0.88.":       "1.0.88",
		"grok 1.0.41 (4220f3b224a6)":       "1.0.41",
		"":                                 "",
		"no version here":                  "",
	} {
		if got := ParseVersion(in); got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"1.2", "1.2.0", 0, true},
		{"0.87.1", "0.84.1", 1, true},
		{"0.9.0", "0.10.0", -1, true},
		{"2026.09.23-86fc751", "2026.09.18", 1, true},
		{"2026.09.23-86fc751", "2026.9.23", 0, true},
		{"codex-cli 0.156.1", "0.156.1", 0, true},
		{"x", "1.0", 0, false},
	} {
		cmp, ok := CompareVersions(c.a, c.b)
		if cmp != c.cmp || ok != c.ok {
			t.Errorf("CompareVersions(%q, %q) = %d, %v", c.a, c.b, cmp, ok)
		}
	}
}

func TestVersionRange(t *testing.T) {
	for _, c := range []struct {
		r    VersionRange
		v    string
		want bool
	}{
		{VersionRange{}, "", true},
		{VersionRange{}, "1.0.0", true},
		{VersionRange{From: "0.76.0"}, "0.76.0", true},
		{VersionRange{From: "0.76.0"}, "pi 0.75.9", false},
		{VersionRange{From: "0.76.0"}, "", false},
		{VersionRange{Before: "2.0"}, "1.9.9", true},
		{VersionRange{Before: "2.0"}, "2.0.0", false},
		{VersionRange{From: "1.0", Before: "2.0"}, "1.5", true},
		{VersionRange{From: "1.0", Before: "2.0"}, "0.9", false},
	} {
		if got := c.r.Contains(c.v); got != c.want {
			t.Errorf("%+v.Contains(%q) = %v", c.r, c.v, got)
		}
	}
}

func TestHasFeature(t *testing.T) {
	pi := All["pi"]
	if !pi.HasFeature(FeatureSessionID, "0.87.1") || pi.HasFeature(FeatureSessionID, "0.75.0") || pi.HasFeature(FeatureSessionID, "") {
		t.Error("pi --session-id is recorded from 0.76.0")
	}
	codex := All["codex"]
	if !codex.HasFeature(FeatureTurnSteer, "codex-cli 0.99.0") || codex.HasFeature(FeatureTurnSteer, "codex-cli 0.98.0") {
		t.Error("codex turn/steer is recorded from 0.99.0")
	}
	if !All["claude"].HasFeature("anything-unrecorded", "") {
		t.Error("an unrecorded feature is on every version")
	}
}

func TestFeaturesAreBounded(t *testing.T) {
	for name, h := range All {
		for f, r := range h.Features {
			if r.From == "" && r.Before == "" {
				t.Errorf("%s: feature %q has no bound; leave it out instead", name, f)
			}
			for _, b := range []string{r.From, r.Before} {
				if b != "" && ParseVersion(b) != b {
					t.Errorf("%s: feature %q bound %q is not a plain dotted version", name, f, b)
				}
			}
		}
	}
}
