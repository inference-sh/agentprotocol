package harness

import (
	"context"
	"errors"
	"testing"
)

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		have, min string
		want      bool
	}{
		{"0.87.1", "0.84.1", true},
		{"0.84.1", "0.84.1", true},
		{"0.80.3", "0.84.1", false},
		{"2.1.281 (Claude Code)", "2.1.281", true},
		{"claude 2.1.9", "2.1.281", false},
		{"2026.09.23-86fc751", "2026.09.23", true},
		{"2026.05.16-0338208", "2026.09.23", false},
		{"codex-cli 0.156.1", "0.156.1", true},
		{"", "0.1.0", false},
		{"garbage", "0.1.0", false},
		{"1.2", "1.2.0", true},
	} {
		if got := versionAtLeast(c.have, c.min); got != c.want {
			t.Errorf("versionAtLeast(%q, %q) = %v", c.have, c.min, got)
		}
	}
}

func TestEveryStatusCheckHasMinVersion(t *testing.T) {
	for name, h := range All {
		if s := h.Auth.Status; s != nil && s.MinVersion == "" {
			t.Errorf("%s: status check without MinVersion", name)
		}
	}
}

// Below MinVersion nothing runs: an old pi reads "auth check" as a prompt.
func TestCheckLoginVersionRefusesOldAgents(t *testing.T) {
	const name = "zz-test-old"
	All[name] = Harness{Name: name, Auth: Auth{Status: &StatusCheck{
		Cmd:        []string{"/nonexistent/would-fail-if-run"},
		MinVersion: "0.84.1",
	}}}
	defer delete(All, name)
	for _, v := range []string{"0.80.3", ""} {
		r, err := CheckLoginVersion(context.Background(), name, v)
		if !errors.Is(err, ErrStatusCheckUnsupported) || r.State != LoginUnknown {
			t.Errorf("version %q: got %v, %v", v, r.State, err)
		}
	}
}
