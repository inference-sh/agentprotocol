//go:build unix

package agentproc

import (
	"bufio"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a strings.Builder safe for the copier and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The agent writes its error to stderr and then answers on stdout. Once the
// answer is read, a flush must have the error in the writer: the two pipes
// are read independently, and without it the answer often wins.
func TestFlushStderrHasWhatWasWrittenBeforeTheAnswer(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for i := 0; i < 200; i++ {
		var errs syncBuffer
		p, err := Start(Options{Command: "sh", Args: []string{"-c", `echo "HTTP 401: User not found." >&2; echo answer; read _`}, Stderr: &errs})
		if err != nil {
			t.Fatal(err)
		}
		readDone := make(chan struct{})
		lines := make(chan string, 1)
		go func() {
			defer close(readDone)
			sc := bufio.NewScanner(p.Stdout)
			for sc.Scan() {
				lines <- sc.Text()
			}
		}()
		p.Reap(readDone)
		select {
		case <-lines:
		case <-time.After(5 * time.Second):
			t.Fatal("no answer on stdout")
		}
		p.FlushStderr()
		if got := errs.String(); !strings.Contains(got, "HTTP 401") {
			_ = p.Kill()
			t.Fatalf("run %d: stderr after the flush = %q", i, got)
		}
		_ = p.Kill()
		<-p.Exited()
	}
}

// A flush after the child is gone, or with stderr discarded, returns at once.
func TestFlushStderrDoesNotWait(t *testing.T) {
	var errs syncBuffer
	p, err := Start(Options{Command: "sh", Args: []string{"-c", `echo bye >&2`}, Stderr: &errs})
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() { _, _ = p.Stdout.Read(make([]byte, 1)); close(readDone) }()
	p.Reap(readDone)
	<-p.Exited()
	if got := errs.String(); got != "bye\n" {
		t.Errorf("stderr once exited = %q", got)
	}
	done := make(chan struct{})
	go func() {
		p.FlushStderr()
		(&Process{}).FlushStderr()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flush waited on a finished or discarded stderr")
	}
}

// Flushes from several goroutines at once each return, and each sees what
// was written before it.
func TestFlushStderrConcurrent(t *testing.T) {
	var errs syncBuffer
	p, err := Start(Options{Command: "sh", Args: []string{"-c", `echo first >&2; echo ready; read _`}, Stderr: &errs})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill(); <-p.Exited() }()
	readDone := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(p.Stdout)
		if sc.Scan() {
			close(ready)
		}
		for sc.Scan() {
		}
	}()
	p.Reap(readDone)
	<-ready
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.FlushStderr()
			if !strings.Contains(errs.String(), "first") {
				t.Error("a flush returned before the line written ahead of it")
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent flushes did not all return")
	}
}

// Flushing does not end the copy: what the child writes to stderr after many
// flushes still arrives.
func TestStderrFlowsAfterFlushes(t *testing.T) {
	var errs syncBuffer
	p, err := Start(Options{Command: "sh", Args: []string{"-c", `echo first >&2; echo ready; read _; echo later >&2; echo done`}, Stderr: &errs})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill(); <-p.Exited() }()
	readDone := make(chan struct{})
	lines := make(chan string, 2)
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(p.Stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	p.Reap(readDone)
	<-lines // ready
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.FlushStderr() }()
	}
	wg.Wait()
	if _, err := p.Stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lines: // done
	case <-time.After(5 * time.Second):
		t.Fatal("no second answer")
	}
	p.FlushStderr()
	if got := errs.String(); got != "first\nlater\n" {
		t.Errorf("stderr = %q", got)
	}
}

// A grandchild that keeps stderr open must not hold reaping either: what the
// agent wrote is copied, and Exited closes after DrainTimeout.
func TestReapDoesNotWaitForAGrandchildHoldingStderr(t *testing.T) {
	var errs syncBuffer
	p, err := Start(Options{Command: "sh", Args: []string{"-c", `echo bye >&2; sleep 30 & exit 3`}, Stderr: &errs})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill() }()
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, p.Stdout); close(readDone) }()
	p.Reap(readDone)
	select {
	case <-p.Exited():
	case <-time.After(DrainTimeout + 3*time.Second):
		t.Fatal("reaping waited for the grandchild holding stderr")
	}
	if got := errs.String(); got != "bye\n" {
		t.Errorf("stderr = %q", got)
	}
}
