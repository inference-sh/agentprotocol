package agentprotocol_test

import (
	"encoding/json"
	"testing"

	ap "github.com/inference-sh/agentprotocol"
)

// The api, belt's daemon and the web app compare these values as strings
// across releases, so each one is pinned to its wire spelling.
func TestHarnessWireValues(t *testing.T) {
	for _, c := range [][2]string{
		{string(ap.DriverACP), "acp"},
		{string(ap.DriverClaudeCode), "claude-code"},
		{string(ap.DriverCodex), "codex"},
		{string(ap.DriverPi), "pi"},

		{string(ap.SupportLevelSupported), "supported"},
		{string(ap.SupportLevelNewerThanTested), "newer-than-tested"},
		{string(ap.SupportLevelOlderThanTested), "older-than-tested"},
		{string(ap.SupportLevelOlderThanSupported), "older-than-supported"},
		{string(ap.SupportLevelUnknown), "unknown"},

		{string(ap.LiveUnknown), "unknown"},
		{string(ap.LiveIdle), "idle"},
		{string(ap.LiveActive), "active"},

		{string(ap.EvidenceLockFile), "lock-file"},
		{string(ap.EvidenceOpenFile), "open-file"},
		{string(ap.EvidenceNoProcess), "no-process"},
		{string(ap.EvidenceProcessInCwd), "process-in-cwd"},
		{string(ap.EvidenceNoProcessInCwd), "no-process-in-cwd"},
		{string(ap.EvidenceHeldElsewhere), "held-elsewhere"},
		{string(ap.EvidenceRecentWrite), "recent-write"},
		{string(ap.EvidenceNone), "none"},
	} {
		if c[0] != c[1] {
			t.Errorf("wire value %q, want %q", c[0], c[1])
		}
	}
}

func TestSessionLivenessJSON(t *testing.T) {
	b, err := json.Marshal(ap.SessionLiveness{State: ap.LiveActive, Evidence: ap.EvidenceLockFile, PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"state":"active","evidence":"lock-file","heuristic":false,"pid":42}`
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}
