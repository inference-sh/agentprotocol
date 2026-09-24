package droid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of droid in the harness-test container: a
// prompt, a mock Read call, an answer, then /compress.
const (
	sampleCWD = "/tmp/harness-test-droid-4202561597/test-repo"
	sampleID  = "ecabd2d0-b0bc-4f4a-b6c8-23992aa928c4"
)

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	var tools int
	for _, e := range s.Messages() {
		if e.Role == transcript.RoleTool {
			tools++
		}
	}
	if tools != 1 {
		t.Errorf("tool entries = %d, want 1", tools)
	}
	if lin := s.Linearize(); len(lin) == 0 {
		t.Error("linearize returned nothing")
	}
}

func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, Codec, "/tmp/some/project")
}

func TestAppend(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", transcript.IsUUID)
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestImported(t *testing.T) {
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// The compact sample is droid 0.226 in the harness-test container: a TUI
// run of two turns and /compress, which compacted into a new session
// 439abd18 whose session_start names a44ad01e as parent, then an ACP
// session/load and one prompt on each. The mock server logged what droid
// sent the model on each resume; the tests below hold the codec to it.
const (
	compactHome   = "testdata/compact"
	compactCWD    = "/tmp/harness-test-droid-673672570/test-repo"
	compactChild  = "439abd18-3bfc-4396-a5d9-872128510266"
	compactParent = "a44ad01e-aa38-4c34-9e46-48c5b5ed649a"
)

func TestCompactSampleRoundTrip(t *testing.T) {
	for _, id := range []string{compactChild, compactParent} {
		transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: compactHome, CWD: compactCWD, ID: id})
	}
}

func readCompact(t *testing.T, id string) *transcript.Session {
	t.Helper()
	st, err := Codec.Open(compactHome)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func texts(es []transcript.Entry) []string {
	var out []string
	for _, e := range es {
		text := e.Text()
		if len(text) > 40 {
			text = text[:40]
		}
		out = append(out, string(e.Role)+":"+text)
	}
	return out
}

// TestCompactedResume: resuming the session /compress made, droid sent the
// model the summary with the environment reminder, then the new turn's
// context and prompt. The person is shown only the turn.
func TestCompactedResume(t *testing.T) {
	s := readCompact(t, compactChild)
	ctx := s.Context()
	if len(ctx) == 0 || len(ctx[0].Content) != 2 {
		t.Fatalf("context = %q, want the summary first", texts(ctx))
	}
	const sent = "A previous instance of Droid has summarized the conversation thus far as follows:\n\n<summary>\nHello from mock server.\n</summary>\n\nIMPORTANT: This summary was created by a previous instance of Droid. Files referenced in the summary may not be available until you explicitly view them again."
	if ctx[0].Role != transcript.RoleUser || ctx[0].Content[0].Text != sent {
		t.Errorf("summary = %s %q, want the user message droid sent", ctx[0].Role, ctx[0].Content[0].Text)
	}
	if !strings.HasPrefix(ctx[0].Content[1].Text, "<system-reminder>\n\nUser system info") {
		t.Errorf("summary's second block = %.60q, want the system info reminder", ctx[0].Content[1].Text)
	}
	want := []string{"user:<system-reminder>\nThe tools listed below", "user:What was my previous question?", "assistant:Hello from mock server."}
	if got := texts(ctx[1:]); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("context after the summary = %q, want %q", got, want)
	}
	want = []string{"user:What was my previous question?", "assistant:Hello from mock server."}
	if got := texts(s.Linearize()); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("linearize = %q, want %q", got, want)
	}
}

// TestUncompactedResume: /compress leaves the session it compacted alone,
// and resuming it sent the model every turn in file order behind the first
// turn's llm_only context row, which the person never sees.
func TestUncompactedResume(t *testing.T) {
	s := readCompact(t, compactParent)
	want := []string{
		"user:<system-reminder>\nThe tools listed below",
		"user:What is the project codename? Reply ONLY",
		"assistant:", "tool:",
		"assistant:Hello from mock server.",
		"user:Tell me more about the project.",
		"assistant:Hello from mock server.",
		"user:What was my previous question?",
		"assistant:Hello from mock server.",
	}
	if got := texts(s.Context()); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("context = %q\nwant %q", got, want)
	}
	if got := texts(s.Linearize()); strings.Join(got, "|") != strings.Join(want[1:], "|") {
		t.Errorf("linearize = %q\nwant %q", got, want[1:])
	}
}

// TestCompactionKeepsAfterAnchor: a compaction inside a session (droid's
// automatic one, saveCompactionSummary with anchorMessage) keeps the
// messages after its anchor. The mock server reports a handful of tokens a
// turn, so no automated run reaches droid's threshold; the rows are the
// capture's, with a compaction_state carrying the anchor the automatic path
// writes.
func TestCompactionKeepsAfterAnchor(t *testing.T) {
	rows := []string{
		`{"type":"session_start","id":"s","title":"t","owner":"unknown","version":2,"cwd":"/w"}`,
		`{"type":"message","id":"u1","timestamp":"2026-09-24T09:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-09-24T09:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"one"}]}}`,
		`{"type":"message","id":"u2","parentId":"a1","timestamp":"2026-09-24T09:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"second"}]}}`,
		`{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-09-24T09:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}`,
		`{"type":"compaction_state","id":"c","timestamp":"2026-09-24T09:00:04.000Z","summaryText":"S","summaryTokens":1,"summaryKind":"llm_summary","anchorMessage":{"id":"a1","index":1},"removedCount":2}`,
		`{"type":"message","id":"u3","parentId":"a2","timestamp":"2026-09-24T09:00:05.000Z","message":{"role":"user","content":[{"type":"text","text":"third"}]}}`,
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".factory", "sessions", transcript.MangledCwd.Name("/w"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.Context() {
		got = append(got, e.ID)
	}
	if strings.Join(got, ",") != "c,u2,a2,u3" {
		t.Errorf("context = %v, want the summary, then u2 onward", got)
	}
}

// Moved to another agent, a compacted session keeps the summary, the one
// record of what the compaction retired.
func TestPortableKeepsSummary(t *testing.T) {
	p := readCompact(t, compactChild).Portable().Entries
	if len(p) == 0 || !strings.HasPrefix(p[0].Text(), "A previous instance of Droid has summarized") {
		t.Errorf("portable = %+v, want the summary first", p)
	}
}

// Written for droid from another agent, reasoning is the message's
// chat-completions reasoning, which droid sends back as reasoning_content to
// a model that takes it; no thinking block is made up.
func TestWriteReasoning(t *testing.T) {
	in := &transcript.Session{Agent: "elsewhere", CWD: "/w", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "go"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{
			{Kind: transcript.BlockReasoning, Text: "weighing it"},
			{Kind: transcript.BlockText, Text: "reading"},
			{Kind: transcript.BlockToolUse, ToolID: "c", Name: "Read", Input: []byte(`{}`)},
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
	raw := string(s.Context()[1].Raw)
	if !strings.Contains(raw, `"chatCompletionReasoningField":"reasoning_content","chatCompletionReasoningContent":"weighing it"`) || strings.Contains(raw, `"thinking"`) {
		t.Errorf("assistant row %s", raw)
	}
}
