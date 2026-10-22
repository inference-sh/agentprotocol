package codex

import (
	"strings"
	"testing"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/codexapp"
)

func TestSpawnedOnlyForSpawnAgent(t *testing.T) {
	prompt := "look around\nthen report"
	c := codexapp.CollabAgentToolCallThreadItem{ID: "c", Tool: codexapp.CollabAgentToolSpawnAgent, ReceiverThreadIDs: []string{"a", "b"}, Prompt: &prompt}
	got := Spawned(c)
	if len(got) != 2 || got[0].SubagentID != "a" || got[1].ParentToolInvocationID != "c" || got[0].Description != "look around" || got[0].Prompt != prompt {
		t.Errorf("spawned = %+v", got)
	}
	c.Tool = codexapp.CollabAgentToolWait
	if len(Spawned(c)) != 0 {
		t.Error("a wait call started a subagent")
	}
}

func TestStatusAndClip(t *testing.T) {
	if Status(codexapp.CollabAgentStatusErrored) != ap.SubagentFailed || Status(codexapp.CollabAgentStatusPendingInit) != ap.SubagentWaiting || !Status(codexapp.CollabAgentStatusShutdown).Ended() {
		t.Error("status mapping")
	}
	if got := clip(strings.Repeat("é", 100)); len([]rune(got)) != 80 || !strings.HasSuffix(got, "...") {
		t.Errorf("clip = %q", got)
	}
}

// A spawned agent's turn ending leaves it open and waiting, with its answer
// as the summary, even when the turn failed.
func TestTurnDoneIsAWait(t *testing.T) {
	msg := codexapp.ThreadItem{}
	if err := msg.UnmarshalJSON([]byte(`{"type":"agentMessage","id":"m","text":"done"}`)); err != nil {
		t.Fatal(err)
	}
	p := TurnDone("thr", codexapp.Turn{Items: []codexapp.ThreadItem{msg}, Status: codexapp.TurnStatusCompleted})
	if p.Status != ap.SubagentWaiting || p.Summary != "done" || p.Status.Ended() {
		t.Errorf("done = %+v", p)
	}
	failed := TurnDone("thr", codexapp.Turn{Status: codexapp.TurnStatusFailed, Error: &codexapp.TurnError{Message: "boom"}})
	if failed.Status != ap.SubagentWaiting || failed.Error != "boom" {
		t.Errorf("failed = %+v", failed)
	}
}
