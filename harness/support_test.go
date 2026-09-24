package harness

import (
	"reflect"
	"strings"
	"testing"
)

func TestSupport(t *testing.T) {
	const name = "zz-test-support"
	All[name] = Harness{Name: name, DisplayName: "Test Agent", InstallCmd: []string{"npm", "install", "-g", "zz"},
		Tested:   TestedVersions{Min: "0.84.1", Max: "0.87.1"},
		Requires: Requirement{Version: "0.80.4", Capability: "RPC agent_settled event"}}
	defer delete(All, name)
	for _, c := range []struct {
		version string
		level   SupportLevel
		reason  string
	}{
		{"0.84.1", Supported, "Test Agent 0.84.1 is a version inference has tested (0.84.1 to 0.87.1)"},
		{"zz 0.87.1 (build)", Supported, "Test Agent 0.87.1 is"},
		{"0.87.2", NewerThanTested, "Test Agent 0.87.2 is newer than 0.87.1, the newest version inference has tested"},
		{"0.82.0", OlderThanTested, "Test Agent 0.82.0 is older than the oldest version inference has tested (0.84.1); it may work"},
		{"0.80.4", OlderThanTested, "Test Agent 0.80.4 is older than"},
		{"0.80.3", OlderThanSupported, "Test Agent 0.80.3 has no RPC agent_settled event, added in 0.80.4"},
		{"", SupportUnknown, "could not read which version of Test Agent is installed; inference has tested 0.84.1 to 0.87.1"},
		{"garbage", SupportUnknown, "could not read"},
	} {
		v := Support(name, c.version)
		if v.Level != c.level || !strings.HasPrefix(v.Reason, c.reason) {
			t.Errorf("%q: %s %q", c.version, v.Level, v.Reason)
		}
		if v.TestedMin != "0.84.1" || v.TestedMax != "0.87.1" || v.Requires != "0.80.4" || !reflect.DeepEqual(v.UpgradeCmd, []string{"npm", "install", "-g", "zz"}) {
			t.Errorf("%q: display data %+v", c.version, v)
		}
	}
	if v := (DetectResult{Name: name, Version: "0.80.3"}).Support(); v.Level != OlderThanSupported {
		t.Errorf("DetectResult.Support: %s", v.Level)
	}
	if v := Support("zz-not-registered", "1.0"); v.Level != SupportUnknown || v.Reason == "" {
		t.Errorf("unregistered: %+v", v)
	}
	if v := Support("windsurf", "1.0"); v.Level != SupportUnknown || !strings.Contains(v.Reason, "Windsurf") {
		t.Errorf("untested agent: %+v", v)
	}
}

// The registry's floors: only a missing capability refuses.
func TestSupportFloors(t *testing.T) {
	for _, c := range []struct {
		name, version string
		level         SupportLevel
		reason        string
	}{
		{"pi", "0.80.3", OlderThanSupported, "Pi Coding Agent 0.80.3 has no RPC agent_settled event"},
		{"pi", "0.84.0", OlderThanTested, "Pi Coding Agent 0.84.0 is older than the oldest version inference has tested (0.87.1); it may work"},
		{"codex", "codex-cli 0.137.0", OlderThanTested, "Codex CLI 0.137.0 is older than"},
		{"codex", "codex-cli 0.55.0", OlderThanSupported, "Codex CLI 0.55.0 has no app-server thread and turn API"},
		{"claude", "2.1.280 (Claude Code)", OlderThanTested, "Claude Code 2.1.280 is older than"},
		{"claude", "1.0.0", OlderThanTested, ""},
		{"cursor", "2026.05.16-0338208", OlderThanTested, ""},
	} {
		v := Support(c.name, c.version)
		if v.Level != c.level || !strings.HasPrefix(v.Reason, c.reason) {
			t.Errorf("%s %s: %s %q", c.name, c.version, v.Level, v.Reason)
		}
	}
}

// A floor is set only with the capability and evidence, and never above
// what was tested.
func TestRequiresTable(t *testing.T) {
	for name, h := range All {
		r := h.Requires
		if r == (Requirement{}) {
			continue
		}
		if r.Version == "" || r.Capability == "" || r.Evidence == "" || ParseVersion(r.Version) != r.Version {
			t.Errorf("%s: incomplete Requires %+v", name, r)
		}
		if h.Tested.Min != "" && !versionAtLeast(h.Tested.Min, r.Version) {
			t.Errorf("%s: floor %s above tested min %s", name, r.Version, h.Tested.Min)
		}
	}
}

func TestUpgradeCommand(t *testing.T) {
	if got := All["hermes"].UpgradeCommand(); !reflect.DeepEqual(got[:3], []string{"pip", "install", "--upgrade"}) {
		t.Errorf("hermes: %v", got)
	}
	if got, want := All["claude"].UpgradeCommand(), All["claude"].InstallCmd; !reflect.DeepEqual(got, want) {
		t.Errorf("claude: %v, want InstallCmd %v", got, want)
	}
}

// Every agent that runs has a tested range, recorded as plain versions with
// its evidence, and the range is not upside down.
func TestTestedTable(t *testing.T) {
	for name, h := range All {
		tv := h.Tested
		if h.Binary == "" || len(h.HeadlessCmd) == 0 {
			if tv != (TestedVersions{}) {
				t.Errorf("%s: nothing runs it, but it has a tested range", name)
			}
			continue
		}
		if tv.Min == "" || tv.Max == "" || tv.Evidence == "" {
			t.Errorf("%s: no tested range: %+v", name, tv)
			continue
		}
		for _, v := range []string{tv.Min, tv.Max} {
			if ParseVersion(v) != v {
				t.Errorf("%s: %q is not a plain dotted version", name, v)
			}
		}
		if c, _ := CompareVersions(tv.Min, tv.Max); c > 0 {
			t.Errorf("%s: Min %s above Max %s", name, tv.Min, tv.Max)
		}
		if h.DriverKind() != "" && Support(name, tv.Max).Level != Supported {
			t.Errorf("%s: its own Max is not Supported", name)
		}
	}
}
