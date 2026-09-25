package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The sample is one ACP run of Oh My Pi in the harness-test container: a
// prompt and an answer. pi's own headless run wrote no session file, so its
// rows are covered through the fork, which shares them.
const (
	sampleCWD = "/tmp/harness-test-omp-511720626/test-repo"
	sampleID  = "01a0cadc-b646-7592-bbc4-c8c8d999cbed"
)

func TestRoundTripOMP(t *testing.T) {
	s := transcripttest.RoundTrip(t, OMP, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	msgs := s.Messages()
	if len(msgs) != 2 || msgs[0].Role != transcript.RoleUser || msgs[1].Role != transcript.RoleAssistant {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestForeign(t *testing.T) {
	transcripttest.Foreign(t, Codec, "/tmp/some/project")
	transcripttest.Foreign(t, OMP, "/tmp/some/project")
}

func TestDashWrappedCwd(t *testing.T) {
	if got := transcript.DashWrappedCwd.Name("/home/ok/inference/go/cli"); got != "--home-ok-inference-go-cli--" {
		t.Errorf("got %q", got)
	}
}

func TestAppend(t *testing.T) {
	transcripttest.Append(t, OMP, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

// The pi sample is one headless run of pi in the harness-test container with
// persistence on (harness-test's transcript probe drops --no-session): a
// system prompt whose content is a plain string, a prompt and an answer. It
// is the session the codec once failed on, and pi and omp diverge here.
const (
	piCWD = "/tmp/harness-test-pi-1772181944"
	piID  = "01a0d020-9480-751b-9f6b-19e2bdede191"
)

func TestRoundTripPi(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: piCWD, ID: piID})
	msgs := s.Messages()
	if len(msgs) != 3 || msgs[0].Role != transcript.RoleSystem || msgs[1].Role != transcript.RoleUser || msgs[2].Role != transcript.RoleAssistant {
		t.Fatalf("messages = %+v", msgs)
	}
	if s.Model != "openai/gpt-4o-mini" {
		t.Errorf("model = %q", s.Model)
	}
	// pi keeps its system prompt as named sections under an empty content
	// and sends them joined; the TUI never shows it.
	sys := msgs[0]
	if sys.Audience != transcript.AudienceModel || !strings.HasPrefix(sys.Text(), "You are an expert coding assistant operating inside pi") {
		t.Errorf("system = audience %d, text %.80q", sys.Audience, sys.Text())
	}
}

func TestAppendPi(t *testing.T) {
	transcripttest.Append(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: piCWD, ID: piID})
}

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", validID)
	transcripttest.ForeignIDs(t, OMP, "/tmp/some/project", validID)
}

func TestListsCWD(t *testing.T) {
	transcripttest.ListsCWD(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: piCWD, ID: piID})
	transcripttest.ListsCWD(t, OMP, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
}

func TestImported(t *testing.T) {
	transcripttest.Imported(t, OMP, transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID})
	transcripttest.Imported(t, Codec, transcripttest.Sample{Home: "testdata/home", CWD: piCWD, ID: piID})
	transcripttest.Imported(t, Codec, piCompacted)
	transcripttest.Imported(t, OMP, ompCompacted)
}

// piCompacted is pi driven over RPC in the harness-test container
// (pi --mode rpc against the mock server, keepRecentTokens 1): a prompt, a
// "bash ls", a "bash" with excludeFromContext (pi's !! prefix), two more
// prompts, a manual compact, and a prompt after it.
var piCompacted = transcripttest.Sample{Home: "testdata/home", CWD: "/tmp/harness-test-pi-rpc/proj", ID: "01a0d2bd-7043-757c-80b2-3f24970d3f7c"}

// ompCompacted is Oh My Pi driven the same way (omp --mode rpc): a prompt,
// "bash ls", a prompt with an @notes.md mention, one more prompt, a manual
// compact, and a prompt after it.
var ompCompacted = transcripttest.Sample{Home: "testdata/home", CWD: "/tmp/harness-test-omp-rpc/proj", ID: "01a0d2bd-a3ca-724e-ae2b-eb71607c7bec"}

func TestRoundTripCompacted(t *testing.T) {
	transcripttest.RoundTrip(t, Codec, piCompacted)
	transcripttest.RoundTrip(t, OMP, ompCompacted)
	transcripttest.Append(t, Codec, piCompacted)
	transcripttest.Append(t, OMP, ompCompacted)
}

func read(t *testing.T, codec transcript.Codec, sample transcripttest.Sample) *transcript.Session {
	t.Helper()
	store, err := codec.Open(sample.Home)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Read(context.Background(), sample.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return s
}

// summary lines an entry list up as "role: text" with the text cut short,
// so a test can compare what the model or the person gets at a glance.
func summary(es []transcript.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		text := e.Text()
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolResult {
				text += b.Text
			}
		}
		if len(text) > 40 {
			text = text[:40]
		}
		out[i] = string(e.Role) + ": " + text
	}
	return out
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s =\n  %q\nwant\n  %q", what, got, want)
	}
}

func TestPiCompactedContext(t *testing.T) {
	s := read(t, Codec, piCompacted)
	if s.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", s.Model)
	}
	// On resume pi gives the model the compaction's own system message, the
	// summary, the kept tail from firstKeptEntryId, and what came after
	// (buildSessionContext).
	equal(t, "context", summary(s.Context()), []string{
		"system: You are an expert coding assistant opera",
		"user: The conversation history before this poi",
		"assistant: Hello from mock server.",
		"user: After compaction.",
		"assistant: Hello from mock server.",
	})
	// A bash run shows as the command and its output; the !! run is shown
	// and kept from the model.
	equal(t, "linearize", summary(s.Linearize()), []string{
		"user: What is the project codename? Reply ONLY",
		"assistant: Hello from mock server.",
		"user: Ran `ls`\n```\nnotes.md\n\n```",
		"user: Ran `echo private`\n```\nprivate\n\n```",
		"user: Second question.",
		"assistant: Hello from mock server.",
		"user: Third question.",
		"assistant: Hello from mock server.",
		"user: Hello from mock server.\n\n---\n\n**Turn Con",
		"user: After compaction.",
		"assistant: Hello from mock server.",
	})
	// Moved to another agent, the session is the summary and what the model
	// kept, without pi's own system prompt or its wrapping of the summary.
	equal(t, "portable", summary(s.Portable().Lower(transcript.Capabilities{}).Entries), []string{
		"user: Hello from mock server.\n\n---\n\n**Turn Con",
		"assistant: Hello from mock server.",
		"user: After compaction.",
		"assistant: Hello from mock server.",
	})
	for _, e := range s.Messages() {
		if strings.HasPrefix(e.Text(), "Ran `echo private`") && e.Audience != transcript.AudienceUser {
			t.Errorf("excluded bash run audience = %d", e.Audience)
		}
	}
}

func TestOMPCompactedContext(t *testing.T) {
	s := read(t, OMP, ompCompacted)
	if s.Model != "mock/gpt-4o-mini" {
		t.Errorf("model = %q", s.Model)
	}
	equal(t, "context", summary(s.Context()), []string{
		"user: Prior model work/tool state available.\nM",
		"assistant: Hello from mock server.",
		"user: After compaction.",
		"assistant: Hello from mock server.",
	})
	// An @file mention is the person's: omp sends it as a developer
	// message holding the file, attributed to the user.
	var mention *transcript.Entry
	for _, e := range s.Messages() {
		if strings.HasPrefix(e.Text(), `<file path="notes.md">`) {
			mention = &e
		}
	}
	if mention == nil || mention.Role != transcript.RoleUser || mention.Audience != transcript.AudienceAll {
		t.Errorf("file mention = %+v", mention)
	}
}

// home writes one session file into a fresh home and returns the home.
func home(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	h := t.TempDir()
	p := filepath.Join(h, dir, "2026-01-01T00-00-00-000Z_s.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

const piDir = ".pi/agent/sessions/--p--"

// Branch summaries (SessionManager.branchWithSummary, session-manager.ts:1604)
// and extension messages (appendCustomMessageEntry, :1342) come from the
// TUI's /tree navigation and from extensions, which no automated run
// drives, so these rows are written by hand in their shape.
func TestBranchSummaryAndCustomMessage(t *testing.T) {
	// Version 2, so pi's load renames the hookMessage role to custom
	// (migrateV2ToV3, session-manager.ts:316).
	h := home(t, piDir,
		`{"type":"session","version":2,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"a1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"first try","timestamp":1}}`,
		`{"type":"branch_summary","id":"b1","parentId":null,"timestamp":"2026-01-01T00:00:02.000Z","fromId":"a1","summary":"tried X"}`,
		`{"type":"custom_message","id":"c1","parentId":"b1","timestamp":"2026-01-01T00:00:03.000Z","customType":"note","content":"hidden context","display":false}`,
		`{"type":"custom_message","id":"c2","parentId":"c1","timestamp":"2026-01-01T00:00:04.000Z","customType":"note","content":[{"type":"text","text":"shown note"}],"display":true}`,
		`{"type":"message","id":"a2","parentId":"c2","timestamp":"2026-01-01T00:00:05.000Z","message":{"role":"hookMessage","customType":"old","content":"legacy hook","display":true,"timestamp":5}}`,
	)
	s := read(t, Codec, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{
		"user: The following is a summary of a branch t",
		"user: hidden context",
		"user: shown note",
		"user: legacy hook",
	})
	equal(t, "linearize", summary(s.Linearize()), []string{
		"user: The following is a summary of a branch t",
		"user: shown note",
		"user: legacy hook",
	})
}

// omp files extension messages as developer messages, not user ones
// (convertMessageToLlm in packages/agent/src/compaction/messages.ts), and a
// directly invoked /skill: prompt as the user's.
func TestOMPCustomMessage(t *testing.T) {
	h := home(t, ".omp/agent/sessions/-p",
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"custom_message","id":"c1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","customType":"note","content":"injected","display":false}`,
		`{"type":"custom_message","id":"c2","parentId":"c1","timestamp":"2026-01-01T00:00:02.000Z","customType":"skill-prompt","content":"run the skill","display":true,"attribution":"user"}`,
		`{"type":"message","id":"d1","parentId":"c2","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"developer","content":[{"type":"text","text":"todo reminder"}],"attribution":"agent","timestamp":3}}`,
		`{"type":"message","id":"p1","parentId":"d1","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"pythonExecution","code":"1+1","output":"2","exitCode":0,"cancelled":false,"truncated":false,"timestamp":4}}`,
	)
	s := read(t, OMP, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{
		"system: injected",
		"user: run the skill",
		"system: todo reminder",
		"user: Ran Python:\n```python\n1+1\n```\nOutput:\n``",
	})
	equal(t, "linearize", summary(s.Linearize()), []string{
		"user: run the skill",
		"user: Ran Python:\n```python\n1+1\n```\nOutput:\n``",
	})
}

// /clear in the omp TUI appends a reset_boundary (appendResetBoundary,
// session-manager.ts:2940); no automated mode reaches it. The model starts
// over after it, and a later compaction wins over an earlier boundary.
func TestOMPResetBoundary(t *testing.T) {
	rows := []string{
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"before","timestamp":1}}`,
		`{"type":"reset_boundary","id":"r1","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z"}`,
		`{"type":"message","id":"u2","parentId":"r1","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":"after","timestamp":3}}`,
	}
	s := read(t, OMP, transcripttest.Sample{Home: home(t, ".omp/agent/sessions/-p", rows...), ID: "s"})
	equal(t, "context", summary(s.Context()), []string{"user: after"})
	equal(t, "linearize", summary(s.Linearize()), []string{"user: before", "user: after"})

	rows = append(rows,
		`{"type":"compaction","id":"k1","parentId":"u2","timestamp":"2026-01-01T00:00:04.000Z","summary":"S","firstKeptEntryId":"u1","tokensBefore":1}`)
	s = read(t, OMP, transcripttest.Sample{Home: home(t, ".omp/agent/sessions/-p", rows...), ID: "s"})
	equal(t, "context after compaction", summary(s.Context()), []string{
		"user: Prior model work/tool state available.\nM",
		"user: before",
		"user: after",
	})
}

// pi applies only the newest compaction on the path: an older one inside
// the kept range contributes nothing, and the kept range may reach back
// past it (buildSessionProjection, session-manager.ts:544). Compaction rows
// written by hand so the kept range can straddle an older one.
func TestPiLatestCompactionOnly(t *testing.T) {
	h := home(t, piDir,
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"one","timestamp":1}}`,
		`{"type":"message","id":"u2","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"user","content":"two","timestamp":2}}`,
		`{"type":"compaction","id":"k1","parentId":"u2","timestamp":"2026-01-01T00:00:03.000Z","summary":"old","firstKeptEntryId":"u2","tokensBefore":1}`,
		`{"type":"message","id":"u3","parentId":"k1","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"user","content":"three","timestamp":4}}`,
		`{"type":"compaction","id":"k2","parentId":"u3","timestamp":"2026-01-01T00:00:05.000Z","summary":"new","firstKeptEntryId":"u1","tokensBefore":1}`,
	)
	s := read(t, Codec, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{
		"user: The conversation history before this poi",
		"user: one",
		"user: two",
		"user: three",
	})
	if got := s.Context()[0].Text(); !strings.Contains(got, "<summary>\nnew\n</summary>") {
		t.Errorf("summary = %q", got)
	}
}

// Context edits come from extensions (SessionManager.appendContextEdit,
// session-manager.ts:1387), so the rows are written by hand. The newest
// edit of an entry on the context path wins; null leaves the entry out of
// the model's context and the TUI still shows it.
func TestPiContextEdit(t *testing.T) {
	h := home(t, piDir,
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"secret","timestamp":1}}`,
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"long answer"}],"provider":"p","model":"m","timestamp":2}}`,
		`{"type":"message","id":"u2","parentId":"a1","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":"next","timestamp":3}}`,
		`{"type":"context_edit","id":"e1","parentId":"u2","timestamp":"2026-01-01T00:00:04.000Z","targetId":"u1","replacement":{"content":"first edit"}}`,
		`{"type":"context_edit","id":"e2","parentId":"e1","timestamp":"2026-01-01T00:00:05.000Z","targetId":"u1","replacement":{"content":[{"type":"text","text":"redacted"}]}}`,
		`{"type":"context_edit","id":"e3","parentId":"e2","timestamp":"2026-01-01T00:00:06.000Z","targetId":"a1","replacement":null}`,
	)
	s := read(t, Codec, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{"user: redacted", "user: next"})
	equal(t, "linearize", summary(s.Linearize()), []string{"user: redacted", "assistant: long answer", "user: next"})
	if s.Model != "m" {
		t.Errorf("model = %q", s.Model)
	}
}

// Version 1 sessions, written by pi before entries had ids, are relinked in
// file order when pi loads them (migrateV1ToV2, session-manager.ts:287),
// and a compaction names its kept entry by file index. No current pi writes
// them, so the rows are by hand.
func TestPiV1(t *testing.T) {
	h := home(t, piDir,
		`{"type":"session","id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"one","timestamp":1}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"provider":"p","model":"m1","timestamp":2}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":"two","timestamp":3}}`,
		`{"type":"compaction","timestamp":"2026-01-01T00:00:04.000Z","summary":"S","firstKeptEntryIndex":3,"tokensBefore":1}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:05.000Z","message":{"role":"user","content":"three","timestamp":5}}`,
	)
	s := read(t, Codec, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "linearize", summary(s.Linearize()), []string{"user: one", "assistant: ok", "user: two", "user: S", "user: three"})
	equal(t, "context", summary(s.Context()), []string{"user: The conversation history before this poi", "user: two", "user: three"})
	if s.Model != "m1" {
		t.Errorf("model = %q", s.Model)
	}
}

// A foreign session's system entry has no pi row, so the entry after it is
// linked past it and the chain from the leaf reaches the first row, whose
// parentId is null as pi writes it.
func TestForeignSystemChain(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	store, err := Codec.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	text := func(r transcript.Role, s string) transcript.Entry {
		return transcript.Entry{Role: r, Content: []transcript.Block{{Kind: transcript.BlockText, Text: s}}}
	}
	id, err := store.Write(ctx, &transcript.Session{Agent: "elsewhere", CWD: "/p", Entries: []transcript.Entry{
		text(transcript.RoleUser, "hi"),
		text(transcript.RoleSystem, "reminder"),
		text(transcript.RoleAssistant, "hello"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := store.Read(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "branch", summary(back.Linearize()), []string{"user: hi", "assistant: hello"})
	if b := back.Branch(); len(b) != len(back.Entries)-1 {
		t.Errorf("branch covers %d of %d rows after the header", len(b), len(back.Entries)-1)
	}
	if !strings.Contains(string(back.Entries[1].Raw), `"parentId":null`) {
		t.Errorf("first row = %s", back.Entries[1].Raw)
	}
	// pi fails the next request on an assistant message without usage.
	for _, e := range back.Entries {
		if e.Role == transcript.RoleAssistant {
			raw := string(e.Raw)
			if !strings.Contains(raw, `"usage":{"input":0,`) || !strings.Contains(raw, `"totalTokens":0`) || !strings.Contains(raw, `"stopReason":"stop"`) {
				t.Errorf("assistant row = %s", raw)
			}
		}
	}
}

const ompDirTest = ".omp/agent/sessions/-p"

// omp's context builder drops what a resumed request must not replay
// (buildSessionContext, session-context.ts:383-726): a retried turn
// (retryRecovery, turn-recovery.ts:783) and an empty error turn, tool calls
// with no result on the path, and an aborted or failed turn with its tool
// results unless an interrupted-thinking note follows it
// (agent-session.ts:3033). Its TUI still shows them. Provider failures and
// aborts do not happen against the mock server, so the rows are by hand.
func TestOMPResumeFilters(t *testing.T) {
	h := home(t, ompDirTest,
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"q1","timestamp":1}}`,
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"toolCall","id":"c1","name":"read","arguments":{}},{"type":"toolCall","id":"c2","name":"read","arguments":{}}],"stopReason":"toolUse","provider":"p","model":"m","timestamp":2}}`,
		`{"type":"message","id":"t1","parentId":"a1","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"read","content":[{"type":"text","text":"r1"}],"isError":false,"timestamp":3}}`,
		`{"type":"message","id":"u2","parentId":"t1","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"user","content":"q2","timestamp":4}}`,
		`{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-01-01T00:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"partial"},{"type":"toolCall","id":"c3","name":"read","arguments":{}}],"stopReason":"error","provider":"p","model":"m","timestamp":5}}`,
		`{"type":"message","id":"t3","parentId":"a2","timestamp":"2026-01-01T00:00:06.000Z","message":{"role":"toolResult","toolCallId":"c3","toolName":"read","content":[{"type":"text","text":"r3"}],"isError":true,"timestamp":6}}`,
		`{"type":"message","id":"u3","parentId":"t3","timestamp":"2026-01-01T00:00:07.000Z","message":{"role":"user","content":"q3","timestamp":7}}`,
		`{"type":"message","id":"a3","parentId":"u3","timestamp":"2026-01-01T00:00:08.000Z","message":{"role":"assistant","content":[{"type":"text","text":"interrupted"}],"stopReason":"aborted","provider":"p","model":"m","timestamp":8}}`,
		`{"type":"message","id":"n3","parentId":"a3","timestamp":"2026-01-01T00:00:09.000Z","message":{"role":"custom","customType":"interrupted-thinking","content":"note","display":false,"timestamp":9}}`,
		`{"type":"message","id":"a4","parentId":"n3","timestamp":"2026-01-01T00:00:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"  "}],"stopReason":"error","provider":"p","model":"m","timestamp":10}}`,
		`{"type":"message","id":"a5","parentId":"a4","timestamp":"2026-01-01T00:00:11.000Z","message":{"role":"assistant","content":[{"type":"text","text":"retried"}],"stopReason":"stop","retryRecovery":{"kind":"x"},"provider":"p","model":"m","timestamp":11}}`,
		`{"type":"message","id":"u4","parentId":"a5","timestamp":"2026-01-01T00:00:12.000Z","message":{"role":"user","content":"q4","timestamp":12}}`,
		`{"type":"message","id":"a6","parentId":"u4","timestamp":"2026-01-01T00:00:13.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c9","name":"read","arguments":{}}],"stopReason":"toolUse","provider":"p","model":"m","timestamp":13}}`,
	)
	s := read(t, OMP, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{
		"user: q1", "assistant: calling", "tool: r1", "user: q2", "user: q3",
		"assistant: interrupted", "system: note", "user: q4",
	})
	equal(t, "linearize", summary(s.Linearize()), []string{
		"user: q1", "assistant: calling", "tool: r1", "user: q2", "assistant: partial", "tool: r3", "user: q3",
		"assistant: interrupted", "assistant:   ", "assistant: retried", "user: q4", "assistant: ",
	})
	for _, e := range s.Context() {
		if e.ID == "a1" && len(e.Content) != 2 {
			t.Errorf("a1 keeps %d blocks, want text and the answered call", len(e.Content))
		}
	}
}

// pi-ai leaves an aborted or failed assistant turn out of every request
// (transformMessages, packages/ai/src/api/transform-messages.ts:201) and
// keeps its tool results. Rows by hand, as for omp.
func TestPiAbortedTurn(t *testing.T) {
	h := home(t, piDir,
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"q1","timestamp":1}}`,
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"read","arguments":{}}],"stopReason":"aborted","provider":"p","model":"m","timestamp":2}}`,
		`{"type":"message","id":"t1","parentId":"a1","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"read","content":[{"type":"text","text":"r1"}],"isError":true,"timestamp":3}}`,
		`{"type":"message","id":"u2","parentId":"t1","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"user","content":"q2","timestamp":4}}`,
	)
	s := read(t, Codec, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{"user: q1", "tool: r1", "user: q2"})
	equal(t, "linearize", summary(s.Linearize()), []string{"user: q1", "assistant: ", "tool: r1", "user: q2"})
}

// omp's context notes tool (tools/context-notes.ts:138) keeps a notebook in
// custom rows, and the newest revision after the last /clear goes to the
// model ahead of everything else (session-context.ts:617). The tool is
// opt-in and experimental, so the rows are by hand.
func TestOMPContextNotes(t *testing.T) {
	rows := []string{
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"q1","timestamp":1}}`,
		`{"type":"custom","id":"k0","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z","customType":"experimental_context_notes","data":{"version":1,"text":"old"}}`,
		`{"type":"custom","id":"k1","parentId":"k0","timestamp":"2026-01-01T00:00:03.000Z","customType":"experimental_context_notes","data":{"version":1,"text":"NOTES"}}`,
		`{"type":"message","id":"a1","parentId":"k1","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop","provider":"p","model":"m","timestamp":4}}`,
	}
	s := read(t, OMP, transcripttest.Sample{Home: home(t, ompDirTest, rows...), ID: "s"})
	ctx := s.Context()
	equal(t, "context", summary(ctx), []string{"system: <experimental-context-notes>\nThis opt-in", "user: q1", "assistant: ok"})
	if len(ctx) > 0 && !strings.HasSuffix(ctx[0].Text(), "Latest notebook revision:\nNOTES\n</experimental-context-notes>") {
		t.Errorf("notes = %q", ctx[0].Text())
	}
	equal(t, "linearize", summary(s.Linearize()), []string{"user: q1", "assistant: ok"})

	rows = append(rows,
		`{"type":"compaction","id":"c1","parentId":"a1","timestamp":"2026-01-01T00:00:05.000Z","summary":"S","firstKeptEntryId":"a1","tokensBefore":1}`,
		`{"type":"message","id":"u2","parentId":"c1","timestamp":"2026-01-01T00:00:06.000Z","message":{"role":"user","content":"q2","timestamp":6}}`)
	s = read(t, OMP, transcripttest.Sample{Home: home(t, ompDirTest, rows...), ID: "s"})
	equal(t, "context after compaction", summary(s.Context()), []string{
		"system: <experimental-context-notes>\nThis opt-in", "user: Prior model work/tool state available.\nM", "assistant: ok", "user: q2",
	})

	rows = append(rows,
		`{"type":"custom","id":"k2","parentId":"u2","timestamp":"2026-01-01T00:00:07.000Z","customType":"experimental_context_notes","data":{"version":1,"text":"NEWER"}}`)
	s = read(t, OMP, transcripttest.Sample{Home: home(t, ompDirTest, rows...), ID: "s"})
	ctx = s.Context()
	equal(t, "context with notes after compaction", summary(ctx), []string{
		"system: <experimental-context-notes>\nThis opt-in", "user: Prior model work/tool state available.\nM", "assistant: ok", "user: q2",
	})
	if len(ctx) > 0 && !strings.Contains(ctx[0].Text(), "NEWER") {
		t.Errorf("notes = %q", ctx[0].Text())
	}
}

// A context-rollover compaction (session-maintenance.ts:1588) summarizes
// nothing of the turn prefix it drops, so omp restores the newest user
// request before its kept range right after the summary
// (session-context.ts:531). Rows by hand: rollover is experimental.
func TestOMPRollover(t *testing.T) {
	h := home(t, ompDirTest,
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"first","timestamp":1}}`,
		`{"type":"message","id":"u2","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"user","content":"latest request","timestamp":2}}`,
		`{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"working"}],"stopReason":"stop","provider":"p","model":"m","timestamp":3}}`,
		`{"type":"compaction","id":"c1","parentId":"a2","timestamp":"2026-01-01T00:00:04.000Z","summary":"S","firstKeptEntryId":"a2","tokensBefore":1,"details":{"kind":"experimental-context-rollover","version":1}}`,
	)
	s := read(t, OMP, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{
		"user: Prior model work/tool state available.\nM", "user: latest request", "assistant: working",
	})
}

// An OpenAI remote compaction (packages/agent/src/compaction/openai.ts:912)
// keeps the /responses/compact output: plain Responses message items and one
// encrypted compaction item. omp replays them in place of the summary on the
// same provider; the messages are read here and the encrypted item stands
// as the summary. The remote endpoint is not mocked, so rows by hand.
func TestOMPRemoteCompaction(t *testing.T) {
	h := home(t, ompDirTest,
		`{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/p"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"q1","timestamp":1}}`,
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"a1"}],"stopReason":"stop","provider":"openai","model":"m","timestamp":2}}`,
		`{"type":"message","id":"u2","parentId":"a1","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":"q2","timestamp":3}}`,
		`{"type":"compaction","id":"c1","parentId":"u2","timestamp":"2026-01-01T00:00:04.000Z","summary":"S","firstKeptEntryId":"","providerReplayThroughEntryId":"a1","tokensBefore":1,"method":"remote","preserveData":{"openaiRemoteCompaction":{"provider":"openai","replacementHistory":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Preserved user"}]},{"type":"reasoning","encrypted_content":"r"},{"type":"compaction","encrypted_content":"enc"}],"compactionItem":{"type":"compaction","encrypted_content":"enc"}}}}`,
	)
	s := read(t, OMP, transcripttest.Sample{Home: h, ID: "s"})
	equal(t, "context", summary(s.Context()), []string{
		"user: Preserved user", "user: Prior model work/tool state available.\nM", "user: q2",
	})
}

// An entry holding several results would lose all but one in pi's
// one-result messages, so the writer refuses it instead of dropping any.
func TestToolEntryWithSeveralResults(t *testing.T) {
	st, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Write(t.Context(), &transcript.Session{Agent: "pi", CWD: "/tmp/p", Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "q"}}},
		{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockToolUse, ToolID: "a", Name: "read"}, {Kind: transcript.BlockToolUse, ToolID: "b", Name: "read"}}},
		{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: "a", Text: "ra"}, {Kind: transcript.BlockToolResult, ToolID: "b", Text: "rb"}}},
	}})
	if err == nil {
		t.Error("a tool entry with two results was written")
	}
}
