package sqlite

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The opencode sample is one real session extracted from a live database: a
// prompt, an assistant turn with a read tool call, and its answer.
const (
	opencodeCWD = "/home/testuser/test-repo"
	opencodeID  = "ses_f3314d1b3ffegWL15JAVSbsnbM"
)

func TestOpencodeRead(t *testing.T) {
	st, err := Opencode.Open("testdata/opencode")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), opencodeCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != opencodeID {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), opencodeID)
	if err != nil {
		t.Fatal(err)
	}
	if s.CWD != opencodeCWD {
		t.Errorf("cwd = %q", s.CWD)
	}
	var user, call, result bool
	for _, e := range s.Messages() {
		if e.Role == transcript.RoleUser {
			user = true
		}
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolUse {
				call = true
			}
			if b.Kind == transcript.BlockToolResult {
				result = true
			}
		}
	}
	if !user || !call || !result {
		t.Errorf("user %v call %v result %v", user, call, result)
	}
	if countTurns(s) == 0 {
		t.Error("no turn in events")
	}
}

// The kilo sample is one ACP run captured the same way; kilo shares
// opencode's schema.
const (
	kiloCWD = "/home/testuser/test-repo"
	kiloID  = "ses_f33145df9ffeeXTyEn5hwHXfJU"
)

func TestKiloRead(t *testing.T) {
	st, err := Kilo.Open("testdata/kilo")
	if err != nil {
		t.Fatal(err)
	}
	infos, err := st.List(t.Context(), kiloCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != kiloID {
		t.Fatalf("list = %+v", infos)
	}
	s, err := st.Read(t.Context(), kiloID)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages()) == 0 {
		t.Error("no messages")
	}
}

func TestOpencodeRoundTrip(t *testing.T) { roundTrip(t, Opencode, "opencode") }
func TestKiloRoundTrip(t *testing.T)     { roundTrip(t, Kilo, "kilo") }

func TestOpencodeKiloRegistered(t *testing.T) {
	for _, agent := range []string{"opencode", "kilo"} {
		if _, ok := transcript.Registered(agent); !ok {
			t.Errorf("%s not registered", agent)
		}
	}
}

// The compact samples are driven through each agent's HTTP server in the
// harness-test container against the mock model: a prompt, a shell command
// the person ran, two more prompts, /summarize with auto (a compaction that
// keeps the last turn and adds a synthetic "Continue" prompt), one more
// prompt, then a prompt reverted and left reverted. The same run adds a
// child session and an archived one.
type compactSample struct {
	codec transcript.Codec
	home  string
	db    string
	id    string
	// Message ids, in store order.
	first, firstAnswer, shell, shellTool, again, againAnswer, tail, tailAnswer,
	compaction, summary, cont, contAnswer, after, afterAnswer, reverted, revertedAnswer string
	// reminder is the id of the synthetic reminder part Kilo appended to
	// the "Say it again." prompt; opencode adds none.
	reminder string
}

var compactSamples = []compactSample{{
	Opencode, "testdata/opencode-compact", ".local/share/opencode/opencode.db", "ses_f2d372e92ffeqU1t8roMrNJ2ak",
	"msg_0d2c8d1a4001grBPqCUm344iPc", "msg_0d2c8d4a3001clrUNtDn6e6wqU", "msg_0d2c8db7e001WQcQbZmqCKWpOE", "msg_0d2c8db810016SdFJDQhzXkenC",
	"msg_0d2c8dbbf001556xRkkkXO1fQ2", "msg_0d2c8dd35001113d38VvEs2rlH", "msg_0d2c8de7e001KNgPzhBNMEE3bT", "msg_0d2c8dfda001n3D2mdREIkxW7E",
	"msg_0d2c8e0d7001hCJMePDYVUDKdA", "msg_0d2c8e0e0001SnXATwwXe4MTpj", "msg_0d2c8e1830017O2EZlI2NdHPLV", "msg_0d2c8e188001NFSoNAwPszocM6",
	"msg_0d2c8e259001RqtvT4JlPz6QPA", "msg_0d2c8e26e001G07t3k75gYFAz9", "msg_0d2c8e311001lNPJlI31iDkoS3", "msg_0d2c8e31b001FM4qiuBsBO108y",
	"",
}, {
	Kilo, "testdata/kilo-compact", ".local/share/kilo/kilo.db", "ses_f2d390136ffepMvWo21lyXa12D",
	"msg_0d2c702310018pa0zStENCq5yy", "msg_0d2c70788001Hfdv228UDvM7gf", "msg_0d2c70be20019506dKxxWBiNUR", "msg_0d2c70be7001sF4nWRUNwQ9Zz1",
	"msg_0d2c70c3a001Co9gFhikgH1eQT", "msg_0d2c70c70001066SdbQKabMF4G", "msg_0d2c70ecf00186OoRdcZ16GZnP", "msg_0d2c70ef6001u9eqgCORAw0zNG",
	"msg_0d2c71051001B6bFRUOelhfMU2", "msg_0d2c71081001yDzE32OQAAAjvG", "msg_0d2c7123e0010dIL7KiPYP6J15", "msg_0d2c7124a001EKfJgCiyBYvP6R",
	"msg_0d2c71434001ZMFoyRCXqpKjaD", "msg_0d2c7145a001tX1Kjlp85jFx6o", "msg_0d2c71601001n7mtrz9yYPNojt", "msg_0d2c7163f001hL3I3Hgam5Zmm7",
	"prt_0d2c70c6d001LUaoh4dl2AhFlI",
}}

func (c compactSample) name() string { return strings.TrimPrefix(c.home, "testdata/") }

func ids(es []transcript.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// TestOpencodeCompactContext checks the model's context on resume against
// what filterCompacted and toModelMessages make of the same rows: the
// compaction prompt as "What did we do so far?" and its summary, the turn
// the compaction kept, then everything after the summary, with the
// synthetic "Continue" prompt and without the reverted turn.
func TestOpencodeCompactContext(t *testing.T) {
	for _, c := range compactSamples {
		t.Run(c.name(), func(t *testing.T) {
			s, err := mustOpen(t, c.codec, c.home).Read(t.Context(), c.id)
			if err != nil {
				t.Fatal(err)
			}
			ctx := s.Context()
			want := []string{c.compaction, c.summary, c.tail, c.tailAnswer, c.cont, c.contAnswer, c.after, c.afterAnswer}
			if got := ids(ctx); !slices.Equal(got, want) {
				t.Fatalf("context = %v\nwant %v", got, want)
			}
			if ctx[0].Role != transcript.RoleUser || ctx[0].Text() != "What did we do so far?" {
				t.Errorf("compaction prompt reaches the model as %s %q", ctx[0].Role, ctx[0].Text())
			}
			if ctx[1].Text() != "The codename is BLUEBIRD." {
				t.Errorf("summary = %q", ctx[1].Text())
			}
			if !strings.HasPrefix(ctx[4].Text(), "Continue if you have next steps") {
				t.Errorf("continue prompt = %q", ctx[4].Text())
			}
			var carrier *transcript.Entry
			for i, e := range s.Entries {
				if e.Compaction != nil {
					if carrier != nil {
						t.Fatalf("second compaction on %s", e.ID)
					}
					carrier = &s.Entries[i]
				}
			}
			if carrier == nil || carrier.ID != c.summary || carrier.Compaction.Keep != c.tail {
				t.Fatalf("compaction carried by %+v", carrier)
			}
			// The summary moves to another agent; the compaction prompt,
			// which the person never saw as text, does not.
			if got := ids(s.Portable().Entries); !slices.Equal(got[:3], []string{c.summary, c.tail, c.tailAnswer}) {
				t.Errorf("portable starts %v", got[:3])
			}
		})
	}
}

// TestOpencodeCompactLinearize checks what the person sees: the history the
// compaction retired, the summary, and neither the compaction prompt (a
// divider), nor synthetic prompts, nor the reverted turn.
func TestOpencodeCompactLinearize(t *testing.T) {
	for _, c := range compactSamples {
		t.Run(c.name(), func(t *testing.T) {
			s, err := mustOpen(t, c.codec, c.home).Read(t.Context(), c.id)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{c.first, c.firstAnswer, c.shellTool, c.again, c.againAnswer, c.tail, c.tailAnswer, c.summary, c.contAnswer, c.after, c.afterAnswer}
			if got := ids(s.Linearize()); !slices.Equal(got, want) {
				t.Fatalf("linearize = %v\nwant %v", got, want)
			}
			for _, e := range s.Messages() {
				if (e.ID == c.reverted || e.ID == c.revertedAnswer) && e.Audience != transcript.AudienceNone {
					t.Errorf("reverted %s has audience %d", e.ID, e.Audience)
				}
			}
		})
	}
}

// TestKiloSyntheticReminder: Kilo appends a synthetic reminder to a prompt.
// The model is given it and the person is not, so it reads as an entry of
// its own for the model, after the prompt's own text.
func TestKiloSyntheticReminder(t *testing.T) {
	c := compactSamples[1]
	s, err := mustOpen(t, c.codec, c.home).Read(t.Context(), c.id)
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	i := slices.IndexFunc(msgs, func(e transcript.Entry) bool { return e.ID == c.again })
	if i < 0 || i+1 >= len(msgs) {
		t.Fatal("prompt not read")
	}
	if msgs[i].Text() != "Say it again." || msgs[i].Audience != transcript.AudienceAll {
		t.Errorf("prompt = %q audience %d", msgs[i].Text(), msgs[i].Audience)
	}
	r := msgs[i+1]
	if r.ID != c.reminder || r.Audience != transcript.AudienceModel || !strings.Contains(r.Text(), "<system-reminder>") {
		t.Errorf("reminder = %s audience %d %q", r.ID, r.Audience, r.Text())
	}
}

// TestOpencodeListRoots: opencode lists neither a subagent's child session
// nor an archived one, and both samples hold one of each.
func TestOpencodeListRoots(t *testing.T) {
	for _, c := range compactSamples {
		t.Run(c.name(), func(t *testing.T) {
			infos, err := mustOpen(t, c.codec, c.home).List(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 1 || infos[0].ID != c.id {
				t.Errorf("list = %+v", infos)
			}
		})
	}
}

// TestOpencodeAppendUnderRevert appends a turn to a session whose last turn
// is reverted. opencode deletes the reverted messages and clears the revert
// before a prompt; a write that did not would leave the new turn behind the
// revert point, hidden, and deleted with the reverted one on the next
// prompt.
func TestOpencodeAppendUnderRevert(t *testing.T) {
	for _, c := range compactSamples {
		t.Run(c.name(), func(t *testing.T) {
			home := copyHome(t, c.home)
			s, err := mustOpen(t, c.codec, home).Read(t.Context(), c.id)
			if err != nil {
				t.Fatal(err)
			}
			s.Entries = append(s.Entries,
				transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON. Remember it."}}},
				transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
			)
			if _, err := mustOpen(t, c.codec, home).Write(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			rows := dump(t, filepath.Join(home, c.db))
			for _, r := range rows["session"] {
				if strings.Contains(r, "id="+c.id+";") && !strings.Contains(r, "revert=<nil>;") {
					t.Errorf("revert not cleared: %s", r)
				}
			}
			for _, r := range append(rows["message"], rows["part"]...) {
				if strings.Contains(r, c.reverted) || strings.Contains(r, c.revertedAnswer) {
					t.Errorf("reverted row kept: %s", r)
				}
			}
			back, err := mustOpen(t, c.codec, home).Read(t.Context(), c.id)
			if err != nil {
				t.Fatal(err)
			}
			for _, view := range [][]transcript.Entry{back.Linearize(), back.Context()} {
				n := len(view)
				if n < 3 || view[n-2].Text() != "The codename is HERON. Remember it." || view[n-1].Text() != "Noted: HERON." ||
					view[n-3].ID != c.afterAnswer {
					t.Errorf("appended turn does not follow the last kept one: %v", ids(view[max(0, n-3):]))
				}
			}
		})
	}
}

// TestOpencodeAnswersAreNotLinks: an assistant message's parentID names the
// prompt it answers, and a prompt answered in two steps (a tool call, then
// text) has two answers naming it. Reading those as entry links made the
// session a tree whose active branch dropped the first answer.
func TestOpencodeAnswersAreNotLinks(t *testing.T) {
	s, err := mustOpen(t, Opencode, "testdata/opencode").Read(t.Context(), opencodeID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(s.Linearize()), len(s.Messages()); got != want || want != 3 {
		t.Errorf("linearize %d of %d messages", got, want)
	}
}

// ocRow is a message written by hand, with its parts, for the shapes the
// mock model cannot make opencode produce: a failed or aborted answer, a
// tool left running, a part-level revert, ignored and transient text.
type ocRow struct {
	id, data string
	parts    [][2]string // id, data
}

// ocSession adds a session with the given rows to a copy of a compact
// sample, so the rows sit in the agent's real schema.
func ocSession(t *testing.T, c compactSample, sid, revert string, rows []ocRow) string {
	t.Helper()
	home := copyHome(t, c.home)
	db, err := openRW(filepath.Join(home, c.db))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rev any
	if revert != "" {
		rev = revert
	}
	if _, err := db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated, revert)
		SELECT ?, project_id, 'fixture', directory, 'fixture', version, 1, 1, ? FROM session WHERE id = ?`, sid, rev, c.id); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`, r.id, sid, 10+i, 10+i, r.data); err != nil {
			t.Fatal(err)
		}
		for _, p := range r.parts {
			if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`, p[0], r.id, sid, 10+i, 10+i, p[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	return home
}

const (
	ocUser      = `{"role":"user","time":{"created":1},"agent":"build","model":{"providerID":"openai","modelID":"gpt-4o-mini"}}`
	ocAssistant = `{"parentID":"%s","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/w","root":"/w"},"cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"modelID":"gpt-4o-mini","providerID":"openai","time":{"created":2}%s}`
)

func ocText(text string, extra string) string {
	return fmt.Sprintf(`{"type":"text","text":%q%s}`, text, extra)
}

// TestOpencodeModelSkips covers what toModelMessages leaves out or stands
// in for (message-v2.ts): a failed answer is not sent (processor.ts records
// the provider error on the message), one aborted after producing text is;
// a tool call left pending or running is answered as interrupted, and one
// cut by an abort (processor.ts marks it interrupted with its output) sends
// that output; ignored prompt text (acp/content.ts, for text meant only for
// the person) is not sent, and a subtask part is sent as fixed text.
func TestOpencodeModelSkips(t *testing.T) {
	a := func(parent, extra string) string { return fmt.Sprintf(ocAssistant, parent, extra) }
	rows := []ocRow{
		{"msg_1", ocUser, [][2]string{{"prt_1", ocText("first", "")}, {"prt_1b", ocText("for the person only", `,"ignored":true`)}}},
		{"msg_2", a("msg_1", `,"error":{"name":"APIError","data":{"message":"rate limited"}},"finish":"error"`), [][2]string{{"prt_2", `{"type":"step-start"}`}, {"prt_2b", ocText("partial", "")}}},
		{"msg_3", ocUser, [][2]string{{"prt_3", ocText("second", "")}}},
		{"msg_4", a("msg_3", `,"error":{"name":"MessageAbortedError","data":{"message":"aborted"}}`), [][2]string{
			{"prt_4", ocText("before the abort", "")},
			{"prt_4b", `{"type":"tool","tool":"bash","callID":"c1","state":{"status":"running","input":{"command":"sleep 9"},"time":{"start":1}}}`},
			{"prt_4c", `{"type":"tool","tool":"bash","callID":"c2","state":{"status":"error","input":{"command":"yes"},"error":"Tool execution aborted","metadata":{"interrupted":true,"output":"y\ny\n"},"time":{"start":1,"end":2}}}`},
		}},
		{"msg_5", ocUser, [][2]string{{"prt_5", `{"type":"subtask","prompt":"look","description":"look","agent":"explore"}`}}},
	}
	home := ocSession(t, compactSamples[0], "ses_fixture", "", rows)
	s, err := mustOpen(t, Opencode, home).Read(t.Context(), "ses_fixture")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]transcript.Entry{}
	for _, e := range s.Messages() {
		byID[e.ID] = e
	}
	if e := byID["msg_2"]; e.Audience != transcript.AudienceUser {
		t.Errorf("failed answer audience %d", e.Audience)
	}
	if e := byID["prt_1b"]; e.Audience != transcript.AudienceUser || e.Text() != "for the person only" {
		t.Errorf("ignored text = %+v", e)
	}
	if got := ids(s.Context()); !slices.Equal(got, []string{"msg_1", "msg_3", "msg_4", "msg_5"}) {
		t.Errorf("context = %v", got)
	}
	var results []string
	for _, b := range byID["msg_4"].Content {
		if b.Kind == transcript.BlockToolResult {
			results = append(results, fmt.Sprintf("%s:%s:%q", b.ToolID, b.Status, b.Text))
		}
	}
	if want := []string{`c1:error:"[Tool execution was interrupted]"`, `c2:ok:"y\ny\n"`}; !slices.Equal(results, want) {
		t.Errorf("results = %v, want %v", results, want)
	}
	if e := byID["msg_5"]; e.Audience != transcript.AudienceModel || e.Text() != "The following tool was executed by the user" {
		t.Errorf("subtask = %d %q", e.Audience, e.Text())
	}
}

// TestKiloAssistantNotices: Kilo keeps ignored and transient assistant text
// (local UI notices, kilo processor.ts and part-lifecycle.ts) out of future
// prompts; opencode sends both.
func TestKiloAssistantNotices(t *testing.T) {
	rows := []ocRow{
		{"msg_1", ocUser, [][2]string{{"prt_1", ocText("hi", "")}}},
		{"msg_2", fmt.Sprintf(ocAssistant, "msg_1", `,"finish":"stop"`), [][2]string{
			{"prt_2", ocText("hello", "")},
			{"prt_2b", ocText("retrying…", `,"metadata":{"kilocode.lifecycle":"transient"}`)},
			{"prt_2c", ocText("a warning", `,"ignored":true`)},
		}},
	}
	for _, c := range compactSamples {
		t.Run(c.name(), func(t *testing.T) {
			home := ocSession(t, c, "ses_fixture", "", rows)
			s, err := mustOpen(t, c.codec, home).Read(t.Context(), "ses_fixture")
			if err != nil {
				t.Fatal(err)
			}
			var sent []string
			for _, e := range s.Context() {
				sent = append(sent, e.Text())
			}
			want := []string{"hi", "helloretrying…a warning"}
			if c.codec == Kilo {
				want = []string{"hi", "hello"}
			}
			if !slices.Equal(sent, want) {
				t.Errorf("context texts = %q, want %q", sent, want)
			}
			if n := len(s.Linearize()); n != len(s.Messages()) {
				t.Errorf("person sees %d of %d entries", n, len(s.Messages()))
			}
		})
	}
}

// TestOpencodePartRevert: a revert from a part inside an assistant message
// (revert.ts revert, a partID after text or tool parts) hides the whole
// message from the TUI, keeps its earlier parts for the model, and a write
// removes the rest the way cleanup does. Kilo also clears the kept
// message's provider error (kilo revert.ts cleanup).
func TestOpencodePartRevert(t *testing.T) {
	rows := []ocRow{
		{"msg_1", ocUser, [][2]string{{"prt_1", ocText("hi", "")}}},
		{"msg_2", fmt.Sprintf(ocAssistant, "msg_1", `,"error":{"name":"APIError","data":{"message":"x"}}`), [][2]string{
			{"prt_2a", ocText("kept", "")},
			{"prt_2b", ocText("undone", "")},
		}},
		{"msg_3", ocUser, [][2]string{{"prt_3", ocText("later", "")}}},
	}
	for _, c := range compactSamples {
		t.Run(c.name(), func(t *testing.T) {
			home := ocSession(t, c, "ses_fixture", `{"messageID":"msg_2","partID":"prt_2b"}`, rows)
			s, err := mustOpen(t, c.codec, home).Read(t.Context(), "ses_fixture")
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(s.Linearize()); !slices.Equal(got, []string{"msg_1"}) {
				t.Errorf("linearize = %v", got)
			}
			wantCtx := []string{"hi"}
			if c.codec == Kilo {
				wantCtx = []string{"hi", "kept"}
			}
			var sent []string
			for _, e := range s.Context() {
				sent = append(sent, e.Text())
			}
			if !slices.Equal(sent, wantCtx) {
				t.Errorf("context = %q, want %q", sent, wantCtx)
			}
			s.Entries = append(s.Entries, transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "next"}}})
			if _, err := mustOpen(t, c.codec, home).Write(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			back, err := mustOpen(t, c.codec, home).Read(t.Context(), "ses_fixture")
			if err != nil {
				t.Fatal(err)
			}
			var texts []string
			for _, e := range back.Linearize() {
				texts = append(texts, e.Text())
			}
			want := []string{"hi", "kept", "next"}
			if !slices.Equal(texts, want) {
				t.Errorf("after write the person sees %q, want %q", texts, want)
			}
			sent = nil
			for _, e := range back.Context() {
				sent = append(sent, e.Text())
			}
			if c.codec == Opencode {
				want = []string{"hi", "next"} // the error stays, so the answer is not sent
			}
			if !slices.Equal(sent, want) {
				t.Errorf("after write the model gets %q, want %q", sent, want)
			}
		})
	}
}
