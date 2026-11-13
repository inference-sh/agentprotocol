package harness

import (
	"sort"
	"strings"
)

// FeatureState is whether a feature works on a version of an agent.
type FeatureState string

const (
	// FeatureWorks: the feature works on this version.
	FeatureWorks FeatureState = "works"
	// FeatureMissing: the feature does not exist or does not fire on this
	// version, for the recorded reason.
	FeatureMissing FeatureState = "missing"
	// FeaturePartial: it works with a recorded limit (a direct row whose
	// harness keeps its own system prompt or tools).
	FeaturePartial FeatureState = "partial"
)

// Feature names Compatibility reports beside the hook events, the checks
// named in KnownIssues and the names in Harness.Features.
const (
	// FeatureDirect is direct mode: the harness serving as a model with the
	// caller's system prompt and tools.
	FeatureDirect = "direct"
)

// FeatureStatus is one feature of one agent version.
type FeatureStatus struct {
	// Feature is "hook:<event>" for a hook event (hook:stop), a check named
	// in KnownIssues (prompt-context, streaming, model), FeatureDirect, or a
	// name from Harness.Features (turn-steer).
	Feature string `json:"feature"`
	// Mode is the mode the status holds in, "" for every mode.
	Mode  Mode         `json:"mode,omitempty"`
	State FeatureState `json:"state"`
	// Reason is why the feature is missing or partial.
	Reason string `json:"reason,omitempty"`
	// Versions are the versions the same State holds for, as far as the
	// record bounds them; zero is every version.
	Versions VersionRange `json:"versions"`
	// Seen is what the conformance suite saw on this version, or on the
	// newest older version it ran, when its history has the check: the
	// hook events (the mock hooks run of each mode) and direct mode.
	Seen *Seen `json:"seen,omitempty"`
}

// Compat is what inference knows about one installed version of an agent.
type Compat struct {
	Agent    string          `json:"agent"`
	Version  string          `json:"version,omitempty"`
	Support  SupportVerdict  `json:"support"`
	Features []FeatureStatus `json:"features"`
}

// Compatibility reports, for the version of the agent named name (what its
// --version printed, or DetectResult.Version), the Support verdict and the
// state of every feature the registry records: each hook event per mode,
// each check KnownIssues names, direct mode, and each version-gated entry of
// Harness.Features. It runs nothing. A feature it lists as works was seen
// working only on versions Support calls tested.
func Compatibility(name, version string) Compat {
	h, ok := All[name]
	if !ok {
		return Compat{Agent: name, Version: ParseVersion(version), Support: Support(name, version)}
	}
	return h.Compatibility(version)
}

// Compatibility is the package-level Compatibility for h.
func (h Harness) Compatibility(version string) Compat {
	c := Compat{Agent: h.Name, Version: ParseVersion(version), Support: h.Support(version)}
	hist, _ := AgentHistory(h.Name)
	seen := func(check string) *Seen {
		if s, ok := hist.Seen(check, version); ok {
			return &s
		}
		return nil
	}
	for _, mode := range h.modes() {
		for _, spec := range hookEvents {
			if spec.agentName(h.Events) == "" {
				continue
			}
			s := h.issueStatus("hook:"+string(spec.Event), mode, string(mode)+":event:"+spec.Tag, version)
			s.Seen = seen(hookCheck(mode, spec.Tag))
			c.Features = append(c.Features, s)
		}
	}
	var checks []string
	for key := range h.KnownIssues {
		if _, rest, _ := strings.Cut(key, ":"); rest != "" && !strings.HasPrefix(rest, "event:") {
			checks = append(checks, key)
		}
	}
	sort.Strings(checks)
	for _, key := range checks {
		mode, check, _ := strings.Cut(key, ":")
		c.Features = append(c.Features, h.issueStatus(check, Mode(mode), key, version))
	}
	if h.Direct != nil {
		s := h.Direct.status(version)
		s.Seen = seen("[direct] direct.tools")
		c.Features = append(c.Features, s)
	}
	names := make([]string, 0, len(h.Features))
	for f := range h.Features {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		r := h.Features[f]
		s := FeatureStatus{Feature: f, State: FeatureWorks, Versions: r}
		if !r.Contains(version) {
			s.State, s.Versions = FeatureMissing, r.outside(version)
			s.Reason = "only on " + r.String()
		}
		c.Features = append(c.Features, s)
	}
	return c
}

// Feature returns the status of one feature in one mode ("" for a feature
// that is not per mode), and whether c lists it.
func (c Compat) Feature(feature string, mode Mode) (FeatureStatus, bool) {
	for _, f := range c.Features {
		if f.Feature == feature && f.Mode == mode {
			return f, true
		}
	}
	return FeatureStatus{}, false
}

// issueStatus is the status of a feature that is missing exactly when the
// KnownIssues entry key applies to version.
func (h Harness) issueStatus(feature string, mode Mode, key, version string) FeatureStatus {
	s := FeatureStatus{Feature: feature, Mode: mode, State: FeatureWorks}
	reason, recorded := h.KnownIssues[key]
	if !recorded {
		return s
	}
	r := h.IssueVersions[key]
	if r.Contains(version) {
		s.State, s.Reason, s.Versions = FeatureMissing, reason, r
	} else {
		s.Versions = r.outside(version)
	}
	return s
}

func (d *Direct) status(version string) FeatureStatus {
	s := FeatureStatus{Feature: FeatureDirect, State: FeatureWorks}
	if d.Partial == "" {
		return s
	}
	if d.PartialVersions.Contains(version) {
		s.State, s.Reason, s.Versions = FeaturePartial, d.Partial, d.PartialVersions
	} else {
		s.Versions = d.PartialVersions.outside(version)
	}
	return s
}

// modes are the modes h has a command for.
func (h Harness) modes() []Mode {
	var out []Mode
	for _, m := range []Mode{ModeHeadless, ModeInteractive, ModeACP, ModeSDK} {
		if skip, _ := h.SkipFor(string(m)); skip == SkipNone {
			out = append(out, m)
		}
	}
	return out
}
