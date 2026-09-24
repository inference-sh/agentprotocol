package harness

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A status check with several providers runs them at once: pi starts one
// Node process per provider, and in turn they outlast any enrollment budget.
func TestCheckLoginProvidersRunInParallel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	dir := t.TempDir()
	check := filepath.Join(dir, "check")
	// Every provider takes 600ms; "yes" is logged in, the rest are out.
	script := "#!/bin/sh\nsleep 0.6\nif [ \"$1\" = yes ]; then echo ready; exit 0; fi\necho not_ready; exit 1\n"
	if err := os.WriteFile(check, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	status := func(providers ...string) *StatusCheck {
		return &StatusCheck{
			Cmd:               []string{check, "{{.Provider}}"},
			Providers:         providers,
			LoggedOutExit:     1,
			LoggedOutContains: "not_ready",
		}
	}
	for _, tc := range []struct {
		name      string
		providers []string
		want      LoginState
	}{
		{"one logged in", []string{"a", "b", "yes", "c", "d"}, LoginLoggedIn},
		{"all logged out", []string{"a", "b", "c", "d", "e"}, LoginLoggedOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "zz-test-providers"
			All[name] = Harness{Name: name, Auth: Auth{Status: status(tc.providers...)}}
			t.Cleanup(func() { delete(All, name) })

			start := time.Now()
			r, err := CheckLogin(context.Background(), name)
			took := time.Since(start)
			if err != nil || r.State != tc.want {
				t.Fatalf("got %v, %v; want %v", r.State, err, tc.want)
			}
			// Five 600ms checks in turn would take 3s.
			if took > 1500*time.Millisecond {
				t.Errorf("took %s: providers ran one after another", took)
			}
		})
	}
}
