package codex_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The rows in this file are shapes the harness-test container cannot
// produce: its mock server answers only with text and function calls, and
// the Codex it runs no longer writes the older shapes. Each test names the
// Codex source that writes or reads its rows.

const synthID = "01a0d300-0000-7000-8000-000000000001"

// synth writes a legacy rollout of rows after a session_meta and reads it
// back.
func synth(t *testing.T, rows ...string) *transcript.Session {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".codex/sessions", "2026", "09", "24", "rollout-2026-09-24T10-00-00-"+synthID+".jsonl")
	lines := []string{`{"timestamp":"2026-09-24T10:00:00.000Z","type":"session_meta","payload":{"id":"` + synthID + `","timestamp":"2026-09-24T10:00:00.000Z","cwd":"/p","originator":"codex_cli_rs","cli_version":"0.40.0"}}`}
	for _, r := range rows {
		lines = append(lines, `{"timestamp":"2026-09-24T10:00:01.000Z",`+r+`}`)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return read(t, home, synthID)
}

func msg(role, text string) string {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return fmt.Sprintf(`"type":"response_item","payload":{"type":"message","role":%q,"content":[{"type":%q,"text":%q}]}`, role, kind, text)
}

func responseItem(payload string) string { return `"type":"response_item","payload":` + payload }

// Before replacement_history, a compacted row carried only the summary, and
// Codex rebuilds the history as the user messages so far and the summary
// (rollout_reconstruction.rs, the replacement_history.is_none() branch).
func TestLegacyCompaction(t *testing.T) {
	s := synth(t,
		msg("developer", "<permissions instructions>p</permissions instructions>"),
		msg("user", "<environment_context>\n<cwd>/p</cwd>\n</environment_context>"),
		msg("user", "first"),
		msg("assistant", "a1"),
		msg("user", "second"),
		msg("assistant", "a2"),
		`"type":"compacted","payload":{"message":"SUMMARY"}`,
		msg("user", "third"),
		msg("assistant", "a3"),
	)
	equal(t, "context", texts(s.Context()), []string{
		"user: first", "user: second", "user: SUMMARY", "user: third", "assistant: a3",
	})
	equal(t, "shown", texts(s.Linearize()), []string{
		"user: first", "assistant: a1", "user: second", "assistant: a2", "user: third", "assistant: a3",
	})
}

// Older Codex recorded a rollback as a thread_rolled_back event, which drops
// the last user turns and the injected context just before them
// (rollout_reconstruction.rs, ThreadRolledBack; history.rs,
// drop_last_n_user_turns).
func TestRollback(t *testing.T) {
	s := synth(t,
		msg("developer", "<permissions instructions>p</permissions instructions>"),
		msg("user", "<environment_context>\n<cwd>/p</cwd>\n</environment_context>"),
		msg("user", "one"),
		msg("assistant", "a1"),
		msg("developer", "<model_switch>m</model_switch>"),
		msg("user", "two"),
		msg("assistant", "a2"),
		`"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}`,
		msg("user", "three"),
		msg("assistant", "a3"),
	)
	equal(t, "context", texts(s.Context()), []string{
		"system: <permissions instructions>p</permissions instructions>",
		"user: <environment_context>",
		"user: one", "assistant: a1", "user: three", "assistant: a3",
	})
	equal(t, "shown", texts(s.Linearize()), []string{
		"user: one", "assistant: a1", "user: three", "assistant: a3",
	})
	for _, e := range s.Messages() {
		switch e.Text() {
		case "two", "a2", "<model_switch>m</model_switch>":
			if e.Audience != transcript.AudienceNone {
				t.Errorf("%q has audience %d, want none", e.Text(), e.Audience)
			}
		}
	}
}

// A rollback past a compaction drops items of its replacement history.
func TestRollbackIntoCompaction(t *testing.T) {
	s := synth(t,
		msg("user", "one"),
		msg("assistant", "a1"),
		`"type":"compacted","payload":{"message":"S","replacement_history":[`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]},`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":`+mustJSON(summaryPrefix+"\nS")+`}]}]}`,
		msg("user", "two"),
		msg("assistant", "a2"),
		`"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":2}`,
	)
	equal(t, "context", texts(s.Context()), []string{"user: one"})
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Items other than messages and function calls that the model is given
// (protocol/src/models.rs, ResponseItem), and the inter-agent rows it reads
// as agent messages (history/src/lib.rs, RolloutItem::InterAgentCommunication).
func TestItemTypes(t *testing.T) {
	s := synth(t,
		msg("user", "go"),
		responseItem(`{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}`),
		responseItem(`{"type":"custom_tool_call_output","call_id":"c1","output":"Done!"}`),
		responseItem(`{"type":"local_shell_call","call_id":"c2","status":"completed","action":{"type":"exec","command":["ls"]}}`),
		responseItem(`{"type":"function_call_output","call_id":"c2","output":"a.txt"}`),
		responseItem(`{"type":"web_search_call","id":"ws1","status":"completed","action":{"type":"search","query":"q"}}`),
		responseItem(`{"type":"tool_search_call","call_id":"c3","execution":"client","arguments":{"query":"files"}}`),
		responseItem(`{"type":"tool_search_output","call_id":"c3","status":"completed","execution":"client","tools":[]}`),
		responseItem(`{"type":"image_generation_call","id":"ig1","status":"completed","revised_prompt":"a cat","result":"aGk="}`),
		responseItem(`{"type":"compaction","encrypted_content":"xyz"}`),
		`"type":"inter_agent_communication","payload":{"author":"/root","recipient":"/root/w","other_recipients":[],"content":"do it","trigger_turn":true}`,
		msg("system", "raw system"),
		msg("assistant", "done"),
	)
	var got []string
	for _, e := range s.Messages() {
		var b []string
		for _, c := range e.Content {
			switch c.Kind {
			case transcript.BlockToolUse:
				b = append(b, "use "+c.Name+" "+c.ToolID+" "+string(c.Input))
			case transcript.BlockToolResult:
				b = append(b, "result "+c.ToolID+" "+c.Text)
			default:
				b = append(b, c.Text)
			}
		}
		got = append(got, fmt.Sprintf("%s/%d: %s", e.Role, e.Audience, strings.Join(b, "; ")))
	}
	equal(t, "messages", got, []string{
		"user/0: go",
		`assistant/0: use apply_patch c1 "*** Begin Patch\n*** End Patch"`,
		"tool/0: result c1 Done!",
		`assistant/0: use local_shell c2 {"type":"exec","command":["ls"]}`,
		"tool/0: result c2 a.txt",
		`assistant/0: use web_search ws1 {"type":"search","query":"q"}`,
		`assistant/0: use tool_search c3 {"query":"files"}`,
		"tool/0: result c3 []",
		`assistant/0: use image_generation ig1 {"prompt":"a cat"}`,
		"system/1: ",
		"user/0: do it",
		"system/3: raw system",
		"assistant/0: done",
	})
	for _, e := range s.Context() {
		if e.Text() == "raw system" {
			t.Error("the model is given a raw system message")
		}
	}
}
