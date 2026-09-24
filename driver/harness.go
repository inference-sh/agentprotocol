package driver

import (
	"fmt"

	"github.com/inference-sh/agentprotocol/harness"
)

// ForHarness returns the backend that runs h as a session against the
// user's own install and login.
//
// It passes the agent's launch command and nothing else from the registry:
// ACPArgs and the SDK arguments pin models and endpoints for harness-test's
// mock, and ACPAutoApproveArgs would take approvals away from the person.
// env is the child environment, where an account profile is selected; nil
// inherits the parent's.
func ForHarness(h harness.Harness, env []string) (Backend, error) {
	switch kind := h.DriverKind(); kind {
	case harness.DriverClaudeCode:
		return &ClaudeBackend{Command: h.Binary, Env: env}, nil
	case harness.DriverCodex:
		return &CodexBackend{Command: h.Binary, Env: env}, nil
	case harness.DriverACP:
		return &ACPBackend{Command: h.ACPCmd[0], Args: append([]string(nil), h.ACPCmd[1:]...), Env: env}, nil
	case "":
		return nil, fmt.Errorf("driver: %s has no session driver", h.Name)
	default:
		return nil, fmt.Errorf("driver: %s names unknown driver %q", h.Name, kind)
	}
}
