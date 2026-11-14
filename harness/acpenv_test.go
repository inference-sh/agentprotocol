package harness

import (
	"slices"
	"testing"
)

func TestACPEnviron(t *testing.T) {
	h := Harness{ACPEnv: map[string]string{"B": "2", "A": "1"}}
	got := h.ACPEnviron([]string{"PATH=/bin", "A=old"})
	if want := []string{"PATH=/bin", "A=1", "B=2"}; !slices.Equal(got, want) {
		t.Errorf("ACPEnviron = %q, want %q", got, want)
	}
	if got := (Harness{}).ACPEnviron(nil); got != nil {
		t.Errorf("no ACPEnv, nil env = %q, want nil (inherit)", got)
	}
	if got := h.ACPEnviron(nil); !slices.Contains(got, "A=1") || len(got) < 3 {
		t.Errorf("nil env = %q, want the parent's environment with A and B", got)
	}
	if v := All["omp"].ACPEnv["OMP_MCP_STARTUP_TIMEOUT_MS"]; v != "0" {
		t.Errorf("omp OMP_MCP_STARTUP_TIMEOUT_MS = %q, want 0", v)
	}
}
