package harness

import (
	"embed"
	"encoding/json"
	"strings"
	"sync"
)

// history is each agent's conformance history, written by the conformance
// module's cmd/history from the suite's CI logs (agentprotocol's nightly and
// belt-sh/harness-test's before it, from 2026-08-28).
//
//go:embed history/*.json
var history embed.FS

// History is what the conformance suite saw of one agent: every version it
// ran, and for each check the outcome it had across those versions.
type History struct {
	Agent    string           `json:"agent"`
	Versions []HistoryVersion `json:"versions"`
	// Checks are keyed "[<run>] <check ID>": the run is the suite's
	// ("acp, mock hooks", "headless, belt hooks", "probes", "direct") and
	// the ID the runner's ("acp/hook.stop", "direct.tools").
	Checks map[string][]HistoryPoint `json:"checks"`
}

// HistoryVersion is one version the suite ran.
type HistoryVersion struct {
	Version string `json:"version"`
	First   string `json:"first"` // YYYY-MM-DD
	Last    string `json:"last"`
	Runs    int    `json:"runs"`
	// Sources are the first and the last run it was seen in, and Suites
	// the suite commits of those runs.
	Sources []string `json:"sources"`
	Suites  []string `json:"suites,omitempty"`
}

// HistoryPoint is a stretch of versions with one outcome: from From to
// Last, among the versions the check ran on.
type HistoryPoint struct {
	From string `json:"from"`
	Last string `json:"last"`
	// Outcome is pass, fail, skip, finding or mixed (the version's runs
	// disagreed), with the runner's answer after a colon when it has one:
	// "skip:known-missing", "finding:more", "pass:as-image". A skip is
	// recorded only when it says the agent cannot do the thing; one that
	// says the check could not be asked is left out.
	Outcome string `json:"outcome"`
	// Message is the line the runner printed, kept for outcomes other than
	// pass.
	Message string `json:"message,omitempty"`
}

var (
	historyMu    sync.Mutex
	historyCache = map[string]*History{}
)

// AgentHistory returns the suite's history of the agent named name, and
// whether it has one.
func AgentHistory(name string) (*History, bool) {
	historyMu.Lock()
	defer historyMu.Unlock()
	if h, ok := historyCache[name]; ok {
		return h, h != nil
	}
	var h *History
	if b, err := history.ReadFile("history/" + name + ".json"); err == nil {
		h = new(History)
		if json.Unmarshal(b, h) != nil {
			h = nil
		}
	}
	historyCache[name] = h
	return h, h != nil
}

// Seen is a check's outcome on the version nearest to one asked about:
// that version when the suite ran it, else the newest older one it ran
// the check on.
type Seen struct {
	Version string `json:"version"`
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
}

// Seen returns the outcome of check (a History.Checks key) on version, or
// on the newest older version the suite ran it on, and whether there is
// one. A version older than any the suite ran has none.
func (h *History) Seen(check, version string) (Seen, bool) {
	if h == nil || ParseVersion(version) == "" {
		return Seen{}, false
	}
	var best *HistoryPoint
	for i, p := range h.Checks[check] {
		if versionAtLeast(version, p.From) {
			best = &h.Checks[check][i]
		}
	}
	if best == nil {
		return Seen{}, false
	}
	s := Seen{Version: best.Last, Outcome: best.Outcome, Message: best.Message}
	if versionAtLeast(best.Last, version) {
		// Inside the stretch: the version itself, if the suite ran it.
		s.Version = ""
		for _, v := range h.Versions {
			if c, ok := CompareVersions(v.Version, version); ok && c == 0 {
				s.Version = v.Version
			}
		}
		if s.Version == "" {
			s.Version = h.newestRun(version)
		}
	}
	return s, true
}

// newestRun is the newest version at or below version that the suite ran
// at all; within a stretch every version it ran the check on had the
// stretch's outcome.
func (h *History) newestRun(version string) string {
	out := ""
	for _, v := range h.Versions {
		if versionAtLeast(version, v.Version) {
			out = v.Version
		}
	}
	return out
}

// hookCheck is the History key of the mock hook check for event in mode,
// as the runner names it ("[acp, mock hooks] acp/hook.stop").
func hookCheck(mode Mode, tag string) string {
	label := strings.ReplaceAll(strings.ToLower(tag), "_", "-")
	return "[" + string(mode) + ", mock hooks] " + string(mode) + "/hook." + label
}
