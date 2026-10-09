package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// The omp samples were captured with omp's temp root at /tmp, which names
// their directories: their cwds are under it.
func TestMain(m *testing.M) {
	os.Setenv("TMPDIR", "/tmp")
	os.Exit(m.Run())
}

// omp names a project's session directory from the temp root first, then
// the home (getDefaultSessionDirName, session-paths.ts, 18.5.0 on). The
// roots here do not exist, so nothing resolves through a symlink.
func TestOMPSessionDir(t *testing.T) {
	t.Setenv("TMPDIR", "/no-such-tmp")
	for _, c := range []struct{ home, cwd, want string }{
		{"/no-such-tmp/h", "/no-such-tmp/h/test-repo", "-tmp-h-test-repo"},
		{"/no-such-home/u", "/no-such-home/u/src/app", "-src-app"},
		{"/no-such-home/u", "/no-such-home/u", "-"},
		{"/no-such-home/u", "/no-such-tmp", "-tmp"},
		{"/no-such-home/u", "/srv/app", "--srv-app--"},
		{"/no-such-home/u", "/no-such-home/user2/app", "--no-such-home-user2-app--"},
	} {
		if got := ompSessionDir(c.home, c.cwd); got != c.want {
			t.Errorf("ompSessionDir(%q, %q) = %q, want %q", c.home, c.cwd, got, c.want)
		}
	}
}

// ompHome copies the omp sample into a fresh home, in the directory omp
// 18.4 named it by: its home was /tmp/harness-test-omp-511720626, and 18.4
// named a cwd from the home before the temp root.
func ompHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	src := filepath.Join("testdata", "home", ".omp", "agent", "sessions", "-tmp-harness-test-omp-511720626-test-repo")
	dst := filepath.Join(h, ".omp", "agent", "sessions", "-test-repo")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	names, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(src, n.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, n.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func sessionFiles(t *testing.T, home string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(home, ".omp", "agent", "sessions", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A session read from the store and written back goes over its own file.
// omp 18.5 names the sample's directory from the temp root, and it loads
// the file it wrote, not a second one with the same id where the new rule
// would put it.
func TestOMPAppendKeepsFile(t *testing.T) {
	h := ompHome(t)
	before := sessionFiles(t, h)
	st, err := OMP.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := st.Read(ctx, sampleID)
	if err != nil {
		t.Fatal(err)
	}
	s.Entries = append(s.Entries, transcript.Entry{Role: transcript.RoleUser,
		Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}})
	id, err := st.Write(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	after := sessionFiles(t, h)
	if id != sampleID || strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("wrote %s: files %q, were %q", id, after, before)
	}
	back, err := st.Read(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Linearize(); got[len(got)-1].Text() != "The codename is HERON." {
		t.Errorf("branch = %q", summary(got))
	}
}

// rows decodes a written session's file, one map per row.
func rows(t *testing.T, home, id string) []map[string]any {
	t.Helper()
	for _, f := range sessionFiles(t, home) {
		if !strings.HasSuffix(f, "_"+id+".jsonl") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var r map[string]any
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	t.Fatalf("no file for %s", id)
	return nil
}

// A session omp gets afresh names its model in a model_change at the root
// of its branch: from 18.6 omp's ACP session/load refuses a session whose
// model it cannot restore.
func TestOMPFreshSessionNamesModel(t *testing.T) {
	turn := func() []transcript.Entry {
		return []transcript.Entry{
			{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON."}}},
			{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
		}
	}
	for _, c := range []struct {
		name, agent, model string
		withStore          bool
		want               string
	}{
		{"own model", "omp", "anthropic/claude-sonnet-4-5", true, "anthropic/claude-sonnet-4-5"},
		{"omp session without one takes the store's", "omp", "", true, "openai/gpt-4o-mini"},
		{"another agent's takes the store's", "claude", "claude-sonnet-4-5", true, "openai/gpt-4o-mini"},
		{"another agent's, empty store", "claude", "anthropic/claude-sonnet-4-5", false, "anthropic/claude-sonnet-4-5"},
		{"nothing to name", "claude", "", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := t.TempDir()
			if c.withStore {
				h = ompHome(t)
			}
			st, err := OMP.Open(h)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			id, err := st.Write(ctx, &transcript.Session{Agent: c.agent, CWD: "/srv/p", Model: c.model, Entries: turn()})
			if err != nil {
				t.Fatal(err)
			}
			rs := rows(t, h, id)
			if c.want == "" {
				for _, r := range rs {
					if r["type"] == "model_change" {
						t.Fatalf("model_change written: %v", r)
					}
				}
				return
			}
			mc := rs[1]
			if mc["type"] != "model_change" || mc["model"] != c.want || mc["parentId"] != nil {
				t.Fatalf("second row = %v", mc)
			}
			if rs[2]["parentId"] != mc["id"] {
				t.Errorf("first message's parent = %v, want the model_change %v", rs[2]["parentId"], mc["id"])
			}
			back, err := st.Read(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if back.Model != c.want {
				t.Errorf("read back model = %q", back.Model)
			}
			equal(t, "context", summary(back.Context()), []string{"user: The codename is HERON.", "assistant: Noted: HERON."})
		})
	}
}
