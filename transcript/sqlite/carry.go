package sqlite

import (
	"slices"
	"strings"

	"github.com/inference-sh/agentprotocol/transcript"
)

// summaryText is a compaction's summary as the single text the agents here
// store one as: the text of each summary entry, in order.
func summaryText(summary []transcript.Entry) string {
	var parts []string
	for _, e := range summary {
		if t := e.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// unkept moves each compaction of a portable session to just before the
// first entry it keeps, and leaves it keeping nothing, for an agent whose
// compaction keeps nothing of what came before it. The model is then given
// the summary followed by the kept entries, as it was before the move; the
// person sees the same order, since no agent shows a summary in the place
// of the history it retired. A compaction whose kept entries hold an
// earlier compaction stays where it is and keeps nothing.
func unkept(entries []transcript.Entry) []transcript.Entry {
	out := slices.Clone(entries)
	for i := 0; i < len(out); i++ {
		c := out[i].Compaction
		if c == nil || c.Keep == "" {
			continue
		}
		marker := out[i]
		marker.Compaction = &transcript.Compaction{Summary: c.Summary}
		out[i] = marker
		j := slices.IndexFunc(out[:i], func(e transcript.Entry) bool { return e.ID == c.Keep })
		if j < 0 || slices.ContainsFunc(out[j:i], func(e transcript.Entry) bool { return e.Compaction != nil }) {
			continue
		}
		copy(out[j+1:i+1], out[j:i])
		out[j] = marker
	}
	return out
}
