package harness

import "testing"

// Every registry entry has a product name and vendor, and no name refers to
// an entry that does not exist — an id that changes must change here too.
func TestProductNames_coverTheRegistryExactly(t *testing.T) {
	for id, h := range All {
		if h.DisplayName == "" || h.Vendor == "" {
			t.Errorf("%s: missing DisplayName/Vendor — add it to productNames", id)
		}
	}
	for id := range productNames {
		if _, ok := All[id]; !ok {
			t.Errorf("productNames has %q but the registry does not", id)
		}
	}
}

func TestDisplay(t *testing.T) {
	if got := Display("claude"); got != "Claude Code" {
		t.Errorf("Display(claude) = %q", got)
	}
	if got := Display("not-a-harness"); got != "not-a-harness" {
		t.Errorf("unknown id should read as itself, got %q", got)
	}
}
