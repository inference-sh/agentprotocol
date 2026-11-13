// Package history turns conformance run logs into a per-version record of
// each agent: which versions ran, and for every check the outcome it had on
// each version. A run log is what the runner prints to stdout and stderr,
// the same whether it came from agentprotocol's nightly or from
// belt-sh/harness-test before it, so logs back to the suite's first nightly
// (2026-08-29) can be read.
package history

import (
	"bufio"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/inference-sh/agentprotocol/harness"
)

// Outcome is a check's mark in the log.
type Outcome string

const (
	Pass    Outcome = "pass"
	Fail    Outcome = "fail"
	Skip    Outcome = "skip"
	Finding Outcome = "finding"
	// Mixed is a version whose runs disagreed on the check: none of its
	// outcomes reached two thirds of them.
	Mixed Outcome = "mixed"
)

// An Outcome in a record carries the runner's answer after a colon when it
// has one, as the expected files do: "skip:known-missing", "pass:as-image".

var marks = map[string]Outcome{"✓": Pass, "✗": Fail, "○": Skip, "●": Finding}

// Section is one run of one agent in a log: the "=== <agent> (<variant>) ==="
// header up to its "=== <agent>: N passed" footer.
type Section struct {
	Agent   string
	Variant string // "headless, mock hooks", "acp, belt hooks"
	Version string // ParseVersion of the "→ version:" line
	Checks  []Check
}

// Check is one result line.
type Check struct {
	Outcome Outcome
	// Key is the line with what varies between runs of the same check
	// (ports, temp paths, counts, hook codes) replaced, prefixed with the
	// section's variant.
	Key string
	// Answer is the suffix of the runner's ID for the line:
	// "known-missing", "tool-not-offered", "" for none.
	Answer  string
	Message string
}

// Measured reports whether the line says something about the agent. A skip
// is measured only when the registry records the agent cannot do it (a
// "known-…" answer); any other skip ("the agent was never offered the tool
// call", "no requests to inspect") says the check could not be asked.
func (c Check) Measured() bool {
	return c.Outcome != Skip || strings.HasPrefix(c.Answer, "known")
}

var (
	stampRe  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z ?`)
	ansiRe   = regexp.MustCompile("\x1b\\[[0-9;]*m")
	headerRe = regexp.MustCompile(`^=== (\S+) \((.+)\) ===$`)
	// nightly.sh's probe runs: "=== kimi/direct: --harness kimi … ===".
	runRe     = regexp.MustCompile(`^=== ([a-z0-9-]+)/([a-z0-9-]+): .* ===$`)
	footerRe  = regexp.MustCompile(`^=== (\S+): \d+ passed`)
	versionRe = regexp.MustCompile(`^\s+→ version: (.+)$`)
	checkRe   = regexp.MustCompile(`^  ([✓✗○●]) (.+)$`)
)

// normalizers make two runs of one check print the same key.
var normalizers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`https?://(127\.0\.0\.1|localhost):\d+`), "URL"},
	{regexp.MustCompile(`/tmp/[^\s)'"]+`), "PATH"},
	{regexp.MustCompile(`HOOK-[A-Z0-9]+-\d+`), "HOOK"},
	{regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "UUID"},
	{regexp.MustCompile(`\d+(\.\d+)*`), "N"},
}

var phaseRe = regexp.MustCompile(`^(headless|interactive|acp|sdk): `)

// Key is msg as a check key under variant: "[acp, mock hooks] acp/hook.stop"
// when a pattern gives its ID, else "[acp, mock hooks] ~" and the line with
// what varies between runs (ports, temp paths, numbers, hook codes)
// replaced.
func Key(patterns []Pattern, variant, msg string) string {
	key, _ := keyAnswer(patterns, variant, msg)
	return key
}

func keyAnswer(patterns []Pattern, variant, msg string) (string, string) {
	if id, answer, ok := Match(patterns, msg); ok {
		for _, n := range normalizers[:len(normalizers)-1] {
			id = n.re.ReplaceAllString(id, n.with)
		}
		if m := phaseRe.FindStringSubmatch(msg); m != nil {
			id = m[1] + "/" + id
		}
		return "[" + variant + "] " + id, answer
	}
	for _, n := range normalizers {
		msg = n.re.ReplaceAllString(msg, n.with)
	}
	return "[" + variant + "] ~" + msg, ""
}

// Parse reads the sections of one log, naming checks by patterns.
func Parse(r io.Reader, patterns []Pattern) []Section {
	var out []Section
	var cur *Section
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := ansiRe.ReplaceAllString(stampRe.ReplaceAllString(sc.Text(), ""), "")
		line = strings.TrimRight(line, "\r")
		if m := headerRe.FindStringSubmatch(line); m != nil {
			out = append(out, Section{Agent: m[1], Variant: m[2]})
			cur = &out[len(out)-1]
			continue
		}
		if m := runRe.FindStringSubmatch(line); m != nil {
			out = append(out, Section{Agent: m[1], Variant: m[2]})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if footerRe.MatchString(line) {
			cur = nil
			continue
		}
		if m := versionRe.FindStringSubmatch(line); m != nil && cur.Version == "" {
			cur.Version = harness.ParseVersion(m[1])
			continue
		}
		if m := checkRe.FindStringSubmatch(line); m != nil {
			key, answer := keyAnswer(patterns, cur.Variant, m[2])
			// A pinned install prints no "→ version:", only "kimi 2.6.1
			// installed".
			if cur.Version == "" && strings.HasSuffix(key, "] binary") {
				cur.Version = harness.ParseVersion(m[2])
			}
			cur.Checks = append(cur.Checks, Check{Outcome: marks[m[1]], Key: key, Answer: answer, Message: m[2]})
		}
	}
	return out
}

// Run is one source of sections: a CI job's log.
type Run struct {
	Date   string // YYYY-MM-DD
	Source string // "agentprotocol nightly 43152242605"
	// Suite is the commit of the suite that ran, which tells a change in
	// the agent from a change in the suite.
	Suite string
	// Green: the job finished successfully.
	Green    bool
	Sections []Section
}

// Agent, Version and Point are the record harness embeds and reads.
type (
	Agent   = harness.History
	Version = harness.HistoryVersion
	Point   = harness.HistoryPoint
)

func settle(n map[Outcome]int) Outcome {
	total, best, bestN := 0, Mixed, 0
	for o, k := range n {
		total += k
		if k > bestN || k == bestN && o < best {
			best, bestN = o, k
		}
	}
	if 3*bestN < 2*total {
		return Mixed
	}
	return best
}

func less(a, b string) bool {
	c, ok := harness.CompareVersions(a, b)
	if !ok || c == 0 {
		return a < b
	}
	return c < 0
}

// Change is a check whose outcome changed from one version to the next
// version it ran on, both outcomes settled.
type Change struct {
	Check   string  `json:"check"`
	Was     Outcome `json:"was"`
	Now     Outcome `json:"now"`
	After   string  `json:"after"` // the last version with Was
	From    string  `json:"from"`  // the first version with Now
	Message string  `json:"message"`
	// SameSuite: the last run of After and the first run of From ran the
	// same suite commit, so the agent changed, not the suite.
	SameSuite bool `json:"same_suite"`
}

// Changes are a's settled transitions, by version and then check.
func Changes(a *Agent) []Change {
	var out []Change
	for key, pts := range a.Checks {
		for i := 1; i < len(pts); i++ {
			prev, p := pts[i-1], pts[i]
			if prev.Outcome == string(Mixed) || p.Outcome == string(Mixed) {
				continue
			}
			after, from := version(a, prev.Last), version(a, p.From)
			same := len(after.Suites) > 0 && len(from.Suites) > 0 &&
				after.Suites[len(after.Suites)-1] != "" && after.Suites[len(after.Suites)-1] == from.Suites[0]
			out = append(out, Change{Check: key, Was: Outcome(prev.Outcome), Now: Outcome(p.Outcome),
				After: prev.Last, From: p.From, Message: p.Message, SameSuite: same})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return less(out[i].From, out[j].From)
		}
		return out[i].Check < out[j].Check
	})
	return out
}

func version(a *Agent, v string) Version {
	for _, x := range a.Versions {
		if x.Version == v {
			return x
		}
	}
	return Version{}
}
