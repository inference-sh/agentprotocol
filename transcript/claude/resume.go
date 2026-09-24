package claude

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// What Claude Code resumes with is not the chain of parents from the file's
// last row. Its loader (loadTranscriptFile and buildConversationChain in
// utils/sessionStorage.ts, and the newer loader in the 2.1.28x binary this
// file follows where the two differ) reads the file into a map of message
// rows and then:
//
//   - splices back the messages a compaction kept, which still name their
//     pre-compaction parents on disk, and drops what the compaction retired;
//   - picks the leaf from the last message row, or from a last-prompt row
//     that pins one (a rewind);
//   - walks parents from the leaf, then brings back the rows of a parallel
//     tool call the single-parent walk misses, and the bookkeeping rows that
//     hang off the leaf.
//
// Before the API call, normalizeMessagesForAPI merges the rows one assistant
// message was streamed into. finish does all of this on the session: model
// links follow the loader's chain, Leaf is the chain's end, and each API
// message is one entry.

// meta is what the loader reads off a row besides its content.
type meta struct {
	Type        string  `json:"type"`
	Subtype     string  `json:"subtype"`
	UUID        string  `json:"uuid"`
	IsSidechain bool    `json:"isSidechain"`
	Timestamp   string  `json:"timestamp"`
	IsMeta      bool    `json:"isMeta"`
	Message     *msgID  `json:"message"`
	Attachment  *attach `json:"attachment"`

	CompactMetadata *compactMetadata `json:"compactMetadata"`

	// last-prompt rows. leafUuid is absent, null or a uuid, and the three
	// mean different things.
	LeafUUID json.RawMessage `json:"leafUuid"`
	Explicit bool            `json:"explicit"`
}

type msgID struct {
	ID string `json:"id"`
}

type attach struct {
	Type string `json:"type"`
}

type compactMetadata struct {
	// PreservedSegment names the kept messages by their ends: the chain from
	// tail up to head, spliced in after anchor.
	PreservedSegment *struct {
		HeadUUID   string `json:"headUuid"`
		AnchorUUID string `json:"anchorUuid"`
		TailUUID   string `json:"tailUuid"`
	} `json:"preservedSegment"`
	// PreservedMessages lists them outright; newer versions write both and
	// read this one first.
	PreservedMessages *struct {
		AnchorUUID string   `json:"anchorUuid"`
		UUIDs      []string `json:"uuids"`
	} `json:"preservedMessages"`
}

func (m *meta) message() bool {
	switch m.Type {
	case "user", "assistant", "attachment", "system":
		return m.UUID != ""
	}
	return false
}

func (m *meta) conversational() bool { return m.Type == "user" || m.Type == "assistant" }

func (m *meta) boundary() bool { return m.Type == "system" && m.Subtype == "compact_boundary" }

// bookkeeping is a row the loader carries along behind a message without it
// being one: attachments, system rows other than a boundary, and meta user
// rows that hold no tool result.
func (m *meta) bookkeeping(e transcript.Entry) bool {
	switch m.Type {
	case "attachment":
		return true
	case "system":
		return m.Subtype != "compact_boundary"
	case "user":
		return m.IsMeta && !hasBlock(e, transcript.BlockToolResult)
	}
	return false
}

func (m *meta) msgID() string {
	if m.Message == nil {
		return ""
	}
	return m.Message.ID
}

func hasBlock(e transcript.Entry, k transcript.BlockKind) bool {
	for _, b := range e.Content {
		if b.Kind == k {
			return true
		}
	}
	return false
}

// loader is Claude's in-memory view of a session: the message rows by uuid,
// with the parents the loader has rewritten held in the entries' ParentID.
type loader struct {
	s    *transcript.Session
	rows []meta
	// byID holds the message rows still in the map. A compaction deletes
	// the rows it retired.
	byID map[string]int
}

func finish(s *transcript.Session) error {
	l := &loader{s: s, rows: make([]meta, len(s.Entries)), byID: map[string]int{}}
	for i, e := range s.Entries {
		if json.Unmarshal(e.Raw, &l.rows[i]) != nil {
			l.rows[i] = meta{}
		}
	}
	l.bridgeProgress()
	for i := range l.rows {
		if l.rows[i].message() {
			l.byID[l.rows[i].UUID] = i
		}
	}
	p := l.scan()
	if p.cleared {
		// A rewind to before the first prompt: Claude resumes with nothing.
		for i := range s.Entries {
			if s.Entries[i].Role != transcript.RoleOpaque {
				s.Entries[i].Audience = transcript.AudienceNone
			}
		}
		return nil
	}
	kept := l.relinkPreserved()
	leaf := l.leaf(p, kept)
	if leaf < 0 {
		return nil
	}
	chain := l.chain(leaf)
	chain = l.recoverParallel(chain)
	chain = l.trailing(leaf, chain)
	for k, i := range chain {
		if k == 0 {
			s.Entries[i].ParentID = ""
		} else {
			s.Entries[i].ParentID = s.Entries[chain[k-1]].ID
		}
	}
	s.Leaf = s.Entries[chain[len(chain)-1]].ID
	l.mergeSplit(chain)
	return nil
}

// bridgeProgress links a row whose parent is a progress row to the progress
// row's own parent. Old versions threaded progress rows through the chain;
// the loader no longer keeps them and bridges the gap (progressBridge).
func (l *loader) bridgeProgress() {
	bridge := map[string]string{}
	for i, r := range l.rows {
		e := &l.s.Entries[i]
		if r.Type == "progress" && r.UUID != "" {
			to, ok := bridge[e.ParentID]
			if !ok {
				to = e.ParentID
			}
			bridge[r.UUID] = to
			continue
		}
		if r.message() {
			if to, ok := bridge[e.ParentID]; ok {
				e.ParentID = to
			}
		}
	}
}

// live reports whether row i is a message row still in the map.
func (l *loader) live(i int) bool {
	r := &l.rows[i]
	return r.message() && l.byID[r.UUID] == i
}

func (l *loader) has(id string) bool {
	_, ok := l.byID[id]
	return id != "" && ok
}

func (l *loader) parent(i int) int {
	j, ok := l.byID[l.s.Entries[i].ParentID]
	if l.s.Entries[i].ParentID == "" || !ok {
		return -1
	}
	return j
}

// pinned is what the loader learns reading rows in file order about which
// leaf to resume from.
type pinned struct {
	last   int // the last main-chain message row
	latest int // the main-chain message row with the latest timestamp
	// leaf is the last last-prompt row's leafUuid since the last compaction.
	leaf string
	// explicit is set when that row was written by a rewind or a fork
	// (explicit:true) and no message row came after it.
	explicit bool
	// cleared is a rewind to before the first prompt: leafUuid null.
	cleared bool
	// any is set once a last-prompt row names a leaf at all.
	any bool
}

func (l *loader) scan() pinned {
	p := pinned{last: -1, latest: -1}
	latest := ""
	for i, r := range l.rows {
		switch {
		case r.message():
			// A fork's briefing is written after the rows it continues.
			if !r.IsSidechain && !(r.Type == "attachment" && r.Attachment != nil && r.Attachment.Type == "fork_briefing") {
				p.last = i
				if r.Timestamp > latest {
					p.latest, latest = i, r.Timestamp
				}
				p.explicit, p.cleared = false, false
			}
			if r.boundary() {
				p.leaf, p.explicit = "", false
			}
		case r.Type == "last-prompt":
			if len(r.LeafUUID) > 0 {
				p.any = true
			}
			var id *string
			_ = json.Unmarshal(r.LeafUUID, &id)
			switch {
			case id != nil && *id != "":
				p.explicit = r.Explicit || p.explicit && *id == p.leaf
				p.leaf, p.cleared = *id, false
			case id == nil && string(r.LeafUUID) == "null" && r.Explicit:
				p.cleared, p.leaf, p.explicit = true, "", false
			}
		}
	}
	return p
}

// relinkPreserved splices in the messages the last compaction kept and
// deletes every other row before that compaction's boundary. The kept rows
// name their pre-compaction parents on disk, since rows are never
// rewritten; the loader chains them one after another behind the anchor
// (the summary), moves the anchor's other children behind the last of them,
// and returns that last one. It returns "" when no compaction kept anything.
// (applyPreservedSegmentRelinks, sessionStorage.ts:1839.)
func (l *loader) relinkPreserved() string {
	last, segAt := -1, -1
	var seg *compactMetadata
	for i, r := range l.rows {
		if !l.live(i) || !r.boundary() {
			continue
		}
		last = i
		if m := r.CompactMetadata; m != nil && (m.PreservedMessages != nil || m.PreservedSegment != nil) {
			seg, segAt = m, i
		}
	}
	if seg == nil {
		return ""
	}
	var anchor string
	var kept []string
	if segAt == last {
		// A compaction with no kept messages after this one makes it stale:
		// nothing is spliced, and everything before the last boundary goes.
		switch {
		case seg.PreservedMessages != nil:
			anchor, kept = seg.PreservedMessages.AnchorUUID, seg.PreservedMessages.UUIDs
		default:
			ps := seg.PreservedSegment
			walked := map[string]bool{}
			reached := false
			for i, ok := l.byID[ps.TailUUID]; ok && !walked[l.rows[i].UUID]; i = l.parent(i) {
				id := l.rows[i].UUID
				walked[id] = true
				kept = append(kept, id)
				if id == ps.HeadUUID {
					reached = true
					break
				}
				if l.parent(i) < 0 {
					break
				}
			}
			if !reached {
				// A kept row is missing: the loader leaves the whole file
				// as it is.
				return ""
			}
			for a, b := 0, len(kept)-1; a < b; a, b = a+1, b-1 {
				kept[a], kept[b] = kept[b], kept[a]
			}
			anchor = ps.AnchorUUID
		}
		for _, id := range kept {
			if !l.has(id) {
				return ""
			}
		}
	}
	keep := make(map[string]bool, len(kept))
	if len(kept) > 0 {
		prev := anchor
		for _, id := range kept {
			l.s.Entries[l.byID[id]].ParentID = prev
			keep[id] = true
			prev = id
		}
		for id, i := range l.byID {
			if l.s.Entries[i].ParentID == anchor && id != kept[0] {
				l.s.Entries[i].ParentID = prev
			}
		}
	}
	pruned := map[string]bool{}
	for id, i := range l.byID {
		if i < last && !keep[id] {
			pruned[id] = true
			delete(l.byID, id)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	tail := kept[len(kept)-1]
	for _, i := range l.byID {
		if l.rows[i].conversational() && pruned[l.s.Entries[i].ParentID] {
			l.s.Entries[i].ParentID = tail
		}
	}
	return tail
}

// up walks from row i to the nearest user or assistant row, i included.
func (l *loader) up(i int) int {
	seen := map[int]bool{}
	for i >= 0 && !seen[i] {
		if l.rows[i].conversational() {
			return i
		}
		seen[i] = true
		i = l.parent(i)
	}
	return -1
}

// descends reports whether row i has ancestor a, or is a.
func (l *loader) descends(i, a int) bool {
	seen := map[int]bool{}
	for i >= 0 && !seen[i] {
		if i == a {
			return true
		}
		seen[i] = true
		i = l.parent(i)
	}
	return false
}

// leaf is the row Claude resumes from, or -1 when there is none.
func (l *loader) leaf(p pinned, kept string) int {
	pin := -1
	if l.has(p.leaf) {
		pin = l.byID[p.leaf]
	}
	pinOK := p.explicit && pin >= 0 && !l.rows[pin].IsSidechain
	start := p.last
	if !p.any && start >= 0 && p.latest >= 0 && p.latest != start {
		start = l.latestDescendant(start)
	}
	if kept == "" || pinOK {
		from := pin
		// A last-prompt row written by the turn loop names that turn's
		// prompt; rows after it continue from there, and the last one is the
		// leaf. Only an explicit pin (a rewind) holds against later rows.
		if from >= 0 && !p.explicit && p.last >= 0 && p.last != from && l.descends(p.last, from) {
			from = p.last
		}
		if kept == "" && from < 0 {
			from = start
		}
		if from >= 0 {
			if i := l.up(from); i >= 0 {
				return i
			}
		}
	}
	// Otherwise every dead end of the tree is a candidate, and the pinned
	// or last row settles between several.
	hasChild := map[string]bool{}
	hasTurn := map[string]bool{}
	for i := range l.rows {
		if !l.live(i) {
			continue
		}
		pid := l.s.Entries[i].ParentID
		hasChild[pid] = true
		if l.rows[i].conversational() {
			hasTurn[pid] = true
		}
	}
	var leaves []int
	in := map[int]bool{}
	for i := range l.rows {
		if !l.live(i) || hasChild[l.rows[i].UUID] {
			continue
		}
		if j := l.up(i); j >= 0 && !hasTurn[l.rows[j].UUID] && !in[j] {
			in[j] = true
			leaves = append(leaves, j)
		}
	}
	switch {
	case len(leaves) == 0:
		return -1
	case len(leaves) == 1:
		return leaves[0]
	}
	from := p.last
	if in[pin] {
		from = pin
	}
	if from >= 0 {
		if i := l.up(from); i >= 0 {
			return i
		}
	}
	best := leaves[0]
	for _, i := range leaves[1:] {
		if l.rows[i].Timestamp > l.rows[best].Timestamp {
			best = i
		}
	}
	return best
}

// latestDescendant is the main-chain row with the latest timestamp that
// descends from row i, or i. A file without last-prompt rows was written
// before the loader trusted file order.
func (l *loader) latestDescendant(i int) int {
	// under memoizes whether a row descends from i, so the scan is linear.
	under := map[int]bool{i: true}
	descends := func(j int) bool {
		var path []int
		found := false
		for k := j; k >= 0; k = l.parent(k) {
			if v, ok := under[k]; ok {
				found = v
				break
			}
			under[k] = false
			path = append(path, k)
		}
		for _, k := range path {
			under[k] = found
		}
		return found
	}
	best, ts := i, ""
	for j := range l.rows {
		r := l.rows[j]
		if j == i || !l.live(j) || r.IsSidechain || r.Timestamp < ts {
			continue
		}
		if descends(j) {
			best, ts = j, r.Timestamp
		}
	}
	return best
}

// gapWindow is how far back the loader looks for a row to continue from
// when a parent is missing.
const gapWindow = 5 * time.Second

// chain walks parents from the leaf, root first. A parent the map does not
// hold is replaced by the row written closest before, within gapWindow.
func (l *loader) chain(leaf int) []int {
	var out []int
	seen := map[int]bool{}
	for i := leaf; i >= 0; {
		seen[i] = true
		out = append(out, i)
		pid := l.s.Entries[i].ParentID
		if pid == "" {
			break
		}
		j, ok := l.byID[pid]
		if !ok || seen[j] {
			j = l.closestBefore(i, seen)
		}
		i = j
	}
	for a, b := 0, len(out)-1; a < b; a, b = a+1, b-1 {
		out[a], out[b] = out[b], out[a]
	}
	return out
}

func (l *loader) closestBefore(i int, seen map[int]bool) int {
	at := l.s.Entries[i].Time
	if at.IsZero() {
		return -1
	}
	best, gap := -1, gapWindow+1
	for _, j := range l.byID {
		t := l.s.Entries[j].Time
		if seen[j] || t.IsZero() || l.rows[j].IsSidechain != l.rows[i].IsSidechain {
			continue
		}
		if d := at.Sub(t); d >= 0 && d <= gapWindow && (d < gap || d == gap && j < best) {
			best, gap = j, d
		}
	}
	return best
}

// recoverParallel brings back what the single-parent walk misses when one
// assistant message was streamed as several rows sharing message.id. Each
// tool_use gets its own row, chained one after another, and each tool
// result names the row of its own call as parent, so the walk keeps one
// branch of that fan. The loader takes every row of the message and every
// tool result under one of them, and places them in file order among the
// chain rows from the message's first row to just past its last.
// (recoverOrphanedParallelToolResults, sessionStorage.ts:2118.)
func (l *loader) recoverParallel(chain []int) []int {
	seen := make(map[int]bool, len(chain))
	pos := make(map[int]int, len(chain))
	for k, i := range chain {
		seen[i] = true
		pos[i] = k
	}
	groups := map[string][]int{}
	results := map[string][]int{}
	children := map[string][]int{}
	for i := range l.rows {
		r := l.rows[i]
		if !l.live(i) {
			continue
		}
		pid := l.s.Entries[i].ParentID
		if pid != "" {
			children[pid] = append(children[pid], i)
		}
		switch {
		case r.Type == "assistant" && r.msgID() != "":
			groups[r.msgID()] = append(groups[r.msgID()], i)
		case r.Type == "user" && pid != "" && hasBlock(l.s.Entries[i], transcript.BlockToolResult):
			results[pid] = append(results[pid], i)
		}
	}
	type window struct {
		start, end int
		recovered  []int
		after      map[int][]int
	}
	var windows []window
	done := map[string]bool{}
	for _, i := range chain {
		r := l.rows[i]
		id := r.msgID()
		if r.Type != "assistant" || id == "" || done[id] {
			continue
		}
		done[id] = true
		members := groups[id]
		var found []int
		for _, m := range members {
			if !seen[m] {
				found = append(found, m)
			}
		}
		for _, m := range members {
			for _, t := range results[l.rows[m].UUID] {
				if !seen[t] {
					seen[t] = true
					found = append(found, t)
				}
			}
		}
		for _, m := range found {
			seen[m] = true
		}
		start := pos[i]
		end := start + 1
		for _, m := range members {
			if k, ok := pos[m]; ok && k >= end {
				end = k + 1
			}
		}
		for end < len(chain) && l.continues(chain[end], id) {
			end++
		}
		// Bookkeeping rows left dangling off the message or its results
		// come along behind them.
		after := map[int][]int{}
		if l.calls(members) {
			from := append([]int(nil), members...)
			for _, m := range members {
				for _, t := range results[l.rows[m].UUID] {
					if seen[t] {
						from = append(from, t)
					}
				}
			}
			for _, m := range from {
				found = append(found, l.tails(m, children, seen)...)
			}
			for _, w := range chain[start+1 : end] {
				if l.rows[w].bookkeeping(l.s.Entries[w]) {
					if t := l.tails(w, children, seen); len(t) > 0 {
						after[w] = t
					}
				}
			}
		}
		if len(found) == 0 && len(after) == 0 {
			continue
		}
		sort.Ints(found)
		windows = append(windows, window{start, end, found, after})
	}
	if len(windows) == 0 {
		return chain
	}
	// Windows are in chain order already; merge the ones that overlap.
	merged := []window{windows[0]}
	for _, w := range windows[1:] {
		p := &merged[len(merged)-1]
		if w.start < p.end {
			p.end = max(p.end, w.end)
			p.recovered = append(p.recovered, w.recovered...)
			sort.Ints(p.recovered)
			for k, v := range w.after {
				p.after[k] = append(p.after[k], v...)
			}
			continue
		}
		merged = append(merged, w)
	}
	var out []int
	next := 0
	for _, w := range merged {
		out = append(out, chain[next:w.start+1]...)
		v := 0
		for _, c := range chain[w.start+1 : w.end] {
			for v < len(w.recovered) && w.recovered[v] < c {
				out = append(out, w.recovered[v])
				v++
			}
			out = append(out, c)
			out = append(out, w.after[c]...)
		}
		out = append(out, w.recovered[v:]...)
		next = w.end
	}
	return append(out, chain[next:]...)
}

// continues reports whether a chain row after an assistant message still
// belongs to its turn: another row of the message, a tool result, injected
// context, an attachment or a system row other than a boundary.
func (l *loader) continues(i int, id string) bool {
	r := l.rows[i]
	switch r.Type {
	case "assistant":
		return r.msgID() == id
	case "user":
		return r.IsMeta || hasBlock(l.s.Entries[i], transcript.BlockToolResult)
	case "attachment":
		return true
	case "system":
		return !r.boundary()
	}
	return false
}

func (l *loader) calls(members []int) bool {
	for _, m := range members {
		if hasBlock(l.s.Entries[m], transcript.BlockToolUse) {
			return true
		}
	}
	return false
}

// tails collects, for each child of row i off the chain, the run of
// bookkeeping rows hanging from it, when it is a single unbranched run of
// them and nothing else.
func (l *loader) tails(i int, children map[string][]int, seen map[int]bool) []int {
	var out []int
	for _, c := range children[l.rows[i].UUID] {
		var run []int
		for c >= 0 {
			if seen[c] || l.rows[c].IsSidechain != l.rows[i].IsSidechain || !l.rows[c].bookkeeping(l.s.Entries[c]) {
				run = nil
				break
			}
			run = append(run, c)
			var next []int
			for _, k := range children[l.rows[c].UUID] {
				if !seen[k] {
					next = append(next, k)
				}
			}
			if len(next) > 1 {
				run = nil
				break
			}
			c = -1
			if len(next) == 1 {
				c = next[0]
			}
		}
		for _, k := range run {
			seen[k] = true
		}
		out = append(out, run...)
	}
	return out
}

// trailing appends the rows other than messages that descend from the leaf
// through rows other than messages (a turn's duration, a hook summary), in
// timestamp order, so the chain ends where the next turn will link.
func (l *loader) trailing(leaf int, chain []int) []int {
	seen := make(map[int]bool, len(chain))
	for _, i := range chain {
		seen[i] = true
	}
	children := map[string][]int{}
	for i := range l.rows {
		r := l.rows[i]
		pid := l.s.Entries[i].ParentID
		if l.live(i) && pid != "" && !r.conversational() {
			children[pid] = append(children[pid], i)
		}
	}
	var found []int
	queue := []string{l.rows[leaf].UUID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, c := range children[id] {
			if seen[c] {
				continue
			}
			seen[c] = true
			found = append(found, c)
			queue = append(queue, l.rows[c].UUID)
		}
	}
	sort.SliceStable(found, func(a, b int) bool { return l.rows[found[a]].Timestamp < l.rows[found[b]].Timestamp })
	return append(chain, found...)
}

// mergeSplit makes each assistant message one entry, as
// normalizeMessagesForAPI does before the call (messages.ts:2250): a row
// joins the latest earlier row of the same message.id, looking back past
// other assistant messages and tool results but not past anything the
// person said. Its blocks move to that entry and it stays behind, opaque,
// holding its place in the chain.
func (l *loader) mergeSplit(chain []int) {
	type apiMsg struct {
		at        int // entry index of an assistant message, else -1
		id        string
		hasResult bool
	}
	var out []apiMsg
	for _, i := range chain {
		e := &l.s.Entries[i]
		if e.Role == transcript.RoleOpaque || !e.Audience.Model() {
			continue
		}
		if e.Role != transcript.RoleAssistant {
			// Consecutive user rows go to the API as one user message.
			res := hasBlock(*e, transcript.BlockToolResult)
			if n := len(out); n > 0 && out[n-1].at < 0 {
				out[n-1].hasResult = out[n-1].hasResult || res
				continue
			}
			out = append(out, apiMsg{at: -1, hasResult: res})
			continue
		}
		id := l.rows[i].msgID()
		merged := false
		for k := len(out) - 1; k >= 0 && id != ""; k-- {
			m := out[k]
			if m.at < 0 && !m.hasResult {
				break
			}
			if m.at >= 0 && m.id == id {
				into := &l.s.Entries[m.at]
				into.Content = append(into.Content, e.Content...)
				e.Role, e.Content = transcript.RoleOpaque, nil
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, apiMsg{at: i, id: id})
		}
	}
}

// stamp gives every entry this write creates a timestamp after every row
// before it. Claude orders rows by timestamp where file order is not enough
// (the older loader resumes from the latest one), so an appended turn must
// not share or predate the time of the row it follows.
func stamp(home string, s *transcript.Session) error {
	var last time.Time
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.Raw != nil || e.Role == transcript.RoleOpaque {
			if e.Time.After(last) {
				last = e.Time
			}
			continue
		}
		t := e.Time
		if t.IsZero() {
			t = s.Updated
		}
		if t.IsZero() {
			t = time.Now()
		}
		// Rows carry milliseconds; two stamps apart by less are equal.
		t = t.Truncate(time.Millisecond)
		if !t.After(last) {
			t = last.Add(time.Millisecond)
		}
		e.Time, last = t, t
	}
	return nil
}
