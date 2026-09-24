package harness

import (
	"reflect"
	"strings"
	"testing"
)

func TestSupport(t *testing.T) {
	const name = "zz-test-support"
	All[name] = Harness{Name: name, DisplayName: "Test Agent", InstallCmd: []string{"npm", "install", "-g", "zz"},
		Tested: TestedVersions{Min: "0.84.1", Max: "0.87.1"}}
	defer delete(All, name)
	for _, c := range []struct {
		version string
		level   SupportLevel
		reason  string
	}{
		{"0.84.1", Supported, "Test Agent 0.84.1 is a version inference has tested (0.84.1 to 0.87.1)"},
		{"zz 0.87.1 (build)", Supported, "Test Agent 0.87.1 is"},
		{"0.87.2", NewerThanTested, "Test Agent 0.87.2 is newer than 0.87.1, the newest version inference has tested"},
		{"0.80.3", OlderThanSupported, "Test Agent 0.80.3 is older than 0.84.1, the oldest version inference has tested"},
		{"", SupportUnknown, "could not read which version of Test Agent is installed; inference has tested 0.84.1 to 0.87.1"},
		{"garbage", SupportUnknown, "could not read"},
	} {
		v := Support(name, c.version)
		if v.Level != c.level || !strings.HasPrefix(v.Reason, c.reason) {
			t.Errorf("%q: %s %q", c.version, v.Level, v.Reason)
		}
		if v.TestedMin != "0.84.1" || v.TestedMax != "0.87.1" || !reflect.DeepEqual(v.UpgradeCmd, []string{"npm", "install", "-g", "zz"}) {
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

func TestSupportUsesProductName(t *testing.T) {
	v := Support("pi", "0.80.3")
	if v.Level != OlderThanSupported || !strings.HasPrefix(v.Reason, "Pi Coding Agent 0.80.3 is older than ") {
		t.Errorf("%+v", v)
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
