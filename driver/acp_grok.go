package driver

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
)

// grok's extensions of ACP, read from its source (grok-build, 2026-09-29)
// and T3 Code's adapter of the same (2026-10-28). It sends them as ACP
// extensions, so with a leading underscore (_x.ai/...); acp.ExtMethod takes
// it off.
const (
	grokAskUserQuestion  = "x.ai/ask_user_question"
	grokExitPlanMode     = "x.ai/exit_plan_mode"
	grokTaskBackgrounded = "x.ai/task_backgrounded"
	grokTaskCompleted    = "x.ai/task_completed"
	grokMonitorEvent     = "x.ai/monitor_event"
	grokPromptComplete   = "x.ai/session/prompt_complete"
	// grok's own session updates, live and as its logs spell them.
	grokSessionNotification = "x.ai/session_notification"
	grokSessionUpdate       = "x.ai/session/update"
)

// grokWakePrefixes start the prompt id of a turn grok starts on its own
// when background work it left running finishes; the rest is the task's id.
var grokWakePrefixes = []string{"task-completed-", "bash-completed-", "monitor-completed-", "subagent-completed-"}

func init() {
	RegisterACPExtension("grok", func() ACPExtension { return &grokExtension{tasks: map[string]bool{}} })
}

// grokExtension adapts one grok session.
//
//   - Questions: _x.ai/ask_user_question is a request grok blocks its tool
//     on; it goes to the person as a form and the answer back keyed by
//     question text. _x.ai/exit_plan_mode asks to leave plan mode with a plan;
//     it goes to the person as an approval.
//   - Background work: a command grok leaves running, or a monitor, starts
//     with a tool call whose rawOutput says so (BackgroundTaskStarted,
//     Monitor) or with _x.ai/task_backgrounded, and ends with
//     _x.ai/task_completed, a KillTask that killed it or a TaskOutput that
//     saw it end. Each is a background.started and a background.completed.
//   - Wake turns: when background work ends, grok starts a turn of its own
//     whose updates carry _meta.promptId task-completed-<task>; it ends
//     with _x.ai/session/prompt_complete (or turn_completed) for that id.
//   - Context window: each model's _meta.totalContextTokens, in session/new
//     or session/load's models, or initialize's _meta.modelState.
type grokExtension struct {
	mu sync.Mutex
	// tasks are the background tasks seen starting and not yet ended.
	tasks map[string]bool
}

func (g *grokExtension) Request(ctx context.Context, h ACPHost, method string, params json.RawMessage) (any, bool, error) {
	method, params = grokUnwrap(method, params)
	switch method {
	case grokAskUserQuestion:
		return g.askUserQuestion(ctx, h, params), true, nil
	case grokExitPlanMode:
		return g.exitPlanMode(ctx, h, params), true, nil
	}
	return nil, false, nil
}

// grokUnwrap reads a request in either of the shapes grok sends: params as
// they are, or wrapped as {"method": ..., "params": ...} (what grok's own
// relay passes on).
func grokUnwrap(method string, params json.RawMessage) (string, json.RawMessage) {
	method = acp.ExtMethod(method)
	var wrapped struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(params, &wrapped) == nil && wrapped.Method != "" && len(wrapped.Params) > 0 {
		return acp.ExtMethod(wrapped.Method), wrapped.Params
	}
	return method, params
}

type grokQuestionParams struct {
	ToolCallID string `json:"toolCallId"`
	Mode       string `json:"mode"`
	Questions  []struct {
		Question    string `json:"question"`
		MultiSelect *bool  `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// askUserQuestion answers _x.ai/ask_user_question:
//
//	{"outcome":"accepted","answers":{"<question>":["<label>",...]},
//	 "annotations":{"<question>":{"notes":"<text>"}}}
//
// An answer of the person's own is the label Other with the text as the
// question's notes, beside any labels chosen. An unanswered question is left
// out; a declined or unanswered form is {"outcome":"cancelled"}.
func (g *grokExtension) askUserQuestion(ctx context.Context, h ACPHost, params json.RawMessage) any {
	cancelled := map[string]string{"outcome": "cancelled"}
	var p grokQuestionParams
	if err := json.Unmarshal(params, &p); err != nil || len(p.Questions) == 0 {
		h.Diagnose("grok asked a question this client could not read; answered cancelled")
		return cancelled
	}
	qs := make([]Question, len(p.Questions))
	for i, q := range p.Questions {
		qs[i] = Question{Text: q.Question, MultiSelect: q.MultiSelect != nil && *q.MultiSelect}
		for _, o := range q.Options {
			qs[i].Options = append(qs[i].Options, QuestionOption{Label: o.Label, Description: o.Description})
		}
	}
	res, answered := h.Ask(ctx, p.ToolCallID, "ask_user_question", ap.StringEncodedMap{"questions": p.Questions}, QuestionsInput("", qs))
	if !answered || res.Decision != ap.InterruptResolutionAllow {
		return cancelled
	}
	answers := map[string][]string{}
	annotations := map[string]map[string]string{}
	for i, a := range QuestionAnswers(qs, res.Values) {
		if a.Empty() {
			continue
		}
		labels := a.Labels
		if a.Text != "" {
			labels = append(labels, "Other")
			annotations[qs[i].Text] = map[string]string{"notes": a.Text}
		}
		answers[qs[i].Text] = labels
	}
	out := map[string]any{"outcome": "accepted", "answers": answers}
	if len(annotations) > 0 {
		out["annotations"] = annotations
	}
	return out
}

// exitPlanMode answers _x.ai/exit_plan_mode: approved, cancelled with the
// person's reason as feedback, or abandoned when nobody answered.
func (g *grokExtension) exitPlanMode(ctx context.Context, h ACPHost, params json.RawMessage) any {
	var p struct {
		ToolCallID  string `json:"toolCallId"`
		PlanContent string `json:"planContent"`
	}
	_ = json.Unmarshal(params, &p)
	res, answered := h.Approve(ctx, p.ToolCallID, "exit_plan_mode", ap.StringEncodedMap{"plan": p.PlanContent})
	switch {
	case !answered:
		return map[string]string{"outcome": "abandoned"}
	case res.Decision == ap.InterruptResolutionAllow:
		return map[string]string{"outcome": "approved"}
	}
	out := map[string]string{"outcome": "cancelled"}
	if res.Reason != "" {
		out["feedback"] = res.Reason
	}
	return out
}

func (g *grokExtension) Notification(h ACPHost, method string, params json.RawMessage) bool {
	method = acp.ExtMethod(method)
	var n struct {
		Update   json.RawMessage `json:"update"`
		PromptID string          `json:"promptId"`
		Stop     string          `json:"stopReason"`
	}
	_ = json.Unmarshal(params, &n)
	switch method {
	case grokTaskBackgrounded:
		var u struct {
			ToolCallID         string `json:"tool_call_id"`
			TaskID             string `json:"task_id"`
			Command            string `json:"command"`
			MonitorDescription string `json:"monitor_description"`
			Description        string `json:"description"`
		}
		if json.Unmarshal(n.Update, &u) == nil {
			kind, desc := ap.BackgroundCommand, u.Description
			if u.MonitorDescription != "" {
				kind, desc = ap.BackgroundMonitor, u.MonitorDescription
			}
			g.started(h, ap.BackgroundStartedPayload{TaskID: u.TaskID, Kind: kind, ToolInvocationID: u.ToolCallID, Command: u.Command, Description: desc})
		}
		return true
	case grokTaskCompleted:
		var u struct {
			Task struct {
				TaskID           string `json:"task_id"`
				ExitCode         *int   `json:"exit_code"`
				Signal           any    `json:"signal"`
				ExplicitlyKilled bool   `json:"explicitly_killed"`
				Output           string `json:"output"`
			} `json:"task_snapshot"`
		}
		if json.Unmarshal(n.Update, &u) == nil {
			t := u.Task
			status := ap.BackgroundCompleted
			switch {
			case t.ExplicitlyKilled:
				status = ap.BackgroundCancelled
			case t.Signal != nil || (t.ExitCode != nil && *t.ExitCode != 0):
				status = ap.BackgroundFailed
			}
			g.ended(h, ap.BackgroundCompletedPayload{TaskID: t.TaskID, Status: status, ExitCode: t.ExitCode, Output: t.Output})
		}
		return true
	case grokPromptComplete:
		h.EndAgentTurn(n.PromptID, n.Stop)
		return true
	case grokSessionNotification, grokSessionUpdate:
		var u struct {
			Kind     string `json:"sessionUpdate"`
			PromptID string `json:"prompt_id"`
			Stop     string `json:"stop_reason"`
		}
		if json.Unmarshal(n.Update, &u) == nil && u.Kind == "turn_completed" {
			h.EndAgentTurn(u.PromptID, u.Stop)
		}
		// The rest (subagent progress, retries, compaction, the task list)
		// is grok's own bookkeeping, known and not an event of ours.
		return true
	case grokMonitorEvent:
		// What a monitor saw; grok tells its model in the wake turn.
		return true
	}
	return false
}

func (g *grokExtension) Update(h ACPHost, n acp.UpdateNotification) {
	var meta struct {
		PromptID string `json:"promptId"`
	}
	if len(n.Meta) > 0 && json.Unmarshal(n.Meta, &meta) == nil {
		for _, prefix := range grokWakePrefixes {
			if strings.HasPrefix(meta.PromptID, prefix) {
				h.BeginAgentTurn(meta.PromptID, ap.TurnOriginBackground, strings.TrimPrefix(meta.PromptID, prefix))
				break
			}
		}
	}

	u := n.Update
	if len(u.RawOutput) == 0 {
		return
	}
	var out struct {
		Type    string `json:"type"`
		TaskID  string `json:"task_id"`
		TaskID2 string `json:"taskId"`
		Command string `json:"command"`
		Summary string `json:"summary"`
		Result  *struct {
			TaskID   string `json:"task_id"`
			Status   string `json:"status"`
			ExitCode *int   `json:"exit_code"`
			Outcome  string `json:"outcome"`
			Output   string `json:"output"`
		} `json:"Result"`
	}
	if json.Unmarshal(u.RawOutput, &out) != nil {
		return
	}
	switch out.Type {
	case "BackgroundTaskStarted":
		g.started(h, ap.BackgroundStartedPayload{TaskID: out.TaskID, Kind: ap.BackgroundCommand, ToolInvocationID: u.ToolCallID, Command: out.Command, Description: out.Summary})
	case "Monitor":
		g.started(h, ap.BackgroundStartedPayload{TaskID: out.TaskID2, Kind: ap.BackgroundMonitor, ToolInvocationID: u.ToolCallID})
	case "KillTask":
		// Only a kill that stopped it, or found it gone, ends it.
		if r := out.Result; r != nil && (r.Outcome == "" || r.Outcome == "killed" || r.Outcome == "already_exited") {
			g.ended(h, ap.BackgroundCompletedPayload{TaskID: r.TaskID, Status: ap.BackgroundCancelled})
		}
	case "TaskOutput":
		if r := out.Result; r != nil {
			if status, ended := grokTaskStatus(r.Status); ended {
				g.ended(h, ap.BackgroundCompletedPayload{TaskID: r.TaskID, Status: status, ExitCode: r.ExitCode, Output: r.Output})
			}
		}
	}
}

// grokTaskStatus maps a TaskOutput status; ended is false while it runs.
func grokTaskStatus(s string) (ap.BackgroundStatus, bool) {
	switch s {
	case "completed":
		return ap.BackgroundCompleted, true
	case "failed":
		return ap.BackgroundFailed, true
	case "cancelled":
		return ap.BackgroundCancelled, true
	case "timed_out":
		return ap.BackgroundTimedOut, true
	}
	return "", false
}

// started reports background work once, however many of grok's messages
// announce it.
func (g *grokExtension) started(h ACPHost, p ap.BackgroundStartedPayload) {
	if p.TaskID == "" {
		return
	}
	g.mu.Lock()
	seen := g.tasks[p.TaskID]
	g.tasks[p.TaskID] = true
	g.mu.Unlock()
	if !seen {
		h.Emit(ap.AgentEventBackgroundStarted, p)
	}
}

// ended reports background work ending once, and only work seen starting.
func (g *grokExtension) ended(h ACPHost, p ap.BackgroundCompletedPayload) {
	g.mu.Lock()
	running := g.tasks[p.TaskID]
	delete(g.tasks, p.TaskID)
	g.mu.Unlock()
	if running {
		h.Emit(ap.AgentEventBackgroundCompleted, p)
	}
}

// ContextWindow reads the current model's _meta.totalContextTokens from the
// session's result, else from initialize's _meta.modelState.
func (g *grokExtension) ContextWindow(initialize, session json.RawMessage) int {
	type models struct {
		Current   string `json:"currentModelId"`
		Available []struct {
			ID   string `json:"modelId"`
			Meta struct {
				Total int `json:"totalContextTokens"`
			} `json:"_meta"`
		} `json:"availableModels"`
	}
	window := func(m *models) int {
		if m == nil {
			return 0
		}
		for _, a := range m.Available {
			if a.ID == m.Current && a.Meta.Total > 0 {
				return a.Meta.Total
			}
		}
		return 0
	}
	var s struct {
		Models *models `json:"models"`
	}
	if json.Unmarshal(session, &s) == nil {
		if w := window(s.Models); w > 0 {
			return w
		}
	}
	var i struct {
		Meta struct {
			ModelState *models `json:"modelState"`
		} `json:"_meta"`
	}
	if json.Unmarshal(initialize, &i) == nil {
		return window(i.Meta.ModelState)
	}
	return 0
}
