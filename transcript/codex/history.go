package codex

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// compacted is a compacted row's payload (history/src/rollout_payload.rs,
// CompactedItemWire). Message is the summary text. ReplacementHistory, when
// present, is the whole history the model is given from here on; rollouts
// from before it carry only the message.
type compacted struct {
	Message            string            `json:"message"`
	ReplacementHistory []json.RawMessage `json:"replacement_history"`
}

// rolledBack is the payload of a thread_rolled_back event.
type rolledBack struct {
	Type     string `json:"type"`
	NumTurns int    `json:"num_turns"`
}

// compactUserMessageMaxTokens bounds the user messages a compaction without
// a replacement history keeps (core/src/compact.rs,
// COMPACT_USER_MESSAGE_MAX_TOKENS), counted at four bytes a token.
const compactUserMessageMaxTokens = 20_000

// slot is one item of the history the model is given, as replay builds it.
type slot struct {
	// entry is the index of the entry in the session, or -1 for an item a
	// compaction put in place of history.
	entry int
	// comp and pos place an item a compaction put in: the compaction row
	// and the item's position in its Summary.
	comp, pos int
	e         transcript.Entry
	kind      itemKind
}

// finish replays the rows the way Codex rebuilds a thread's history when it
// resumes (core/src/session/rollout_reconstruction.rs): response items join
// the history, a compacted row replaces it, and a thread_rolled_back event
// drops the last user turns from it. A compaction becomes the row's
// Compaction; a rollback makes the entries it dropped AudienceNone, and
// takes the items it dropped from a compaction's replacement out of its
// Summary.
func finish(s *transcript.Session) error {
	var history []slot
	dropped := map[[2]int]bool{}
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Raw == nil {
			continue
		}
		var r row
		if json.Unmarshal(e.Raw, &r) != nil {
			continue
		}
		switch r.Type {
		case "response_item", "inter_agent_communication":
			if e.Role == transcript.RoleOpaque || !e.Audience.Model() {
				continue
			}
			kind := itemKind{turn: true}
			if r.Type == "response_item" {
				_, kind, _, _ = decodeItem(r.Payload)
			}
			history = append(history, slot{entry: i, e: *e, kind: kind})
		case "compacted":
			var c compacted
			if err := json.Unmarshal(r.Payload, &c); err != nil {
				return err
			}
			at, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
			hideSummaryOutput(s, i, c.Message)
			var next []slot
			if c.ReplacementHistory != nil {
				for _, raw := range c.ReplacementHistory {
					m, kind, ok, err := decodeItem(raw)
					if err != nil {
						return err
					}
					if ok && m.Audience.Model() {
						m.Time = at
						// Inside the replacement the summary is the
						// conversation so far, and so are the kept turns;
						// only the context Codex injects stays the model's.
						if isSummary(m) {
							m.Audience = transcript.AudienceAll
						}
						next = append(next, slot{entry: -1, comp: i, e: m, kind: kind})
					}
				}
			} else {
				next = legacyCompaction(history, c.Message, i, at)
			}
			for k := range next {
				next[k].pos = k
			}
			summary := make([]transcript.Entry, len(next))
			for k, sl := range next {
				summary[k] = sl.e
			}
			e.Compaction = &transcript.Compaction{Summary: summary}
			history = next
		case "event_msg":
			var ev rolledBack
			if json.Unmarshal(r.Payload, &ev) != nil || ev.Type != "thread_rolled_back" {
				continue
			}
			cut := rollbackCut(history, ev.NumTurns)
			for _, sl := range history[cut:] {
				if sl.entry >= 0 {
					s.Entries[sl.entry].Audience = transcript.AudienceNone
				} else {
					dropped[[2]int{sl.comp, sl.pos}] = true
				}
			}
			history = history[:cut]
		}
	}
	if len(dropped) > 0 {
		for i := range s.Entries {
			c := s.Entries[i].Compaction
			if c == nil {
				continue
			}
			var kept []transcript.Entry
			for k, m := range c.Summary {
				if !dropped[[2]int{i, k}] {
					kept = append(kept, m)
				}
			}
			c.Summary = kept
		}
	}
	return nil
}

// rollbackCut is where dropping the last n user turns cuts the history
// (context_manager/history.rs, drop_last_n_user_turns): at the n-th turn
// from the end, or at the first turn when there are fewer, and before any
// injected context sitting just ahead of that turn. A history with no turn
// keeps everything.
func rollbackCut(history []slot, n int) int {
	var turns []int
	for k, sl := range history {
		if sl.kind.turn {
			turns = append(turns, k)
		}
	}
	if n <= 0 || len(turns) == 0 {
		return len(history)
	}
	cut := turns[0]
	if n < len(turns) {
		cut = turns[len(turns)-n]
	}
	for cut > turns[0] && history[cut-1].kind.preamble {
		cut--
	}
	return cut
}

// legacyCompaction is the history a compacted row without a replacement
// history leaves: the user messages so far, newest first up to the token
// budget, then the summary as a user message (core/src/compact.rs,
// build_compacted_history). Codex cuts the one message that crosses the
// budget down to what remains of it; it is kept whole here.
func legacyCompaction(history []slot, message string, comp int, at time.Time) []slot {
	var kept []slot
	remaining := compactUserMessageMaxTokens
	for k := len(history) - 1; k >= 0 && remaining > 0; k-- {
		sl := history[k]
		if !sl.kind.turn || sl.e.Role != transcript.RoleUser || isSummary(sl.e) {
			continue
		}
		kept = append(kept, slot{entry: -1, comp: comp, e: sl.e, kind: sl.kind})
		remaining -= (len(sl.e.Text()) + 3) / 4
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	if message == "" {
		message = "(no summary available)"
	}
	return append(kept, slot{entry: -1, comp: comp, kind: itemKind{turn: true}, e: transcript.Entry{
		Role:    transcript.RoleUser,
		Time:    at,
		Content: []transcript.Block{{Kind: transcript.BlockText, Text: message}},
	}})
}

// isSummary reports whether an entry is the user message a compaction
// leaves in place of the history it retired.
func isSummary(e transcript.Entry) bool {
	return e.Role == transcript.RoleUser && strings.HasPrefix(e.Text(), summaryPrefix+"\n")
}

// hideSummaryOutput marks the model's answer to a local compaction request
// as not shown. Codex records that answer in the history before replacing
// the history with it, and never displays it as a message
// (core/src/compact.rs: the summary is the last assistant message). The
// compaction turn records no prompt of its own, so that answer follows the
// previous turn's last item; an answer right after a prompt answers the
// prompt, and is shown even when its text is the summary's (a session
// another agent wrote, whose last answer and summary can read the same).
func hideSummaryOutput(s *transcript.Session, comp int, message string) {
	summary, ok := strings.CutPrefix(message, summaryPrefix+"\n")
	if !ok {
		return
	}
	for k := comp - 1; k >= 0; k-- {
		e := &s.Entries[k]
		if e.Role != transcript.RoleAssistant || e.Text() == "" {
			continue
		}
		if e.Text() != summary || e.Audience != transcript.AudienceAll {
			return
		}
		for j := k - 1; j >= 0; j-- {
			if p := s.Entries[j]; p.Role != transcript.RoleOpaque {
				if p.Role == transcript.RoleUser && p.Audience == transcript.AudienceAll {
					return
				}
				break
			}
		}
		e.Audience = transcript.AudienceModel
		return
	}
}
