package qwen

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/transcripttest"
)

// The samples are runs of qwen-code 0.24.4 in the harness-test container.
// The compaction probe left two sessions: sampleID (a prompt with a
// read_file call, three more turns, /compress, then the prompt the probe
// asked after resuming) and compactedID (a prompt, then /compress).
// slashID is an ACP run driven by hand: a prompt, /stats, /about, a prompt.
const (
	sampleCWD   = "/tmp/harness-test-qwen-3398585528/test-repo"
	sampleID    = "2d4caa81-62a5-40d0-bdd6-82814ff62697"
	compactedID = "31e0f725-8d82-41d8-812e-3f0217f759ba"
	slashCWD    = "/tmp/qhome/repo"
	slashID     = "76101e2f-25b5-49b7-a951-d3153e2db574"
)

var sample = transcripttest.Sample{Home: "testdata/home", CWD: sampleCWD, ID: sampleID}

func TestRoundTrip(t *testing.T) {
	s := transcripttest.RoundTrip(t, Codec, sample)
	if s.CWD != sampleCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	var call, result bool
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolUse && b.Name == "read_file" && b.ToolID == "call_mock_1" {
				call = true
			}
			if b.Kind == transcript.BlockToolResult && b.ToolID == "call_mock_1" && b.Text == "test" && e.Role == transcript.RoleTool {
				result = true
			}
		}
	}
	if !call || !result {
		t.Errorf("tool call %v, result %v", call, result)
	}
	if s.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", s.Model)
	}
}

func TestForeign(t *testing.T) { transcripttest.Foreign(t, Codec, "/tmp/some/project") }

func TestAppend(t *testing.T) { transcripttest.Append(t, Codec, sample) }

func TestForeignIDs(t *testing.T) {
	transcripttest.ForeignIDs(t, Codec, "/tmp/some/project", transcript.IsUUID)
}

func TestListsCWD(t *testing.T) { transcripttest.ListsCWD(t, Codec, sample) }

func TestImported(t *testing.T) { transcripttest.Imported(t, Codec, sample) }

// After /compress, Qwen's resume gives the model the compression's
// compressedHistory in place of everything before it, then the rows after
// it. The probe that made the sample resumed after /compress and asked "What
// was my previous question?"; the model was sent 6 messages: the system
// prompt, the startup-context reminder Qwen rebuilds on every start, the 3
// compressed entries, and that prompt. The first two are not in the store,
// so the context read before that prompt holds 3 entries and now holds 5.
func TestCompressContext(t *testing.T) {
	s := read(t, "testdata/home", sampleID)
	want := []string{
		"user: Hello from mock server.\n\nResume the prior task using the summary above. Continue from the last in-flight step; do not acknowledge the summary, do not re-introduce, do not greet the user again.",
		"assistant: Got it. Thanks for the additional context!",
		"user: Recently accessed file (full current content embedded):\n\n## /tmp/harness-test-qwen-3398585528/test-repo/README.md\n\n```\ntest\n```",
		"user: What was my previous question?",
		"assistant: Hello from mock server.",
	}
	if got := lines(s.Context()); !reflect.DeepEqual(got, want) {
		t.Errorf("context:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The person still sees the whole conversation, the command included.
	shown := strings.Join(lines(s.Linearize()), "\n")
	for _, w := range []string{"user: What is the project codename? Reply ONLY the codename.", "user: /compress", "user: What was my previous question?"} {
		if !strings.Contains(shown, w) {
			t.Errorf("linearized conversation lacks %q:\n%s", w, shown)
		}
	}

	c := read(t, "testdata/home", compactedID).Context()
	if len(c) != 3 || c[1].Text() != "Got it. Thanks for the additional context!" {
		t.Errorf("context after a trailing /compress = %q, want the 3 compressed entries", lines(c))
	}
	for _, e := range c {
		if strings.Contains(e.Text(), "/compress") {
			t.Errorf("the /compress echo reached the model: %q", e.Text())
		}
	}
}

// An ACP slash command whose result stays local is recorded as the typed
// command's user row and a slash_command result. Qwen shows both and gives
// the model neither.
func TestSlashCommand(t *testing.T) {
	s := read(t, "testdata/home", slashID)
	want := []string{
		"user: What is the project codename?",
		"assistant: Hello from mock server.",
		"user: Second question?",
		"assistant: Hello from mock server.",
	}
	if got := lines(s.Context()); !reflect.DeepEqual(got, want) {
		t.Errorf("context = %q, want %q", got, want)
	}
	shown := strings.Join(lines(s.Linearize()), "\n")
	if !strings.Contains(shown, "user: /stats") || !strings.Contains(shown, "user: /about") {
		t.Errorf("the commands are not shown:\n%s", shown)
	}
}

// Qwen names a project directory after the cwd with every character other
// than a letter or digit turned into a dash, so a cwd with a dot, an
// underscore or a space lives somewhere other than a dash-for-slash name.
func TestProjectDir(t *testing.T) {
	for cwd, want := range map[string]string{
		"/tmp/harness-test-qwen-3398585528/test-repo": "-tmp-harness-test-qwen-3398585528-test-repo",
		"/home/me/my_app.v2/src dir":                  "-home-me-my-app-v2-src-dir",
		"/w/café/😀":                                   "-w-caf----",
	} {
		if got := ProjectDir(cwd); got != want {
			t.Errorf("ProjectDir(%q) = %q, want %q", cwd, got, want)
		}
	}

	cwd := "/home/me/my_app.v2/src dir"
	home := t.TempDir()
	store, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := store.Write(ctx, &transcript.Session{CWD: cwd, Entries: []transcript.Entry{
		{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".qwen/projects/-home-me-my-app-v2-src-dir/chats", id+".jsonl")); err != nil {
		t.Errorf("session not where Qwen looks: %v", err)
	}
	infos, err := store.List(ctx, cwd)
	if err != nil || len(infos) != 1 || infos[0].ID != id {
		t.Errorf("listing %q = %+v, %v", cwd, infos, err)
	}
}

// Qwen lists a session only when its file name is a UUID-like id, so a
// session from an agent with ids of another shape gets a UUID.
func TestForeignSessionID(t *testing.T) {
	store, err := Codec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Write(context.Background(), &transcript.Session{ID: "ses_3f2a9c", Agent: "opencode", CWD: "/tmp/p", Entries: []transcript.Entry{
		{ID: "m1", Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "hi"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionFile.MatchString(id + ".jsonl") {
		t.Errorf("written as %q, which Qwen does not list", id)
	}
	if s := read(t, store, id); s.ID != id || len(s.Messages()) != 1 {
		t.Errorf("read back %q with %d messages", s.ID, len(s.Messages()))
	}
}

// An artifact or sources record is written with the tail as its parent but
// never moves the tail (appendRecordStrict with updateActiveTail: false in
// chatRecordingService.ts recordSessionArtifactEvent), and Qwen's loader
// never takes it as the leaf. A row appended after one continues the
// conversation.
func TestSideRecordsOffChain(t *testing.T) {
	const last = "7bd5b978-c1a1-49f7-8b43-614a4643b178"
	home := fixture(t, slashCWD, slashID,
		`{"uuid":"aaaaaaaa-0000-4000-8000-000000000001","parentUuid":"`+last+`","sessionId":"`+slashID+`","timestamp":"2026-09-24T09:26:52.000Z","type":"system","provenance":"system","cwd":"/tmp/qhome/repo","version":"0.24.4","subtype":"session_artifact_event","systemPayload":{}}`,
		`{"uuid":"aaaaaaaa-0000-4000-8000-000000000002","parentUuid":"aaaaaaaa-0000-4000-8000-000000000001","sessionId":"`+slashID+`","timestamp":"2026-09-24T09:26:53.000Z","type":"system","provenance":"system","cwd":"/tmp/qhome/repo","version":"0.24.4","subtype":"session_sources_snapshot","systemPayload":{}}`,
	)
	store, err := Codec.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, store, slashID)
	s.Entries = append(s.Entries, transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Third question?"}}})
	if _, err := store.Write(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	back := read(t, store, slashID)
	msgs := back.Messages()
	added := msgs[len(msgs)-1]
	if added.Text() != "Third question?" || added.ParentID != last {
		t.Errorf("appended row %q has parent %q, want %q", added.Text(), added.ParentID, last)
	}
	if got := lines(back.Context()); len(got) != 5 || got[0] != "user: What is the project codename?" {
		t.Errorf("context after the append = %q", got)
	}
}

// A record written twice under one uuid (a retried append; sessionService.ts
// forkSession: "a checkpoint line repeated by a crash-retry") is one record
// to Qwen: the first copy's parent holds, and the copies' parts are joined.
func TestRepeatedRecord(t *testing.T) {
	const assistant = "b582d609-2e92-40e9-8c71-56d126dcb864"
	home := fixture(t, slashCWD, slashID,
		`{"uuid":"`+assistant+`","parentUuid":"7bd5b978-c1a1-49f7-8b43-614a4643b178","sessionId":"`+slashID+`","timestamp":"2026-09-24T09:26:52.000Z","type":"assistant","provenance":"assistant_output","cwd":"/tmp/qhome/repo","version":"0.24.4","message":{"role":"model","parts":[{"text":" And more."}]},"model":"gpt-4o-mini"}`,
	)
	s := read(t, home, slashID)
	want := []string{
		"user: What is the project codename?",
		"assistant: Hello from mock server.",
		"user: /stats",
		"user: /about",
		"user: Second question?",
		"assistant: Hello from mock server. And more.",
	}
	if got := lines(s.Linearize()); !reflect.DeepEqual(got, want) {
		t.Errorf("linearized = %q, want %q", got, want)
	}
}

// A rewind re-roots the tail at the rewound turn's parent and appends a
// rewind record there (chatRecordingService.ts rewindRecording); the turns
// after it are a dead branch. Rewind is TUI-only, so the row is written here.
func TestRewind(t *testing.T) {
	home := fixture(t, slashCWD, slashID,
		`{"uuid":"aaaaaaaa-0000-4000-8000-000000000003","parentUuid":"72dfd7a0-c473-4430-a193-ae323d45add5","sessionId":"`+slashID+`","timestamp":"2026-09-24T09:26:52.000Z","type":"system","provenance":"system","cwd":"/tmp/qhome/repo","version":"0.24.4","subtype":"rewind","systemPayload":{}}`,
	)
	s := read(t, home, slashID)
	want := []string{"user: What is the project codename?", "assistant: Hello from mock server."}
	if got := lines(s.Linearize()); !reflect.DeepEqual(got, want) {
		t.Errorf("linearized = %q, want %q", got, want)
	}
	if got := lines(s.Context()); !reflect.DeepEqual(got, want) {
		t.Errorf("context = %q, want %q", got, want)
	}
}

// fixture copies a sample session into a fresh home, with extra rows
// appended, and returns the home.
func fixture(t *testing.T, cwd, id string, extra ...string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata/home", root, ProjectDir(cwd), "chats", id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dir := filepath.Join(home, root, ProjectDir(cwd), "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := string(src) + strings.Join(extra, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// read loads a session from a home directory or an open store.
func read(t *testing.T, from any, id string) *transcript.Session {
	t.Helper()
	store, ok := from.(transcript.Store)
	if !ok {
		var err error
		if store, err = Codec.Open(from.(string)); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func lines(es []transcript.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = string(e.Role) + ": " + e.Text()
	}
	return out
}
