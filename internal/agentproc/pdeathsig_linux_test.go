package agentproc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The host process for TestChildDiesWithSIGKILLedHost: it starts an agent,
// reports the agent's pid and waits to be killed.
func TestHelperHost(t *testing.T) {
	if os.Getenv("AGENTPROC_HELPER_HOST") != "1" {
		t.Skip("helper process")
	}
	p, err := Start(Options{Command: "sleep", Args: []string{"60"}})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println(p.cmd.Process.Pid)
	select {}
}

func TestChildDiesWithSIGKILLedHost(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep")
	}
	host := exec.Command(os.Args[0], "-test.run=^TestHelperHost$")
	host.Env = append(os.Environ(), "AGENTPROC_HELPER_HOST=1")
	stdout, err := host.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read agent pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("helper said %q", line)
	}

	_ = host.Process.Kill()
	_ = host.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("agent %d outlived its SIGKILLed host", pid)
}

// alive reports whether pid is a running process; a zombie waiting for
// whichever process adopted it counts as dead.
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] != 'Z'
}
