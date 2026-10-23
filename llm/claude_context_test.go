package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	claudetx "github.com/inference-sh/agentprotocol/transcript/claude"
)

func TestWorkDirIsTheKeys(t *testing.T) {
	root := t.TempDir()
	a, err := workDir(root, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := workDir(root, "chat-1")
	c, _ := workDir(root, "chat-2")
	if a != b || a == c {
		t.Fatalf("chat-1 got %s and %s, chat-2 %s", a, b, c)
	}
	x, _ := workDir(root, "")
	y, _ := workDir(root, "")
	if x == y {
		t.Fatalf("keyless calls share %s", x)
	}
}

// A process of a conversation that ends removes its own session only: the
// next process of the same conversation shares the directories.
func TestRemoveFilesKeepsTheNextProcesss(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	dir, _ := workDir(root, "chat")
	project := claudetx.Codec.Layout.Dir(home, dir)
	_ = os.MkdirAll(filepath.Join(project, "old"), 0o700)
	for _, f := range []string{"old.jsonl", "new.jsonl"} {
		_ = os.WriteFile(filepath.Join(project, f), []byte("{}\n"), 0o600)
	}
	(&claudeConv{home: home, dir: dir, session: "old"}).removeFiles()
	if _, err := os.Stat(filepath.Join(project, "old.jsonl")); !os.IsNotExist(err) {
		t.Error("the ended process's session is still there")
	}
	if _, err := os.Stat(filepath.Join(project, "new.jsonl")); err != nil {
		t.Errorf("the next process's session is gone: %v", err)
	}
	(&claudeConv{home: home, dir: dir, session: "new"}).removeFiles()
	for _, p := range []string{project, dir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is left behind", p)
		}
	}
}

func TestProbedForDir(t *testing.T) {
	p := probed{dir: "/probe/work", attachments: []json.RawMessage{
		json.RawMessage(`{"type":"environment","snapshot":{"workingDirectory":"/probe/work","isGitRepo":false,"platform":"linux"}}`),
		json.RawMessage(`{"type":"model","identity":{"modelId":"m"},"text":"t"}`),
		json.RawMessage(`{"type":"date","date":"2001-01-01"}`),
	}}
	day := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)
	got := p.forDir("/work/chat", day)
	want := []string{
		`{"snapshot":{"isGitRepo":false,"platform":"linux","workingDirectory":"/work/chat"},"type":"environment"}`,
		`{"identity":{"modelId":"m"},"text":"t","type":"model"}`,
		`{"date":"2026-10-23","type":"date"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d attachments", len(got))
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("%d: got %s, want %s", i, got[i], want[i])
		}
	}
}
