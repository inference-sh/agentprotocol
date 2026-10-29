package driver

import (
	"context"
	"encoding/json"
	"testing"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
	"github.com/inference-sh/agentprotocol/protocol/claudecode"
)

var testQuestions = []Question{
	{Header: "Database", Text: "Which database?", Options: []QuestionOption{{Label: "Redis", Description: "In-memory"}, {Label: "Postgres"}}},
	{Text: "Which checks?", MultiSelect: true, Options: []QuestionOption{{Label: "lint"}, {Label: "test"}}},
}

// Each question is a choice (one, or several) and a field for an answer of
// the person's own.
func TestQuestionsInput(t *testing.T) {
	in := QuestionsInput("pick", testQuestions)
	var s struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(in.Schema, &s); err != nil {
		t.Fatal(err)
	}
	if s.Type != "object" || len(s.Properties) != 4 || in.Message != "pick" {
		t.Fatalf("schema %s", in.Schema)
	}
	if q := s.Properties["q1"]; q["type"] != "string" || q["title"] != "Database" || q["description"] != "Which database?" || len(q["oneOf"].([]any)) != 2 {
		t.Fatalf("single choice %v", q)
	}
	if q := s.Properties["q2"]; q["type"] != "array" || q["title"] != "Which checks?" {
		t.Fatalf("multiple choice %v", q)
	}
	if q := s.Properties["q2_other"]; q["type"] != "string" {
		t.Fatalf("own answer %v", q)
	}
}

// Answers come back per question: chosen labels, the person's own text, and
// a value that is no choice taken as their text.
func TestQuestionAnswers(t *testing.T) {
	got := QuestionAnswers(testQuestions, map[string]any{
		"q1":       "Sqlite",
		"q2":       []any{"lint", "test"},
		"q2_other": " also vet ",
	})
	if got[0].Text != "Sqlite" || len(got[0].Labels) != 0 {
		t.Fatalf("q1 = %+v", got[0])
	}
	if len(got[1].Labels) != 2 || got[1].Text != "also vet" {
		t.Fatalf("q2 = %+v", got[1])
	}
	if a := QuestionAnswers(testQuestions, nil); !a[0].Empty() || !a[1].Empty() {
		t.Fatalf("nothing answered = %+v", a)
	}
}

// AskUserQuestion is asked as a form, and answered with Claude's answers
// map: the question's text to its labels, then the person's own words.
func TestClaudeAskUserQuestion(t *testing.T) {
	req := claudecode.PermissionRequest{ToolName: "AskUserQuestion", Input: json.RawMessage(`{"questions":[
		{"question":"Which database?","header":"Database","multiSelect":false,"options":[{"label":"Redis","description":"In-memory"},{"label":"Postgres"}]},
		{"question":"Which checks?","header":"Checks","multiSelect":true,"options":[{"label":"lint"},{"label":"test"}]}]}`)}
	qs := claudeQuestions(req)
	if len(qs) != 2 || qs[0].Header != "Database" || !qs[1].MultiSelect {
		t.Fatalf("questions %+v", qs)
	}
	got := claudeAnswers(qs, map[string]any{"q1": "Postgres", "q2": []any{"lint"}, "q2_other": "vet"})
	if got["Which database?"] != "Postgres" || got["Which checks?"] != "lint, vet" {
		t.Fatalf("answers %v", got)
	}
	if claudeQuestions(claudecode.PermissionRequest{ToolName: "Bash", Input: req.Input}) != nil {
		t.Fatal("a Bash call is no question")
	}
}

// fakeHost records what an extension did and answers as told.
type fakeHost struct {
	res      Resolution
	answered bool
	asked    []ap.ApprovalRequiredPayload
	events   []ap.AgentEventType
	payloads []any
	turns    []string
}

func (h *fakeHost) Ask(_ context.Context, id, name string, args ap.StringEncodedMap, in ap.InputForm) (Resolution, bool) {
	h.asked = append(h.asked, ap.ApprovalRequiredPayload{ToolInvocationID: id, ToolName: name, Arguments: args, Input: &in, Reason: ap.InterruptReasonInput})
	return h.res, h.answered
}
func (h *fakeHost) Approve(_ context.Context, id, name string, args ap.StringEncodedMap) (Resolution, bool) {
	h.asked = append(h.asked, ap.ApprovalRequiredPayload{ToolInvocationID: id, ToolName: name, Arguments: args, Reason: ap.InterruptReasonToolApproval})
	return h.res, h.answered
}
func (h *fakeHost) Emit(t ap.AgentEventType, p any) {
	h.events = append(h.events, t)
	h.payloads = append(h.payloads, p)
}
func (h *fakeHost) BeginAgentTurn(id string, _ ap.TurnOrigin, _ string) {
	h.turns = append(h.turns, "begin "+id)
}
func (h *fakeHost) EndAgentTurn(id, _ string) { h.turns = append(h.turns, "end "+id) }
func (h *fakeHost) Diagnose(string)           {}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Leaving plan mode is an approval of the plan: approved, cancelled with the
// person's reason as feedback, abandoned when nobody answered.
func TestGrokExitPlanMode(t *testing.T) {
	g := ACPExtensionFor("grok")
	params := json.RawMessage(`{"sessionId":"s","toolCallId":"call-p","planContent":"1. do it"}`)
	for _, tc := range []struct {
		host *fakeHost
		want string
	}{
		{&fakeHost{res: Allow(), answered: true}, `{"outcome":"approved"}`},
		{&fakeHost{res: Deny("smaller steps"), answered: true}, `{"feedback":"smaller steps","outcome":"cancelled"}`},
		{&fakeHost{}, `{"outcome":"abandoned"}`},
	} {
		res, handled, err := g.Request(context.Background(), tc.host, "_x.ai/exit_plan_mode", params)
		if !handled || err != nil || asJSON(t, res) != tc.want {
			t.Fatalf("answered %s (handled %v, %v), want %s", asJSON(t, res), handled, err, tc.want)
		}
		if p := tc.host.asked[0]; p.ToolName != "exit_plan_mode" || p.ToolInvocationID != "call-p" || p.Arguments["plan"] != "1. do it" {
			t.Fatalf("asked %+v", p)
		}
	}
}

// grok's relay wraps a request as {method, params}; it reads the same.
func TestGrokWrappedRequest(t *testing.T) {
	g := ACPExtensionFor("grok")
	h := &fakeHost{}
	params := json.RawMessage(`{"method":"x.ai/ask_user_question","params":{"toolCallId":"call-w","questions":[{"question":"Go?","options":[{"label":"yes"}]}]}}`)
	res, handled, _ := g.Request(context.Background(), h, "_x.ai/ask_user_question", params)
	if !handled || asJSON(t, res) != `{"outcome":"cancelled"}` || len(h.asked) != 1 || h.asked[0].ToolInvocationID != "call-w" {
		t.Fatalf("answered %s, asked %+v", asJSON(t, res), h.asked)
	}
	if _, handled, _ := g.Request(context.Background(), h, "_x.ai/unknown", nil); handled {
		t.Fatal("an unknown method is left to method-not-found")
	}
}

// A kill that stopped the task ends it as cancelled; a kill that failed
// does not; a monitor tick does not.
func TestGrokKillAndTicks(t *testing.T) {
	g := ACPExtensionFor("grok")
	h := &fakeHost{}
	up := func(raw string) {
		g.Update(h, acp.UpdateNotification{Update: acp.SessionUpdate{Kind: acp.UpdateKindToolCallUpdate, ToolCallID: "call-x", RawOutput: json.RawMessage(raw)}})
	}
	up(`{"type":"Monitor","taskId":"m1","timeoutMs":36000000,"persistent":false}`)
	up(`{"type":"Bash","exit_code":0,"output":"tick"}`)
	up(`{"type":"KillTask","Result":{"task_id":"m1","outcome":"not_killed"}}`)
	if len(h.events) != 1 || h.events[0] != ap.AgentEventBackgroundStarted {
		t.Fatalf("events %v", h.events)
	}
	if p := h.payloads[0].(ap.BackgroundStartedPayload); p.Kind != ap.BackgroundMonitor || p.TaskID != "m1" {
		t.Fatalf("started %+v", p)
	}
	up(`{"type":"KillTask","Result":{"task_id":"m1","outcome":"killed"}}`)
	up(`{"type":"KillTask","Result":{"task_id":"m1","outcome":"killed"}}`)
	if len(h.events) != 2 || h.payloads[1].(ap.BackgroundCompletedPayload).Status != ap.BackgroundCancelled {
		t.Fatalf("events %v %v", h.events, h.payloads)
	}
}

// The wake turn opens on its first update and closes on its prompt_complete,
// or on grok's turn_completed for it.
func TestGrokWakeTurn(t *testing.T) {
	g := ACPExtensionFor("grok")
	h := &fakeHost{}
	g.Update(h, acp.UpdateNotification{Meta: json.RawMessage(`{"promptId":"monitor-completed-m1"}`)})
	g.Notification(h, "_x.ai/session_notification", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"turn_completed","prompt_id":"monitor-completed-m1","stop_reason":"end_turn"}}`))
	if len(h.turns) != 2 || h.turns[0] != "begin monitor-completed-m1" || h.turns[1] != "end monitor-completed-m1" {
		t.Fatalf("turns %v", h.turns)
	}
	if !g.Notification(h, "_x.ai/monitor_event", nil) || g.Notification(h, "_x.ai/other", nil) {
		t.Fatal("known notifications are taken, unknown ones are not")
	}
}

// The window is the current model's, from the session's result or else
// initialize's model state.
func TestGrokContextWindow(t *testing.T) {
	g := ACPExtensionFor("grok")
	models := `{"currentModelId":"b","availableModels":[{"modelId":"a","_meta":{"totalContextTokens":1}},{"modelId":"b","_meta":{"totalContextTokens":256000}}]}`
	if w := g.ContextWindow(nil, json.RawMessage(`{"sessionId":"s","models":`+models+`}`)); w != 256000 {
		t.Fatalf("from session/new: %d", w)
	}
	if w := g.ContextWindow(json.RawMessage(`{"_meta":{"modelState":`+models+`}}`), json.RawMessage(`{"sessionId":"s"}`)); w != 256000 {
		t.Fatalf("from initialize: %d", w)
	}
	if w := g.ContextWindow(nil, nil); w != 0 {
		t.Fatalf("from nothing: %d", w)
	}
}
