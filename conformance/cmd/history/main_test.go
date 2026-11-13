package main

import (
	"testing"

	"github.com/inference-sh/agentprotocol/conformance/history"
)

func TestAllGreen(t *testing.T) {
	kimi := []history.Section{{Agent: "kimi"}}
	droid := []history.Section{{Agent: "droid"}}
	runs := []history.Run{
		{Source: "n1", Green: true, Sections: kimi},  // surface
		{Source: "n1", Green: false, Sections: kimi}, // probes
		{Source: "n1", Green: true, Sections: droid}, // droid's only job
		{Source: "n2", Green: true, Sections: kimi},
	}
	allGreen(runs)
	for i, want := range []bool{false, false, true, true} {
		if runs[i].Green != want {
			t.Errorf("run %d green = %v, want %v", i, runs[i].Green, want)
		}
	}
}
