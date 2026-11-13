package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/inference-sh/agentprotocol/conformance/runner"
	"github.com/inference-sh/agentprotocol/harness"
)

// agentVersion is --agent-version. It lives here rather than in main's flag
// block so the flag and its check stay in one place.
var agentVersion = flag.String("agent-version", "",
	"install this exact version of the --harness agent instead of the latest (npm pkg@v, pip pkg==v, or the agent's pinned installer; see README)")

// agentVersionFrom is --agent-version-from: the version an earlier run's
// report says it tested. The nightly passes the first run's report to the
// runs after it, so one job measures one version even when the agent
// publishes mid-job (droid 1.6.0 on 2026-11-07: the npm package was out at
// 04:09:32 and its linux-x64 binary at 04:12:06, and the run that installed
// between them could not start droid).
var agentVersionFrom = flag.String("agent-version-from", "",
	"install the version this --report file says it tested, when the agent can install a chosen version; otherwise the latest")

// agentMinAge is --agent-min-age. The nightly sets it so its first run of an
// npm agent never installs a release whose platform binaries are still
// publishing; runs after the first take that run's version.
var agentMinAge = flag.Duration("agent-min-age", 0,
	"with no version chosen, install the newest npm release at least this old instead of the latest (0: the latest)")

// resolveAgentVersion sets --agent-version from --agent-version-from. A
// report with no version, or an agent with no way to install a chosen one,
// leaves the latest and says so.
func resolveAgentVersion(targets []string) {
	if *agentVersionFrom == "" || *agentVersion != "" {
		return
	}
	if len(targets) != 1 {
		fmt.Fprintf(os.Stderr, "--agent-version-from needs exactly one --harness, got %d\n", len(targets))
		os.Exit(2)
	}
	h := harness.All[targets[0]]
	var rep struct {
		Results []struct{ Harness, Version string } `json:"results"`
	}
	data, err := os.ReadFile(*agentVersionFrom)
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil {
		fmt.Printf("  → %s: %v; installing the latest\n", *agentVersionFrom, err)
		return
	}
	for _, r := range rep.Results {
		if r.Harness != h.Name {
			continue
		}
		if v, ok := runner.PinnableVersion(h, r.Version); ok {
			*agentVersion = v
			fmt.Printf("  → %s %s, the version %s tested\n", h.Name, v, *agentVersionFrom)
			return
		}
		fmt.Printf("  → %s cannot pin %q from %s; installing the latest\n", h.Name, r.Version, *agentVersionFrom)
		return
	}
	fmt.Printf("  → %s has no %s result; installing the latest\n", *agentVersionFrom, h.Name)
}

// settleAgentVersion sets --agent-version to an npm agent's newest release at
// least --agent-min-age old when nothing else chose a version. Any failure
// to ask npm leaves the latest and says so.
func settleAgentVersion(targets []string) {
	if *agentMinAge <= 0 || *agentVersion != "" || len(targets) != 1 {
		return
	}
	h := harness.All[targets[0]]
	if len(h.InstallCmd) == 0 || filepath.Base(h.InstallCmd[0]) != "npm" {
		return
	}
	if _, err := runner.PinnedInstallCmd(h, "0.0.0"); err != nil {
		return
	}
	pkg := h.InstallCmd[len(h.InstallCmd)-1]
	out, err := exec.Command("npm", "view", pkg, "time", "dist-tags.latest", "--json").Output()
	var info struct {
		Time   map[string]string `json:"time"`
		Latest string            `json:"dist-tags.latest"`
	}
	if err == nil {
		err = json.Unmarshal(out, &info)
	}
	if err != nil {
		fmt.Printf("  → npm view %s: %v; installing the latest\n", pkg, err)
		return
	}
	times := map[string]time.Time{}
	for v, ts := range info.Time {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			times[v] = t
		}
	}
	v, ok := runner.SettledVersion(times, info.Latest, time.Now(), *agentMinAge)
	if !ok {
		fmt.Printf("  → no %s release is %s old; installing the latest\n", pkg, *agentMinAge)
		return
	}
	*agentVersion = v
	if v != info.Latest {
		fmt.Printf("  → %s %s: latest %s is younger than %s\n", h.Name, v, info.Latest, *agentMinAge)
	}
}

// checkAgentVersion refuses --agent-version for anything but one agent: a
// version means nothing across agents.
func checkAgentVersion(targets []string) {
	if *agentVersion == "" || len(targets) == 1 {
		return
	}
	fmt.Fprintf(os.Stderr, "--agent-version needs exactly one --harness, got %d\n", len(targets))
	os.Exit(2)
}
