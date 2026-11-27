package runner

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/inference-sh/agentprotocol/conformance/server"
)

// checkIsolation asks whether anything from the real home directory reached
// the model. Every run gets a home of its own, so a request that names a path
// under the real one came from outside the test: an agent that resolves its
// config from the password database instead of $HOME, or a file the suite did
// not plant. Either makes the run's other checks depend on whoever's machine
// it ran on, and for a user it is context from outside the session.
func (r *TestRunner) checkIsolation(phase string, entries []server.LogEntry) {
	real := filepath.Clean(r.realHome())
	if real == "" || real == "/" || r.home == "" || filepath.Clean(r.home) == real {
		return
	}
	// A work dir under the real home (TMPDIR set there) names it legitimately.
	if strings.HasPrefix(filepath.Clean(r.workDir())+"/", real+"/") {
		return
	}
	if len(entries) == 0 {
		return
	}
	fmt.Printf("[check] isolation (%s)\n", phase)
	needle := []byte(real + "/")
	for _, e := range entries {
		if i := bytes.Index(e.Body, needle); i >= 0 {
			from, to := max(0, i-60), min(len(e.Body), i+len(needle)+80)
			r.finding("isolation:real-home", fmt.Sprintf("%s: a request to %s named a path under the real home %s: …%s…",
				phase, e.Path, real, strings.ReplaceAll(string(e.Body[from:to]), "\n", " ")))
			return
		}
	}
	r.pass("isolation", fmt.Sprintf("%s: no request named a path under the real home", phase))
}

// realHome is the home directory the suite was started with; tests set
// testRealHome.
func (r *TestRunner) realHome() string {
	if r.testRealHome != "" {
		return r.testRealHome
	}
	return originalHome
}
