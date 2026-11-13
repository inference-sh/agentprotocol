package history

import (
	"sort"
	"time"
)

// Tallies are the runs behind each recent version's outcomes, kept beside
// the history (tests/history/<agent>.tally.json) so a later run of the same
// version is weighed with the earlier ones: a check's outcome on a version
// is the one at least two thirds of its runs printed, else Mixed. A version
// last run more than TallyWindow before the agent's newest run is dropped
// from the tallies, and its outcomes stay as the history records them.
type Tallies struct {
	// Ingested are the runs already counted, by source, with their date.
	Ingested map[string]string `json:"ingested"`
	// Versions are version → check → outcome → how many runs printed it.
	Versions map[string]map[string]map[Outcome]int `json:"versions"`
}

// TallyWindow is how long a version's runs are kept in its tallies.
const TallyWindow = 14 * 24 * time.Hour

// Fold builds each agent's record and tallies from runs alone.
func Fold(runs []Run) (map[string]*Agent, map[string]*Tallies) {
	agents, tallies := map[string]*Agent{}, map[string]*Tallies{}
	MergeAll(agents, tallies, runs)
	return agents, tallies
}

// MergeAll merges runs into agents and tallies, adding an entry for an
// agent neither has.
func MergeAll(agents map[string]*Agent, tallies map[string]*Tallies, runs []Run) {
	names := map[string]bool{}
	for _, r := range runs {
		for _, s := range r.Sections {
			names[s.Agent] = true
		}
	}
	for name := range names {
		agents[name], tallies[name] = Merge(agents[name], tallies[name], name, runs)
	}
}

type outcome struct {
	o   Outcome
	msg string
}

// Merge adds the sections of runs for the agent named name to a and t (nil
// for none) and returns them. A run already in t.Ingested is not counted
// again.
func Merge(a *Agent, t *Tallies, name string, runs []Run) (*Agent, *Tallies) {
	if a == nil {
		a = &Agent{Agent: name}
	}
	if t == nil {
		t = &Tallies{}
	}
	if t.Ingested == nil {
		t.Ingested = map[string]string{}
	}
	if t.Versions == nil {
		t.Versions = map[string]map[string]map[Outcome]int{}
	}
	table := expand(a)

	byVersion := map[string]int{}
	for i, v := range a.Versions {
		byVersion[v.Version] = i
	}
	// A run older than the tallies keep is not merged: its version's other
	// runs are no longer there to weigh it against.
	cutoff := windowStart(a)
	sorted := append([]Run(nil), runs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Date < sorted[j].Date })
	counted := map[string]bool{} // version + source: the run is counted once per version
	// lines are the last line each outcome printed in these runs, by
	// version, check and outcome; an outcome they did not print keeps the
	// line the history has.
	lines := map[string]string{}
	fresh := map[string]bool{} // sources first seen in this merge
	touched := map[string]bool{}
	for _, run := range sorted {
		if _, done := t.Ingested[run.Source]; done && !fresh[run.Source] || run.Date < cutoff {
			continue
		}
		for _, s := range run.Sections {
			if s.Agent != name || s.Version == "" || len(s.Checks) == 0 {
				continue
			}
			fresh[run.Source] = true
			i, ok := byVersion[s.Version]
			if !ok {
				a.Versions = append(a.Versions, Version{Version: s.Version, First: run.Date, Last: run.Date})
				i = len(a.Versions) - 1
				byVersion[s.Version] = i
			}
			v := &a.Versions[i]
			if run.Date < v.First {
				v.First = run.Date
			}
			if run.Date > v.Last {
				v.Last = run.Date
			}
			v.Green = v.Green || run.Green
			if k := s.Version + " " + run.Source; !counted[k] {
				counted[k] = true
				v.Runs++
				if len(v.Sources) < 2 {
					v.Sources = append(v.Sources, run.Source)
					v.Suites = append(v.Suites, run.Suite)
				} else {
					v.Sources[1], v.Suites[1] = run.Source, run.Suite
				}
			}
			checks := t.Versions[s.Version]
			if checks == nil {
				checks = map[string]map[Outcome]int{}
				t.Versions[s.Version] = checks
			}
			for _, c := range s.Checks {
				if !c.Measured() {
					continue
				}
				o := c.Outcome
				if c.Answer != "" {
					o += Outcome(":" + c.Answer)
				}
				if checks[c.Key] == nil {
					checks[c.Key] = map[Outcome]int{}
				}
				checks[c.Key][o]++
				if o != Pass {
					lines[s.Version+"\x00"+c.Key+"\x00"+string(o)] = c.Message
				}
			}
			touched[s.Version] = true
		}
	}
	for _, run := range sorted {
		if fresh[run.Source] {
			t.Ingested[run.Source] = run.Date
		}
	}
	for v := range touched {
		row := map[string]outcome{}
		for key, counts := range t.Versions[v] {
			o := settle(counts)
			msg := ""
			if o != Mixed && o != Pass {
				msg = lines[v+"\x00"+key+"\x00"+string(o)]
				if old := table[v][key]; msg == "" && old.o == o {
					msg = old.msg
				}
			}
			row[key] = outcome{o, msg}
		}
		table[v] = row
	}
	sort.Slice(a.Versions, func(i, j int) bool { return less(a.Versions[i].Version, a.Versions[j].Version) })
	a.Checks = compress(a.Versions, table)
	prune(a, t)
	return a, t
}

// expand is a's outcomes per version: each point stands for every version
// from its From to its Last.
func expand(a *Agent) map[string]map[string]outcome {
	table := map[string]map[string]outcome{}
	for key, pts := range a.Checks {
		for _, p := range pts {
			for _, v := range a.Versions {
				if less(v.Version, p.From) || less(p.Last, v.Version) {
					continue
				}
				if table[v.Version] == nil {
					table[v.Version] = map[string]outcome{}
				}
				table[v.Version][key] = outcome{Outcome(p.Outcome), p.Message}
			}
		}
	}
	return table
}

// compress turns outcomes per version into stretches of one outcome.
func compress(versions []Version, table map[string]map[string]outcome) map[string][]Point {
	out := map[string][]Point{}
	for _, v := range versions {
		for key, oc := range table[v.Version] {
			pts := out[key]
			if n := len(pts); n > 0 && pts[n-1].Outcome == string(oc.o) {
				pts[n-1].Last = v.Version
				if oc.msg != "" {
					pts[n-1].Message = oc.msg
				}
				continue
			}
			out[key] = append(pts, Point{From: v.Version, Last: v.Version, Outcome: string(oc.o), Message: oc.msg})
		}
	}
	return out
}

// prune drops tallies of versions last run more than TallyWindow before the
// agent's newest run, and ingested runs older than that.
func prune(a *Agent, t *Tallies) {
	cutoff := windowStart(a)
	if cutoff == "" {
		return
	}
	for _, v := range a.Versions {
		if v.Last < cutoff {
			delete(t.Versions, v.Version)
		}
	}
	for src, date := range t.Ingested {
		if date < cutoff {
			delete(t.Ingested, src)
		}
	}
}

// windowStart is the first date TallyWindow covers before a's newest run,
// "" when a has none.
func windowStart(a *Agent) string {
	newest := ""
	for _, v := range a.Versions {
		if v.Last > newest {
			newest = v.Last
		}
	}
	end, err := time.Parse("2006-01-02", newest)
	if err != nil {
		return ""
	}
	return end.Add(-TallyWindow).Format("2006-01-02")
}
