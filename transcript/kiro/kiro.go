// Package kiro reads and writes kiro-cli sessions.
//
// kiro keeps two files per session under ~/.kiro/sessions/cli: <id>.jsonl
// with one message per row, and <id>.json, a sidecar with the session's
// metadata and, per user turn, the ids of the messages that made it up. A
// written session needs both.
//
// The transcript is kiro's event log: besides the messages it holds the
// rows /compact and /clear write, which change what the model is given on
// resume and not what session/load replays; see compacted.
package kiro

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec is the kiro-cli session store.
var Codec transcript.Codec = codec{}

var files = transcript.JSONL{
	Agent: "kiro",
	Layout: transcript.Layout{
		Root: ".kiro/sessions/cli",
		Ext:  ".jsonl",
		Peek: peek,
	},
	Decode:  decode,
	Finish:  compacted,
	Encode:  encode,
	Prepare: prepare,
	After:   writeSidecar,
	// kiro records a compaction as its own; it keeps no row it shows and
	// does not send.
	Caps: transcript.Capabilities{Compaction: true},
}

// codec wraps the JSONL store so Read also loads the sidecar, which the
// generic store does not know about.
type codec struct{}

func (codec) Open(home string) (transcript.Store, error) {
	st, err := files.Open(home)
	if err != nil {
		return nil, err
	}
	return &store{Store: st, home: home}, nil
}

type store struct {
	transcript.Store
	home string
}

// Write gives new entries of a kiro session their ids before writing, so
// the transcript rows and the sidecar's message lists name the same
// messages. Another agent's session is left to the JSONL store, which
// takes it through Portable and prepare, where its ids are assigned once
// each compaction is matched to the entries it keeps.
func (st *store) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.Agent == "kiro" {
		transcript.AssignIDs(s, transcript.UUIDs, false)
	}
	return st.Store.Write(ctx, s)
}

// prepare puts another agent's compactions in the form a Compaction row
// holds: the messages each keeps are copied into it, with the ids their
// own rows get.
func prepare(home string, s *transcript.Session) error {
	if s.Agent != "kiro" {
		alternate(s)
		cancelLast(s)
		keepAnsweredPrompts(s)
		kept := keptFrom(s)
		transcript.AssignIDs(s, transcript.UUIDs, false)
		snapshot(s, kept)
	}
	return nil
}

// alternate joins the adjacent messages of a portable session that kiro
// would otherwise hold as two in a row from the same side. kiro's history
// alternates a user message (a Prompt or a ToolResults row) with an
// assistant one, as its own sessions do; given two assistant messages, two
// prompts, two ToolResults rows or a prompt beside a ToolResults row, kiro
// 2.24.0 loads the session and fails its first request on "invalid
// conversation history received" without answering the prompt. Joined user
// messages are a ToolResults row when either held a tool's result, which
// kiro sends with the text beside the results. A compaction between two
// messages keeps them apart, and a compaction keeping a message that is
// joined keeps the message it joined.
func alternate(s *transcript.Session) {
	side := func(r transcript.Role) transcript.Role {
		if r == transcript.RoleTool {
			return transcript.RoleUser
		}
		return r
	}
	renamed := map[string]string{}
	var out []transcript.Entry
	last := -1 // the latest message in out since the latest compaction
	for _, e := range s.Entries {
		switch {
		case e.Compaction != nil:
			last = -1
		case e.Role != transcript.RoleUser && e.Role != transcript.RoleAssistant && e.Role != transcript.RoleTool:
			// Not a message kiro writes, so nothing between two that are.
		case last >= 0 && side(e.Role) == side(out[last].Role):
			prev := &out[last]
			prev.Content = append(slices.Clip(prev.Content), e.Content...)
			if e.Role == transcript.RoleTool {
				prev.Role = transcript.RoleTool
			}
			switch {
			case prev.ID == "":
				prev.ID = e.ID
			case e.ID != "":
				renamed[e.ID] = prev.ID
			}
			continue
		default:
			last = len(out)
		}
		out = append(out, e)
	}
	for _, e := range out {
		if c := e.Compaction; c != nil {
			if to, ok := renamed[c.Keep]; ok {
				c.Keep = to
			}
		}
	}
	s.Entries = out
}

// cancelledPrompt is kiro's row for a prompt that got no answer.
var cancelledPrompt = json.RawMessage(`{"version":"v1","kind":"CancelledPrompt"}`)

// cancelLast follows a portable session's last user message with a
// CancelledPrompt row when nothing answered it. kiro fails the next request
// of a history ending on a user message, since the prompt it sends next
// would be a second one in a row. The row keeps the message in the session
// and shown on session/load, and out of what the model is given, which is
// how kiro treats a prompt it cancelled; the person's next prompt then
// follows an answer. A last tool result gets the row too: without it kiro
// refuses the whole session, and with it only that result is left out of
// the model's context (kiro-cli 2.24.0 sends the call without it).
func cancelLast(s *transcript.Session) {
	for i := len(s.Entries) - 1; i >= 0; i-- {
		switch s.Entries[i].Role {
		case transcript.RoleUser, transcript.RoleTool:
			s.Entries = append(s.Entries, transcript.Entry{Raw: cancelledPrompt})
			return
		case transcript.RoleAssistant:
			return
		}
		if s.Entries[i].Compaction != nil {
			return
		}
	}
}

// keepAnsweredPrompts gives a compaction that keeps nothing, and is followed
// by an answer, the turn that answer belongs to: kiro sends a compaction's
// summary at the head of the first prompt after it and fails the first
// request of a history that opens with an answer ("invalid conversation
// history received"). Claude's compaction, for one, keeps nothing and its
// model answers the summary itself. The kept range reaches back to the
// answer's prompt, which moves from the history the compaction retired to
// what it keeps, as keptFrom does for a range starting inside a turn. An
// answer with no prompt before it, back to the start or an earlier
// compaction, has nothing to open the history with and is left out.
func keepAnsweredPrompts(s *transcript.Session) {
	for i := 0; i < len(s.Entries); i++ {
		c := s.Entries[i].Compaction
		if c == nil || c.Keep != "" {
			continue
		}
		next := -1
		for k := i + 1; k < len(s.Entries) && s.Entries[k].Compaction == nil; k++ {
			if r := s.Entries[k].Role; r == transcript.RoleUser || r == transcript.RoleAssistant || r == transcript.RoleTool {
				next = k
				break
			}
		}
		if next < 0 || s.Entries[next].Role != transcript.RoleAssistant {
			continue
		}
		prompt := -1
		for k := i - 1; k >= 0 && s.Entries[k].Compaction == nil; k-- {
			if s.Entries[k].Role == transcript.RoleUser {
				prompt = k
				break
			}
		}
		if prompt < 0 {
			s.Entries = slices.Delete(s.Entries, next, next+1)
			continue
		}
		if s.Entries[prompt].ID == "" {
			s.Entries[prompt].ID = transcript.NewUUID()
		}
		c.Keep = s.Entries[prompt].ID
	}
}

// keptFrom finds, for each compaction of a portable session, the index of
// the first entry it keeps, before ids are assigned and Keep can no longer
// be matched. A compaction keeping nothing maps to itself.
func keptFrom(s *transcript.Session) map[int]int {
	out := map[int]int{}
	for i, e := range s.Entries {
		if e.Compaction == nil {
			continue
		}
		out[i] = i
		k := e.Compaction.Keep
		if k == "" {
			continue
		}
		j := slices.IndexFunc(s.Entries[:i], func(x transcript.Entry) bool { return x.ID == k })
		if j < 0 {
			continue
		}
		// kiro keeps whole turns, each opening with a prompt, and sends
		// the summary at the head of the first one; a history opening
		// with an answer gets no reply. Kept history that starts inside a
		// turn keeps that turn's prompt too.
		for j > 0 && s.Entries[j].Role != transcript.RoleUser && s.Entries[j-1].Compaction == nil {
			j--
		}
		out[i] = j
	}
	return out
}

// snapshot puts each compaction in the form a Compaction row holds, as
// the reader gives it: its summary as one entry, followed by copies of the
// messages it keeps, which kiro gives the model after the summary.
func snapshot(s *transcript.Session, kept map[int]int) {
	for i, from := range kept {
		c := s.Entries[i].Compaction
		summary := transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: storedSummary(c.Summary)}}}
		next := &transcript.Compaction{Summary: []transcript.Entry{summary}}
		for _, e := range s.Entries[from:i] {
			if e.Role != transcript.RoleOpaque {
				next.Summary = append(next.Summary, e)
			}
		}
		s.Entries[i].Compaction = next
	}
}

// storedSummary is a summary as a Compaction row stores it: the text of its
// entries, without the context entry kiro wraps it in on resume, which a
// summary read from kiro carries.
func storedSummary(summary []transcript.Entry) string {
	var parts []string
	for _, e := range summary {
		t := e.Text()
		if inner, ok := strings.CutPrefix(t, summaryPrefix); ok {
			t = strings.TrimSuffix(inner, summarySuffix)
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (st *store) Read(ctx context.Context, id string) (*transcript.Session, error) {
	s, err := st.Store.Read(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := readSidecar(st.home, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Vendor is what a kiro session carries in Session.Vendor: the sidecar
// document as read, field by field, so a write keeps everything in it that
// this codec does not model.
type Vendor struct {
	Sidecar map[string]json.RawMessage
}

// sidecar is the part of <id>.json the codec reads and writes. Everything
// else in the document passes through Vendor untouched.
type sidecar struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Title     string `json:"title,omitempty"`
}

type row struct {
	Version string `json:"version"`
	Kind    string `json:"kind"`
	Data    data   `json:"data"`
}

type data struct {
	MessageID string          `json:"message_id"`
	Content   []block         `json:"content"`
	Meta      *meta           `json:"meta,omitempty"`
	Results   json.RawMessage `json:"results,omitempty"`
}

// compaction is the data of a Compaction row: the summary, and the messages
// the model keeps after it, as the engine held them once it had compacted.
type compaction struct {
	Summary          string            `json:"summary"`
	MessagesSnapshot []snapshotMessage `json:"messages_snapshot"`
}

// snapshotMessage is a message in a Compaction row's snapshot. It carries
// the id of the row it was copied from.
type snapshotMessage struct {
	ID      string  `json:"id"`
	Role    string  `json:"role"`
	Content []block `json:"content"`
	Meta    *meta   `json:"meta,omitempty"`
}

type meta struct {
	Timestamp int64 `json:"timestamp"`
}

type block struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type toolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type toolResult struct {
	ToolUseID string  `json:"toolUseId"`
	Content   []block `json:"content"`
	Status    string  `json:"status"`
}

// toolOutcome is a ToolResults row's record of one tool's run, keyed by
// the tool use id: the tool as kiro parsed it, which kiro accepts as null,
// and the result as {"Success": {"items": [...]}} or {"Error": {"Custom":
// message}}. kiro rejects a history whose record has any other shape
// ("invalid conversation history received" on the next request).
type toolOutcome struct {
	Tool   json.RawMessage `json:"tool"`
	Result toolOutput      `json:"result"`
}

type toolOutput struct {
	Success *toolItems `json:"Success,omitempty"`
	Error   *toolError `json:"Error,omitempty"`
}

// toolItems is a successful run's output, each item {"Text": ...} or
// {"Image": ...}.
type toolItems struct {
	Items []map[string]json.RawMessage `json:"items"`
}

type toolError struct {
	Custom string `json:"Custom"`
}

func peek(path string) (transcript.Info, error) {
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A transcript without its sidecar is still a transcript; the
			// id is the file name and the cwd is unknown.
			return transcript.Info{}, nil
		}
		return transcript.Info{}, err
	}
	var sc sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return transcript.Info{}, err
	}
	in := transcript.Info{ID: sc.SessionID, CWD: sc.CWD, Title: sc.Title}
	if t, err := time.Parse(time.RFC3339Nano, sc.UpdatedAt); err == nil {
		in.Updated = t
	}
	return in, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	e := transcript.Entry{ID: r.Data.MessageID}
	switch r.Kind {
	case "Prompt":
		e.Role = transcript.RoleUser
	case "AssistantMessage":
		e.Role = transcript.RoleAssistant
	case "ToolResults":
		e.Role = transcript.RoleTool
	default:
		return transcript.Entry{}, false, nil
	}
	if r.Data.Meta != nil && r.Data.Meta.Timestamp > 0 {
		e.Time = time.Unix(r.Data.Meta.Timestamp, 0).UTC()
	}
	var err error
	if e.Content, err = content(e.ID, r.Data.Content); err != nil {
		return transcript.Entry{}, false, err
	}
	return e, true, nil
}

// content decodes a message's blocks.
func content(id string, bs []block) ([]transcript.Block, error) {
	var out []transcript.Block
	for _, b := range bs {
		switch b.Kind {
		case "text":
			var t string
			if err := json.Unmarshal(b.Data, &t); err != nil {
				return nil, fmt.Errorf("message %s: text: %w", id, err)
			}
			if t != "" {
				out = append(out, transcript.Block{Kind: transcript.BlockText, Text: t})
			}
		case "toolUse":
			var tu toolUse
			if err := json.Unmarshal(b.Data, &tu); err != nil {
				return nil, fmt.Errorf("message %s: toolUse: %w", id, err)
			}
			out = append(out, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tu.ToolUseID, Name: tu.Name, Input: tu.Input})
		case "toolResult":
			var tr toolResult
			if err := json.Unmarshal(b.Data, &tr); err != nil {
				return nil, fmt.Errorf("message %s: toolResult: %w", id, err)
			}
			var text strings.Builder
			for _, c := range tr.Content {
				if c.Kind != "text" {
					continue
				}
				var t string
				if err := json.Unmarshal(c.Data, &t); err != nil {
					return nil, fmt.Errorf("message %s: toolResult text: %w", id, err)
				}
				text.WriteString(t)
			}
			st := transcript.StatusOK
			if tr.Status != "success" {
				st = transcript.StatusError
			}
			out = append(out, transcript.Block{Kind: transcript.BlockToolResult, ToolID: tr.ToolUseID, Text: text.String(), Status: st})
			// An image the tool returned is in the result's own content; it
			// follows the result here.
			for _, c := range tr.Content {
				if c.Kind != "image" {
					continue
				}
				b, ok, err := imageBlock(c.Data)
				if err != nil {
					return nil, fmt.Errorf("message %s: toolResult image: %w", id, err)
				}
				if ok {
					b.ToolID = tr.ToolUseID
					out = append(out, b)
				}
			}
		case "image":
			b, ok, err := imageBlock(b.Data)
			if err != nil {
				return nil, fmt.Errorf("message %s: image: %w", id, err)
			}
			if ok {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// image is an image block's data as kiro's v2 engine writes it: the format
// ("png", "jpeg", "gif", "webp") and its bytes as an array of numbers.
type image struct {
	Format string `json:"format"`
	Source struct {
		Kind string `json:"kind"`
		Data []int  `json:"data"`
	} `json:"source"`
}

// imageBlock reads an image block. A source other than bytes has not been
// seen and is skipped.
func imageBlock(raw json.RawMessage) (transcript.Block, bool, error) {
	var im image
	if err := json.Unmarshal(raw, &im); err != nil {
		return transcript.Block{}, false, err
	}
	if im.Source.Kind != "bytes" {
		return transcript.Block{}, false, nil
	}
	return transcript.Block{Kind: transcript.BlockImage, MediaType: ImageType(im.Format), Data: Bytes(im.Source.Data)}, true, nil
}

// ImageType is the media type of an image format as kiro names it, in
// either engine ("png" in v2, "Png" in v1).
func ImageType(format string) string {
	return "image/" + strings.ToLower(format)
}

// Bytes turns kiro's array of byte values into bytes.
func Bytes(vals []int) []byte {
	out := make([]byte, len(vals))
	for i, v := range vals {
		out[i] = byte(v)
	}
	return out
}

// imageFormats are the formats kiro takes, by media type.
var imageFormats = map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif", "image/webp": "webp"}

// encodeImage writes an image block the way kiro's v2 engine does. An image
// known only by reference, or in a format kiro does not take, has no form
// there and is left out.
func encodeImage(b transcript.Block) (block, bool, error) {
	format, ok := imageFormats[b.MediaType]
	if !ok || len(b.Data) == 0 {
		return block{}, false, nil
	}
	var im image
	im.Format = format
	im.Source.Kind = "bytes"
	im.Source.Data = make([]int, len(b.Data))
	for i, c := range b.Data {
		im.Source.Data[i] = int(c)
	}
	raw, err := json.Marshal(im)
	return block{Kind: "image", Data: raw}, err == nil, err
}

// compacted applies the rows that change what the model is given. On resume
// kiro gives the model, after a Compaction row, a context entry holding the
// row's summary and then the messages the row's snapshot kept; after a Clear
// row, nothing from before it. session/load replays every message either
// way. A CancelledPrompt row takes the user message before it out of what
// the model is given, which session/load still replays (event_log.rs in
// kiro-cli-chat 2.24.0: "CancelledPrompt: expected last message to be user
// message"). The Rewind command forks into a new session and leaves this
// one as it was, and a prompt cancelled over ACP is answered with an
// ordinary message ("Response was interrupted by the user"), so neither
// needs anything here.
func compacted(s *transcript.Session) error {
	for i := range s.Entries {
		e := &s.Entries[i]
		var r struct {
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(e.Raw, &r) != nil {
			continue
		}
		switch r.Kind {
		case "CancelledPrompt":
			for j := i - 1; j >= 0; j-- {
				m := &s.Entries[j]
				if m.Role == transcript.RoleOpaque {
					continue
				}
				if m.Role == transcript.RoleUser || m.Role == transcript.RoleTool {
					m.Audience = transcript.AudienceUser
				}
				break
			}
		case "Clear":
			e.Compaction = &transcript.Compaction{}
		case "Compaction":
			var c compaction
			if err := json.Unmarshal(r.Data, &c); err != nil {
				return fmt.Errorf("compaction: %w", err)
			}
			kept := []transcript.Entry{SummaryMessage(c.Summary)}
			for _, m := range c.MessagesSnapshot {
				k := transcript.Entry{ID: m.ID, Role: transcript.RoleAssistant}
				var err error
				if k.Content, err = content(m.ID, m.Content); err != nil {
					return err
				}
				if m.Role == "user" {
					k.Role = transcript.RoleUser
					if slices.ContainsFunc(k.Content, func(b transcript.Block) bool { return b.Kind == transcript.BlockToolResult }) {
						k.Role = transcript.RoleTool
					}
				}
				if m.Meta != nil && m.Meta.Timestamp > 0 {
					k.Time = time.Unix(m.Meta.Timestamp, 0).UTC()
				}
				kept = append(kept, k)
			}
			e.Compaction = &transcript.Compaction{Summary: kept}
			if keep, ok := inPlace(s.Entries[:i], kept[1:]); ok {
				e.Compaction = &transcript.Compaction{Summary: kept[:1], Keep: keep}
			}
		}
	}
	return nil
}

// inPlace reports whether a Compaction row's snapshot is the messages
// right before the row, unchanged, as kiro copies the turns it keeps, and
// returns the id of the first. The compaction then keeps those messages
// rather than copies of them, so the history they belong to is told from
// the history the compaction retired. A snapshot kiro truncated or
// otherwise changed stays copies.
func inPlace(before, snap []transcript.Entry) (string, bool) {
	if len(snap) == 0 {
		return "", false
	}
	var msgs []transcript.Entry
	for _, e := range before {
		if e.Role != transcript.RoleOpaque {
			msgs = append(msgs, e)
		}
	}
	if len(msgs) < len(snap) {
		return "", false
	}
	msgs = msgs[len(msgs)-len(snap):]
	for i, k := range snap {
		m := msgs[i]
		if m.ID != k.ID || m.Role != k.Role || !reflect.DeepEqual(m.Content, k.Content) {
			return "", false
		}
	}
	return msgs[0].ID, true
}

// SummaryMessage is the message kiro gives the model in place of the
// history a compaction summarized: the summary in a context entry, which
// kiro sends at the head of the first user message, ahead of the context
// entries it builds for every request (agent instructions, workspace
// files). Both of kiro's engines send it.
func SummaryMessage(summary string) transcript.Entry {
	return transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: summaryPrefix + summary + summarySuffix}}}
}

const (
	summaryPrefix = "--- CONTEXT ENTRY BEGIN ---\nThis summary contains ALL relevant information from our previous conversation including tool uses, results, code analysis, and file operations. YOU MUST reference this information when answering questions and explicitly acknowledge specific details from the summary when they're relevant to the current question.\n\nSUMMARY CONTENT:\n"
	summarySuffix = "\n--- CONTEXT ENTRY END ---"
)

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	if e.Role == transcript.RoleOpaque && e.Compaction != nil {
		return encodeCompaction(*e.Compaction, s)
	}
	kind, d, err := message(e, s)
	if kind == "" || err != nil {
		return nil, err
	}
	return json.Marshal(row{Version: "v1", Kind: kind, Data: d})
}

// strategy is how kiro 2.24.0 compacted the sample session, which a
// Compaction row records beside its summary.
var strategy = json.RawMessage(`{"message_pairs_to_exclude":2,"context_window_percent_to_exclude":2,"truncate_large_messages":false,"max_message_length":25000}`)

// encodeCompaction writes a Compaction row from a compaction in the form
// snapshot gives it: the summary, then the messages kept, each copied into
// the row's snapshot with the id of its own row, as kiro copies them.
func encodeCompaction(c transcript.Compaction, s *transcript.Session) (json.RawMessage, error) {
	type compactionOut struct {
		Summary          string            `json:"summary"`
		Strategy         json.RawMessage   `json:"strategy"`
		MessagesSnapshot []snapshotMessage `json:"messages_snapshot"`
	}
	out := compactionOut{Strategy: strategy, MessagesSnapshot: []snapshotMessage{}}
	if len(c.Summary) > 0 {
		out.Summary = c.Summary[0].Text()
	}
	for _, e := range c.Summary[min(1, len(c.Summary)):] {
		kind, d, err := message(e, s)
		if err != nil {
			return nil, err
		}
		role := "user"
		switch kind {
		case "":
			continue
		case "AssistantMessage":
			role = "assistant"
		}
		out.MessagesSnapshot = append(out.MessagesSnapshot, snapshotMessage{ID: d.MessageID, Role: role, Content: d.Content, Meta: d.Meta})
	}
	d, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Version string          `json:"version"`
		Kind    string          `json:"kind"`
		Data    json.RawMessage `json:"data"`
	}{"v1", "Compaction", d})
}

// message is an entry as a message row's kind and data. The kind is empty
// for an entry that is not a message.
func message(e transcript.Entry, s *transcript.Session) (string, data, error) {
	var kind string
	switch e.Role {
	case transcript.RoleUser:
		kind = "Prompt"
	case transcript.RoleAssistant:
		kind = "AssistantMessage"
	case transcript.RoleTool:
		kind = "ToolResults"
	default:
		return "", data{}, nil
	}
	if e.ID == "" {
		e.ID = transcript.NewUUID()
	}
	// Reasoning is not written. kiro keeps a model's reasoning as a thinking
	// block holding the provider's signature, its redacted content and a
	// digest of the tools it was made under, and sends it back as the
	// history's ReasoningContentForHistory with that signature (ThinkingBlock
	// in kiro-cli-chat 2.24.0). Reasoning from another agent has no signature
	// kiro's models accept.
	d := data{MessageID: e.ID, Content: []block{}}
	results := map[string]toolOutcome{}
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			bd, err := json.Marshal(b.Text)
			if err != nil {
				return "", data{}, err
			}
			d.Content = append(d.Content, block{Kind: "text", Data: bd})
		case transcript.BlockToolUse:
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			bd, err := json.Marshal(toolUse{ToolUseID: b.ToolID, Name: b.Name, Input: in})
			if err != nil {
				return "", data{}, err
			}
			d.Content = append(d.Content, block{Kind: "toolUse", Data: bd})
		case transcript.BlockToolResult:
			td, err := json.Marshal(b.Text)
			if err != nil {
				return "", data{}, err
			}
			status := "success"
			if b.Status == transcript.StatusError {
				status = "error"
			}
			rc := []block{{Kind: "text", Data: td}}
			for _, im := range e.Content {
				if im.Kind != transcript.BlockImage || im.ToolID != b.ToolID {
					continue
				}
				ib, ok, err := encodeImage(im)
				if err != nil {
					return "", data{}, err
				}
				if ok {
					rc = append(rc, ib)
				}
			}
			bd, err := json.Marshal(toolResult{ToolUseID: b.ToolID, Content: rc, Status: status})
			if err != nil {
				return "", data{}, err
			}
			d.Content = append(d.Content, block{Kind: "toolResult", Data: bd})
			out := toolOutcome{Tool: json.RawMessage(`null`)}
			if b.Status == transcript.StatusError {
				out.Result.Error = &toolError{Custom: b.Text}
			} else {
				out.Result.Success = &toolItems{}
				for _, c := range rc {
					item := "Text"
					if c.Kind == "image" {
						item = "Image"
					}
					out.Result.Success.Items = append(out.Result.Success.Items, map[string]json.RawMessage{item: c.Data})
				}
			}
			results[b.ToolID] = out
		case transcript.BlockImage:
			// A tool's image went into its result above. A user message
			// carries an image of its own, a prompt joined to a
			// ToolResults row included, which kiro sends with the row's
			// text; an assistant message has none, and kiro has no block
			// for any other file.
			if b.ToolID != "" || kind == "AssistantMessage" {
				continue
			}
			ib, ok, err := encodeImage(b)
			if err != nil {
				return "", data{}, err
			}
			if ok {
				d.Content = append(d.Content, ib)
			}
		}
	}
	switch kind {
	case "Prompt":
		t := e.Time
		if t.IsZero() {
			t = s.Updated
		}
		d.Meta = &meta{Timestamp: t.Unix()}
	case "ToolResults":
		rj, err := json.Marshal(results)
		if err != nil {
			return "", data{}, err
		}
		d.Results = rj
	}
	return kind, d, nil
}

// writeSidecar writes <id>.json beside the transcript. A session read from
// kiro carries its sidecar in Vendor and gets it back with only the identity
// fields updated. Any other session gets a complete document: kiro rejects a
// sidecar missing any field it deserializes ("failed to parse session
// metadata"), so every field a kiro-written sidecar has is written, with the
// model, agent and creation reason taken from kiro's latest sidecar in the
// same store, which holds values kiro accepts.
func writeSidecar(ctx context.Context, path string, s *transcript.Session) error {
	doc := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Sidecar {
			doc[k] = val
		}
	}
	if _, ok := doc["session_state"]; !ok {
		t := template(filepath.Dir(path), s)
		state, err := json.Marshal(sessionState{
			Version: "v1",
			ConversationMetadata: conversationMetadata{
				UserTurnMetadatas: turns(s, t),
			},
			RTSModelState: rtsModelState{ConversationID: s.ID, ModelInfo: t.ModelInfo},
			Permissions: permissions{
				Filesystem: filesystem{
					AllowedReadPaths: []string{s.CWD}, AllowedWritePaths: []string{},
					DeniedReadPaths: []string{}, DeniedWritePaths: []string{},
				},
				TrustedTools: []string{}, DeniedTools: []string{}, AllowedCommands: []string{},
			},
			AgentName: t.AgentName,
		})
		if err != nil {
			return err
		}
		doc["session_state"] = state
		reason, err := json.Marshal(t.CreatedReason)
		if err != nil {
			return err
		}
		doc["session_created_reason"] = reason
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	if err := merge(doc, sidecar{
		SessionID: s.ID,
		CWD:       s.CWD,
		CreatedAt: s.Created.UTC().Format(time.RFC3339Nano),
		UpdatedAt: s.Updated.UTC().Format(time.RFC3339Nano),
		Title:     title,
	}); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".json", out, 0o644)
}

func merge(dst map[string]json.RawMessage, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for k, val := range fields {
		dst[k] = val
	}
	return nil
}

// sidecarTemplate is what a new sidecar copies from kiro's own: values of
// fields kiro validates that a session from elsewhere has no source for.
type sidecarTemplate struct {
	ModelInfo     modelInfo
	AgentName     string
	CreatedReason string
}

// template reads kiro's most recent sidecar in dir for the model, agent and
// creation reason. With none there, it falls back to kiro's built-in agent
// and the session's own model.
func template(dir string, s *transcript.Session) sidecarTemplate {
	model := s.Model
	if model == "" {
		model = "auto"
	}
	t := sidecarTemplate{
		ModelInfo:     modelInfo{ModelName: model, ModelID: model, ContextWindowTokens: 200000},
		AgentName:     "kiro_default",
		CreatedReason: "subagent",
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var newest string
	var newestTime time.Time
	for _, p := range paths {
		if strings.TrimSuffix(filepath.Base(p), ".json") == s.ID {
			continue
		}
		fi, err := os.Stat(p)
		if err == nil && fi.ModTime().After(newestTime) {
			newest, newestTime = p, fi.ModTime()
		}
	}
	if newest == "" {
		return t
	}
	raw, err := os.ReadFile(newest)
	if err != nil {
		return t
	}
	var doc struct {
		Reason string `json:"session_created_reason"`
		State  struct {
			RTS struct {
				ModelInfo *modelInfo `json:"model_info"`
			} `json:"rts_model_state"`
			AgentName string `json:"agent_name"`
		} `json:"session_state"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return t
	}
	if doc.State.RTS.ModelInfo != nil && doc.State.RTS.ModelInfo.ModelID != "" {
		t.ModelInfo = *doc.State.RTS.ModelInfo
	}
	if doc.State.AgentName != "" {
		t.AgentName = doc.State.AgentName
	}
	if doc.Reason != "" {
		t.CreatedReason = doc.Reason
	}
	return t
}

// The session_state document, with every field a kiro-written sidecar has.
type sessionState struct {
	Version              string               `json:"version"`
	ConversationMetadata conversationMetadata `json:"conversation_metadata"`
	RTSModelState        rtsModelState        `json:"rts_model_state"`
	Permissions          permissions          `json:"permissions"`
	AgentName            string               `json:"agent_name"`
	Goal                 *string              `json:"goal"`
}

type conversationMetadata struct {
	UserTurnMetadatas    []userTurn `json:"user_turn_metadatas"`
	LastContextUsage     *float64   `json:"last_context_usage"`
	UserTurnStartRequest *string    `json:"user_turn_start_request"`
	LastRequest          *string    `json:"last_request"`
}

type userTurn struct {
	LoopID                      loopID      `json:"loop_id"`
	Result                      *turnResult `json:"result,omitempty"`
	MessageIDs                  []string    `json:"message_ids"`
	TotalRequestCount           int         `json:"total_request_count"`
	NumberOfCycles              int         `json:"number_of_cycles"`
	BuiltinToolUses             int         `json:"builtin_tool_uses"`
	TurnDuration                duration    `json:"turn_duration"`
	EndReason                   string      `json:"end_reason"`
	EndTimestamp                string      `json:"end_timestamp"`
	InputTokenCount             int         `json:"input_token_count"`
	OutputTokenCount            int         `json:"output_token_count"`
	CacheReadInputTokenCount    int         `json:"cache_read_input_token_count"`
	CacheWriteInputTokenCount   int         `json:"cache_write_input_token_count"`
	Model                       string      `json:"model"`
	AssistantResponseLength     int         `json:"assistant_response_length"`
	RequestAttempts             int         `json:"request_attempts"`
	ContextUsagePercentage      *float64    `json:"context_usage_percentage"`
	FinalContextUsagePercentage *float64    `json:"final_context_usage_percentage"`
	MeteringUsage               []string    `json:"metering_usage"`
	UserPromptLength            int         `json:"user_prompt_length"`
}

type loopID struct {
	AgentID agentID `json:"agent_id"`
	Rand    uint32  `json:"rand"`
}

type agentID struct {
	Name     string  `json:"name"`
	ParentID *string `json:"parent_id"`
	Rand     *uint32 `json:"rand"`
}

type turnResult struct {
	Ok resultMessage `json:"Ok"`
}

type resultMessage struct {
	ID      string  `json:"id"`
	Role    string  `json:"role"`
	Content []block `json:"content"`
	Meta    meta    `json:"meta"`
}

type duration struct {
	Secs  int64 `json:"secs"`
	Nanos int64 `json:"nanos"`
}

type rtsModelState struct {
	ConversationID         string    `json:"conversation_id"`
	ModelInfo              modelInfo `json:"model_info"`
	ContextUsagePercentage *float64  `json:"context_usage_percentage"`
}

type modelInfo struct {
	ModelName           string `json:"model_name"`
	ModelID             string `json:"model_id"`
	ContextWindowTokens int    `json:"context_window_tokens"`
}

type permissions struct {
	Filesystem      filesystem `json:"filesystem"`
	TrustedTools    []string   `json:"trusted_tools"`
	DeniedTools     []string   `json:"denied_tools"`
	AllowedCommands []string   `json:"allowed_commands"`
}

type filesystem struct {
	AllowedReadPaths  []string `json:"allowed_read_paths"`
	AllowedWritePaths []string `json:"allowed_write_paths"`
	DeniedReadPaths   []string `json:"denied_read_paths"`
	DeniedWritePaths  []string `json:"denied_write_paths"`
}

// turns builds one user-turn record per user message: the ids of the
// messages the turn holds, its last assistant message as the result, and
// the counts kiro records.
func turns(s *transcript.Session, t sidecarTemplate) []userTurn {
	out := []userTurn{}
	var last *transcript.Entry
	msgs := s.Messages()
	for i := range msgs {
		e := msgs[i]
		if e.Role == transcript.RoleUser || len(out) == 0 {
			if len(out) > 0 {
				finish(&out[len(out)-1], last)
			}
			var r [4]byte
			if _, err := rand.Read(r[:]); err != nil {
				panic(err)
			}
			out = append(out, userTurn{
				LoopID:           loopID{AgentID: agentID{Name: "kiro_default"}, Rand: binary.BigEndian.Uint32(r[:])},
				EndReason:        "UserTurnEnd",
				EndTimestamp:     s.Updated.UTC().Format(time.RFC3339Nano),
				Model:            t.ModelInfo.ModelID,
				MeteringUsage:    []string{},
				NumberOfCycles:   1,
				UserPromptLength: len(e.Text()),
			})
			last = nil
		}
		turn := &out[len(out)-1]
		turn.MessageIDs = append(turn.MessageIDs, e.ID)
		if e.Role == transcript.RoleAssistant {
			turn.TotalRequestCount++
			turn.RequestAttempts++
			for _, b := range e.Content {
				if b.Kind == transcript.BlockToolUse {
					turn.BuiltinToolUses++
				}
			}
			last = &msgs[i]
		}
	}
	if len(out) > 0 {
		finish(&out[len(out)-1], last)
	}
	return out
}

// finish records a turn's last assistant message as its result.
func finish(turn *userTurn, last *transcript.Entry) {
	if last == nil {
		return
	}
	content := []block{}
	for _, b := range last.Content {
		if b.Kind == transcript.BlockText {
			d, _ := json.Marshal(b.Text)
			content = append(content, block{Kind: "text", Data: d})
		}
	}
	ts := last.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	turn.Result = &turnResult{Ok: resultMessage{ID: last.ID, Role: "assistant", Content: content, Meta: meta{Timestamp: ts.Unix()}}}
	turn.AssistantResponseLength = len(last.Text())
}

// readSidecar loads a session's sidecar into Vendor so a Write reproduces
// it.
func readSidecar(home string, s *transcript.Session) error {
	raw, err := os.ReadFile(filepath.Join(home, ".kiro", "sessions", "cli", s.ID+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	var sc sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return err
	}
	if s.CWD == "" {
		s.CWD = sc.CWD
	}
	s.Title = sc.Title
	if t, err := time.Parse(time.RFC3339Nano, sc.CreatedAt); err == nil {
		s.Created = t
	}
	if t, err := time.Parse(time.RFC3339Nano, sc.UpdatedAt); err == nil {
		s.Updated = t
	}
	s.Vendor = &Vendor{Sidecar: doc}
	return nil
}
