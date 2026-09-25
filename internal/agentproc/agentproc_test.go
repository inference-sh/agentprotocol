//go:build unix

package agentproc

import (
	"io"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func start(t *testing.T, script string) (*Process, chan string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	p, err := Start(Options{Command: "sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 1)
	readDone := make(chan struct{})
	go func() {
		b, _ := io.ReadAll(p.Stdout)
		out <- string(b)
		close(readDone)
	}()
	p.Reap(readDone)
	return p, out
}

// A grandchild that keeps stdout open must not hold reaping: the output the
// agent wrote before exiting is read, and Exited closes after DrainTimeout.
func TestReapDoesNotWaitForAGrandchildHoldingStdout(t *testing.T) {
	p, out := start(t, `echo hello; sleep 30 & exit 3`)
	defer func() { _ = p.Kill() }()
	select {
	case <-p.Exited():
	case <-time.After(DrainTimeout + 3*time.Second):
		t.Fatal("reaping waited on the grandchild")
	}
	if got := <-out; got != "hello\n" {
		t.Errorf("stdout = %q", got)
	}
	if code := p.State().ExitCode(); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

// Close kills a child that ignores end of input, and the kill reaches the
// processes it started.
func TestCloseKillsTheGroup(t *testing.T) {
	p, _ := start(t, `sleep 30 & sleep 30`)
	pgid, err := syscall.Getpgid(p.Pid())
	if err != nil || pgid != p.Pid() {
		t.Fatalf("pgid = %d, %v; want the child's own group %d", pgid, err, p.Pid())
	}
	if err := p.Close(100 * time.Millisecond); err == nil {
		t.Error("a killed child reported a clean exit")
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(-pgid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the group still has members after Close killed it")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
