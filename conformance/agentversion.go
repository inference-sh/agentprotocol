package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

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

// checkAgentVersion refuses --agent-version for anything but one agent: a
// version means nothing across agents.
func checkAgentVersion(targets []string) {
	if *agentVersion == "" || len(targets) == 1 {
		return
	}
	fmt.Fprintf(os.Stderr, "--agent-version needs exactly one --harness, got %d\n", len(targets))
	os.Exit(2)
}
