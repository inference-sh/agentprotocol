// Command history builds harness/history/<agent>.json, which the harness
// package embeds, from downloaded CI job
// logs: every version of each agent the suite ran, and each check's outcome
// on each version. It reads an index of jobs, one tab-separated line each,
//
//	repo  workflow  run  created  event  branch  sha  job  name  conclusion
//
// and the logs as <logs>/<repo with / as _>/<job>.log.gz. Pull request runs
// are left out: they test a change, not the agent. It prints each settled
// change of a check's outcome between two versions.
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
	out := flag.String("out", "../harness/history", "directory to write <agent>.json to")
	runnerDir := flag.String("runner", "runner", "the runner's source, read for check IDs")
	flag.Parse()
	if *index == "" || *logs == "" {
		flag.Usage()
		os.Exit(2)
	}
	patterns, err := history.Patterns(*runnerDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	runs, err := readRuns(*index, *logs, patterns)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	agents := history.Fold(runs)
	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, n := range names {
		a := agents[n]
		b, _ := json.MarshalIndent(a, "", "  ")
		if err := os.WriteFile(filepath.Join(*out, n+".json"), append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("== %s: %d versions, %s to %s\n", n, len(a.Versions), a.Versions[0].Version, a.Versions[len(a.Versions)-1].Version)
		for _, c := range history.Changes(a) {
			mark := "suite?"
			if c.SameSuite {
				mark = "AGENT "
			}
			fmt.Printf("  %s %s→%s  %s %s→%s  %s\n", mark, c.After, c.From, c.Check, c.Was, c.Now, c.Message)
		}
	}
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
		repo, workflow, run, created, event, branch, sha, job := col[0], col[1], col[2], col[3], col[4], col[5], col[6], col[7]
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
