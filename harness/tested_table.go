package harness

// ciGreen is the evidence most rows share: the agent's harness-test
// workflow (belt-sh/harness-test, .github/workflows/<agent>.yml) finished
// green with this version installed, every job passing, including the
// session job ("test (acp)") that drives the agent through its DriverKind
// backend. Versions are read from each green run's "→ version:" log line;
// the runs searched were 2026-09-20 to 2026-09-24, each run pinned to the
// agentprotocol release harness-test required that day.
const ciGreen = "harness-test CI green, session job included, 2026-09-20..24"

// testedTable is Tested per agent. Min is the oldest version with evidence,
// Max the newest; nothing between is claimed beyond what the Evidence says.
// Raise Max when harness-test's CI or nightly is green on a newer release;
// extend Min only with a green run pinned to the older version (README,
// "Supported versions").
var testedTable = map[string]TestedVersions{
	"claude": {Min: "2.1.281", Max: "2.1.282",
		Evidence: ciGreen + " (2.1.281, 2.1.282; 2.1.278 and 2.1.280 passed only in runs without the session job, which claude, codex, cursor and pi have run since harness-test c80ec93 on 2026-09-24); " +
			"agentprotocol CI pins 2.1.281 for the claude driver's real-binary tests"},
	"codex": {Min: "0.156.1", Max: "0.156.1",
		Evidence: ciGreen + " (0.156.1; 0.155.1 passed only in runs without the session job); agentprotocol CI pins 0.156.1; codexapp is generated from 0.156.1"},
	"copilot": {Min: "1.0.86", Max: "1.0.88",
		Evidence: ciGreen + " (1.0.86, 1.0.87, 1.0.88)"},
	"cursor": {Min: "2026.09.23", Max: "2026.09.23",
		Evidence: ciGreen + " (2026.09.23-86fc751; 2026.09.18-9a7762b passed only in runs without the session job)"},
	"droid": {Min: "0.223.0", Max: "0.226.2",
		Evidence: ciGreen + " (0.223.0, 0.223.2, 0.225.1, 0.225.2, 0.226.1, 0.226.2)"},
	"gemini": {Min: "0.60.0", Max: "0.61.0",
		Evidence: ciGreen + " (0.60.0, 0.61.0)"},
	"goose": {Min: "1.51.0", Max: "1.52.0",
		Evidence: ciGreen + " (1.51.0, 1.52.0)"},
	"grok": {Min: "1.0.34", Max: "1.0.41",
		Evidence: ciGreen + " (1.0.34, 1.0.40, 1.0.41)"},
	"hermes": {Min: "0.19.0", Max: "0.19.0",
		Evidence: ciGreen + " (0.19.0 only)"},
	"kilo": {Min: "7.7.5", Max: "7.7.9",
		Evidence: ciGreen + " (7.7.5, 7.7.6, 7.7.7, 7.7.9)"},
	"kimi": {Min: "2.0.2", Max: "2.1.1",
		Evidence: ciGreen + " (2.0.2, 2.1.0, 2.1.1)"},
	"kiro": {Min: "2.22.1", Max: "2.24.0",
		Evidence: ciGreen + " (2.22.1, 2.23.1, 2.24.0)"},
	"omp": {Min: "18.2.6", Max: "18.3.0",
		Evidence: ciGreen + " (18.2.6, 18.2.7, 18.2.8, 18.2.11, 18.3.0)"},
	"opencode": {Min: "1.18.31", Max: "1.18.32",
		Evidence: ciGreen + " (1.18.31, 1.18.32)"},
	"pi": {Min: "0.87.1", Max: "0.87.1",
		Evidence: ciGreen + " (0.87.1; 0.86.0, 0.86.1 and 0.87.0 passed only in runs without the session job); agentprotocol CI pins 0.87.1 for the pi driver's real-binary tests"},
	"qwen": {Min: "0.24.1", Max: "0.24.5",
		Evidence: ciGreen + " (0.24.1 through 0.24.5)"},
	// windsurf has no CLI: nothing to run, so nothing tested.
}

// requiresTable is Requires per agent: set only where a capability the
// session driver depends on is known to be missing below the version.
// Every other agent is refused at no version.
var requiresTable = map[string]Requirement{
	"pi": {Version: "0.80.4", Capability: "RPC agent_settled event, which the pi driver ends a turn on",
		Evidence: "pi CHANGELOG 0.80.4: \"Added extension and RPC agent_settled events\"; driver/pi.go closes a turn only on agent_settled"},
	"codex": {Version: "0.56.0", Capability: "app-server thread and turn API (thread/start, turn/start)",
		Evidence: "codex-rs/app-server-protocol/src/protocol/common.rs: thread/start, thread/resume, turn/start, turn/interrupt, turn/completed and item/* are at tag rust-v0.56.0 and absent at rust-v0.55.0"},
	// cursor: not set. `cursor-agent acp` (hidden) is in the 2026.05.16-0338208,
	// 2026.09.18-9a7762b and 2026.09.23-86fc751 bundles (dist-package/index.js,
	// command("acp")); no version without it has been found.
}

// upgradeCmds are UpgradeCmd where InstallCmd does not upgrade.
var upgradeCmds = map[string][]string{
	// pip install is a no-op for an installed package; --upgrade fetches the
	// latest release.
	"hermes": {"pip", "install", "--upgrade", "--break-system-packages", "hermes-agent[acp]"},
}

// features are Features per agent: what exists only from (or until) some
// version, with where that is recorded.
var features = map[string]map[string]VersionRange{
	"pi": {
		// --session-id, which the pi driver resumes a session with: "Added
		// --session-id to let CLI callers use an exact project-local session
		// ID" (pi CHANGELOG, 0.76.0).
		FeatureSessionID: {From: "0.76.0"},
	},
	"codex": {
		// turn/steer, which the codex driver sends a prompt into a running
		// turn with: in app-server-protocol common.rs at rust-v0.99.0, absent
		// at rust-v0.98.0.
		FeatureTurnSteer: {From: "0.99.0"},
	},
}

// FeatureTurnSteer is codex app-server's turn/steer request.
const FeatureTurnSteer = "turn-steer"

// FeatureSessionID is an agent's --session-id flag.
const FeatureSessionID = "session-id"

func init() {
	for name, t := range testedTable {
		decorate("testedTable", name, func(h *Harness) { h.Tested = t })
	}
	for name, r := range requiresTable {
		decorate("requiresTable", name, func(h *Harness) { h.Requires = r })
	}
	for name, cmd := range upgradeCmds {
		decorate("upgradeCmds", name, func(h *Harness) { h.UpgradeCmd = cmd })
	}
	for name, f := range features {
		decorate("features", name, func(h *Harness) { h.Features = f })
	}
}
