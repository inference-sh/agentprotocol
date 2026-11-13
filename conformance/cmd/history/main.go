// Command history builds harness/history/<agent>.json, which the harness
// package embeds, from downloaded CI job logs: every version of each agent
// the suite ran, and each check's outcome on each version. It reads an index
// of jobs, one tab-separated line each,
//
//	repo  workflow  run  created  event  branch  sha  job  name  conclusion
//
// and the logs as <logs>/<repo with / as _>/<job>.log.gz. Pull request runs
// are left out: they test a change, not the agent.
//
// Without -merge it builds the history from the logs alone; with -merge it
// adds them to the committed history and tallies (tests/history), which is
// what the History workflow does after each nightly. It prints each settled
// change of a check's outcome between two versions (with -merge, those the
// logs added), AGENT where both sides ran the same suite commit, and exits 3
// when there is an AGENT change.
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/inference-sh/agentprotocol/conformance/history"
)

func main() {
	index := flag.String("index", "", "job index (see the command's doc)")
	logs := flag.String("logs", "", "directory of <repo>/<job>.log.gz")
	out := flag.String("out", "../harness/history", "directory of <agent>.json")
	talliesDir := flag.String("tallies", "tests/history", "directory of <agent>.tally.json")
	merge := flag.Bool("merge", false, "add the logs to the history and tallies in -out and -tallies")
	runnerDir := flag.String("runner", "runner", "the runner's source, read for check IDs")
	summary := flag.String("summary", "", "also write the changes as Markdown to this file")
	flag.Parse()
	if *index == "" || *logs == "" {
		flag.Usage()
		os.Exit(2)
	}
	agentChange, err := run(*index, *logs, *out, *talliesDir, *runnerDir, *summary, *merge)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if agentChange {
		os.Exit(3)
	}
}

func run(index, logs, out, talliesDir, runnerDir, summary string, merge bool) (bool, error) {
	patterns, err := history.Patterns(runnerDir)
	if err != nil {
		return false, err
	}
	runs, err := readRuns(index, logs, patterns)
	if err != nil {
		return false, err
	}
	agents, tallies := map[string]*history.Agent{}, map[string]*history.Tallies{}
	before := map[string]bool{}
	if merge {
		if err := load(out, talliesDir, agents, tallies); err != nil {
			return false, err
		}
		for _, a := range agents {
			for _, c := range history.Changes(a) {
				before[changeKey(a.Agent, c)] = true
			}
		}
	}
	history.MergeAll(agents, tallies, runs)

	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, dir := range []string{out, talliesDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
	}
	var md strings.Builder
	agentChange := false
	for _, n := range names {
		a := agents[n]
		if err := writeJSON(filepath.Join(out, n+".json"), a); err != nil {
			return false, err
		}
		if err := writeJSON(filepath.Join(talliesDir, n+".tally.json"), tallies[n]); err != nil {
			return false, err
		}
		if len(a.Versions) > 0 {
			fmt.Printf("== %s: %d versions, %s to %s\n", n, len(a.Versions), a.Versions[0].Version, a.Versions[len(a.Versions)-1].Version)
		}
		for _, c := range history.Changes(a) {
			if before[changeKey(n, c)] {
				continue
			}
			mark := "suite?"
			if c.SameSuite {
				mark, agentChange = "AGENT", true
			}
			fmt.Printf("  %-6s %s→%s  %s %s→%s  %s\n", mark, c.After, c.From, c.Check, c.Was, c.Now, c.Message)
			fmt.Fprintf(&md, "| %s | %s | %s → %s | `%s` | %s → %s | %s |\n", mark, n, c.After, c.From, c.Check, c.Was, c.Now, mdCell(c.Message))
		}
	}
	if summary != "" {
		text := "No check changed outcome between versions.\n"
		if md.Len() > 0 {
			text = "| | agent | versions | check | outcome | line |\n|---|---|---|---|---|---|\n" + md.String() +
				"\nAGENT: both sides ran the same suite commit, so the agent changed. suite?: the suite changed too.\n"
		}
		if err := os.WriteFile(summary, []byte(text), 0o644); err != nil {
			return false, err
		}
	}
	return agentChange, nil
}

func changeKey(agent string, c history.Change) string {
	return agent + "\x00" + c.Check + "\x00" + c.From + "\x00" + string(c.Now)
}

func mdCell(s string) string {
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

func load(out, talliesDir string, agents map[string]*history.Agent, tallies map[string]*history.Tallies) error {
	files, err := filepath.Glob(filepath.Join(out, "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		a := new(history.Agent)
		if err := readJSON(f, a); err != nil {
			return err
		}
		agents[name] = a
		t := new(history.Tallies)
		if err := readJSON(filepath.Join(talliesDir, name+".tally.json"), t); err == nil {
			tallies[name] = t
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readRuns(index, logs string, patterns []history.Pattern) ([]history.Run, error) {
	f, err := os.Open(index)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var runs []history.Run
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		col := strings.Split(sc.Text(), "\t")
		if len(col) < 10 {
			continue
		}
		repo, workflow, run, created, event, branch, sha, job, conclusion := col[0], col[1], col[2], col[3], col[4], col[5], col[6], col[7], col[9]
		if event == "pull_request" || event == "push" && branch != "main" || seen[job] {
			continue
		}
		seen[job] = true
		lf, err := os.Open(filepath.Join(logs, strings.ReplaceAll(repo, "/", "_"), job+".log.gz"))
		if err != nil {
			continue
		}
		zr, err := gzip.NewReader(lf)
		if err != nil {
			lf.Close()
			continue
		}
		sections := history.Parse(zr, patterns)
		lf.Close()
		if len(sections) == 0 {
			continue
		}
		runs = append(runs, history.Run{
			Date:     created[:10],
			Source:   fmt.Sprintf("%s %s %s", repo[strings.Index(repo, "/")+1:], workflow, run),
			Suite:    sha,
			Green:    conclusion == "success",
			Sections: sections,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	fillVersions(runs)
	return runs, nil
}

// fillVersions gives a section that printed no version (probe runs before
// --agent-version-from printed "installed" alone) the version the other
// jobs of its run read for the agent, when they read exactly one.
func fillVersions(runs []history.Run) {
	seen := map[string]map[string]bool{} // source + agent → versions
	for _, r := range runs {
		for _, s := range r.Sections {
			if s.Version != "" {
				k := r.Source + " " + s.Agent
				if seen[k] == nil {
					seen[k] = map[string]bool{}
				}
				seen[k][s.Version] = true
			}
		}
	}
	for _, r := range runs {
		for i := range r.Sections {
			s := &r.Sections[i]
			if vs := seen[r.Source+" "+s.Agent]; s.Version == "" && len(vs) == 1 {
				for v := range vs {
					s.Version = v
				}
			}
		}
	}
}
