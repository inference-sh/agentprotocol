package sqlite

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/claude"
	"github.com/inference-sh/agentprotocol/transcript/codex"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The samples are runs in the harness-test container against its mock
// model. goose 1.52.0: an ACP run with a tool call, three more turns
// resumed over ACP, /compact, and one turn after it. hermes is hermes at
// be59f041 (store schema 30): an ACP run with a tool call, then five turns
// over ACP with a compression threshold low enough that it compacted twice
// on its own. hermes-0.19 is the released v0.19.0 (schema 22): the same ACP
// run, two more ACP turns, then the CLI resumed for a turn, /compress and one
// turn after it.
const (
	gooseCWD    = "/tmp/harness-test-goose-907330133/test-repo"
	gooseID     = "20260924_1"
	hermesID    = "23c2e6bd-e80f-4746-80ec-9c7e8a80cb91"
	hermes019ID = "7538b1c0-6bca-4cc3-94e3-f603c8142094"
)

func TestGooseRead(t *testing.T) {
	st, err := Goose.Open("testdata/goose")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), gooseCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != gooseID {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), gooseID)
	if err != nil {
		t.Fatal(err)
	}
	var calls, results int
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				calls++
				if b.Name != "shell" {
					t.Errorf("tool name = %q", b.Name)
				}
			case transcript.BlockToolResult:
				results++
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Errorf("calls %d results %d", calls, results)
	}
	if turns := countTurns(s); turns == 0 {
		t.Error("no turn started in events")
	}
}

func TestHermesRead(t *testing.T) {
	st, err := Hermes.Open("testdata/hermes")
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read(t.Context(), hermesID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[transcript.Role]int{}
	for _, e := range s.Messages() {
		roles[e.Role]++
	}
	if roles[transcript.RoleUser] == 0 || roles[transcript.RoleAssistant] == 0 || roles[transcript.RoleTool] == 0 {
		t.Errorf("roles = %v", roles)
	}
}

// TestRoundTrip writes each store into a fresh home and reads it back, the
// same shape the JSONL conformance test uses, since the SQLite stores share
// no code with it.
func TestGooseRoundTrip(t *testing.T)  { roundTrip(t, Goose, "goose") }
func TestHermesRoundTrip(t *testing.T) { roundTrip(t, Hermes, "hermes") }

func roundTrip(t *testing.T, codec transcript.Codec, agent string) {
	t.Helper()
	home := t.TempDir()
	st, err := codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s := &transcript.Session{Agent: agent, CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "What is the codename?"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "call_1", Name: "read", Input: []byte(`{"path":"README.md"}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "call_1", Name: "read", Text: "HERON", Status: transcript.StatusOK}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "HERON"}}},
	}}
	id, err := st.Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	back, err := st.Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// The check is on content, not message count: goose and hermes keep a
	// tool result in its own message, while opencode folds it into the
	// assistant message, and both are faithful.
	msgs := back.Messages()
	if len(msgs) == 0 {
		t.Fatal("no messages read back")
	}
	first := msgs[0]
	if first.Role != transcript.RoleUser || first.Text() != "What is the codename?" {
		t.Errorf("first message = %+v", first)
	}
	var call, result, finalText bool
	for _, e := range msgs {
		for _, b := range e.Content {
			switch {
			case b.Kind == transcript.BlockToolUse && b.ToolID == "call_1":
				call = true
			case b.Kind == transcript.BlockToolResult && b.Text == "HERON":
				result = true
			case b.Kind == transcript.BlockText && b.Text == "HERON" && e.Role == transcript.RoleAssistant:
				finalText = true
			}
		}
	}
	if !call || !result || !finalText {
		t.Errorf("call %v, result %v, finalText %v", call, result, finalText)
	}
	if got, _ := st.List(t.Context(), "/tmp/p"); len(got) != 1 || got[0].ID != id {
		t.Errorf("list = %+v", got)
	}
}

func TestRegistered(t *testing.T) {
	for _, agent := range []string{"goose", "hermes"} {
		if _, ok := transcript.Registered(agent); !ok {
			t.Errorf("%s not registered", agent)
		}
	}
}

func countTurns(s *transcript.Session) int {
	var n int
	for _, ev := range s.Events() {
		if ev.Type == "turn.started" {
			n++
		}
	}
	return n
}

// TestGooseWriteKeepsExisting reproduces the harness seed probe: goose has
// run a turn today, so its store holds today's first session, and a
// hand-built session is then written with no id. The write must take a new id
// and leave goose's session intact.
func TestGooseWriteKeepsExisting(t *testing.T) {
	st, err := Goose.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("20060102") + "_1"
	if _, err := st.Write(t.Context(), &transcript.Session{ID: today, CWD: gooseCWD, Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "goose's own turn"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: gooseCWD, Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "planted"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if id == today {
		t.Fatalf("the hand-built session took the id %s of goose's own session", id)
	}
	own, err := st.Read(t.Context(), today)
	if err != nil {
		t.Fatalf("goose's own session is gone: %v", err)
	}
	if msgs := own.Messages(); len(msgs) != 1 || msgs[0].Text() != "goose's own turn" {
		t.Errorf("goose's own session was replaced: %+v", msgs)
	}
}

// TestHermesHandBuiltRestorable checks the columns hermes's ACP adapter reads
// to restore a session: it restores only source "acp", and takes the
// directory from model_config. With source "cli" the seed probe's session
// loaded as an empty one and the planted fact never reached the model.
func TestHermesHandBuiltRestorable(t *testing.T) {
	home := t.TempDir()
	st, err := Hermes.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	db, done, err := openRO(filepath.Join(home, hermesPath))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	defer db.Close()
	var source, cwd string
	if err := db.QueryRow(`SELECT source, json_extract(model_config, '$.cwd') FROM sessions WHERE id = ?`, id).Scan(&source, &cwd); err != nil {
		t.Fatal(err)
	}
	if source != "acp" {
		t.Errorf("source = %q; hermes's ACP adapter restores only \"acp\"", source)
	}
	if cwd != "/tmp/p" {
		t.Errorf("model_config cwd = %q", cwd)
	}
}

// said lists entries as "role: text", with a tool call or result as its
// tool id, so a test can compare what the person sees or the model gets.
func said(es []transcript.Entry) []string {
	var out []string
	for _, e := range es {
		x := e.Text()
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockToolUse:
				x += "call " + b.ToolID
			case transcript.BlockToolResult:
				x += "result " + b.ToolID
			}
		}
		if len(x) > 60 {
			x = x[:60]
		}
		out = append(out, string(e.Role)+": "+x)
	}
	return out
}

func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n  got  %q\n  want %q", what, got, want)
	}
}

func readSample(t *testing.T, codec transcript.Codec, home, id string) *transcript.Session {
	t.Helper()
	s, err := mustOpen(t, codec, home).Read(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestGooseAudience checks the goose sample against what goose shows and
// sends. Its turn-context rows are for the model only; /compact kept every
// earlier row for the person only, and added a summary and a continuation
// for the model only, then showed the person its own echo.
func TestGooseAudience(t *testing.T) {
	s := readSample(t, Goose, "testdata/goose", gooseID)
	const hello = "assistant: Hello from mock server."
	sameLines(t, "shown", said(s.Linearize()), []string{
		"user: What is the project codename? Reply ONLY the codename.",
		"assistant: call call_mock_1",
		"tool: result call_mock_1",
		hello,
		"user: The codename is HERON. Remember it.", hello,
		"user: What colour is the sky?", hello,
		"user: Name a prime number.", hello,
		"user: /compact",
		"assistant: Compaction complete",
		"user: What was the codename?", hello,
	})
	ctx := said(s.Context())
	if len(ctx) != 5 || ctx[0] != "user: Hello from mock server." ||
		!strings.HasPrefix(ctx[1], "assistant: Your context was compacted at the user's request.") ||
		ctx[2] != "user: What was the codename?" || !strings.HasPrefix(ctx[3], "user: <turn-context>") || ctx[4] != hello {
		t.Errorf("model context: %q", ctx)
	}
	// The summary moves with the conversation; the history it retired, the
	// continuation instruction and the turn context stay behind.
	sameLines(t, "portable", said(s.Portable().Lower(transcript.Capabilities{}).Entries), []string{
		"user: Hello from mock server.",
		"user: What was the codename?", hello,
	})
}

// TestGooseBlocks decodes the content blocks the mock model cannot make
// goose write, in the JSON goose serializes them as
// (goose-provider-types/src/conversation/message.rs MessageContentBlock,
// tool_result_serde.rs): thinking, a system notification, an error, a
// confirmation request, audience annotations and failed tool results.
func TestGooseBlocks(t *testing.T) {
	both := gooseVisibility("")
	cases := []struct {
		name, role, content string
		meta                gooseMeta
		want                []transcript.Block
		audience            transcript.Audience
	}{
		{
			name: "thinking is reasoning", role: "assistant", meta: both,
			content: `[{"type":"thinking","thinking":"pondering","signature":"sig"},{"type":"redactedThinking","data":"xx"},{"type":"text","text":"done"}]`,
			want:    []transcript.Block{{Kind: transcript.BlockReasoning, Text: "pondering"}, {Kind: transcript.BlockText, Text: "done"}},
		},
		{
			name: "a notification is shown and never sent", role: "assistant", meta: both,
			content:  `[{"type":"systemNotification","notificationType":"inlineMessage","msg":"Compacting"}]`,
			want:     []transcript.Block{{Kind: transcript.BlockText, Text: "Compacting"}},
			audience: transcript.AudienceUser,
		},
		{
			name: "an error is shown and never sent", role: "assistant", meta: both,
			content:  `[{"type":"error","kind":"contextLengthExceeded","message":"too long"}]`,
			want:     []transcript.Block{{Kind: transcript.BlockText, Text: "too long"}},
			audience: transcript.AudienceUser,
		},
		{
			name: "a confirmation request is shown and never sent", role: "assistant", meta: both,
			content:  `[{"type":"toolConfirmationRequest","id":"c1","toolName":"shell","arguments":{},"prompt":null}]`,
			audience: transcript.AudienceUser,
		},
		{
			name: "annotated text goes to its audience", role: "assistant", meta: both,
			content: `[{"type":"text","text":"for you","annotations":{"audience":["user"]}},{"type":"text","text":"for the model","annotations":{"audience":["assistant"]}}]`,
			want:    []transcript.Block{{Kind: transcript.BlockText, Text: "for the model"}},
		},
		{
			name: "a tool result is what the model was given", role: "user", meta: both,
			content: `[{"type":"toolResponse","id":"t1","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"raw","annotations":{"audience":["assistant"]}},{"type":"text","text":"pretty","annotations":{"audience":["user"]}}],"isError":false}}}]`,
			want:    []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "t1", Text: "raw", Status: transcript.StatusOK}},
		},
		{
			name: "isError is an error", role: "user", meta: both,
			content: `[{"type":"toolResponse","id":"t2","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"exit 1"}],"isError":true}}}]`,
			want:    []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "t2", Text: "exit 1", Status: transcript.StatusError}},
		},
		{
			name: "a failed call is an error", role: "user", meta: both,
			content: `[{"type":"toolResponse","id":"t3","toolResult":{"status":"error","error":"no such tool"}}]`,
			want:    []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "t3", Text: "no such tool", Status: transcript.StatusError}},
		},
		{
			// goose's ACP prompt has no document case (acp/server.rs
			// convert_acp_prompt_to_message); its SDK writes one
			// (goose-sdk bindings.rs to_goose_content).
			name: "a document is a file", role: "user", meta: both,
			content: `[{"type":"text","text":"read this"},{"type":"document","data":"JVBERi0=","mimeType":"application/pdf","name":"q3.pdf"}]`,
			want:    []transcript.Block{{Kind: transcript.BlockText, Text: "read this"}, {Kind: transcript.BlockFile, MediaType: "application/pdf", Name: "q3.pdf", Data: []byte("%PDF-")}},
		},
		{
			name: "user-only metadata", role: "user", meta: gooseVisibility(`{"userVisible":true,"agentVisible":false}`),
			content:  `[{"type":"text","text":"/compact"}]`,
			want:     []transcript.Block{{Kind: transcript.BlockText, Text: "/compact"}},
			audience: transcript.AudienceUser,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := gooseEntry(c.role, c.content, c.meta)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(e.Content)
			want, _ := json.Marshal(c.want)
			if string(got) != string(want) {
				t.Errorf("content %s, want %s", got, want)
			}
			if e.Audience != c.audience {
				t.Errorf("audience %d, want %d", e.Audience, c.audience)
			}
		})
	}
}

// The goose image sample is a goose 1.52.0 ACP run in the harness-test
// container: a prompt with a PNG, then a read_image call on the same PNG.
const (
	gooseImageHome = "testdata/goose-image"
	gooseImageID   = "20260924_2"
)

// TestGooseImages reads the prompt's image and the one read_image returned,
// which follows the call's result.
func TestGooseImages(t *testing.T) {
	png, err := os.ReadFile("testdata/pic.png")
	if err != nil {
		t.Fatal(err)
	}
	s := readSample(t, Goose, gooseImageHome, gooseImageID)
	want := []string{"user: image image/png   97", "tool: image image/png  call_mock_1 97"}
	sameLines(t, "model", media(s.Context()), want)
	sameLines(t, "person", media(s.Linearize()), want)
	for _, e := range s.Context() {
		for i, b := range e.Content {
			if b.Kind == transcript.BlockImage && !bytes.Equal(b.Data, png) {
				t.Errorf("%s image is not the PNG sent", e.Role)
			}
			if b.Kind == transcript.BlockImage && b.ToolID != "" && e.Content[i-1].Kind != transcript.BlockToolResult {
				t.Errorf("tool image does not follow its result: %+v", e.Content)
			}
		}
	}
}

// TestGooseImagesImported writes the image sample as another agent's
// session: the images go back as goose's image items, in the prompt and in
// the tool's result.
func TestGooseImagesImported(t *testing.T) {
	transcripttest.Imported(t, Goose, transcripttest.Sample{Home: copyHome(t, gooseImageHome), ID: gooseImageID})
	s := readSample(t, Goose, gooseImageHome, gooseImageID)
	want := media(s.Portable().Lower(transcript.Capabilities{}).Entries)
	s.Agent = "elsewhere"
	home := t.TempDir()
	id, err := mustOpen(t, Goose, home).Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	sameLines(t, "images", media(readSample(t, Goose, home, id).Context()), want)
}

// TestGooseReasoningImported writes another agent's reasoning as a thinking
// item with the empty signature goose gives reasoning an OpenAI-compatible
// model streams, which goose sends back as reasoning_content.
func TestGooseReasoningImported(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s := &transcript.Session{Agent: "elsewhere", CWD: "/work", Created: now, Updated: now, Entries: []transcript.Entry{
		{ID: "u1", Role: transcript.RoleUser, Time: now, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
		{ID: "a1", ParentID: "u1", Role: transcript.RoleAssistant, Time: now.Add(time.Second), Content: []transcript.Block{
			{Kind: transcript.BlockReasoning, Text: "weighing it"},
			{Kind: transcript.BlockText, Text: "hello"},
		}},
	}}
	home := t.TempDir()
	id, err := mustOpen(t, Goose, home).Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	got := readSample(t, Goose, home, id)
	var raw string
	for _, e := range got.Context() {
		if e.Role == transcript.RoleAssistant {
			b, _ := json.Marshal(e.Content)
			raw = string(b)
			if !strings.Contains(string(e.Raw), `{"type":"thinking","thinking":"weighing it","signature":""}`) {
				t.Errorf("stored %s, want a thinking item with an empty signature", e.Raw)
			}
		}
	}
	if want := `[{"kind":"reasoning","text":"weighing it"},{"kind":"text","text":"hello"}]`; raw != want {
		t.Errorf("assistant %s, want %s", raw, want)
	}
}

// TestGooseAppendOrder writes a turn whose time is before the sample's last
// row. goose reads rows back by created_timestamp, so the turn must still
// be stored after them, as goose's own append clamps it.
func TestGooseAppendOrder(t *testing.T) {
	home := copyHome(t, "testdata/goose")
	s := readSample(t, Goose, home, gooseID)
	s.Entries = append(s.Entries,
		transcript.Entry{Role: transcript.RoleUser, Time: s.Created.Add(-time.Hour), Content: []transcript.Block{{Kind: transcript.BlockText, Text: "late but last"}}})
	if _, err := mustOpen(t, Goose, home).Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	back := said(readSample(t, Goose, home, gooseID).Linearize())
	if back[len(back)-1] != "user: late but last" {
		t.Errorf("appended turn reads back at %q", back)
	}
	db, done, err := openRO(filepath.Join(home, goosePath))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	defer db.Close()
	var last, before int64
	if err := db.QueryRow(`SELECT created_timestamp FROM messages WHERE session_id = ? AND content_json LIKE '%late but last%'`, gooseID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT MAX(created_timestamp) FROM messages WHERE session_id = ? AND content_json NOT LIKE '%late but last%'`, gooseID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if last < before {
		t.Errorf("appended row stored at %d, before the latest row at %d: goose would load it earlier", last, before)
	}
}

// TestHermes019Audience checks the hermes v0.19 sample. Its /compress archived
// every row (active 0, compacted 1), put a copy of the first message back,
// merged the summary into a copy of the last reply, and re-inserted the
// latest turn. The person still sees the archived rows; the model gets the
// live ones. The user rows also carry api_content, the prompt with hook
// context appended, which is what the model was sent; the person saw
// content.
func TestHermes019Audience(t *testing.T) {
	s := readSample(t, Hermes, "testdata/hermes-0.19", hermes019ID)
	const hello = "assistant: Hello from mock server."
	sameLines(t, "shown", said(s.Linearize()), []string{
		"user: What is the project codename? Reply ONLY the codename.",
		"assistant: call call_mock_1",
		"tool: result call_mock_1",
		hello,
		"user: The codename is HERON. Remember it.", hello,
		"user: What colour is the sky?", hello,
		"user: Name a prime number.", hello,
		// The summary carrier shows only the reply merged into it.
		hello,
		"user: Name a prime number.", hello,
		"user: What was the codename?", hello,
	})
	ctx := said(s.Context())
	// Prompts reach the model as hermes sent them, the hook's context
	// appended (api_content); said cuts each line at 60 characters.
	if len(ctx) != 6 || ctx[0] != "user: What is the project codename? Reply ONLY the codename.\n\nThe " ||
		!strings.HasPrefix(ctx[1], "assistant: [PRIOR CONTEXT") || ctx[2] != "user: Name a prime number.\n\nThe project codename is HOOK-HERMES-17" {
		t.Errorf("model context: %q", ctx)
	}
	for _, e := range s.Context() {
		if strings.Contains(e.Text(), "HERON. Remember") {
			t.Errorf("the model is given archived history: %q", e.Text())
		}
	}
	// Another agent gets the prompts as the person typed them, and the
	// summary with the reply merged into it, without hermes' framing.
	entries := s.Portable().Lower(transcript.Capabilities{}).Entries
	port := said(entries)
	if len(port) != 6 || port[0] != "user: What is the project codename? Reply ONLY the codename." ||
		!strings.HasPrefix(entries[1].Text(), "Hello from mock server.\n\n## Historical Task Snapshot\n") || entries[1].Role != transcript.RoleAssistant {
		t.Errorf("portable: %q", port)
	}
	noHermesFraming(t, entries)
}

// noHermesFraming checks that entries carry none of the framing hermes puts
// around a summary; it is hermes' own and stays with hermes.
func noHermesFraming(t *testing.T, entries []transcript.Entry) {
	t.Helper()
	for _, e := range entries {
		for _, m := range []string{hermesSummaryPrefix, hermesSummaryEnd, hermesPriorHeader, hermesPriorDelimiter} {
			if strings.Contains(e.Text(), m) {
				t.Errorf("portable carries hermes' framing %q in %q", m, e.Text())
			}
		}
	}
}

// TestHermesAudience checks the sample from hermes's current store:
// compaction flags the summary row, supersedes the carried tail with
// rewind flags (active 0, compacted 0) and re-inserts copies, and a second
// compaction archives the first one's summary and copies.
func TestHermesAudience(t *testing.T) {
	s := readSample(t, Hermes, "testdata/hermes", hermesID)
	const hello = "assistant: Hello from mock server."
	var shown []string
	for _, x := range said(s.Linearize()) {
		shown = append(shown, strings.SplitN(x, " Background", 2)[0])
	}
	sameLines(t, "shown", shown, []string{
		"user: What is the project codename? Reply ONLY the codename.",
		"assistant: call call_mock_1",
		"tool: result call_mock_1",
		hello,
		"user: The codename is HERON. Remember it.",
		hello, // the first summary's carrier, as the reply merged into it
		"user: What colour is the sky?",
		hello, // the copy of the reply the second compaction carried
		"user: Name a prime number.", hello,
		"user: What was the codename?", hello,
		"user: Thanks.", hello,
	})
	ctx := s.Context()
	if len(ctx) != 8 || !strings.HasPrefix(ctx[0].Text(), hermesSummaryPrefix) || ctx[0].Role != transcript.RoleUser {
		t.Fatalf("model context: %q", said(ctx))
	}
	port := s.Portable().Lower(transcript.Capabilities{}).Entries
	if len(port) != 8 || !strings.HasPrefix(port[0].Text(), "## Historical Task Snapshot\n") || !strings.Contains(ctx[0].Text(), port[0].Text()) {
		t.Errorf("portable does not start with the bare summary: %q", said(port))
	}
	noHermesFraming(t, port)
	retired := map[string]bool{}
	for _, e := range s.Entries {
		if !e.Audience.Model() {
			retired[e.ID] = true
		}
	}
	for _, e := range port[1:] {
		if retired[e.ID] {
			t.Errorf("portable carries retired history: %s %q", e.ID, e.Text())
		}
	}
}

// TestHermes019Rewrite writes the v0.19 sample back unchanged and with a
// turn appended. Rows the model is no longer given must survive both.
func TestHermes019Rewrite(t *testing.T) {
	home := copyHome(t, "testdata/hermes-0.19")
	db := filepath.Join(home, hermesPath)
	before := dump(t, db)
	s := readSample(t, Hermes, home, hermes019ID)
	if _, err := mustOpen(t, Hermes, home).Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, db); strings.Join(got["messages"], "\n") != strings.Join(before["messages"], "\n") {
		t.Error("an unchanged rewrite changed the messages table")
	}
	s.Entries = append(s.Entries, transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "one more"}}})
	if _, err := mustOpen(t, Hermes, home).Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	back := readSample(t, Hermes, home, hermes019ID)
	if len(back.Messages()) != len(s.Messages()) {
		t.Errorf("read back %d messages, wrote %d", len(back.Messages()), len(s.Messages()))
	}
	if lin := said(back.Linearize()); lin[len(lin)-1] != "user: one more" {
		t.Errorf("appended turn is not last: %q", lin)
	}
}

// TestHermesContentShapes decodes rows the mock model cannot make hermes
// write: content stored as a JSON list of parts
// (hermes_state_messages.py _encode_content), and the bookkeeping roles the
// messaging gateway writes (gateway/run_turn.py session_meta), which hermes
// strips before the model sees history.
func TestHermesContentShapes(t *testing.T) {
	e, err := hermesEntry("user", hermesJSONPrefix+`[{"type":"text","text":"look at this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}},{"type":"text","text":"and this"}]`, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := said([]transcript.Entry{e}); got[0] != "user: look at thisand this" || len(e.Content) != 3 {
		t.Errorf("list content decodes to %q (%d blocks)", got, len(e.Content))
	}
	for _, role := range []string{"session_meta", "system"} {
		e, err := hermesEntry(role, "meta", "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if e.Role != transcript.RoleOpaque {
			t.Errorf("%s row decodes as %q", role, e.Role)
		}
	}
	// A bookkeeping row survives a rewrite.
	home := copyHome(t, "testdata/hermes")
	db, err := openRW(filepath.Join(home, hermesPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, 'session_meta', '{"platform":"telegram"}', 1790242941.0)`, hermesID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := readSample(t, Hermes, home, hermesID)
	n := len(s.Messages())
	if _, err := mustOpen(t, Hermes, home).Write(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	back := readSample(t, Hermes, home, hermesID)
	if len(back.Entries) != len(s.Entries) || len(back.Messages()) != n {
		t.Errorf("rewrite kept %d of %d rows", len(back.Entries), len(s.Entries))
	}
}

// TestHermesImages decodes the images hermes keeps in a JSON list of parts.
// The released hermes writes a turn's images as "[screenshot]" text
// (agent/session_persistence.py _durable_content), which is what an ACP
// image prompt in the harness-test container stored; the list is stored
// when hermes writes its live messages back (hermes_state_messages.py
// replace_messages, on a rewind or a compaction): a prompt's images as
// image_url parts with a data: URL (acp_adapter/content.py _image_parts) or
// an http(s) one (gateway/platforms/api_server.py _normalize_image_part),
// and a screenshot a tool returned (tool_executor.py, a multimodal result
// as an OpenAI content list).
func TestHermesImages(t *testing.T) {
	cases := []struct {
		name, role, content, toolID string
		want                        []transcript.Block
	}{{
		name: "a prompt's image", role: "user",
		content: hermesJSONPrefix + `[{"type": "text", "text": "[Attached image: pic.png]"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw=="}}]`,
		want:    []transcript.Block{{Kind: transcript.BlockText, Text: "[Attached image: pic.png]"}, {Kind: transcript.BlockImage, MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}},
	}, {
		name: "an image by URL", role: "user",
		content: hermesJSONPrefix + `[{"type": "image_url", "image_url": {"url": "https://example.com/a.jpg", "detail": "high"}}]`,
		want:    []transcript.Block{{Kind: transcript.BlockImage, URI: "https://example.com/a.jpg"}},
	}, {
		name: "a tool's screenshot", role: "tool", toolID: "call_1",
		content: hermesJSONPrefix + `[{"type": "text", "text": "captured"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw=="}}]`,
		want: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "call_1", Name: "computer_use", Text: "captured", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "call_1", MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
		},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := ""
			if c.toolID != "" {
				name = "computer_use"
			}
			e, err := hermesEntry(c.role, c.content, c.toolID, name, "", "")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(e.Content)
			want, _ := json.Marshal(c.want)
			if string(got) != string(want) {
				t.Errorf("content %s, want %s", got, want)
			}
		})
	}
}

// TestHermesImagesWritten writes another agent's images into hermes: a
// prompt's and a tool's go in as image_url parts and read back as they were;
// a file, which hermes has no part for, is dropped.
func TestHermesImagesWritten(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	s := &transcript.Session{Agent: "elsewhere", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{
			{Kind: transcript.BlockText, Text: "what is this"},
			{Kind: transcript.BlockImage, MediaType: "image/png", Data: png},
			{Kind: transcript.BlockFile, MediaType: "application/pdf", Name: "a.pdf", Data: []byte("%PDF-")},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "shot", Input: json.RawMessage(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{
			{Kind: transcript.BlockToolResult, ToolID: "c1", Name: "shot", Text: "done", Status: transcript.StatusOK},
			{Kind: transcript.BlockImage, ToolID: "c1", MediaType: "image/png", Data: png},
		}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "a PNG"}}},
	}}
	home := t.TempDir()
	id, err := mustOpen(t, Hermes, home).Write(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	back := readSample(t, Hermes, home, id)
	sameLines(t, "images", media(back.Context()), []string{"user: image image/png   4", "tool: image image/png  c1 4"})
	sameLines(t, "text", said(back.Context()), []string{"user: what is this", "assistant: call c1", "tool: result c1", "assistant: a PNG"})
	if r := back.Context()[2].Content[0]; r.Text != "done" {
		t.Errorf("tool result reads back as %q", r.Text)
	}
}

// TestHermesListCompressionChain lists a legacy compression rotation: hermes
// ended the session it compressed with end_reason 'compression' and moved
// the conversation into a child session (hermes_state_compression.py
// publish_compression_child). No run of the hermes in the container rotates;
// it compacts in place. hermes lists the chain once, under the child.
func TestHermesListCompressionChain(t *testing.T) {
	home := copyHome(t, "testdata/hermes")
	db, err := openRW(filepath.Join(home, hermesPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sessions (id, source, model_config, started_at, ended_at, end_reason, title) VALUES ('root', 'acp', '{"cwd":"/w"}', 100, 200, 'compression', 'the chat')`,
		`INSERT INTO sessions (id, source, model_config, parent_session_id, started_at, ended_at, end_reason) VALUES ('mid', 'acp', '{"cwd":"/w"}', 'root', 200, 300, 'compression')`,
		`INSERT INTO sessions (id, source, model_config, parent_session_id, started_at) VALUES ('tip', 'acp', '{"cwd":"/w"}', 'mid', 300)`,
		`INSERT INTO sessions (id, source, model_config, parent_session_id, started_at) VALUES ('helper', 'acp', '{"cwd":"/w","_delegate_from":"tip"}', 'tip', 310)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	infos, err := mustOpen(t, Hermes, home).List(t.Context(), "/w")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != "tip" || infos[0].Title != "the chat" {
		t.Errorf("list = %+v, want the chain once as its tip", infos)
	}
}

// TestHermesAPIContent: hermes stores the prompt as the person typed it
// and, beside it, the bytes it sent, here with the hook's context
// appended. The model is given those bytes on resume; the person, and
// another agent, get the prompt.
func TestHermesAPIContent(t *testing.T) {
	s := readSample(t, Hermes, "testdata/hermes-0.19", hermes019ID)
	const prompt = "What is the project codename? Reply ONLY the codename."
	var sent, shown bool
	for _, e := range s.Context() {
		if strings.HasPrefix(e.Text(), prompt+"\n\nThe project codename is HOOK-HERMES-") {
			sent = true
		}
	}
	for _, e := range s.Linearize() {
		if e.Text() == prompt {
			shown = true
		}
	}
	if !sent || !shown {
		t.Errorf("hook context sent to the model: %v; prompt shown as typed: %v", sent, shown)
	}
	for _, e := range s.Portable().Lower(transcript.Capabilities{}).Entries {
		if strings.Contains(e.Text(), "HOOK-HERMES-") {
			t.Errorf("hermes' hook context moved with the session: %q", e.Text())
		}
	}
}

// TestHermesReasoningReplay: hermes replays an assistant row's reasoning
// only to a provider that requires it echoed back (DeepSeek, Kimi, MiMo),
// as reasoning_content: that column verbatim when set, else the reasoning
// of a turn without tool calls, else a one-space pad
// (agent/message_sanitization.py apply_reasoning_content_policy). Every
// other provider is given none. The ACP adapter resumes on the provider
// the session records (acp_adapter/session.py). What the person sees keeps
// the reasoning either way. The mock model streams no reasoning; the
// reasoning_content column is set as hermes pins it on a row
// (hermes_state_messages.py append_message).
func TestHermesReasoningReplay(t *testing.T) {
	home := copyHome(t, "testdata/hermes")
	st := mustOpen(t, Hermes, home)
	text := func(s string) transcript.Block { return transcript.Block{Kind: transcript.BlockText, Text: s} }
	think := func(s string) transcript.Block { return transcript.Block{Kind: transcript.BlockReasoning, Text: s} }
	id, err := st.Write(t.Context(), &transcript.Session{CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{text("hi")}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{think("THINK-1"), text("hello"),
			{Kind: transcript.BlockToolUse, ToolID: "c1", Name: "read", Input: json.RawMessage(`{}`)}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "c1", Name: "read", Text: "ok"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{think("THINK-2"), text("done")}},
		{Role: transcript.RoleUser, Content: []transcript.Block{text("again")}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{think("THINK-3"), text("sure")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	reasoning := func(es []transcript.Entry) []string {
		var out []string
		for _, e := range es {
			for _, b := range e.Content {
				if b.Kind == transcript.BlockReasoning {
					out = append(out, b.Text)
				}
			}
		}
		return out
	}

	// The writer names no provider: hermes runs it on the configured one,
	// here none that asks for reasoning back.
	s := readSample(t, Hermes, home, id)
	sameLines(t, "context reasoning, strict provider", reasoning(s.Context()), nil)
	sameLines(t, "shown reasoning", reasoning(s.Linearize()), []string{"THINK-1", "THINK-2", "THINK-3"})

	db, err := openRW(filepath.Join(home, hermesPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`UPDATE sessions SET model_config = json_set(model_config, '$.provider', 'deepseek') WHERE id = ?`,
		`UPDATE messages SET reasoning_content = 'SENT-3' WHERE session_id = ? AND reasoning = 'THINK-3'`,
	} {
		if _, err := db.Exec(q, id); err != nil {
			t.Fatal(err)
		}
	}
	s = readSample(t, Hermes, home, id)
	sameLines(t, "context reasoning, deepseek", reasoning(s.Context()), []string{"THINK-2", "SENT-3"})
	sameLines(t, "shown reasoning", reasoning(s.Linearize()), []string{"THINK-1", "THINK-2", "THINK-3"})
}

// TestGooseImportsLoad moves the sessions goose refused to load after an
// import ("Session not found": a row whose content does not deserialize
// fails the whole load) into goose: claude's and codex's image runs, where a
// tool returned only an image, and kiro's image run and its /clear, which
// carries a compaction without a summary. Every item written must have the
// fields goose's content types require, and nothing the model is sent may
// be an empty text.
func TestGooseImportsLoad(t *testing.T) {
	for _, src := range []struct {
		name     string
		codec    transcript.Codec
		home, id string
	}{
		{"claude.images", claude.Codec, "../claude/testdata/home", "07f0f8b5-1255-4c04-b97b-e1f209e315ac"},
		{"codex.images", codex.Codec, "../codex/testdata/home", "01a0d2e7-4559-78b3-a835-f49e78b998c0"},
		{"kiro.images", Kiro, "../kiro/testdata/home", "d2db899b-7e34-4f60-be19-750e06623c82"},
		{"kiro.clear", Kiro, "../kiro/testdata/home", "dcce2ace-938a-4799-a666-20d485cc9a42"},
	} {
		t.Run(src.name, func(t *testing.T) {
			s := readSample(t, src.codec, src.home, src.id)
			s.ID = ""
			home := t.TempDir()
			id, err := mustOpen(t, Goose, home).Write(t.Context(), s)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", "file:"+filepath.Join(home, goosePath))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows, err := db.Query("SELECT content_json, metadata_json FROM messages WHERE session_id = ?", id)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var content, meta string
				if err := rows.Scan(&content, &meta); err != nil {
					t.Fatal(err)
				}
				var items []map[string]json.RawMessage
				if err := json.Unmarshal([]byte(content), &items); err != nil {
					t.Fatal(err)
				}
				checkGooseItems(t, items)
				if strings.Contains(meta, `"agentVisible":true`) && content == `[{"type":"text","text":""}]` {
					t.Errorf("the model is sent an empty message")
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// checkGooseItems checks each content item has the fields goose's
// deserializer requires of its type (TextContent's text, ImageContent's data
// and mimeType), a tool result's output items included.
func checkGooseItems(t *testing.T, items []map[string]json.RawMessage) {
	t.Helper()
	required := map[string][]string{"text": {"text"}, "image": {"data", "mimeType"}, "document": {"data", "mimeType"}}
	for _, it := range items {
		var typ string
		json.Unmarshal(it["type"], &typ)
		for _, f := range required[typ] {
			if _, ok := it[f]; !ok {
				t.Errorf("%s item without %s: %v", typ, f, it)
			}
		}
		if typ != "toolResponse" {
			continue
		}
		var r struct {
			Value struct {
				Content []map[string]json.RawMessage `json:"content"`
			} `json:"value"`
		}
		json.Unmarshal(it["toolResult"], &r)
		checkGooseItems(t, r.Value.Content)
		for _, c := range r.Value.Content {
			if string(c["type"]) == `"text"` && string(c["text"]) == `""` && len(r.Value.Content) > 1 {
				t.Errorf("tool output with an empty text item beside its image: %s", it["toolResult"])
			}
		}
	}
}
