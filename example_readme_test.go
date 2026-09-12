package agentprotocol_test

// The README shows this library being used. A snippet that does not compile is
// worse than no snippet, so every example there is reproduced here and built
// by `go test`. Change one and change the other.

import (
	"context"
	"fmt"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/acp"
	"github.com/inference-sh/agentprotocol/driver"
)

// README: "Drive a coding agent"
func ExampleSpawn() {
	ctx := context.Background()

	proc, err := acp.Spawn(ctx, acp.ProcessConfig{
		Command: "claude-code-acp",
		Dir:     "/path/to/project",
	}, acp.ClientInfo{Name: "my-app", Version: "1.0"}, acp.Handler{
		OnUpdate: func(n acp.UpdateNotification) {
			fmt.Print(n.Update.Text())
		},
		OnPermission: func(ctx context.Context, r acp.PermissionRequest) (acp.PermissionResponse, error) {
			if id, ok := r.PickOption(acp.OptionKindAllowOnce); ok {
				return acp.Selected(id), nil
			}
			return acp.Cancelled(), nil
		},
	})
	if err != nil {
		return
	}
	defer proc.Wait()

	if _, err := proc.NewSession(ctx, "/path/to/project", nil); err != nil {
		return
	}
	_, _ = proc.Prompt(ctx, "add a test for the parser")
}

// README: choosing which account the agent uses.
func ExampleEnviron() {
	_ = acp.ProcessConfig{
		Command: "claude-code-acp",
		Env:     acp.Environ("CLAUDE_CONFIG_DIR", "/home/me/.claude-work"),
	}
}

// README: "Events"
func ExampleNewEvent() {
	runID, chatID := "run_1", "chat_1"

	ev := ap.NewEvent(ap.AgentEventToolStarted, runID, chatID,
		ap.ToolStartedPayload{ToolName: "bash", ToolType: ap.ToolTypeCall})

	if p, ok := ap.PayloadAs[ap.ToolStartedPayload](ev, ap.AgentEventToolStarted); ok {
		fmt.Println(p.ToolName)
	}
	// Output: bash
}

// README: "State"
func ExampleAgentRunState() {
	state := ap.AgentRunStateWorking

	fmt.Println(state.CanTransitionTo(ap.AgentRunStateCompleted))
	fmt.Println(state.IsTerminal())
	fmt.Println(state.IsInterrupted())
	// Output:
	// true
	// false
	// false
}

// README: "driver"
func ExampleSession() {
	ctx := context.Background()
	backend := &driver.ACPBackend{Command: "claude-code-acp"}

	sess, err := backend.Open(ctx, driver.SessionConfig{RunID: "run_1", WorkDir: "/tmp"})
	if err != nil {
		return
	}
	defer sess.Close()

	go func() {
		for ev := range sess.Events() {
			switch ev.Type {
			case ap.AgentEventContentDelta:
				// stream it to a user
			case ap.AgentEventApprovalRequired:
				p, _ := ap.PayloadAs[ap.ApprovalRequiredPayload](ev, ev.Type)
				_ = sess.Resolve(ctx, p.ToolInvocationID, driver.Allow())
			}
		}
	}()

	_ = sess.Prompt(ctx, driver.TextInput("what changed in this repo today?"))
}
