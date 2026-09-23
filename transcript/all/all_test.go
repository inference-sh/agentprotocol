package all

import (
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
