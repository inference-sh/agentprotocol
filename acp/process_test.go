package acp

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// An agent that keeps running after its stdin closes must not hold Wait
// forever. Cursor's `agent acp` (2026.09.23) is one: it answers initialize,
// then ignores end of input.
func TestWaitKillsAnAgentThatIgnoresEOF(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	script := `read line
id=$(printf '%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}\n' "$id"
exec sleep 60 </dev/null`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Spawn(ctx, ProcessConfig{Command: "sh", Args: []string{"-c", script}}, ClientInfo{Name: "test", Version: "1"}, Handler{})
	if err != nil {
		t.Fatal(err)
	}
	p.ShutdownGrace = -1
	p.ExitGrace = 200 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return for an agent that ignores end of input")
	}
}
