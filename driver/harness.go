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
//
// ForHarness does not look at the installed version. A caller that has it
// (DetectResult.Version) should use ForHarnessVersion, or read
// harness.Support itself.
func ForHarness(h harness.Harness, env []string) (Backend, error) {
	switch kind := h.DriverKind(); kind {
	case harness.DriverClaudeCode:
		return &ClaudeBackend{Command: h.Binary, Env: env}, nil
	case harness.DriverCodex:
		return &CodexBackend{Command: h.Binary, Env: env}, nil
	case harness.DriverPi:
		return &PiBackend{Command: h.Binary, Env: env}, nil
	case harness.DriverACP:
		return &ACPBackend{Command: h.ACPCmd[0], Args: append([]string(nil), h.ACPCmd[1:]...), Env: env}, nil
	case "":
		return nil, fmt.Errorf("driver: %s has no session driver", h.Name)
	default:
		return nil, fmt.Errorf("driver: %s names unknown driver %q", h.Name, kind)
	}
}

// UnsupportedVersionError is ForHarnessVersion's refusal: the installed
// version is older than any inference has tested. Verdict carries the
// user-facing reason, the tested range and the upgrade command.
type UnsupportedVersionError struct {
	Verdict harness.SupportVerdict
}

func (e *UnsupportedVersionError) Error() string { return "driver: " + e.Verdict.Reason }

// ForHarnessVersion is ForHarness for a caller that knows the installed
// version. It returns the harness.Support verdict with the backend, and
// refuses only an OlderThanSupported version, with an
// *UnsupportedVersionError; a newer or unreadable version gets a backend and
// the verdict to warn with. The backend is told the version, so a feature
// the registry records for a version range (harness.Harness.Features) is
// used only where the installed version has it.
func ForHarnessVersion(h harness.Harness, version string, env []string) (Backend, harness.SupportVerdict, error) {
	v := h.Support(version)
	if v.Level == harness.OlderThanSupported {
		return nil, v, &UnsupportedVersionError{Verdict: v}
	}
	b, err := ForHarness(h, env)
	if err != nil {
		return nil, v, err
	}
	if pi, ok := b.(*PiBackend); ok {
		pi.Version = version
	}
	return b, v, nil
}
