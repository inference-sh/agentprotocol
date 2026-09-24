package harness

import (
	"regexp"
	"strconv"
	"strings"
)

var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// ParseVersion returns the first dotted number in what an agent's
// --version prints: "2.1.281" from "2.1.281 (Claude Code)", "0.156.1" from
// "codex-cli 0.156.1", "2026.09.23" from "2026.09.23-86fc751", "0.19.0" from
// "Hermes Agent v0.19.0 (2026.7.20)". It returns "" when there is none.
func ParseVersion(s string) string { return versionRe.FindString(s) }

// CompareVersions compares the versions ParseVersion reads from a and b,
// numerically per component, a missing component counting as 0 ("1.2" equals
// "1.2.0"). ok is false when either has no version.
func CompareVersions(a, b string) (cmp int, ok bool) {
	pa, pb := ParseVersion(a), ParseVersion(b)
	if pa == "" || pb == "" {
		return 0, false
	}
	as, bs := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// versionAtLeast reports have >= min. A version it cannot read is not at
// least anything.
func versionAtLeast(have, min string) bool {
	c, ok := CompareVersions(have, min)
	return ok && c >= 0
}

// VersionRange is the versions of an agent something applies to: From
// inclusive, Before exclusive, an empty bound open.
//
// It is how the registry records behaviour that depends on the installed
// version, so an agent that changed a flag keeps one entry rather than one
// per version: a feature that appeared is {From: "0.76.0"}, one that was
// removed {Before: "2.0.0"}. Harness.Features holds them per agent and
// HasFeature reads them; StatusCheck.MinVersion is the same idea for the
// login check.
type VersionRange struct {
	From   string `json:"from,omitempty"`
	Before string `json:"before,omitempty"`
}

// Contains reports whether version is in the range. A version that cannot
// be read is in no bounded range: a caller that does not know what is
// installed does not use something only some versions have.
func (r VersionRange) Contains(version string) bool {
	if r.From == "" && r.Before == "" {
		return true
	}
	if ParseVersion(version) == "" {
		return false
	}
	if r.From != "" && !versionAtLeast(version, r.From) {
		return false
	}
	if r.Before != "" && versionAtLeast(version, r.Before) {
		return false
	}
	return true
}

// HasFeature reports whether the installed version of h has a named
// feature (a flag, subcommand or protocol field recorded in h.Features).
// A feature not recorded there is not version-specific and is always
// present; a recorded one is present only on versions in its range, so an
// unreadable version has none of them.
func (h Harness) HasFeature(feature, version string) bool {
	r, ok := h.Features[feature]
	return !ok || r.Contains(version)
}
