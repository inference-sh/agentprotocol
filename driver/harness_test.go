package driver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/driver"
	"github.com/inference-sh/agentprotocol/harness"
)

func TestDriverNamesMatchBackendKinds(t *testing.T) {
	for name, want := range map[string]string{
		harness.DriverACP:        driver.KindACP,
		harness.DriverClaudeCode: driver.KindClaude,
		harness.DriverCodex:      driver.KindCodex,
		harness.DriverPi:         driver.KindPi,
	} {
		if name != want {
			t.Errorf("harness driver %q, backend kind %q", name, want)
		}
	}
}

func TestForHarness(t *testing.T) {
	for name, h := range harness.All {
		b, err := driver.ForHarness(h, nil)
		if h.DriverKind() == "" {
			if err == nil {
				t.Errorf("%s: no driver, but ForHarness returned %T", name, b)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if b.Kind() != h.DriverKind() {
			t.Errorf("%s: backend %s, registry says %s", name, b.Kind(), h.DriverKind())
		}
		if acp, ok := b.(*driver.ACPBackend); ok {
			for _, a := range acp.Args {
				for _, auto := range h.ACPAutoApproveArgs {
					if a == auto {
						t.Errorf("%s: auto-approve argument %q passed", name, a)
					}
				}
			}
		}
	}
	for _, name := range []string{"claude", "codex", "pi"} {
		if got := harness.All[name].DriverKind(); got == harness.DriverACP || got == "" {
			t.Errorf("%s runs on %q, want its native driver", name, got)
		}
	}
}

func TestForHarnessVersion(t *testing.T) {
	pi := harness.All["pi"]

	_, v, err := driver.ForHarnessVersion(pi, "0.80.3", nil)
	var uv *driver.UnsupportedVersionError
	if !errors.As(err, &uv) || v.Level != harness.OlderThanSupported || uv.Verdict.Reason != v.Reason {
		t.Fatalf("0.80.3: %v, %+v", err, v)
	}
	if len(v.UpgradeCmd) == 0 || v.TestedMin == "" {
		t.Errorf("refusal carries no upgrade data: %+v", v)
	}

	for _, ver := range []string{pi.Tested.Max, "99.0.0", "", "0.84.0"} {
		b, v, err := driver.ForHarnessVersion(pi, ver, nil)
		if err != nil {
			t.Fatalf("%q: %v", ver, err)
		}
		if b.(*driver.PiBackend).Version != ver {
			t.Errorf("%q: backend not told the version", ver)
		}
		want := map[string]harness.SupportLevel{pi.Tested.Max: harness.Supported, "99.0.0": harness.NewerThanTested, "": harness.SupportUnknown, "0.84.0": harness.OlderThanTested}[ver]
		if v.Level != want {
			t.Errorf("%q: %s, want %s", ver, v.Level, want)
		}
	}

	b, v, err := driver.ForHarnessVersion(harness.All["codex"], "codex-cli 0.98.0", nil)
	if err != nil || v.Level != harness.OlderThanTested || b.(*driver.CodexBackend).Version != "codex-cli 0.98.0" {
		t.Fatalf("codex 0.98.0: %v %+v", err, v)
	}
	if b.Capabilities().Steer {
		t.Error("codex 0.98.0 has no turn/steer, but reports Steer")
	}
	if !(&driver.CodexBackend{}).Capabilities().Steer || !(&driver.CodexBackend{Version: "0.156.1"}).Capabilities().Steer {
		t.Error("codex with turn/steer reports no Steer")
	}

	if _, _, err := driver.ForHarnessVersion(harness.All["windsurf"], "1.0", nil); err == nil || errors.As(err, &uv) {
		t.Errorf("windsurf: want the no-driver error, got %v", err)
	}
}

// pi's --session-id is recorded from 0.76.0: an older pi cannot resume, and
// the backend says so before starting anything.
func TestPiResumeNeedsSessionID(t *testing.T) {
	old := &driver.PiBackend{Command: "/nonexistent/pi-would-fail-if-run", Version: "0.75.0"}
	if old.Capabilities().Resume {
		t.Error("pi 0.75.0 reports Resume")
	}
	_, err := old.Open(context.Background(), driver.SessionConfig{ResumeSessionID: "abc"})
	if err == nil || !strings.Contains(err.Error(), "--session-id") {
		t.Errorf("resume on 0.75.0: %v", err)
	}
	for _, v := range []string{"", "0.87.1"} {
		if !(&driver.PiBackend{Version: v}).Capabilities().Resume {
			t.Errorf("pi %q: Resume false", v)
		}
	}
}
