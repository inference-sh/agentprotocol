package runner

import (
	"fmt"

	ap "github.com/inference-sh/agentprotocol"
	"slices"
	"time"
)

// subagentSettle is how long the probe waits after the turn for a subagent's
// end, which an agent can report after its own turn is over.
const subagentSettle = 10 * time.Second

// probeSubagents makes the agent start a subagent and asks whether the
// driver reports it: started (linked to the tool call that started it),
// ended, and its own output tagged with it rather than passed off as the
// agent's. The mock answers the agent with the registry's call to its
// subagent tool, once, and answers the subagent's own request with text, so
// the subagent ends after one answer.
func (r *TestRunner) probeSubagents() {
	if r.harness.DriverKind() == "" {
		return
	}
	fmt.Println("[probe] subagents (an agent handing work to another)")
	name := r.harness.Name
	tc := r.harness.ToolCallSubagent
	if tc.Name == "" || r.server == nil {
		r.skip("subagents:no-tool", fmt.Sprintf("subagents: %s has no subagent tool in the registry", name))
		return
	}
	r.server.PrepareToolCallIn(tc.Namespace, tc.Name, r.expand(tc.Args), r.harness.ToolCallPath)
	r.subagentOutput = true
	defer func() { r.subagentOutput = false }()
	d, ok := r.startProbeTurn("subagents")
	if !ok {
		return
	}
	if !r.server.ToolCallServed() {
		d.Close()
		r.skip("subagents:not-offered", fmt.Sprintf("subagents: %s's turn never reached the request that calls %s", name, tc.Name))
		return
	}
	deadline := time.Now().Add(subagentSettle)
	for time.Now().Before(deadline) {
		if o := d.Subagents(); len(o.Started) > 0 && (len(o.Completed) > 0 || idleWithAnswer(o, o.Started[0].SubagentID)) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	o := d.Subagents()
	d.Close()

	if len(o.Started) == 0 {
		r.finding("subagents.started:not-reported", fmt.Sprintf("subagents: %s called %s and no subagent was reported", name, tc.Name))
		return
	}
	st := o.Started[0]
	switch {
	case st.ParentToolInvocationID == "":
		r.pass("subagents.started:unlinked", fmt.Sprintf("subagents: %s reported subagent %s with no parent tool call", name, st.SubagentID))
	case slices.Contains(o.ToolsStarted, st.ParentToolInvocationID):
		r.pass("subagents.started:linked", fmt.Sprintf("subagents: %s reported subagent %s, started by tool call %s", name, st.SubagentID, st.ParentToolInvocationID))
	default:
		r.fail("subagents.started", fmt.Sprintf("subagents: %s's subagent names parent %s, which is not a tool call the agent reported", name, st.ParentToolInvocationID))
	}

	switch {
	case len(o.Completed) == 0 && idleWithAnswer(o, st.SubagentID):
		// codex keeps a spawned agent open after its turn, for the parent
		// to send more or close; its answer is in and nothing has ended.
		r.pass("subagents.completed:idle", fmt.Sprintf("subagents: %s reported subagent %s waiting with its answer; it stays open until the agent closes it", name, st.SubagentID))
	case len(o.Completed) == 0:
		r.finding("subagents.completed:not-reported", fmt.Sprintf("subagents: %s reported no end for subagent %s", name, st.SubagentID))
	case o.Completed[0].SubagentID != st.SubagentID:
		r.fail("subagents.completed", fmt.Sprintf("subagents: %s ended subagent %s, not the one it started (%s)", name, o.Completed[0].SubagentID, st.SubagentID))
	default:
		r.pass("subagents.completed", fmt.Sprintf("subagents: %s reported subagent %s ended: %s", name, st.SubagentID, o.Completed[0].Status))
	}

	if o.Output > 0 {
		r.pass("subagents.output", fmt.Sprintf("subagents: %s sent %d event(s) of the subagent's own output, tagged with it", name, o.Output))
	} else {
		r.finding("subagents.output:none", fmt.Sprintf("subagents: %s sent none of the subagent's own output", name))
	}
}

// idleWithAnswer reports whether a subagent is waiting with an answer: done
// with its task and still open.
func idleWithAnswer(o SubagentObservation, id string) bool {
	for _, u := range o.Updated {
		if u.SubagentID == id && u.Status == ap.SubagentWaiting && u.Summary != "" {
			return true
		}
	}
	return false
}
