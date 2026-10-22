package claude

import (
	"testing"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/protocol/claudecode"
)

func TestStatus(t *testing.T) {
	for in, want := range map[string]ap.SubagentStatus{"running": ap.SubagentRunning, "pending": ap.SubagentWaiting, "paused": ap.SubagentWaiting,
		"completed": ap.SubagentCompleted, "failed": ap.SubagentFailed, "killed": ap.SubagentCancelled, "stopped": ap.SubagentCancelled, "new": ""} {
		if got := Status(in); got != want {
			t.Errorf("Status(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStartedPrefersTheSubagentType(t *testing.T) {
	p := Started(claudecode.TaskStarted{TaskID: "t", ToolUseID: "u", SubagentType: "Explore", TaskType: "local_agent", ParentTaskID: "p", IsBackgrounded: true})
	if p.SubagentID != "t" || p.ParentToolInvocationID != "u" || p.Kind != "Explore" || p.ParentSubagentID != "p" || !p.Background {
		t.Errorf("started = %+v", p)
	}
	if p := Started(claudecode.TaskStarted{TaskID: "t", TaskType: "local_bash"}); p.Kind != "local_bash" {
		t.Errorf("kind without a subagent type = %q", p.Kind)
	}
}

func TestCompletedCarriesTheUsage(t *testing.T) {
	c := Completed(claudecode.TaskNotification{TaskID: "t", Status: "stopped", Summary: "s", Usage: &claudecode.TaskUsage{TotalTokens: 9, ToolUses: 2, DurationMS: 30}})
	if c.Status != ap.SubagentCancelled || c.Usage == nil || *c.Usage != (ap.SubagentUsage{TotalTokens: 9, ToolUses: 2, DurationMs: 30}) {
		t.Errorf("completed = %+v", c)
	}
}
