package kimi

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/pi"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Kimi Code in the harness-test container: a
// prompt, a mock Read call, its result, an answer.
const (
	sampleCWD = "/tmp/harness-test-kimi-2532946395/test-repo"
	sampleID  = "session_9ef8feda-5261-4097-892f-94cf87e40a76"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	// The context rows hold the conversation in order: the hook's output,
	// the prompt, the date reminder kimi injects, the step that calls Read,
	// its result, the answer.
	want := []string{"user", "user", "user", "assistant", "tool", "assistant"}
	if got := roles(s.Messages()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roles = %v, want %v", got, want)
	}
}

func roles(es []transcript.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = string(e.Role)
	}
	return out
}

func texts(es []transcript.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Text()
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolResult {
				out[i] = "result:" + b.Text
			}
		}
		if len(out[i]) > 40 {
			out[i] = out[i][:40]
		}
	}
	return out
}

func read(t *testing.T, home, id string) *transcript.Session {
	t.Helper()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReminderIsForTheModel checks the sample's injected date reminder goes
// to the model and is not shown, and that the hook's output, which kimi shows
// as a notice, is both.
func TestReminderIsForTheModel(t *testing.T) {
	s := read(t, "testdata/home", sampleID)
	var reminder, hook *transcript.Entry
	for i, e := range s.Entries {
		switch {
		case strings.HasPrefix(e.Text(), "<system-reminder>"):
			reminder = &s.Entries[i]
		case strings.HasPrefix(e.Text(), "<hook_result"):
			hook = &s.Entries[i]
		}
	}
	if reminder == nil || reminder.Audience != transcript.AudienceModel {
		t.Errorf("date reminder = %+v, want an entry for the model only", reminder)
	}
	if hook == nil || hook.Audience != transcript.AudienceAll {
		t.Errorf("hook output = %+v, want an entry for everyone", hook)
	}
	if n := len(s.Context()); n != 6 {
		t.Errorf("context has %d messages, want 6: %q", n, texts(s.Context()))
	}
	if n := len(s.Linearize()); n != 5 {
		t.Errorf("linearize has %d messages, want 5: %q", n, texts(s.Linearize()))
	}
}

// The compaction sample is an ACP run in the harness-test container: four
// prompts, /compact, and a prompt in a resumed process. The request kimi
// sent after resuming carried the four prompts, the summary, the note to
// carry on, the new prompt and the date reminder, which is what Context must
// return.
const compactID = "session_e6a7c75a-7138-457e-a523-c68eba24215e"

func TestCompaction(t *testing.T) {
	s := read(t, "testdata/home", compactID)
	got := texts(s.Context())
	want := []string{
		"My name is ALICE-7.",
		"What files are in this repository?",
		"Summarise what you have done so far.",
		"What was the first thing I asked you?",
		"The conversation so far has been compact",
		"<system-reminder>\nContext compaction is ",
		"After compaction: what is my name?",
		"<system-reminder>\nToday's date is 2026-0",
		"Hello from mock server.",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("context = %q\nwant %q", got, want)
	}
	// The retired turns are still shown.
	if n := len(s.Linearize()); n != 10 {
		t.Errorf("linearize has %d messages, want the 10 of five turns: %q", n, texts(s.Linearize()))
	}
}

// The undo sample is the kimi TUI driven in the harness-test container: two
// prompts, /undo 1, a third prompt. Kimi forks a branch at the end of the
// first turn and continues there; the second turn stays in the file.
const undoID = "session_1cd19842-07cd-4872-ba3e-8e7124b6d4cc"

func TestUndo(t *testing.T) {
	s := read(t, "testdata/home", undoID)
	want := []string{"First prompt KEEP-ONE.", "Hello from mock server.", "Third prompt KEEP-THREE.", "Hello from mock server."}
	if got := texts(s.Linearize()); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("linearize = %q, want %q", got, want)
	}
	for _, e := range s.Context() {
		if strings.Contains(e.Text(), "DROP-TWO") {
			t.Errorf("the undone prompt reaches the model")
		}
	}
	undone := 0
	for _, e := range s.Messages() {
		if e.Audience == transcript.AudienceNone {
			undone++
		}
	}
	if undone != 2 {
		t.Errorf("%d messages are for no one, want the undone prompt and its answer", undone)
	}
}

func TestWorkspaceDir(t *testing.T) {
	if got := workspaceDir(sampleCWD); got != "wd_test-repo_46412412ac9a" {
		t.Errorf("workspaceDir = %q", got)
	}
}

func TestForeign(t *testing.T) { transcripttest.Foreign(t, Codec, "/tmp/some/project") }

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// TestContextRows writes a hand-built session and checks the rows kimi
// rebuilds the model's context from, which the harness seed probe found
// missing: kimi resumed the session and the model never saw the planted fact.
func TestContextRows(t *testing.T) {
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "call_1", Name: "Read", Input: []byte(`{"path":"README.md"}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "call_1", Text: "test", Status: transcript.StatusOK}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	wire := filepath.Join(home, root, workspaceDir("/tmp/p"), id, "agents", "main", "wire.jsonl")
	f, err := os.Open(wire)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var userInContext, answerInContext bool
	callUUID, resultParent := "", ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
			Event   struct {
				Type       string `json:"type"`
				UUID       string `json:"uuid"`
				ParentUUID string `json:"parentUuid"`
				Part       struct {
					Text string `json:"text"`
				} `json:"part"`
			} `json:"event"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		switch {
		case r.Type == "context.append_message" && strings.Contains(string(r.Message), "The codename is HERON."):
			userInContext = true
		case r.Type == "context.append_loop_event" && r.Event.Type == "content.part" && r.Event.Part.Text == "Noted: HERON.":
			answerInContext = true
		case r.Type == "context.append_loop_event" && r.Event.Type == "tool.call":
			callUUID = r.Event.UUID
		case r.Type == "context.append_loop_event" && r.Event.Type == "tool.result":
			resultParent = r.Event.ParentUUID
		}
	}
	if !userInContext {
		t.Error("no context.append_message carries the user's text")
	}
	if !answerInContext {
		t.Error("no content.part carries the assistant's text")
	}
	if callUUID == "" || resultParent != callUUID {
		t.Errorf("tool.result parentUuid %q does not name the tool.call uuid %q", resultParent, callUUID)
	}
}

// Kimi names a prompt msg_<ULID> and an assistant step, which is where an
// assistant message is read from, by UUID.
func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", func(id string) bool {
		return validMessageID(id) || transcript.IsUUID(id)
	})
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestImported(t *testing.T) {
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// writeTool writes a hand-built session with one tool call and its result,
// and returns the home and the session id.
func writeTool(t *testing.T, status transcript.Status) (string, string) {
	t.Helper()
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Read the README."}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "call_1", Name: "Read", Input: []byte(`{"path":"README.md"}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "call_1", Text: "RESULT-42", Status: status}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Done."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return home, id
}

type loopRowShape struct {
	Type  string `json:"type"`
	Event struct {
		Type         string `json:"type"`
		UUID         string `json:"uuid"`
		StepUUID     string `json:"stepUuid"`
		FinishReason string `json:"finishReason"`
		Result       map[string]json.RawMessage
	} `json:"event"`
}

func wireRows(t *testing.T, home, id string) []loopRowShape {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, root, workspaceDir("/tmp/p"), id, "agents", "main", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []loopRowShape
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r loopRowShape
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestToolResultBeforeStepEnd checks a written step records its tool result
// before it ends. Kimi's fold answers every call still waiting at step.end
// with "Tool execution was interrupted" and ignores a result that comes
// later, so a written session resumed with that as the tool's output.
func TestToolResultBeforeStepEnd(t *testing.T) {
	home, id := writeTool(t, transcript.StatusOK)
	callStep, result, end := "", -1, -1
	for i, r := range wireRows(t, home, id) {
		switch r.Event.Type {
		case "tool.call":
			callStep = r.Event.StepUUID
		case "tool.result":
			result = i
		case "step.end":
			if r.Event.UUID == callStep && callStep != "" {
				end = i
			}
		}
	}
	if result < 0 || end < 0 || result > end {
		t.Errorf("tool.result at row %d, the calling step's step.end at row %d: the result must come first", result, end)
	}
	for _, e := range read(t, home, id).Context() {
		if e.Role == transcript.RoleTool && e.Content[0].Text != "RESULT-42" {
			t.Errorf("the model is given %q for the tool call", e.Content[0].Text)
		}
	}
}

// TestToolError checks a failed tool result survives a write and a read:
// kimi reads result.isError.
func TestToolError(t *testing.T) {
	home, id := writeTool(t, transcript.StatusError)
	for _, r := range wireRows(t, home, id) {
		if r.Event.Type == "tool.result" && string(r.Event.Result["isError"]) != "true" {
			t.Errorf("tool.result written as %v, want isError true", r.Event.Result)
		}
	}
	var got []transcript.Status
	for _, e := range read(t, home, id).Context() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolResult {
				got = append(got, b.Status)
			}
		}
	}
	if len(got) != 1 || got[0] != transcript.StatusError {
		t.Errorf("tool results read back with status %v, want [error]", got)
	}
}

// TestIndexDirty checks a write leaves the mark that makes kimi rebuild its
// cached session list: appending to a session that exists moves nothing
// else kimi checks.
func TestIndexDirty(t *testing.T) {
	s := read(t, "testdata/home", sampleID)
	s.Entries = append(s.Entries,
		transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "One more."}}},
		transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Sure."}}},
	)
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	marks, _ := filepath.Glob(filepath.Join(home, root, ".index-dirty", sampleID+".*"))
	if len(marks) != 1 {
		t.Errorf("dirty marks = %v, want one for %s", marks, sampleID)
	}
}

// stage writes rows as a session's wire log under a fresh home.
func stage(t *testing.T, rows ...string) (string, string) {
	t.Helper()
	home, id := t.TempDir(), "session_test"
	dir := filepath.Join(home, root, workspaceDir("/tmp/p"), id)
	if err := os.MkdirAll(filepath.Join(dir, "agents", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"id":"session_test","cwd":"/tmp/p"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "main", "wire.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, id
}

const (
	metaRow = `{"type":"metadata","protocol_version":"1.5"}`
	promptA = `{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"A"}],"id":"msg_A","toolCalls":[],"origin":{"kind":"user"}}}`
	answerA = `{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"sa"}}
{"type":"context.append_loop_event","event":{"type":"content.part","stepUuid":"sa","part":{"type":"text","text":"a"}}}
{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"sa","finishReason":"end_turn"}}`
	promptB = `{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"B"}],"id":"msg_B","toolCalls":[],"origin":{"kind":"user"}}}`
	answerB = `{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"sb"}}
{"type":"context.append_loop_event","event":{"type":"content.part","stepUuid":"sb","part":{"type":"text","text":"b"}}}
{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"sb","finishReason":"end_turn"}}`
)

// TestClear checks context.clear, which kimi writes when a caller resets an
// agent's context (agent/contextMemory/contextMemoryService.ts:87) and no
// CLI command drives: the model starts over, the person still sees it all.
func TestClear(t *testing.T) {
	home, id := stage(t, metaRow, promptA, answerA, `{"type":"context.clear"}`, promptB, answerB)
	s := read(t, home, id)
	if got := texts(s.Context()); strings.Join(got, "|") != "B|b" {
		t.Errorf("context = %q, want B and its answer", got)
	}
	if got := texts(s.Linearize()); strings.Join(got, "|") != "A|a|B|b" {
		t.Errorf("linearize = %q, want every turn", got)
	}
}

// TestLegacyUndo checks a context.undo no agent.switched pairs with, which
// kimi wrote before undo became a branch and still folds (contextOps.ts:90-97).
func TestLegacyUndo(t *testing.T) {
	home, id := stage(t, metaRow, promptA, answerA, promptB, answerB, `{"type":"context.undo","count":1}`)
	s := read(t, home, id)
	if got := texts(s.Context()); strings.Join(got, "|") != "A|a" {
		t.Errorf("context = %q, want the first turn", got)
	}
	if got := texts(s.Linearize()); strings.Join(got, "|") != "A|a" {
		t.Errorf("linearize = %q, want the first turn", got)
	}
	if n := len(s.Messages()); n != 4 {
		t.Errorf("%d messages kept, want the undone turn kept too", n)
	}
}

func TestAudienceOf(t *testing.T) {
	for kind, want := range map[string]transcript.Audience{
		"user":           transcript.AudienceAll,
		"hook_result":    transcript.AudienceAll,
		"task":           transcript.AudienceAll,
		"cron_job":       transcript.AudienceAll,
		"shell_command":  transcript.AudienceAll,
		"injection":      transcript.AudienceModel,
		"system_trigger": transcript.AudienceModel,
		"retry":          transcript.AudienceModel,
	} {
		if got := audienceOf(contextMsg{Role: "user", Origin: &origin{Kind: kind}}); got != want {
			t.Errorf("origin %s: audience %d, want %d", kind, got, want)
		}
	}
}

// TestHeldBackMessage checks a message appended while a tool call waits for
// its result. Kimi appends context messages whenever something produces one
// (agent/contextMemory/contextMemoryService.ts:52-60), a notification in
// the middle of a tool call included, and its fold holds such a message back
// until the results are in (loopEventFold.ts:141-147, 184-200), so the model
// sees it after the result.
func TestHeldBackMessage(t *testing.T) {
	home, id := stage(t, metaRow, promptA,
		`{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"sa"}}`,
		`{"type":"context.append_loop_event","event":{"type":"tool.call","uuid":"ca","stepUuid":"sa","toolCallId":"call_1","name":"Read","args":{}}}`,
		`{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"N"}],"toolCalls":[],"origin":{"kind":"task","taskId":"t1","status":"completed","notificationId":"n1"}}}`,
		`{"type":"context.append_loop_event","event":{"type":"tool.result","parentUuid":"ca","toolCallId":"call_1","result":{"output":"R"}}}`,
		`{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"sa","finishReason":"tool_use"}}`,
		answerB)
	s := read(t, home, id)
	want := "A||result:R|N|b"
	if got := texts(s.Context()); strings.Join(got, "|") != want {
		t.Errorf("context = %q, want %q", got, want)
	}
	if got := texts(s.Linearize()); strings.Join(got, "|") != want {
		t.Errorf("linearize = %q, want %q", got, want)
	}

	// An appended turn continues after the last message delivered.
	s.Entries = append(s.Entries,
		transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "C"}}},
		transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "c"}}},
	)
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if got := texts(s.Context()); strings.Join(got, "|") != want+"|C|c" {
		t.Errorf("context of the written session = %q, want %q", got, want+"|C|c")
	}
	if got := texts(read(t, home, id).Context()); strings.Join(got, "|") != want+"|C|c" {
		t.Errorf("context after append = %q, want %q", got, want+"|C|c")
	}
}

// TestLegacyCompactionUndo checks an undo that cuts into the history a
// legacy compaction kept. Kimi wrote context.apply_compaction without
// keptUserMessageCount before 2.0 (the legacy tail, contextOps.ts:145), and
// its undo removes the kept prompt with everything after it
// (contextOps.ts:91-96).
func TestLegacyCompactionUndo(t *testing.T) {
	home, id := stage(t, metaRow, promptA, answerA, promptB, answerB,
		`{"type":"context.apply_compaction","summary":"S","compactedCount":2}`,
		`{"type":"context.undo","count":1}`)
	s := read(t, home, id)
	if got := texts(s.Context()); strings.Join(got, "|") != "S" {
		t.Errorf("context = %q, want only the summary", got)
	}
}

// Moved to another agent, a compacted session keeps the summary, the one
// record of what the compaction retired.
func TestPortableKeepsSummary(t *testing.T) {
	s := read(t, "testdata/home", compactID)
	var found bool
	for _, e := range s.Portable().Lower(transcript.Capabilities{}).Entries {
		for _, c := range s.Context() {
			if c.Text() == e.Text() && c.ID == "" && e.Role == transcript.RoleUser && !strings.Contains(e.Text(), "<system-reminder>") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("portable carries no summary")
	}
}

// Written for kimi from another agent, reasoning is a think part of the
// step and of the appended message, which kimi sends back to the model as
// reasoning_content.
func TestWriteReasoning(t *testing.T) {
	in := &transcript.Session{Agent: "elsewhere", CWD: "/w", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "go"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{
			{Kind: transcript.BlockReasoning, Text: "weighing it"},
			{Kind: transcript.BlockText, Text: "reading"},
			{Kind: transcript.BlockToolUse, ToolID: "c", Name: "ReadFile", Input: []byte(`{}`)},
		}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "c", Text: "body", Status: transcript.StatusOK}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockReasoning, Text: "only thought"}}},
	}}
	st, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.Context() {
		for _, b := range e.Content {
			got = append(got, string(e.Role)+" "+string(b.Kind)+" "+b.Text)
		}
	}
	want := []string{"user text go", "assistant reasoning weighing it", "assistant text reading", "assistant tool_use ", "tool tool_result body", "assistant reasoning only thought"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("context\n  %q\nwant\n  %q", got, want)
	}
	var raw strings.Builder
	for _, e := range s.Entries {
		raw.Write(e.Raw)
	}
	for _, row := range []string{
		`"part":{"type":"think","think":"weighing it"}`,
		`"content":[{"type":"think","think":"weighing it"},{"type":"text","text":"reading"}]`,
	} {
		if !strings.Contains(raw.String(), row) {
			t.Errorf("no %s in\n%s", row, raw.String())
		}
	}
}

// A compacted session from another agent, pi's compaction probe in the
// harness-test container (four prompts, one run as a private shell command,
// a compaction keeping the last answer, a prompt after it), keeps its whole
// history in the log, the private command's output with it, and a
// context.apply_compaction after it gives the model the summary, the kept
// answer and what followed.
func TestWriteCompacted(t *testing.T) {
	src, err := pi.Codec.Open("../pi/testdata/home")
	if err != nil {
		t.Fatal(err)
	}
	in, err := src.Read(t.Context(), "01a0d2bd-7043-757c-80b2-3f24970d3f7c")
	if err != nil {
		t.Fatal(err)
	}
	in.Agent, in.ID = "elsewhere", ""
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, home, id)
	const answer = "Hello from mock server."
	want := []string{
		"What is the project codename? Reply ONLY", answer,
		"Ran `ls`\n```\nnotes.md\n\n```", "Ran `echo private`\n```\nprivate\n\n```",
		"Second question.", answer,
		"Third question.", answer,
		"After compaction.", answer,
	}
	if got := texts(s.Linearize()); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("linearize = %q\nwant %q", got, want)
	}
	want = []string{"The conversation history before this poi", answer, "After compaction.", answer}
	if got := texts(s.Context()); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("context = %q\nwant %q", got, want)
	}
}

// A session from another agent carries that agent's id, which kimi, listing
// only session_<uuid> directories (createSessionId in
// sessionLifecycleService.ts), would never find; it is written under a new
// one.
func TestForeignSessionID(t *testing.T) {
	home := t.TempDir()
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{ID: "01a0d2bd-7043-757c-80b2-3f24970d3f7c", Agent: "elsewhere", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rest, ok := strings.CutPrefix(id, "session_"); !ok || !transcript.IsUUID(rest) {
		t.Errorf("written as %q, want session_<uuid>", id)
	}
	if got := texts(read(t, home, id).Linearize()); strings.Join(got, "|") != "hi" {
		t.Errorf("read back %q", got)
	}
}
