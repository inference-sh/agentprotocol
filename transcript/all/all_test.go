package all

import (
	"context"
	"errors"
	"github.com/inference-sh/agentprotocol/transcript"
	"os"
	"path/filepath"
	"testing"

	"github.com/inference-sh/agentprotocol/harness"
)

// TestRegistryMatchesCodecs is the invariant that keeps the two lists in
// step. Every harness whose registry row names a codec directly is a pure-Go
// codec and must be reachable through Open; every codec in the aggregator
// must belong to a real harness. The database-backed agents (goose, hermes)
// leave the registry field nil and register at runtime from the sqlite
// module, which tests its own registration.
func TestRegistryMatchesCodecs(t *testing.T) {
	for id, h := range harness.All {
		if h.Sessions == nil {
			continue
		}
		st, ok, err := Open(id, t.TempDir())
		if err != nil {
			t.Errorf("%s: open: %v", id, err)
		}
		if !ok || st == nil {
			t.Errorf("%s names a codec in its registry row but Open did not find one", id)
		}
		if _, inTable := Codecs[id]; !inTable {
			t.Errorf("%s has a registry codec but is missing from the aggregator table", id)
		}
	}
	for id := range Codecs {
		if _, ok := harness.All[id]; !ok {
			t.Errorf("aggregator has codec %q with no harness", id)
		}
	}
}

// TestOpenUnknown reports nothing for an agent with no codec, rather than an
// error.
func TestOpenUnknown(t *testing.T) {
	if _, ok, err := Open("no-such-agent", t.TempDir()); err != nil || ok {
		t.Errorf("Open(unknown) = ok %v, err %v", ok, err)
	}
}

// TestList builds one home from several agents' samples and lists it in one
// call: every agent's session appears with its agent, newest first, and an
// agent with no store contributes nothing.
func TestList(t *testing.T) {
	home := t.TempDir()
	samples := map[string]string{
		"claude": "../claude/testdata/home",
		"codex":  "../codex/testdata/home",
		"kimi":   "../kimi/testdata/home",
	}
	for _, src := range samples {
		if err := copyTree(src, home); err != nil {
			t.Fatal(err)
		}
	}
	got, errs := List(t.Context(), home, "")
	for _, e := range errs {
		if e.Agent != "broken-for-test" {
			t.Fatalf("error: %v", e)
		}
	}
	agents := map[string]bool{}
	for i, s := range got {
		agents[s.Agent] = true
		if i > 0 && s.Updated.After(got[i-1].Updated) {
			t.Errorf("not newest first at %d", i)
		}
	}
	for a := range samples {
		if !agents[a] {
			t.Errorf("no %s session listed; got %+v", a, got)
		}
	}
	if len(agents) != len(samples) {
		t.Errorf("listed agents %v, want only %v", agents, samples)
	}
}

// TestListReportsBrokenStore keeps listing when one store cannot be read,
// and reports that store's error.
func TestListReportsBrokenStore(t *testing.T) {
	transcript.Register("broken-for-test", brokenCodec{})
	home := t.TempDir()
	if err := copyTree("../claude/testdata/home", home); err != nil {
		t.Fatal(err)
	}
	got, errs := List(t.Context(), home, "")
	var claude bool
	for _, s := range got {
		if s.Agent == "claude" {
			claude = true
		}
	}
	if !claude {
		t.Error("claude's session missing when another store is broken")
	}
	if len(errs) != 1 || errs[0].Agent != "broken-for-test" || !errors.Is(errs[0].Err, errBroken) {
		t.Errorf("errors = %v, want the broken store's", errs)
	}
}

var errBroken = errors.New("store unreadable")

type brokenCodec struct{}

func (brokenCodec) Open(string) (transcript.Store, error) { return brokenStore{}, nil }

type brokenStore struct{}

func (brokenStore) List(context.Context, string) ([]transcript.Info, error) { return nil, errBroken }
func (brokenStore) Read(context.Context, string) (*transcript.Session, error) {
	return nil, errBroken
}
func (brokenStore) Write(context.Context, *transcript.Session) (string, error) {
	return "", errBroken
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		return os.Chtimes(target, fi.ModTime(), fi.ModTime())
	})
}
