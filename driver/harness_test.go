package driver_test

import (
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
