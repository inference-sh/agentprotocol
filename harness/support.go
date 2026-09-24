package harness

import "fmt"

// TestedVersions is the range of an agent's versions inference has
// verified: Min the oldest, Max the newest, both as the agent prints them
// (dotted, no prefix). Evidence says what verified them. Zero when nothing
// has.
type TestedVersions struct {
	Min      string
	Max      string
	Evidence string
}

// Requirement is a capability the driver depends on and the version it
// appeared in.
type Requirement struct {
	// Version is the first version with Capability.
	Version string
	// Capability names it for users: "RPC agent_settled event, which ends a
	// turn".
	Capability string
	// Evidence is where Version comes from (changelog, schema at a tag).
	Evidence string
}

// SupportLevel is what Support concluded about an installed version.
type SupportLevel string

const (
	// Supported: the version is inside the tested range.
	Supported SupportLevel = "supported"
	// NewerThanTested: newer than anything tested. Likely fine; warn.
	NewerThanTested SupportLevel = "newer-than-tested"
	// OlderThanTested: older than anything tested but not below
	// Harness.Requires. It may work; warn.
	OlderThanTested SupportLevel = "older-than-tested"
	// OlderThanSupported: below Harness.Requires, so something the driver
	// needs is missing. Do not drive it; show Reason and UpgradeCmd.
	OlderThanSupported SupportLevel = "older-than-supported"
	// SupportUnknown: the version could not be read, the agent is not in
	// the registry, or nothing about it has been tested. Warn.
	SupportUnknown SupportLevel = "unknown"
)

// SupportVerdict is Support's answer, shaped for display: Reason is a
// sentence safe to show an end user, and the rest is data.
type SupportVerdict struct {
	Level  SupportLevel `json:"level"`
	Reason string       `json:"reason"`
	// Version is the dotted version read from what was passed in, "" when
	// none could be.
	Version   string `json:"version,omitempty"`
	TestedMin string `json:"tested_min,omitempty"`
	TestedMax string `json:"tested_max,omitempty"`
	// Requires is the version below which the agent is refused, "" when
	// none is.
	Requires string `json:"requires,omitempty"`
	// UpgradeCmd installs the latest release over the installed one.
	UpgradeCmd []string `json:"upgrade_cmd,omitempty"`
}

// Support reports whether version (what the agent's --version printed, or
// DetectResult.Version) of the agent named name is one inference has tested.
// It runs nothing and reads no files.
func Support(name, version string) SupportVerdict {
	h, ok := All[name]
	if !ok {
		return SupportVerdict{Level: SupportUnknown, Version: ParseVersion(version),
			Reason: fmt.Sprintf("%s is not an agent inference knows", name)}
	}
	return h.Support(version)
}

// Support is the package-level Support for h.
func (h Harness) Support(version string) SupportVerdict {
	product := Display(h.Name)
	if h.DisplayName != "" {
		product = h.DisplayName
	}
	v := SupportVerdict{
		Version:    ParseVersion(version),
		TestedMin:  h.Tested.Min,
		TestedMax:  h.Tested.Max,
		Requires:   h.Requires.Version,
		UpgradeCmd: h.UpgradeCommand(),
	}
	switch {
	case v.Version != "" && h.Requires.Version != "" && !versionAtLeast(v.Version, h.Requires.Version):
		v.Level = OlderThanSupported
		v.Reason = fmt.Sprintf("%s %s has no %s, added in %s", product, v.Version, h.Requires.Capability, h.Requires.Version)
	case h.Tested.Min == "" || h.Tested.Max == "":
		v.Level = SupportUnknown
		v.Reason = fmt.Sprintf("inference has not tested any version of %s", product)
	case v.Version == "":
		v.Level = SupportUnknown
		v.Reason = fmt.Sprintf("could not read which version of %s is installed; inference has tested %s", product, rangeText(h.Tested))
	case !versionAtLeast(v.Version, h.Tested.Min):
		v.Level = OlderThanTested
		v.Reason = fmt.Sprintf("%s %s is older than the oldest version inference has tested (%s); it may work", product, v.Version, h.Tested.Min)
	case !versionAtLeast(h.Tested.Max, v.Version):
		v.Level = NewerThanTested
		v.Reason = fmt.Sprintf("%s %s is newer than %s, the newest version inference has tested", product, v.Version, h.Tested.Max)
	default:
		v.Level = Supported
		v.Reason = fmt.Sprintf("%s %s is a version inference has tested (%s)", product, v.Version, rangeText(h.Tested))
	}
	return v
}

// Support is the verdict on the version detection read. It runs nothing.
func (r DetectResult) Support() SupportVerdict { return Support(r.Name, r.Version) }

// UpgradeCommand is the command that installs h's latest release over an
// installed one: UpgradeCmd, else InstallCmd.
func (h Harness) UpgradeCommand() []string {
	cmd := h.UpgradeCmd
	if len(cmd) == 0 {
		cmd = h.InstallCmd
	}
	return append([]string(nil), cmd...)
}

func rangeText(t TestedVersions) string {
	if t.Min == t.Max {
		return t.Min
	}
	return t.Min + " to " + t.Max
}
