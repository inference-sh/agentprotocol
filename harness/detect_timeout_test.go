package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestGetVersionIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	dir := t.TempDir()
	stall := filepath.Join(dir, "stall")
	// A version check that never returns, and a grandchild that keeps the
	// output pipe open after the shell is killed.
	if err := os.WriteFile(stall, []byte("#!/bin/sh\nsleep 30 &\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fast := filepath.Join(dir, "fast")
	if err := os.WriteFile(fast, []byte("#!/bin/sh\necho 'fast 1.2.3'\necho second line\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	old := VersionTimeout
	VersionTimeout = 300 * time.Millisecond
	defer func() { VersionTimeout = old }()

	start := time.Now()
	if v := getVersion(stall); v != "" {
		t.Errorf("stalled binary reported version %q", v)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("getVersion took %s past a %s timeout", d, VersionTimeout)
	}
	if v := getVersion(fast); v != "fast 1.2.3" {
		t.Errorf("fast binary: %q", v)
	}
}

func TestVersionLine(t *testing.T) {
	cases := []struct{ stdout, stderr, want string }{
		{"codex-cli 0.130.0\n", "WARNING: proceeding, even though we could not update PATH\n", "codex-cli 0.130.0"},
		{"", "WARNING: something\nclaude 2.1.0\n", "claude 2.1.0"},
		{"Loading...\n1.4.2\n", "", "1.4.2"},
		{"no version here\n", "", "no version here"},
		{"", "only stderr\n", "only stderr"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := versionLine([]byte(c.stdout), []byte(c.stderr)); got != c.want {
			t.Errorf("versionLine(%q, %q) = %q, want %q", c.stdout, c.stderr, got, c.want)
		}
	}
}

func TestDetectAllIsSortedAndComplete(t *testing.T) {
	results := DetectAll()
	if len(results) != len(All) {
		t.Fatalf("%d results for %d agents", len(results), len(All))
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Name >= results[i].Name {
			t.Fatalf("not sorted at %d: %s, %s", i, results[i-1].Name, results[i].Name)
		}
	}
}
