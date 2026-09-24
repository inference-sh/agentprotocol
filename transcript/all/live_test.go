package all

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/harness"
	"github.com/inference-sh/agentprotocol/internal/proc"
	"github.com/inference-sh/agentprotocol/transcript"
)

// The helper is this test binary started under another name, standing in
// for an agent: it opens the file LIVE_HELPER_FILE names, reports ready, and
// holds it until its stdin closes.
func TestMain(m *testing.M) {
	if f := os.Getenv("LIVE_HELPER_FILE"); f != "" {
		h, err := os.Open(f)
		if err != nil {
			os.Exit(2)
		}
		defer h.Close()
		os.Stdout.WriteString("ready\n")
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const testAgent = "livetest"

func withTestAgent(t *testing.T) {
	t.Helper()
	harness.All[testAgent] = harness.Harness{Name: testAgent, Binary: "livetest-agent"}
	t.Cleanup(func() { delete(harness.All, testAgent) })
}

// startAgent runs the helper as livetest-agent, with HOME home, in cwd,
// holding file open. It returns the pid and a function that stops it.
func startAgent(t *testing.T, home, cwd, file string) (int, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Args[0] = "livetest-agent"
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "HOME="+home, "LIVE_HELPER_FILE="+file)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper did not start: %q", line)
	}
	stop := func() { stdin.Close(); cmd.Wait() }
	t.Cleanup(stop)
	return cmd.Process.Pid, stop
}

// TestLiveAgainstRealProcess runs the decision against the real process
// table: a process of the agent holding a session's file proves that
// session live; another session in the same directory is only possibly
// live; one elsewhere is idle; with the process gone, all are idle.
func TestLiveAgainstRealProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process table is read on Linux")
	}
	withTestAgent(t)
	home, project, other := t.TempDir(), t.TempDir(), t.TempDir()
	held := filepath.Join(project, "held.jsonl")
	if err := os.WriteFile(held, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	session := func(root, cwd string) Session {
		return Session{Agent: testAgent, Info: transcript.Info{ID: "s", CWD: cwd, Root: root, Path: root, Updated: old}}
	}
	pid, stop := startAgent(t, home, project, held)

	p := NewProbe(home)
	if l := p.Live(session(held, project)); l.State != transcript.LiveActive || l.Evidence != transcript.EvidenceOpenFile || l.PID != pid || l.Heuristic {
		t.Errorf("held session = %+v, want active on open-file proof from pid %d", l, pid)
	}
	if l := p.Live(session(filepath.Join(project, "other.jsonl"), project)); l.State != transcript.LiveActive || l.Evidence != transcript.EvidenceProcessInCwd || !l.Heuristic {
		t.Errorf("sibling session = %+v, want a labelled process-in-cwd heuristic", l)
	}
	if l := p.Live(session(filepath.Join(other, "x.jsonl"), other)); l.State != transcript.LiveIdle || l.Evidence != transcript.EvidenceNoProcessInCwd || !l.Heuristic {
		t.Errorf("session elsewhere = %+v, want heuristic idle", l)
	}
	// A process of the agent under another HOME is not this user's agent.
	if l := NewProbe(t.TempDir()).Live(session(held, project)); l.State != transcript.LiveIdle || l.Evidence != transcript.EvidenceNoProcess {
		t.Errorf("probe for another home = %+v, want no process", l)
	}

	stop()
	if l := NewProbe(home).Live(session(held, project)); l.State != transcript.LiveIdle || l.Evidence != transcript.EvidenceNoProcess || l.Heuristic {
		t.Errorf("after exit = %+v, want idle on no-process proof", l)
	}
}

// TestLiveHolders: an in-use marker naming a running process proves the
// session live on any Unix; a marker whose process is gone does not.
func TestLiveHolders(t *testing.T) {
	withTestAgent(t)
	p := &Probe{home: "/h", ok: false, now: time.Now()}
	s := Session{Agent: testAgent, Info: transcript.Info{ID: "s", Holders: []int{os.Getpid()}}}
	if runtime.GOOS != "windows" {
		if l := p.Live(s); l.State != transcript.LiveActive || l.Evidence != transcript.EvidenceLockFile || l.PID != os.Getpid() {
			t.Errorf("live holder = %+v", l)
		}
	}
	s.Holders = []int{deadPID(t)}
	if l := p.Live(s); l.State != transcript.LiveUnknown {
		t.Errorf("dead holder, no process table = %+v, want unknown", l)
	}
}

// TestLiveWithoutProcessTable is the macOS path today: only markers and
// recent writes, both labelled.
func TestLiveWithoutProcessTable(t *testing.T) {
	withTestAgent(t)
	now := time.Now()
	p := &Probe{home: "/h", ok: false, now: now}
	recent := Session{Agent: testAgent, Info: transcript.Info{ID: "s", Updated: now.Add(-10 * time.Second)}}
	if l := p.Live(recent); l.State != transcript.LiveActive || l.Evidence != transcript.EvidenceRecentWrite || !l.Heuristic {
		t.Errorf("recent write = %+v", l)
	}
	quiet := Session{Agent: testAgent, Info: transcript.Info{ID: "s", Updated: now.Add(-time.Hour)}}
	if l := p.Live(quiet); l.State != transcript.LiveUnknown || l.Evidence != transcript.EvidenceNone {
		t.Errorf("quiet session = %+v, want unknown: a quiet session may still be open", l)
	}
}

// TestLiveSharedStore: a store that keeps every session in one database
// has no Root, so an agent holding that database open says nothing about
// which session; only the directory decides.
func TestLiveSharedStore(t *testing.T) {
	withTestAgent(t)
	p := &Probe{home: "/h", ok: true, now: time.Now(), procs: []proc.Process{
		{PID: 42, Argv: []string{"livetest-agent"}, CWD: "/work/a", Home: "/h", Open: []string{"/h/.store/sessions.db"}},
	}}
	s := Session{Agent: testAgent, Info: transcript.Info{ID: "s", CWD: "/work/b", Path: "/h/.store/sessions.db", Updated: time.Now().Add(-time.Hour)}}
	if l := p.Live(s); l.State != transcript.LiveIdle || l.Evidence != transcript.EvidenceNoProcessInCwd {
		t.Errorf("shared store, agent elsewhere = %+v", l)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if proc.Alive(cmd.Process.Pid) {
		t.Skip("pid reused")
	}
	return cmd.Process.Pid
}
