package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

func init() {
	transcript.Register("opencode", Opencode)
	transcript.Register("kilo", Kilo)
}

// Opencode is the opencode session store, and Kilo the store of Kilo Code,
// which forked opencode's schema. Both keep every session in one database
// with session, message and part tables whose payloads are JSON in a `data`
// column; a session belongs to a project keyed by its worktree directory.
var (
	Opencode transcript.Codec = openCodec{"opencode", ".local/share/opencode/opencode.db"}
	Kilo     transcript.Codec = openCodec{"kilo", ".local/share/kilo/kilo.db"}
)

type openCodec struct{ agent, rel string }

func (c openCodec) Open(home string) (transcript.Store, error) {
	return &openStore{agent: c.agent, path: filepath.Join(home, c.rel)}, nil
}

type openStore struct{ agent, path string }

// openMessage is the message `data` payload. Only the fields the codec reads
// or writes are named; Vendor keeps the rest for a same-agent write.
type openMessage struct {
	Role       string   `json:"role"`
	ParentID   string   `json:"parentID,omitempty"`
	Time       openTime `json:"time"`
	Agent      string   `json:"agent,omitempty"`
	ProviderID string   `json:"providerID,omitempty"`
	ModelID    string   `json:"modelID,omitempty"`
	// Model is the model a prompt was sent with.
	Model json.RawMessage `json:"model,omitempty"`
	// Summary is true on the assistant message a compaction wrote. A user
	// message keeps an object of file diffs under the same key.
	Summary json.RawMessage `json:"summary,omitempty"`
	Finish  string          `json:"finish,omitempty"`
	Error   *openError      `json:"error,omitempty"`
}

type openError struct {
	Name string `json:"name"`
}

type openTime struct {
	Created   int64 `json:"created,omitempty"`
	Completed int64 `json:"completed,omitempty"`
}

// openPart is the part `data` payload.
type openPart struct {
	ID        string          `json:"-"`
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Synthetic bool            `json:"synthetic,omitempty"`
	Ignored   bool            `json:"ignored,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	CallID    string          `json:"callID,omitempty"`
	State     *openToolState  `json:"state,omitempty"`
	// TailStartID is set on a compaction part to the first message the
	// compaction keeps after its summary.
	TailStartID string `json:"tail_start_id,omitempty"`
	openFile
}

// openFile is what a file part holds, and what each of a tool's attachments
// holds: its type, its name, and a URL that is a data: URL once opencode has
// read the file (prompt.ts resolvePart) or the file: URL of a text file or
// directory it inlined as text instead.
type openFile struct {
	Mime     string `json:"mime,omitempty"`
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url,omitempty"`
}

type openToolState struct {
	Status      string          `json:"status"`
	Input       json.RawMessage `json:"input,omitempty"`
	Output      string          `json:"output,omitempty"`
	Error       string          `json:"error,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Attachments []openFile      `json:"attachments,omitempty"`
	Time        struct {
		// Compacted is set when a prune cleared the output for the model.
		Compacted int64 `json:"compacted,omitempty"`
	} `json:"time"`
}

// openRevert is the session's revert column: the message, and optionally
// the part within it, from which the person undid the conversation.
type openRevert struct {
	MessageID string `json:"messageID"`
	PartID    string `json:"partID,omitempty"`
}

// openVendor carries what a write needs from a session read from the store:
// the agent, provider and model its messages ran with, which a message
// appended to it must name too.
type openVendor struct {
	Agent      string
	ProviderID string
	ModelID    string
}

func (st *openStore) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
	if _, err := os.Stat(st.path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()
	cols, err := openColumns(ctx, db, "session")
	if err != nil {
		return nil, fmt.Errorf("opencode: list: %w", err)
	}
	// opencode lists the sessions a person started: a subagent's session
	// has a parent and an archived one a time_archived (session.ts listGlobal
	// with roots, and the archived filter every listing applies).
	var where []string
	args := []any{}
	if cols["parent_id"] {
		where = append(where, "parent_id IS NULL")
	}
	if cols["time_archived"] {
		where = append(where, "time_archived IS NULL")
	}
	if cwd != "" {
		where = append(where, "directory = ?")
		args = append(args, cwd)
	}
	q := "SELECT id, directory, title, time_updated FROM session"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("opencode: list: %w", err)
	}
	defer rows.Close()
	var out []transcript.Info
	for rows.Next() {
		var id, dir, title string
		var updated int64
		if err := rows.Scan(&id, &dir, &title, &updated); err != nil {
			return nil, err
		}
		out = append(out, transcript.Info{ID: id, CWD: dir, Title: title, Updated: time.UnixMilli(updated).UTC(), Path: st.path})
	}
	transcript.SortNewest(out)
	return out, rows.Err()
}

// openRow is one message as read, with its parts.
type openRow struct {
	id    string
	raw   json.RawMessage
	m     openMessage
	parts []openPart
}

func (st *openStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	if _, err := os.Stat(st.path); errors.Is(err, os.ErrNotExist) {
		return nil, transcript.ErrNotFound
	}
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	s := &transcript.Session{ID: id, Agent: st.agent}
	var created, updated int64
	err = db.QueryRowContext(ctx, "SELECT directory, title, time_created, time_updated FROM session WHERE id = ?", id).
		Scan(&s.CWD, &s.Title, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("opencode: read session: %w", err)
	}
	s.Created, s.Updated = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	rev, err := openRevertOf(ctx, db, id)
	if err != nil {
		return nil, err
	}
	vendor := &openVendor{}
	s.Vendor = vendor

	// Parts, grouped by message.
	partRows, err := db.QueryContext(ctx, "SELECT id, message_id, data FROM part WHERE session_id = ? ORDER BY time_created, id", id)
	if err != nil {
		return nil, fmt.Errorf("opencode: read parts: %w", err)
	}
	defer partRows.Close()
	partsByMsg := map[string][]openPart{}
	for partRows.Next() {
		var partID, msgID, data string
		if err := partRows.Scan(&partID, &msgID, &data); err != nil {
			return nil, err
		}
		var p openPart
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, fmt.Errorf("opencode: part: %w", err)
		}
		p.ID = partID
		partsByMsg[msgID] = append(partsByMsg[msgID], p)
	}
	if err := partRows.Err(); err != nil {
		return nil, err
	}

	msgRows, err := db.QueryContext(ctx, "SELECT id, data FROM message WHERE session_id = ? ORDER BY time_created, id", id)
	if err != nil {
		return nil, fmt.Errorf("opencode: read messages: %w", err)
	}
	defer msgRows.Close()
	var msgs []openRow
	for msgRows.Next() {
		var msgID, data string
		if err := msgRows.Scan(&msgID, &data); err != nil {
			return nil, err
		}
		r := openRow{id: msgID, raw: json.RawMessage(data), parts: partsByMsg[msgID]}
		if err := json.Unmarshal([]byte(data), &r.m); err != nil {
			return nil, fmt.Errorf("opencode: message: %w", err)
		}
		if r.m.Agent != "" {
			vendor.Agent = r.m.Agent
		}
		if r.m.Role == "assistant" && r.m.ProviderID != "" {
			vendor.ProviderID, vendor.ModelID = r.m.ProviderID, r.m.ModelID
			s.Model = r.m.ModelID
		}
		msgs = append(msgs, r)
	}
	if err := msgRows.Err(); err != nil {
		return nil, err
	}
	resume, err := openResumeModel(ctx, db, id, msgs, st.agent == "kilo")
	if err != nil {
		return nil, err
	}
	s.Entries = openEntries(msgs, rev, st.agent == "kilo", resume)
	return s, nil
}

// openView is one part as the person and the model each receive it.
type openView struct {
	id          string
	blocks      []transcript.Block
	model, user bool
	// sent is what the model gets of the part when that is not blocks;
	// empty when it gets nothing, nil when it gets blocks.
	sent []transcript.Block
}

// addTo appends the view to an entry: its blocks to what the person sees,
// and what the model gets of it to the entry's ModelContent once any part
// of the entry reaches the model differently.
func (v openView) addTo(e *transcript.Entry) {
	if v.sent != nil && e.ModelContent == nil {
		e.ModelContent = append([]transcript.Block{}, e.Content...)
	}
	if e.ModelContent != nil {
		if v.sent != nil {
			e.ModelContent = append(e.ModelContent, v.sent...)
		} else {
			e.ModelContent = append(e.ModelContent, v.blocks...)
		}
	}
	e.Content = append(e.Content, v.blocks...)
}

// openEntries turns messages into entries the way opencode builds the
// model's context on a prompt: the revert cleanup that runs first
// (revert.ts cleanup), then filterCompacted and toModelMessages
// (message-v2.ts). What the person sees follows the TUI. A message whose
// parts differ in who they are for, such as a prompt with a synthetic
// reminder appended, becomes one entry per run of parts with the same
// audience: the first keeps the message id, the others take the id of
// their first part.
func openEntries(msgs []openRow, rev *openRevert, kilo bool, resume openModelRef) []transcript.Entry {
	views := make([][]openView, len(msgs))
	for i, r := range msgs {
		views[i] = openViews(r, kilo, resume)
	}

	// The TUI hides every message from the revert point on, and the next
	// prompt deletes them before the model sees anything. With a part named,
	// the message holding it keeps its earlier parts.
	live, reverted := len(msgs), len(msgs)
	if rev != nil {
		for i, r := range msgs {
			if r.id != rev.MessageID {
				continue
			}
			live, reverted = i, i
			for j := i; j < len(msgs); j++ {
				for k := range views[j] {
					views[j][k].user = false
					if j > i || rev.PartID == "" {
						views[j][k].model = false
					}
				}
			}
			if rev.PartID == "" {
				break
			}
			live = i + 1
			if !slices.ContainsFunc(r.parts, func(p openPart) bool { return p.ID == rev.PartID }) {
				break
			}
			for k := range views[i] {
				if views[i][k].id >= rev.PartID {
					views[i][k].model = false
				}
			}
			if kilo && r.m.Role == "assistant" {
				// Kilo clears the provider error of the message it keeps
				// part of (kilo revert.ts cleanup), so its parts are sent.
				msgs[i].m.Error = nil
			}
			break
		}
	}

	// An assistant message that failed is not sent, unless it was aborted
	// after producing something (message-v2.ts toModelMessages).
	unsent := make([]bool, len(msgs))
	for i, r := range msgs {
		unsent[i] = i >= live || r.m.Role == "assistant" && r.m.Error != nil && !openAbortedWithContent(r)
		if unsent[i] {
			for k := range views[i] {
				views[i][k].model = false
			}
		}
	}

	compaction, carrier := openCompaction(msgs[:live], views)

	var out []transcript.Entry
	for i, r := range msgs {
		start := len(out)
		for _, v := range views[i] {
			if len(v.blocks) == 0 {
				continue
			}
			if n := len(out); n > start {
				last := &out[n-1]
				if last.Audience == openAudience(v.model, v.user) {
					v.addTo(last)
					continue
				}
			}
			id := r.id
			if len(out) > start {
				id = v.id
			}
			e := openEntry(r, id, v.model, v.user, nil)
			v.addTo(&e)
			out = append(out, e)
		}
		// An answer whose every part the model is given as nothing, such as
		// one that only reasoned, is left out of the request
		// (toModelMessages keeps a message only with a part to send).
		for k := start; k < len(out); k++ {
			if e := &out[k]; e.ModelContent != nil && len(e.ModelContent) == 0 {
				e.Audience = openAudience(false, e.Audience.User())
				e.ModelContent = nil
			}
		}
		if len(out) == start {
			// A message with no part the entry can hold, like an answer that
			// only stepped, is still a message.
			out = append(out, openEntry(r, r.id, !unsent[i], i < reverted, nil))
		}
		if i == carrier {
			out[start].Compaction = compaction
		}
	}
	return out
}

func openEntry(r openRow, id string, model, user bool, blocks []transcript.Block) transcript.Entry {
	// opencode's parentID names the prompt an answer replies to, not a
	// previous row: the session is a list in store order, so it is not an
	// entry link.
	e := transcript.Entry{ID: id, Role: transcript.Role(r.m.Role), Content: blocks, Audience: openAudience(model, user), Raw: r.raw}
	if r.m.Time.Created > 0 {
		e.Time = time.UnixMilli(r.m.Time.Created).UTC()
	}
	return e
}

func openAudience(model, user bool) transcript.Audience {
	switch {
	case model && user:
		return transcript.AudienceAll
	case model:
		return transcript.AudienceModel
	case user:
		return transcript.AudienceUser
	}
	return transcript.AudienceNone
}

func openAbortedWithContent(r openRow) bool {
	if r.m.Error.Name != "MessageAbortedError" {
		return false
	}
	return slices.ContainsFunc(r.parts, func(p openPart) bool { return p.Type != "step-start" && p.Type != "reasoning" })
}

// openCompaction finds the compaction filterCompacted applies: the newest
// compaction prompt whose summary finished without error. It returns what
// that compaction does and the index of the message carrying it, which is
// the summary filterCompacted places right after the prompt; -1 when there
// is none. The model then gets the prompt (as the fixed question opencode
// sends for it) and the summary, the kept tail from tail_start_id up to the
// prompt, and everything after the summary. The prompt shows the person
// only a divider, so it is for nobody outside the summary.
func openCompaction(msgs []openRow, views [][]openView) (*transcript.Compaction, int) {
	completed := map[string]bool{}
	for i := len(msgs) - 1; i >= 0; i-- {
		r := msgs[i]
		if r.m.Role == "assistant" && string(r.m.Summary) == "true" && r.m.Finish != "" && r.m.Error == nil {
			completed[r.m.ParentID] = true
			continue
		}
		if r.m.Role != "user" || !completed[r.id] {
			continue
		}
		k := slices.IndexFunc(r.parts, func(p openPart) bool { return p.Type == "compaction" })
		if k < 0 {
			continue
		}
		c := &transcript.Compaction{}
		if tail := r.parts[k].TailStartID; tail != "" && tail != r.id {
			// A tail that is not an earlier message leaves filterCompacted
			// looking for it through the whole session, which then reaches
			// the model uncut.
			if !slices.ContainsFunc(msgs[:i], func(m openRow) bool { return m.id == tail }) {
				return nil, -1
			}
			c.Keep = tail
		}
		s := -1
		for j := i + 1; j < len(msgs); j++ {
			if m := msgs[j]; m.m.Role == "assistant" && string(m.m.Summary) == "true" && m.m.ParentID == r.id {
				s = j
				break
			}
		}
		if s < 0 {
			return nil, -1
		}
		c.Summary = append(c.Summary, openModelView(msgs[i], views[i])...)
		c.Summary = append(c.Summary, openModelView(msgs[s], views[s])...)
		for k := range views[i] {
			views[i][k].model, views[i][k].user = false, false
		}
		for k := range views[s] {
			views[s][k].model = false
		}
		return c, s
	}
	return nil, -1
}

// openModelView is a message as the model receives it, one entry, for
// everyone when the person is shown it too.
func openModelView(r openRow, views []openView) []transcript.Entry {
	e := openEntry(r, r.id, true, false, nil)
	shown := false
	for _, v := range views {
		if v.model {
			v.addTo(&e)
			shown = shown || v.user && len(v.blocks) > 0
		}
	}
	if len(e.Content) == 0 || e.ModelContent != nil && len(e.ModelContent) == 0 {
		return nil
	}
	e.Audience = openAudience(true, shown)
	e.Raw = nil
	return []transcript.Entry{e}
}

// kiloTransient is the part metadata key Kilo marks UI-only text with.
const kiloTransient = "kilocode.lifecycle"

// openViews maps each part of a message to its blocks and who gets them.
func openViews(r openRow, kilo bool, resume openModelRef) []openView {
	// The TUI shows a prompt's files beneath its typed text, and shows
	// nothing of a prompt without any (TUI UserMessage).
	typed := slices.ContainsFunc(r.parts, func(p openPart) bool { return p.Type == "text" && !p.Synthetic && p.Text != "" })
	var out []openView
	for _, p := range r.parts {
		v := openView{id: p.ID, model: true, user: true}
		text := func(t string) {
			v.blocks = []transcript.Block{{Kind: transcript.BlockText, Text: t}}
		}
		switch r.m.Role {
		case "user":
			switch p.Type {
			case "text":
				// The model is not given ignored text; the TUI does not show
				// synthetic text (message-v2.ts toModelMessages, TUI
				// UserMessage).
				if p.Text != "" {
					text(p.Text)
				}
				v.model, v.user = !p.Ignored, !p.Synthetic
			case "file":
				// A text file or a directory reaches the model as the
				// synthetic text opencode read it into; the part itself is
				// only shown (message-v2.ts toModelMessages).
				v.blocks = []transcript.Block{mediaBlock(p.Mime, p.URL, p.Filename)}
				v.model = p.Mime != "text/plain" && p.Mime != "application/x-directory"
				v.user = typed
			case "compaction":
				text("What did we do so far?")
				v.user = false
			case "subtask":
				text("The following tool was executed by the user")
				v.user = false
			}
		case "assistant":
			switch p.Type {
			case "text":
				if p.Text != "" {
					text(p.Text)
				}
				if kilo && (p.Ignored || openTransient(p)) {
					// Kilo keeps local UI notices out of future prompts
					// (kilo message-v2.ts toModelMessages).
					v.model = false
				}
			case "reasoning":
				v.blocks = []transcript.Block{{Kind: transcript.BlockReasoning, Text: p.Text}}
				v.sent = openReasoningSent(p, r.m, resume)
			case "tool":
				v.blocks = openToolBlocks(p, kilo)
			}
		}
		out = append(out, v)
	}
	return out
}

// openResumeModel is the model opencode resumes a session on over ACP: the
// model the session records, else that of its last prompt that names one,
// else that of its last answer (acp/service.ts restoreSession,
// restoreFromMessages). Kilo records none on the session and starts from
// the messages. Either falls back to the configured default when that
// model is gone; the reader cannot see the configuration and assumes it is
// still there.
func openResumeModel(ctx context.Context, db *sql.DB, id string, msgs []openRow, kilo bool) (openModelRef, error) {
	if !kilo {
		cols, err := openColumns(ctx, db, "session")
		if err != nil {
			return openModelRef{}, err
		}
		if cols["model"] {
			var raw sql.NullString
			if err := db.QueryRowContext(ctx, "SELECT model FROM session WHERE id = ?", id).Scan(&raw); err != nil {
				return openModelRef{}, fmt.Errorf("opencode: read session: %w", err)
			}
			var m struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
			}
			if raw.Valid && json.Unmarshal([]byte(raw.String), &m) == nil && m.ID != "" && m.ProviderID != "" {
				return openModelRef{ProviderID: m.ProviderID, ModelID: m.ID}, nil
			}
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		var m openModelRef
		if msgs[i].m.Role == "user" && json.Unmarshal(msgs[i].m.Model, &m) == nil && m.ProviderID != "" && m.ModelID != "" {
			return m, nil
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i].m; m.ProviderID != "" && m.ModelID != "" {
			return openModelRef{ProviderID: m.ProviderID, ModelID: m.ModelID}, nil
		}
	}
	return openModelRef{}, nil
}

// openReplayHandles names, for the providers whose SDK replays reasoning
// only by what the provider issued with it, the part metadata keys that
// carry it: OpenAI's Responses API an item id or encrypted content, and
// Anthropic and Bedrock a signature or redacted data. Without one the SDK
// skips the part ("Non-OpenAI reasoning parts are not supported",
// "unsupported reasoning metadata"; opencode's transform drops unsigned
// Bedrock reasoning itself). The other SDKs send the text: OpenAI-compatible
// ones as reasoning_content, Google as a thought part.
var openReplayHandles = map[string][]string{
	"openai":         {"itemId", "reasoningEncryptedContent"},
	"azure":          {"itemId", "reasoningEncryptedContent"},
	"anthropic":      {"signature", "redactedData"},
	"amazon-bedrock": {"signature", "redactedContent", "redactedData"},
}

// openReasoningSent is what the model gets of a reasoning part, nil when
// it gets the part as it is (message-v2.ts toModelMessages). An answer
// that ran on another model than the one the session resumes on gives its
// reasoning as plain text, when there is any; on the same model the
// reasoning goes to the provider with the part's metadata, and the
// provider's SDK decides (openReplayHandles).
func openReasoningSent(p openPart, m openMessage, resume openModelRef) []transcript.Block {
	if m.ProviderID != resume.ProviderID || m.ModelID != resume.ModelID {
		if strings.TrimSpace(p.Text) == "" {
			return []transcript.Block{}
		}
		return []transcript.Block{{Kind: transcript.BlockText, Text: p.Text}}
	}
	keys, signed := openReplayHandles[m.ProviderID]
	if !signed {
		if p.Text == "" {
			return []transcript.Block{}
		}
		return nil
	}
	var meta map[string]map[string]json.RawMessage
	_ = json.Unmarshal(p.Metadata, &meta)
	for _, ns := range meta {
		for _, k := range keys {
			if v, ok := ns[k]; ok && string(v) != "null" {
				return nil
			}
		}
	}
	return []transcript.Block{}
}

func openTransient(p openPart) bool {
	var meta map[string]json.RawMessage
	if json.Unmarshal(p.Metadata, &meta) != nil {
		return false
	}
	var v string
	return json.Unmarshal(meta[kiloTransient], &v) == nil && v == "transient"
}

// openToolBlocks splits a tool part into its call and, once there is one,
// its result, as toModelMessages sends them. A call still pending or
// running when the session stopped is answered as interrupted, and one
// interrupted by an abort sends the output it had. The images and files a
// completed call returned follow its result.
func openToolBlocks(p openPart, kilo bool) []transcript.Block {
	use := transcript.Block{Kind: transcript.BlockToolUse, ToolID: p.CallID, Name: p.Tool}
	if p.State == nil {
		return []transcript.Block{use}
	}
	use.Input = p.State.Input
	res := transcript.Block{Kind: transcript.BlockToolResult, ToolID: p.CallID, Name: p.Tool, Status: transcript.StatusOK}
	switch p.State.Status {
	case "completed":
		// A pruned output (state.time.compacted) reaches the model as
		// "[Old tool result content cleared]"; the person still sees it
		// all, and the entry keeps it.
		res.Text = p.State.Output
		return append([]transcript.Block{use, res}, openAttachments(p, kilo)...)
	case "error":
		var meta struct {
			Interrupted bool            `json:"interrupted"`
			Output      json.RawMessage `json:"output"`
		}
		var out string
		if json.Unmarshal(p.State.Metadata, &meta) == nil && meta.Interrupted && json.Unmarshal(meta.Output, &out) == nil {
			res.Text = out
			break
		}
		res.Text, res.Status = p.State.Error, transcript.StatusError
	case "pending", "running":
		res.Text, res.Status = "[Tool execution was interrupted]", transcript.StatusError
	default:
		return []transcript.Block{use}
	}
	return []transcript.Block{use, res}
}

// openAttachments are the images and files a completed tool call returned,
// as the model is given them: only those held as data: URLs, none once a
// prune cleared the output, and in Kilo none from send_file, whose
// attachments are for delivery to a phone (message-v2.ts toModelMessages
// and toModelOutput). The TUI shows no attachment, so one the model is not
// given is for nobody and stays only in the row. A provider that takes no
// media in a tool result gets them in a user message after the call's; the
// block keeps them with the result either way.
func openAttachments(p openPart, kilo bool) []transcript.Block {
	if p.State.Time.Compacted != 0 || kilo && p.Tool == "send_file" {
		return nil
	}
	var out []transcript.Block
	for _, a := range p.State.Attachments {
		if !strings.HasPrefix(a.URL, "data:") || !strings.Contains(a.URL, ",") {
			continue
		}
		b := mediaBlock(a.Mime, a.URL, a.Filename)
		b.ToolID = p.CallID
		out = append(out, b)
	}
	return out
}

// mediaBlock is an image or a file an agent keeps as a URL: the bytes of a
// data: URL, or else the URL as the block's URI. mediaType is the type the
// agent records beside the URL, if any; the data: URL's own names it
// otherwise.
func mediaBlock(mediaType, url, name string) transcript.Block {
	b := transcript.Block{Kind: transcript.BlockFile, MediaType: mediaType, URI: url, Name: name}
	if mt, data, ok := transcript.ParseDataURL(url); ok {
		b.URI, b.Data = "", data
		if b.MediaType == "" {
			b.MediaType = mt
		}
	}
	if strings.HasPrefix(b.MediaType, "image/") {
		b.Kind = transcript.BlockImage
	}
	return b
}

// mediaURL is a block's content as a URL: a data: URL of its bytes, or the
// URI it points at.
func mediaURL(b transcript.Block) string {
	if len(b.Data) == 0 {
		return b.URI
	}
	mt := b.MediaType
	if mt == "" {
		mt = "application/octet-stream"
	}
	return transcript.DataURL(mt, b.Data)
}

// openRevertOf reads a session's revert state, nil when there is none or
// the store predates it.
func openRevertOf(ctx context.Context, q openQuerier, sessionID string) (*openRevert, error) {
	cols, err := openColumns(ctx, q, "session")
	if err != nil {
		return nil, err
	}
	if !cols["revert"] {
		return nil, nil
	}
	var raw sql.NullString
	if err := q.QueryRowContext(ctx, "SELECT revert FROM session WHERE id = ?", sessionID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("opencode: read revert: %w", err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "null" {
		return nil, nil
	}
	var r openRevert
	if err := json.Unmarshal([]byte(raw.String), &r); err != nil {
		return nil, fmt.Errorf("opencode: revert: %w", err)
	}
	if r.MessageID == "" {
		return nil, nil
	}
	return &r, nil
}

type openQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func openColumns(ctx context.Context, q openQuerier, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// Write persists a session without disturbing anything it read. Entries read
// from the store (Raw set) keep their message and part rows exactly; rows of
// entries the session no longer holds are removed; new entries are inserted
// after them as messages opencode accepts, with every field its message and
// part schemas require. New entries appended to a session under a revert
// first remove the reverted messages, as a prompt does. A session opencode
// does not have yet gets a session row, bound to its directory's project.
func (st *openStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.Agent != st.agent {
		s = s.Portable().Lower(openCaps)
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return "", err
	}
	db, err := openRW(st.path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := openSchema(ctx, db); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	ids := openIDs{}
	now := time.Now()
	if s.Created.IsZero() {
		s.Created = now
	}
	exists := false
	if s.ID != "" {
		var one int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM session WHERE id = ?", s.ID).Scan(&one); {
		case err == nil:
			exists = true
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	} else {
		s.ID = ids.next("ses", now, true)
	}
	if !exists {
		if err := insertOpenSession(ctx, tx, s); err != nil {
			return "", err
		}
	}

	// Keep what was read; drop rows for entries the session no longer has.
	keep := map[string]bool{}
	for _, e := range s.Messages() {
		if e.Raw != nil {
			keep[e.ID] = true
		}
	}
	stale, err := openMessageIDs(ctx, tx, s.ID)
	if err != nil {
		return "", err
	}
	for _, id := range stale {
		if keep[id] {
			continue
		}
		for _, q := range []string{"DELETE FROM part WHERE message_id = ?", "DELETE FROM message WHERE id = ?"} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return "", fmt.Errorf("opencode: remove message: %w", err)
			}
		}
	}
	adding := slices.ContainsFunc(s.Messages(), func(e transcript.Entry) bool {
		return e.Raw == nil && (e.Role == transcript.RoleUser || e.Role == transcript.RoleAssistant)
	})
	if adding && exists {
		// A prompt into a session under a revert first deletes what was
		// reverted; messages appended without doing so would sit behind
		// the revert point, hidden, and go with the rest on the next prompt.
		if err := openCleanup(ctx, tx, s.ID, st.agent == "kilo", now); err != nil {
			return "", err
		}
	}
	lastUser, err := openLastUser(ctx, tx, s.ID)
	if err != nil {
		return "", err
	}

	d, err := openDefaults(ctx, tx, s)
	if err != nil {
		return "", err
	}
	results := toolResults(s)
	at := now
	added := false
	kilo := st.agent == "kilo"
	insert := func(data func(ms int64) any, parts func(msgID string, ms int64) []openPartOut) (string, error) {
		at = at.Add(time.Millisecond)
		ms := at.UnixMilli()
		msgID := ids.next("msg", at, false)
		raw, err := json.Marshal(data(ms))
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)",
			msgID, s.ID, ms, ms, string(raw)); err != nil {
			return "", fmt.Errorf("opencode: write message: %w", err)
		}
		for _, part := range parts(msgID, ms) {
			pd, err := json.Marshal(part)
			if err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)",
				ids.next("prt", at, false), msgID, s.ID, ms, ms, string(pd)); err != nil {
				return "", fmt.Errorf("opencode: write part: %w", err)
			}
		}
		added = true
		return msgID, nil
	}
	user := func(ms int64) any {
		return openUser{Role: "user", Time: openTime{Created: ms}, Agent: d.agent, Model: openModelRef{ProviderID: d.provider, ModelID: d.model}}
	}
	answer := func(parent, agent string, summary bool) func(ms int64) any {
		return func(ms int64) any {
			return openAssistant{
				ParentID: parent, Role: "assistant", Mode: agent, Agent: agent, Summary: summary,
				Path: openPath{CWD: s.CWD, Root: s.CWD}, Tokens: openTokens{Cache: openCache{}},
				ModelID: d.model, ProviderID: d.provider, Time: openTime{Created: ms, Completed: ms}, Finish: "stop",
			}
		}
	}
	// held maps each written entry to the message holding it, a tool result
	// to the answer whose tool part holds it, so a compaction can name the
	// first message it keeps.
	held := map[string]string{}
	lastAnswer := ""
	for i, e := range s.Entries {
		if e.Raw != nil {
			continue
		}
		if e.Role == transcript.RoleOpaque && e.Compaction != nil {
			// A compaction is a prompt holding a compaction part and the
			// summary answering it (compaction.ts create, process), placed
			// after the tail it keeps (message-v2.ts filterCompacted).
			tail := openKept(s.Entries[:i], e.Compaction.Keep, held)
			auto := false
			prompt, err := insert(user, func(string, int64) []openPartOut {
				return []openPartOut{{Type: "compaction", Auto: &auto, TailStartID: tail}}
			})
			if err != nil {
				return "", err
			}
			lastUser = prompt
			summary := summaryText(e.Compaction.Summary)
			if _, err := insert(answer(prompt, "compaction", true), func(string, int64) []openPartOut {
				return []openPartOut{{Type: "text", Text: &summary}}
			}); err != nil {
				return "", err
			}
			continue
		}
		var data func(ms int64) any
		// An entry shown and not sent is its text, marked ignored, which
		// the model is not given and the TUI shows (message-v2.ts
		// toModelMessages). Only a prompt can be one in opencode; Kilo
		// also ignores an answer's text.
		shownOnly := e.Audience == transcript.AudienceUser
		if shownOnly && (len(openIgnored(e)) == 0 || e.Role == transcript.RoleAssistant && !kilo) {
			continue
		}
		switch e.Role {
		case transcript.RoleUser:
			data = user
		case transcript.RoleAssistant:
			data = answer(lastUser, d.agent, false)
		case transcript.RoleTool:
			// Tool results fold into the answer's part that called them.
			if e.ID != "" {
				held[e.ID] = lastAnswer
			}
			continue
		default:
			// opencode has no system messages.
			continue
		}
		msgID, err := insert(data, func(msgID string, ms int64) []openPartOut {
			newPart := func() openAttachment {
				return openAttachment{ID: ids.next("prt", at, false), SessionID: s.ID, MessageID: msgID}
			}
			if shownOnly {
				return openIgnored(e)
			}
			return openParts(e, results, ms, newPart)
		})
		if err != nil {
			return "", err
		}
		if e.ID != "" {
			held[e.ID] = msgID
		}
		if e.Role == transcript.RoleUser {
			lastUser = msgID
		} else {
			lastAnswer = msgID
		}
	}
	if added && exists {
		if _, err := tx.ExecContext(ctx, "UPDATE session SET time_updated = ? WHERE id = ?", at.UnixMilli(), s.ID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.ID, nil
}

// openCaps is what opencode and Kilo record of another agent's session: a
// compaction as their own, and a prompt shown and not sent as ignored text.
var openCaps = transcript.Capabilities{Compaction: true, UserOnly: true}

// openKept is the message a compaction's tail starts at: the one holding
// the entry keep names, or the first message written after it. Empty when
// the compaction keeps nothing written.
func openKept(before []transcript.Entry, keep string, held map[string]string) string {
	if keep == "" {
		return ""
	}
	i := slices.IndexFunc(before, func(e transcript.Entry) bool { return e.ID == keep })
	if i < 0 {
		return ""
	}
	for _, e := range before[i:] {
		if id := held[e.ID]; e.ID != "" && id != "" {
			return id
		}
	}
	return ""
}

// openIgnored is a shown-only entry as parts: its text, ignored. Its images
// and files have no ignored form and are left out.
func openIgnored(e transcript.Entry) []openPartOut {
	var parts []openPartOut
	for _, b := range e.Content {
		if b.Kind == transcript.BlockText && b.Text != "" {
			text := b.Text
			parts = append(parts, openPartOut{Type: "text", Text: &text, Ignored: true})
		}
	}
	return parts
}

// The message and part payloads opencode validates on load. The id,
// sessionID and messageID it also requires come from the row's columns.
type openUser struct {
	Role  string       `json:"role"`
	Time  openTime     `json:"time"`
	Agent string       `json:"agent"`
	Model openModelRef `json:"model"`
}

// openModelRef names a model the way a prompt does.
type openModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

type openAssistant struct {
	ParentID   string     `json:"parentID"`
	Role       string     `json:"role"`
	Mode       string     `json:"mode"`
	Agent      string     `json:"agent"`
	Path       openPath   `json:"path"`
	Cost       float64    `json:"cost"`
	Tokens     openTokens `json:"tokens"`
	ModelID    string     `json:"modelID"`
	ProviderID string     `json:"providerID"`
	Time       openTime   `json:"time"`
	Finish     string     `json:"finish,omitempty"`
	// Summary marks the answer a compaction wrote.
	Summary bool `json:"summary,omitempty"`
}

type openPath struct {
	CWD  string `json:"cwd"`
	Root string `json:"root"`
}

type openTokens struct {
	Input     float64   `json:"input"`
	Output    float64   `json:"output"`
	Reasoning float64   `json:"reasoning"`
	Cache     openCache `json:"cache"`
}

type openCache struct {
	Read  float64 `json:"read"`
	Write float64 `json:"write"`
}

type openSpan struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type openPartOut struct {
	Type    string  `json:"type"`
	Text    *string `json:"text,omitempty"`
	Ignored bool    `json:"ignored,omitempty"`
	// Auto and TailStartID are a compaction part's.
	Auto        *bool         `json:"auto,omitempty"`
	TailStartID string        `json:"tail_start_id,omitempty"`
	Time        *openSpan     `json:"time,omitempty"`
	CallID      string        `json:"callID,omitempty"`
	Tool        string        `json:"tool,omitempty"`
	State       *openStateOut `json:"state,omitempty"`
	openFile
}

type openStateOut struct {
	Status      string           `json:"status"`
	Input       json.RawMessage  `json:"input"`
	Output      *string          `json:"output,omitempty"`
	Error       string           `json:"error,omitempty"`
	Title       *string          `json:"title,omitempty"`
	Metadata    json.RawMessage  `json:"metadata,omitempty"`
	Time        openSpan         `json:"time"`
	Attachments []openAttachment `json:"attachments,omitempty"`
}

// openAttachment is a file part a tool returned, which names its own part
// id, session and message like a part row does (tools.ts).
type openAttachment struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	openFile
}

// openDefaultsT is who new messages say ran them.
type openDefaultsT struct{ agent, provider, model string }

// openDefaults picks the agent, provider and model new messages name: those
// of the session being appended to, else those of the store's latest
// assistant message, else the session's Model split as provider/model.
func openDefaults(ctx context.Context, tx *sql.Tx, s *transcript.Session) (openDefaultsT, error) {
	d := openDefaultsT{agent: "build"}
	if v, ok := s.Vendor.(*openVendor); ok && v.ProviderID != "" {
		if v.Agent != "" {
			d.agent = v.Agent
		}
		d.provider, d.model = v.ProviderID, v.ModelID
		return d, nil
	}
	var data string
	err := tx.QueryRowContext(ctx, `SELECT data FROM message WHERE json_extract(data, '$.role') = 'assistant' ORDER BY time_created DESC LIMIT 1`).Scan(&data)
	if err == nil {
		var m openMessage
		if json.Unmarshal([]byte(data), &m) == nil && m.ProviderID != "" {
			if m.Agent != "" {
				d.agent = m.Agent
			}
			d.provider, d.model = m.ProviderID, m.ModelID
			return d, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	d.model = s.Model
	if i := strings.Index(s.Model, "/"); i > 0 {
		d.provider = s.Model[:i]
	}
	return d, nil
}

// openCleanup does what opencode does to a session under a revert before a
// prompt (revert.ts cleanup): it deletes the messages from the revert point
// on, or with a part named, the messages after the one holding it and that
// message's parts from the named one on, and clears the revert. Kilo also
// clears the provider error of an assistant message it kept part of.
func openCleanup(ctx context.Context, tx *sql.Tx, sessionID string, kilo bool, now time.Time) error {
	rev, err := openRevertOf(ctx, tx, sessionID)
	if err != nil || rev == nil {
		return err
	}
	msgs, err := openMessageIDs(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if i := slices.Index(msgs, rev.MessageID); i >= 0 {
		from := i
		if rev.PartID != "" {
			from++
		}
		for _, id := range msgs[from:] {
			for _, q := range []string{"DELETE FROM part WHERE message_id = ?", "DELETE FROM message WHERE id = ?"} {
				if _, err := tx.ExecContext(ctx, q, id); err != nil {
					return fmt.Errorf("opencode: remove reverted message: %w", err)
				}
			}
		}
		if rev.PartID != "" {
			res, err := tx.ExecContext(ctx, "DELETE FROM part WHERE message_id = ? AND id >= ? AND EXISTS (SELECT 1 FROM part WHERE message_id = ? AND id = ?)",
				rev.MessageID, rev.PartID, rev.MessageID, rev.PartID)
			if err != nil {
				return fmt.Errorf("opencode: remove reverted parts: %w", err)
			}
			if n, _ := res.RowsAffected(); n > 0 && kilo {
				if _, err := tx.ExecContext(ctx,
					"UPDATE message SET data = json_remove(data, '$.error'), time_updated = ? WHERE id = ? AND json_extract(data, '$.role') = 'assistant' AND json_type(data, '$.error') IS NOT NULL",
					now.UnixMilli(), rev.MessageID); err != nil {
					return fmt.Errorf("opencode: clear reverted error: %w", err)
				}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE session SET revert = NULL, time_updated = ? WHERE id = ?", now.UnixMilli(), sessionID); err != nil {
		return fmt.Errorf("opencode: clear revert: %w", err)
	}
	return nil
}

// openLastUser is the session's newest user message, which an appended
// assistant message answers.
func openLastUser(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM message WHERE session_id = ? AND json_extract(data, '$.role') = 'user' ORDER BY time_created DESC, id DESC LIMIT 1",
		sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// openMessageIDs lists a session's messages in the order opencode loads
// them.
func openMessageIDs(ctx context.Context, tx *sql.Tx, sessionID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM message WHERE session_id = ? ORDER BY time_created, id", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// insertOpenSession adds a session row for a session opencode does not have,
// bound to the project of its directory the way opencode binds its own.
func insertOpenSession(ctx context.Context, tx *sql.Tx, s *transcript.Session) error {
	created := s.Created.UnixMilli()
	updated := created
	if !s.Updated.IsZero() {
		updated = s.Updated.UnixMilli()
	}
	projectID, err := ensureProject(ctx, tx, s.CWD, created)
	if err != nil {
		return err
	}
	version := "0.0.0"
	if err := tx.QueryRowContext(ctx, "SELECT version FROM session ORDER BY time_updated DESC LIMIT 1").Scan(&version); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	title := s.Title
	if title == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			title = msgs[0].Text()
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, projectID, "imported-session", s.CWD, title, version, created, updated); err != nil {
		return fmt.Errorf("opencode: write session: %w", err)
	}
	return nil
}

// ensureProject returns the project bound to a directory, creating the
// project and the binding when opencode has none. opencode resolves a
// directory to its project through project_directory where that table
// exists, and through the project's worktree otherwise.
func ensureProject(ctx context.Context, tx *sql.Tx, dir string, at int64) (string, error) {
	hasDirs, err := tableExists(ctx, tx, "project_directory")
	if err != nil {
		return "", err
	}
	var id string
	if hasDirs {
		err = tx.QueryRowContext(ctx, "SELECT project_id FROM project_directory WHERE directory = ?", dir).Scan(&id)
	} else {
		err = tx.QueryRowContext(ctx, "SELECT id FROM project WHERE worktree = ?", dir).Scan(&id)
	}
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, "SELECT id FROM project WHERE worktree = ?", dir).Scan(&id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		id = transcript.NewUUID()
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO project (id, worktree, vcs, time_created, time_updated, sandboxes) VALUES (?, ?, 'git', ?, ?, '[]')",
			id, dir, at, at); err != nil {
			return "", fmt.Errorf("opencode: write project: %w", err)
		}
	}
	if hasDirs {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO project_directory (project_id, directory, time_created) VALUES (?, ?, ?)",
			id, dir, at); err != nil {
			return "", fmt.Errorf("opencode: bind directory: %w", err)
		}
	}
	return id, nil
}

func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n)
	return n > 0, err
}

// toolResult is a tool call's outcome, collected across a whole session so a
// call and its result fold into one opencode part even when the normalized
// model keeps them in separate entries.
type toolResult struct {
	text  string
	isErr bool
	// media are the images and files the call returned.
	media []transcript.Block
}

func toolResults(s *transcript.Session) map[string]toolResult {
	out := map[string]toolResult{}
	for _, e := range s.Messages() {
		for _, b := range e.Content {
			switch {
			case b.Kind == transcript.BlockToolResult:
				r := out[b.ToolID]
				r.text, r.isErr = b.Text, b.Status == transcript.StatusError
				out[b.ToolID] = r
			case isMedia(b) && b.ToolID != "":
				r := out[b.ToolID]
				r.media = append(r.media, b)
				out[b.ToolID] = r
			}
		}
	}
	return out
}

func isMedia(b transcript.Block) bool {
	return b.Kind == transcript.BlockImage || b.Kind == transcript.BlockFile
}

// openParts turns an entry's blocks into the parts opencode stores, each
// with the fields its part schema requires. A tool call and its result fold
// into one tool part, with the images and files the call returned as its
// attachments; newPart names each attachment's part. A prompt's image or
// file becomes a file part. opencode sends the model no file part of an
// answer, so an answer's own image or file is dropped.
func openParts(e transcript.Entry, results map[string]toolResult, ms int64, newPart func() openAttachment) []openPartOut {
	var parts []openPartOut
	span := openSpan{Start: ms, End: ms}
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			text := b.Text
			parts = append(parts, openPartOut{Type: "text", Text: &text})
		case transcript.BlockImage, transcript.BlockFile:
			if e.Role == transcript.RoleUser && b.ToolID == "" {
				parts = append(parts, openPartOut{Type: "file", openFile: openFileOf(b)})
			}
		case transcript.BlockReasoning:
			text := b.Text
			parts = append(parts, openPartOut{Type: "reasoning", Text: &text, Time: &span})
		case transcript.BlockToolUse:
			input := b.Input
			if !isObject(input) {
				wrapped, _ := json.Marshal(map[string]json.RawMessage{"input": orNull(input)})
				input = wrapped
			}
			st := &openStateOut{Status: "completed", Input: input, Metadata: json.RawMessage(`{}`), Time: span}
			r, ok := results[b.ToolID]
			switch {
			case ok && r.isErr:
				st.Status, st.Error, st.Metadata = "error", r.text, nil
			default:
				out, title := r.text, b.Name
				st.Output, st.Title = &out, &title
				for _, m := range r.media {
					a := newPart()
					a.Type, a.openFile = "file", openFileOf(m)
					st.Attachments = append(st.Attachments, a)
				}
			}
			parts = append(parts, openPartOut{Type: "tool", CallID: b.ToolID, Tool: b.Name, State: st})
		}
	}
	return parts
}

// openFileOf is an image or file block as a file part holds it. opencode
// sends the model a file part's URL as it is and reads bytes only from a
// data: URL, so a block with its bytes becomes one; a path it points at
// becomes a file: URL, the form opencode keeps a local file in.
func openFileOf(b transcript.Block) openFile {
	f := openFile{Mime: b.MediaType, Filename: b.Name, URL: mediaURL(b)}
	if f.Mime == "" {
		f.Mime = "application/octet-stream"
	}
	if filepath.IsAbs(f.URL) {
		f.URL = (&neturl.URL{Scheme: "file", Path: f.URL}).String()
	}
	return f
}

func isObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &m) == nil && m != nil
}

func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// openIDs mints ids the way opencode's Identifier does: a prefix, twelve hex
// digits of the millisecond time with a counter in the low bits (inverted for
// ids that sort newest first, as session ids do), and fourteen base62
// characters.
type openIDs struct{ counter uint64 }

func (g *openIDs) next(prefix string, at time.Time, descending bool) string {
	g.counter++
	v := (uint64(at.UnixMilli())<<12 | (g.counter & 0xfff)) & 0xffffffffffff
	if descending {
		v = ^v & 0xffffffffffff
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var rnd [14]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic(err)
	}
	for i := range rnd {
		rnd[i] = alphabet[int(rnd[i])%len(alphabet)]
	}
	return fmt.Sprintf("%s_%012x%s", prefix, v, rnd[:])
}

func openSchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS project (
  id TEXT PRIMARY KEY, worktree TEXT NOT NULL, vcs TEXT, name TEXT,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, sandboxes TEXT NOT NULL DEFAULT '[]');
CREATE TABLE IF NOT EXISTS project_directory (
  project_id TEXT NOT NULL, directory TEXT NOT NULL, type TEXT, strategy TEXT, time_created INTEGER NOT NULL,
  PRIMARY KEY (project_id, directory));
CREATE TABLE IF NOT EXISTS session (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, workspace_id TEXT, parent_id TEXT,
  slug TEXT NOT NULL, directory TEXT NOT NULL, path TEXT, title TEXT NOT NULL, version TEXT NOT NULL,
  cost REAL DEFAULT 0 NOT NULL, tokens_input INTEGER DEFAULT 0 NOT NULL, tokens_output INTEGER DEFAULT 0 NOT NULL,
  tokens_reasoning INTEGER DEFAULT 0 NOT NULL, tokens_cache_read INTEGER DEFAULT 0 NOT NULL,
  tokens_cache_write INTEGER DEFAULT 0 NOT NULL, agent TEXT, model TEXT,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS message (
  id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS part (
  id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);`
	_, err := db.ExecContext(ctx, ddl)
	return err
}
