package runner

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/inference-sh/agentprotocol/conformance/server"
)

// portableToolName is the tool name every API this suite mocks accepts:
// OpenAI and Anthropic both reject a name over 64 characters or with anything
// but letters, digits, _ and -. An agent that builds names from MCP server and
// tool names can pass that without noticing on the API it was written for,
// and a gateway that forwards its requests to another provider gets a 400.
var portableToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkToolNames reports tool names in this phase's requests that some
// provider would refuse.
func (r *TestRunner) checkToolNames(phase string, entries []server.LogEntry) {
	seen := map[string]bool{}
	for _, e := range entries {
		for _, n := range server.ToolNamesIn(e.Body) {
			seen[n] = true
		}
	}
	if len(seen) == 0 {
		return
	}
	fmt.Printf("[check] tool names (%s)\n", phase)
	var bad []string
	for n := range seen {
		if !portableToolName.MatchString(n) {
			bad = append(bad, fmt.Sprintf("%q (%d chars)", n, len(n)))
		}
	}
	if len(bad) == 0 {
		r.pass("tool-names", fmt.Sprintf("%s: all %d tool names are at most 64 of [A-Za-z0-9_-]", phase, len(seen)))
		return
	}
	sort.Strings(bad)
	r.finding("tool-names", fmt.Sprintf("%s: %s declared tool names some providers refuse: %s", phase, r.harness.Name, strings.Join(bad, ", ")))
}
