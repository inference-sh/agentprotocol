package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

func init() { transcript.Register("hermes", Hermes) }

// Hermes is the hermes session store. hermes keeps every session in
// ~/.hermes/state.db, with a sessions table and a messages table whose tool
// calls and reasoning are separate columns.
var Hermes transcript.Codec = hermesCodec{}

const hermesPath = ".hermes/state.db"

type hermesCodec struct{}

func (hermesCodec) Open(home string) (transcript.Store, error) {
	return &hermesStore{path: filepath.Join(home, hermesPath)}, nil
}

type hermesStore struct{ path string }

type hermesToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// hermesSession is one sessions row as List needs it: enough to tell which
// sessions hermes offers to resume and which one a compression chain resumes
// at.
type hermesSession struct {
	id, parent, endReason, source, cwd, title, updated string
	started, ended                                     sql.NullFloat64
	branched, delegated, reset                         bool
}

// List returns the sessions hermes offers to resume. hermes hides subagent
// runs and the continuation a legacy compression rotated into (a child of a
// session that ended with end_reason 'compression'), and shows each
// compression chain once, under its live tip's id and title
// (hermes_state_sessions.py list_sessions_rich, _project_compression_tips).
func (st *hermesStore) List(ctx context.Context, cwd string) ([]transcript.Info, error) {
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
	// hermes often leaves the cwd column empty and records the directory in
	// model_config instead.
	cwdExpr, err := hermesCWD(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("hermes: list: %w", err)
	}
	cols, err := columns(ctx, db, "sessions")
	if err != nil {
		return nil, fmt.Errorf("hermes: list: %w", err)
	}
	// The oldest stores have no lineage columns; every session there is a
	// root.
	lineage := "'', '', 0, 0, 0"
	if cols["parent_session_id"] && cols["end_reason"] {
		lineage = "COALESCE(parent_session_id, ''), COALESCE(end_reason, '')"
		for _, marker := range []string{"_branched_from", "_delegate_from", "_reset_from"} {
			if cols["model_config"] {
				lineage += ", json_extract(model_config, '$." + marker + "') IS NOT NULL"
			} else {
				lineage += ", 0"
			}
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT id, "+cwdExpr+", COALESCE(title, ''), COALESCE(ended_at, started_at, ''), COALESCE(source, ''), started_at, ended_at, "+lineage+" FROM sessions")
	if err != nil {
		return nil, fmt.Errorf("hermes: list: %w", err)
	}
	defer rows.Close()
	var all []*hermesSession
	byID := map[string]*hermesSession{}
	for rows.Next() {
		h := &hermesSession{}
		if err := rows.Scan(&h.id, &h.cwd, &h.title, &h.updated, &h.source, &h.started, &h.ended, &h.parent, &h.endReason, &h.branched, &h.delegated, &h.reset); err != nil {
			return nil, err
		}
		all = append(all, h)
		byID[h.id] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	children := map[string][]*hermesSession{}
	for _, h := range all {
		if h.parent != "" {
			children[h.parent] = append(children[h.parent], h)
		}
	}
	var out []transcript.Info
	for _, h := range all {
		if !h.listable(byID) {
			continue
		}
		info := transcript.Info{ID: h.id, CWD: h.cwd, Title: h.title, Updated: parseTime(h.updated), Path: st.path}
		if tip := h.compressionTip(children); tip != h {
			info.ID, info.CWD, info.Updated = tip.id, tip.cwd, parseTime(tip.updated)
			if tip.title != "" {
				info.Title = tip.title
			}
		}
		if cwd != "" && info.CWD != cwd {
			continue
		}
		out = append(out, info)
	}
	transcript.SortNewest(out)
	return out, nil
}

// listable reports whether hermes's session picker shows a session: a root,
// or a branch or reset child, and never a subagent run
// (hermes_state_common.py _LISTABLE_CHILD_SQL).
func (h *hermesSession) listable(byID map[string]*hermesSession) bool {
	if h.delegated {
		return false
	}
	if h.parent == "" || h.branched || h.reset {
		return true
	}
	p, ok := byID[h.parent]
	return ok && p.endReason == "branched" && h.started.Float64 >= p.ended.Float64
}

// compressionTip follows a chain of legacy compression rotations to the
// session hermes resumes, preferring a child that rotated again, then one
// still open, then the most recently started (hermes_state_compression.py
// _CHAIN_STEP_SQL, less its last-activity tiebreak).
func (h *hermesSession) compressionTip(children map[string][]*hermesSession) *hermesSession {
	seen := map[string]bool{h.id: true}
	cur := h
	for cur.endReason == "compression" {
		var next *hermesSession
		rank := func(c *hermesSession) int {
			switch {
			case c.endReason == "compression":
				return 0
			case !c.ended.Valid:
				return 1
			}
			return 2
		}
		for _, c := range children[cur.id] {
			if c.branched || c.delegated || c.reset || c.source == "tool" || seen[c.id] {
				continue
			}
			if next == nil || rank(c) < rank(next) ||
				rank(c) == rank(next) && (c.started.Float64 > next.started.Float64 ||
					c.started.Float64 == next.started.Float64 && c.id > next.id) {
				next = c
			}
		}
		if next == nil {
			break
		}
		seen[next.id] = true
		cur = next
	}
	return cur
}

// hermesRow is one messages row with the columns that decide who it is for.
// Stores from before a column existed read it as its default.
type hermesRow struct {
	id                    int64
	role, content, toolID string
	toolName, reasoning   string
	toolCalls             sql.NullString
	// reasoningContent is what hermes replays as the provider's
	// reasoning_content, when it pinned one on the row.
	reasoningContent             sql.NullString
	ts                           sql.NullFloat64
	active, compacted, summary   bool
	displayKind, displayMetadata string
	// apiContent is the text hermes sent the model for the row, when it
	// differs from what was stored for the person: hook and plugin context
	// appended to a prompt, or a sanitized reply.
	apiContent string
}

// hermesMessageColumns selects a messages row, naming a default for each
// column an older store does not have.
func hermesMessageColumns(cols map[string]bool) string {
	col := func(name, expr, missing string) string {
		if cols[name] {
			return expr
		}
		return missing
	}
	return strings.Join([]string{
		"id", "role", "COALESCE(content, '')", "COALESCE(tool_call_id, '')", "COALESCE(tool_name, '')",
		"tool_calls", "COALESCE(reasoning, '')", col("reasoning_content", "reasoning_content", "NULL"), "timestamp",
		col("active", "COALESCE(active, 1) != 0", "1"),
		col("compacted", "COALESCE(compacted, 0) != 0", "0"),
		col("_compressed_summary", "COALESCE(_compressed_summary, 0) != 0", "0"),
		col("display_kind", "COALESCE(display_kind, '')", "''"),
		col("display_metadata", "COALESCE(display_metadata, '')", "''"),
		col("api_content", "COALESCE(api_content, '')", "''"),
	}, ", ")
}

// Read loads a session the way hermes does: rows in id order, since
// timestamps are not monotonic. Every row is kept, including the ones hermes
// no longer gives the model; hermesAudience decides who each one is for.
func (st *hermesStore) Read(ctx context.Context, id string) (*transcript.Session, error) {
	if _, err := os.Stat(st.path); errors.Is(err, os.ErrNotExist) {
		return nil, transcript.ErrNotFound
	}
	db, done, err := openRO(st.path)
	if err != nil {
		return nil, err
	}
	defer done()
	defer db.Close()

	s := &transcript.Session{ID: id, Agent: "hermes"}
	var cwd, title, start, updated, source, model sql.NullString
	cwdExpr, err := hermesCWD(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("hermes: read session: %w", err)
	}
	err = db.QueryRowContext(ctx, "SELECT "+cwdExpr+", title, started_at, ended_at, source, model FROM sessions WHERE id = ?", id).
		Scan(&cwd, &title, &start, &updated, &source, &model)
	if err == sql.ErrNoRows {
		return nil, transcript.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hermes: read session: %w", err)
	}
	s.CWD, s.Title, s.Model = cwd.String, title.String, model.String
	s.Created, s.Updated = parseTime(start.String), parseTime(updated.String)
	if s.Updated.IsZero() {
		s.Updated = s.Created
	}
	s.Vendor = &hermesVendor{Source: source.String}

	echo, err := hermesEchoesReasoning(ctx, db, id, s.Model)
	if err != nil {
		return nil, fmt.Errorf("hermes: read session: %w", err)
	}
	cols, err := columns(ctx, db, "messages")
	if err != nil {
		return nil, fmt.Errorf("hermes: read messages: %w", err)
	}
	rows, err := db.QueryContext(ctx, "SELECT "+hermesMessageColumns(cols)+" FROM messages WHERE session_id = ? ORDER BY id", id)
	if err != nil {
		return nil, fmt.Errorf("hermes: read messages: %w", err)
	}
	defer rows.Close()
	var hrows []hermesRow
	for rows.Next() {
		var r hermesRow
		if err := rows.Scan(&r.id, &r.role, &r.content, &r.toolID, &r.toolName, &r.toolCalls, &r.reasoning, &r.reasoningContent, &r.ts,
			&r.active, &r.compacted, &r.summary, &r.displayKind, &r.displayMetadata, &r.apiContent); err != nil {
			return nil, err
		}
		e, err := hermesEntry(r.role, r.content, r.toolID, r.toolName, r.toolCalls.String, r.reasoning)
		if err != nil {
			return nil, err
		}
		e.ModelContent = hermesAPIContent(r, e.Content)
		if r.role == "assistant" {
			e.ModelContent = hermesReasoningSent(r, e, echo)
		}
		// Raw marks the entry as read; a rewrite keeps its row as is.
		e.ID = "hermes-row:" + strconv.FormatInt(r.id, 10)
		e.Raw = json.RawMessage(strconv.Quote(r.content))
		if r.ts.Valid {
			e.Time = time.Unix(int64(r.ts.Float64), 0).UTC()
		}
		s.Entries = append(s.Entries, e)
		hrows = append(hrows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hermesAudience(s, hrows)
	return s, nil
}

// hermesAPIContent is what hermes gives the model for a user or assistant
// row that carries api_content: those bytes in place of its text, "replay
// the exact bytes sent live" (agent/turn_context.py), with its tool calls
// kept. Nil when the row has none, or they match what was stored.
func hermesAPIContent(r hermesRow, content []transcript.Block) []transcript.Block {
	if r.apiContent == "" || r.apiContent == r.content || (r.role != "user" && r.role != "assistant") {
		return nil
	}
	out := []transcript.Block{{Kind: transcript.BlockText, Text: hermesText(r.apiContent)}}
	for _, b := range content {
		if b.Kind != transcript.BlockText {
			out = append(out, b)
		}
	}
	return out
}

// hermesEchoesReasoning reports whether the provider a resumed session runs
// on is one hermes replays reasoning to. The ACP adapter resumes on the
// provider and base URL in model_config, else the billing columns, and the
// session's model (acp_adapter/session.py). A session that names no
// provider runs on the configured one; its model.reasoning_echo opt-in in
// config.yaml is not consulted.
func hermesEchoesReasoning(ctx context.Context, q *sql.DB, id, model string) (bool, error) {
	cols, err := columns(ctx, q, "sessions")
	if err != nil {
		return false, err
	}
	pick := func(key, column string) string {
		var parts []string
		if cols["model_config"] {
			parts = append(parts, "NULLIF(json_extract(model_config, '$."+key+"'), '')")
		}
		if cols[column] {
			parts = append(parts, "NULLIF("+column+", '')")
		}
		return "COALESCE(" + strings.Join(append(parts, "''"), ", ") + ")"
	}
	var provider, baseURL string
	err = q.QueryRowContext(ctx, "SELECT "+pick("provider", "billing_provider")+", "+pick("base_url", "billing_base_url")+" FROM sessions WHERE id = ?", id).
		Scan(&provider, &baseURL)
	if err != nil {
		return false, err
	}
	return hermesEchoFamily(provider, model, baseURL), nil
}

// hermesEchoFamily is hermes's table of the providers that reject a replayed
// assistant turn without its reasoning_content: Kimi by provider id or host
// only (aggregators re-exporting its models reject the field), DeepSeek and
// MiMo by provider, model name or host (agent/message_sanitization.py
// _REASONING_ECHO_RULES). Every other provider rejects the field, and
// hermes strips it.
func hermesEchoFamily(provider, model, baseURL string) bool {
	host := ""
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		host = u.Hostname()
	} else if u, err := url.Parse("//" + baseURL); err == nil {
		host = u.Hostname()
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	onHost := func(domains ...string) bool {
		for _, d := range domains {
			if host != "" && (host == d || strings.HasSuffix(host, "."+d)) {
				return true
			}
		}
		return false
	}
	lower, model := strings.ToLower(provider), strings.ToLower(model)
	switch {
	case provider == "kimi-coding" || provider == "kimi-coding-cn" || onHost("api.kimi.com", "moonshot.ai", "moonshot.cn"):
		return true
	case lower == "deepseek" || strings.Contains(model, "deepseek") || onHost("api.deepseek.com"):
		return true
	case lower == "xiaomi" || strings.Contains(model, "mimo") || onHost("api.xiaomimimo.com", "xiaomimimo.com"):
		return true
	}
	return false
}

// hermesReasoningSent is an assistant row's content as the model is given
// it, with the reasoning hermes replays in place of the reasoning it shows
// (agent/message_sanitization.py apply_reasoning_content_policy). Without
// echo the model gets none. With echo it gets the row's reasoning_content
// verbatim, else the reasoning of a turn without tool calls; a turn with
// tool calls and no reasoning_content gets a one-space pad instead, so
// another provider's reasoning is not leaked, and a pad carries nothing.
// Nil when that is what the entry already says.
func hermesReasoningSent(r hermesRow, e transcript.Entry, echo bool) []transcript.Block {
	base := e.ModelContent
	if base == nil {
		base = e.Content
	}
	sent := ""
	if echo {
		calls := slices.ContainsFunc(e.Content, func(b transcript.Block) bool { return b.Kind == transcript.BlockToolUse })
		switch {
		case r.reasoningContent.Valid:
			sent = r.reasoningContent.String
		case !calls:
			sent = r.reasoning
		}
		if strings.TrimSpace(sent) == "" {
			sent = ""
		}
	}
	if sent == r.reasoning {
		return e.ModelContent
	}
	var out []transcript.Block
	if sent != "" {
		out = append(out, transcript.Block{Kind: transcript.BlockReasoning, Text: sent})
	}
	for _, b := range base {
		if b.Kind != transcript.BlockReasoning {
			out = append(out, b)
		}
	}
	if out == nil {
		out = []transcript.Block{}
	}
	return out
}

// hermesAudience sets who each row is for, from the flags hermes keeps on it.
//
// The model is given the active rows (hermes_state_messages.py get_messages,
// "AND active = 1"). A compaction archives the rows it summarized (active 0,
// compacted 1), and a rewind, an undo or a compaction's copy of its tail
// supersedes rows with active 0, compacted 0. The person is shown the display
// history (_display_rows_from_conn, _dedupe_display_generations): active and
// compaction-archived rows, not model-only ones, each logical message once at
// the place its first copy holds, and then the TUI's projection
// (tui_gateway/session_history.py _history_to_messages): a compaction summary
// shows only the prior-tail content merged into it, a display_kind "hidden"
// row and a "[System:" notice not at all.
//
// A summary hermes still gives the model carries a Compaction whose Summary
// is what the model has at that point, the summary last, so the summary
// travels with the conversation while the row itself shows what the person
// sees.
func hermesAudience(s *transcript.Session, rows []hermesRow) {
	firstOfKey := map[string]bool{}
	// copies are live rows the person sees through an earlier copy of the
	// same message: the head a compaction keeps is re-inserted before its
	// summary while the archived original holds the display place.
	copies := map[string]bool{}
	for i, r := range rows {
		e := &s.Entries[i]
		if e.Role == transcript.RoleOpaque {
			continue
		}
		isSummary := r.summary || hermesSummaryKind(r.content) != ""
		shown := false
		if (r.active || r.compacted) && !hermesModelOnly(r.displayMetadata) {
			key := strings.Join([]string{r.role, r.content, strconv.FormatFloat(r.ts.Float64, 'g', -1, 64), r.toolID, r.toolCalls.String, r.toolName}, "\x00")
			if !firstOfKey[key] {
				firstOfKey[key] = true
				shown = true
			} else if r.active {
				copies[e.ID] = true
			}
		}
		full := *e
		switch {
		case isSummary:
			prior := hermesSummaryPrior(r.content)
			shown = shown && prior != ""
			if prior != "" {
				e.Content = []transcript.Block{{Kind: transcript.BlockText, Text: prior}}
			}
		case r.displayKind == "hidden":
			shown = false
		case e.Role == transcript.RoleUser && strings.HasPrefix(strings.TrimLeft(e.Text(), " \t\r\n"), "[System:"):
			shown = false
		}
		model := r.active
		if isSummary && r.active {
			full.Audience, full.Compaction, full.Raw = transcript.AudienceAll, nil, nil
			before := (&transcript.Session{Entries: s.Entries[:i]}).Context()
			stored := map[string][]transcript.Block{}
			for _, x := range s.Entries[:i] {
				stored[x.ID] = x.Content
			}
			for j := range before {
				if copies[before[j].ID] {
					before[j].Audience = transcript.AudienceAll
				}
				// Context gives the bytes hermes sent; the summary keeps
				// what was stored, with those bytes beside it, so another
				// agent gets the prompt and not hermes' hook output.
				if c, ok := stored[before[j].ID]; ok && before[j].ModelContent != nil {
					before[j].Content = c
				}
			}
			e.Compaction = &transcript.Compaction{Summary: append(before, full)}
			model = !shown
		}
		e.Audience = audienceOf(shown, model)
	}
}

// audienceOf maps whether an entry is shown and whether the model is given it
// to an Audience.
func audienceOf(user, model bool) transcript.Audience {
	switch {
	case user && model:
		return transcript.AudienceAll
	case user:
		return transcript.AudienceUser
	case model:
		return transcript.AudienceModel
	}
	return transcript.AudienceNone
}

// hermesModelOnly reports a row the model reads but no display shows
// (agent/context_compressor.py MODEL_ONLY_DISPLAY_METADATA_KEY).
func hermesModelOnly(meta string) bool {
	if meta == "" {
		return false
	}
	var m map[string]any
	if json.Unmarshal([]byte(meta), &m) != nil {
		// Rows from before the write guard hold the object encoded twice.
		var inner string
		if json.Unmarshal([]byte(meta), &inner) != nil || json.Unmarshal([]byte(inner), &m) != nil {
			return false
		}
	}
	switch v := m["model_only"].(type) {
	case bool:
		return v
	case float64:
		return v != 0
	}
	return false
}

// The markers hermes frames a compaction summary with
// (agent/context_compressor.py). Every summary prefix hermes has shipped
// starts with one of the first two.
const (
	hermesSummaryPrefix       = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted"
	hermesLegacySummaryPrefix = "[CONTEXT SUMMARY]:"
	hermesSummaryEnd          = "--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
	hermesPriorHeader         = "[PRIOR CONTEXT — for reference only; not a new message]"
	hermesPriorDelimiter      = "[END OF PRIOR CONTEXT — COMPACTION SUMMARY BELOW]"
)

// hermesSummaryKind classifies content as a standalone summary, a summary
// merged into a carried message, or neither (classify_summary_content). The
// flag column is recent; hermes itself falls back to the content.
func hermesSummaryKind(content string) string {
	starts := func(s string) bool {
		return strings.HasPrefix(s, hermesSummaryPrefix) || strings.HasPrefix(s, hermesLegacySummaryPrefix)
	}
	text := strings.TrimLeft(hermesText(content), " \t\r\n")
	if _, after, ok := strings.Cut(text, hermesPriorDelimiter); ok {
		if starts(strings.TrimLeft(after, " \t\r\n")) {
			return "merged"
		}
		return ""
	}
	if starts(text) {
		return "standalone"
	}
	return ""
}

// hermesSummaryPrior is the part of a summary row the person is shown: the
// carried message merged in before the summary, or whatever follows a legacy
// end marker. A standalone summary shows nothing
// (_strip_context_summary_handoff_message).
func hermesSummaryPrior(content string) string {
	text := hermesText(content)
	if before, _, ok := strings.Cut(text, hermesPriorDelimiter); ok {
		prior := strings.TrimSpace(before)
		if strings.HasPrefix(prior, hermesPriorHeader) {
			prior = strings.TrimLeft(prior[len(hermesPriorHeader):], " \t\r\n")
		}
		return prior
	}
	if _, after, ok := strings.Cut(text, hermesSummaryEnd); ok {
		return strings.TrimLeft(after, " \t\r\n")
	}
	return ""
}

// hermesJSONPrefix marks content hermes stored as a JSON list of parts
// rather than a string (hermes_state_messages.py _encode_content).
const hermesJSONPrefix = "\x00json:"

// hermesText is a row's content as text: the string itself, or the text
// parts of a JSON list joined.
func hermesText(content string) string {
	var b strings.Builder
	for _, x := range hermesBlocks(content) {
		if x.Kind == transcript.BlockText {
			b.WriteString(x.Text)
		}
	}
	return b.String()
}

// hermesBlocks splits content into blocks. A list keeps one text block per
// text item and one image block per image_url item, whose URL is the image
// as a data: URL or where it is (acp_adapter/content.py _image_parts,
// gateway/platforms/api_server.py _normalize_image_part); audio has no
// block here.
func hermesBlocks(content string) []transcript.Block {
	text := func(t string) transcript.Block { return transcript.Block{Kind: transcript.BlockText, Text: t} }
	raw, ok := strings.CutPrefix(content, hermesJSONPrefix)
	if !ok {
		if content == "" {
			return nil
		}
		return []transcript.Block{text(content)}
	}
	var parts []json.RawMessage
	if json.Unmarshal([]byte(raw), &parts) != nil {
		var single json.RawMessage
		if json.Unmarshal([]byte(raw), &single) != nil {
			return []transcript.Block{text(content)}
		}
		parts = []json.RawMessage{single}
	}
	var out []transcript.Block
	for _, p := range parts {
		var s string
		if json.Unmarshal(p, &s) == nil {
			out = append(out, text(s))
			continue
		}
		var item struct {
			Type     string          `json:"type"`
			Text     *string         `json:"text"`
			Content  *string         `json:"content"`
			ImageURL json.RawMessage `json:"image_url"`
		}
		if json.Unmarshal(p, &item) != nil {
			continue
		}
		switch {
		case item.Text != nil:
			out = append(out, text(*item.Text))
		case item.Type == "text" && item.Content != nil:
			out = append(out, text(*item.Content))
		case item.Type == "image_url" || item.Type == "input_image":
			// The URL is an object's url on a Chat Completions part and the
			// value itself on a Responses one.
			var ref struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(item.ImageURL, &ref) != nil {
				json.Unmarshal(item.ImageURL, &ref.URL)
			}
			if ref.URL != "" {
				b := mediaBlock("", ref.URL, "")
				b.Kind = transcript.BlockImage
				out = append(out, b)
			}
		}
	}
	return out
}

// hermesEntry decodes a row. Roles other than user, assistant and tool
// (system, session_meta) are hermes's bookkeeping, stripped before the model
// sees history, and are kept as opaque rows.
func hermesEntry(role, content, toolID, toolName, toolCalls, reasoning string) (transcript.Entry, error) {
	e := transcript.Entry{Role: transcript.Role(role)}
	switch role {
	case "tool":
		// A multimodal result (a screenshot) is an OpenAI content list
		// (tool_executor.py); its images follow the result.
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: toolID, Name: toolName, Text: hermesText(content), Status: transcript.StatusOK}}
		for _, b := range hermesBlocks(content) {
			if isMedia(b) {
				b.ToolID = toolID
				e.Content = append(e.Content, b)
			}
		}
	case "assistant":
		if reasoning != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: reasoning})
		}
		e.Content = append(e.Content, hermesBlocks(content)...)
		if toolCalls != "" {
			var calls []hermesToolCall
			if err := json.Unmarshal([]byte(toolCalls), &calls); err != nil {
				return transcript.Entry{}, fmt.Errorf("hermes: tool_calls: %w", err)
			}
			for _, c := range calls {
				e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: c.ID, Name: c.Function.Name, Input: arguments(c.Function.Arguments)})
			}
		}
	case "user":
		e.Content = hermesBlocks(content)
	default:
		e.Role = transcript.RoleOpaque
	}
	return e, nil
}

func arguments(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

// hermesCWD is the SQL for a session's directory: the cwd column, or the cwd
// hermes records inside model_config when it leaves the column empty. Older
// hermes databases have no cwd column, so the expression names only the
// columns the sessions table has.
func hermesCWD(ctx context.Context, q queryer) (string, error) {
	cols, err := columns(ctx, q, "sessions")
	if err != nil {
		return "", err
	}
	var parts []string
	if cols["cwd"] {
		parts = append(parts, "NULLIF(cwd, '')")
	}
	if cols["model_config"] {
		parts = append(parts, "json_extract(model_config, '$.cwd')")
	}
	return "COALESCE(" + strings.Join(append(parts, "''"), ", ") + ")", nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// columns lists a table's columns.
func columns(ctx context.Context, q queryer, table string) (map[string]bool, error) {
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

// hermesVendor carries the source a session was opened from, which hermes
// requires on every session row.
type hermesVendor struct{ Source string }

// Write persists a session without disturbing anything it read. Rows of
// entries read from the store (Raw set) are kept exactly; rows of entries the
// session no longer holds are removed; new entries are inserted after them.
// A session hermes does not have gets a sessions row with the columns hermes
// requires, source and started_at.
func (st *hermesStore) Write(ctx context.Context, s *transcript.Session) (string, error) {
	if s.Agent != "hermes" {
		s = s.Portable()
	}
	if s.ID == "" {
		s.ID = transcript.NewUUID()
	}
	now := time.Now()
	if s.Created.IsZero() {
		s.Created = now
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return "", err
	}
	db, err := openRW(st.path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := hermesSchema(ctx, db); err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var one int
	exists := true
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", s.ID).Scan(&one); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		exists = false
	}
	if !exists {
		// hermes's ACP adapter restores a session only when its source is
		// "acp"; for any other source, session/resume silently starts an
		// empty session instead (acp_adapter/session.py _restore). Its CLI
		// resume does not check the source, so "acp" serves both.
		source := "acp"
		if v, ok := s.Vendor.(*hermesVendor); ok && v.Source != "" {
			source = v.Source
		}
		// The ACP adapter takes the session's directory from model_config.
		modelConfig, err := json.Marshal(map[string]string{"cwd": s.CWD})
		if err != nil {
			return "", err
		}
		title := s.Title
		if title == "" {
			if msgs := s.Messages(); len(msgs) > 0 {
				title = msgs[0].Text()
			}
		}
		// Older hermes databases have no cwd column; model_config carries
		// the directory in every version.
		cols, err := columns(ctx, tx, "sessions")
		if err != nil {
			return "", err
		}
		names := []string{"id", "source", "started_at", "title", "model", "model_config"}
		vals := []any{s.ID, source, epoch(s.Created), title, nullIfEmpty(s.Model), string(modelConfig)}
		if cols["cwd"] {
			names, vals = append(names, "cwd"), append(vals, s.CWD)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO sessions ("+strings.Join(names, ", ")+") VALUES (?"+strings.Repeat(", ?", len(names)-1)+")",
			vals...); err != nil {
			return "", fmt.Errorf("hermes: write session: %w", err)
		}
	}

	// Every row read is kept, bookkeeping rows and rows hermes no longer
	// gives the model included.
	keep := map[string]bool{}
	for _, e := range s.Entries {
		if e.Raw != nil {
			keep[strings.TrimPrefix(e.ID, "hermes-row:")] = true
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM messages WHERE session_id = ?", s.ID)
	if err != nil {
		return "", err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		if !keep[strconv.FormatInt(id, 10)] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", id); err != nil {
			return "", fmt.Errorf("hermes: remove message: %w", err)
		}
	}

	at := now
	added := false
	for _, e := range s.Messages() {
		if e.Raw != nil {
			continue
		}
		role, content, toolID, toolName, calls, reasoning := hermesColumns(e)
		ts := e.Time
		if ts.IsZero() {
			at = at.Add(time.Millisecond)
			ts = at
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, reasoning) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			s.ID, role, content, nullIfEmpty(toolID), calls, nullIfEmpty(toolName), epoch(ts), reasoning); err != nil {
			return "", fmt.Errorf("hermes: write message: %w", err)
		}
		added = true
	}
	if added {
		if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET message_count = (SELECT count(*) FROM messages WHERE session_id = ?) WHERE id = ?", s.ID, s.ID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.ID, nil
}

// epoch is the fractional unix seconds hermes stores its times as.
func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func hermesColumns(e transcript.Entry) (role, content, toolID, toolName string, toolCalls, reasoning any) {
	switch e.Role {
	case transcript.RoleTool:
		for _, b := range e.Content {
			if b.Kind == transcript.BlockToolResult {
				// The images the tool returned go in its content after the
				// text, as hermes keeps a multimodal result.
				blocks := []transcript.Block{{Kind: transcript.BlockText, Text: b.Text}}
				for _, m := range e.Content {
					if m.Kind == transcript.BlockImage && m.ToolID == b.ToolID {
						blocks = append(blocks, m)
					}
				}
				return "tool", hermesContent(blocks), b.ToolID, b.Name, nil, nil
			}
		}
		return "tool", "", "", "", nil, nil
	case transcript.RoleAssistant:
		var text, reason string
		var calls []hermesToolCall
		for _, b := range e.Content {
			switch b.Kind {
			case transcript.BlockText:
				// An answer's image has no place in the OpenAI messages
				// hermes sends, so it is dropped.
				text += b.Text
			case transcript.BlockReasoning:
				reason += b.Text
			case transcript.BlockToolUse:
				var c hermesToolCall
				c.ID, c.Type = b.ToolID, "function"
				c.Function.Name = b.Name
				c.Function.Arguments = string(b.Input)
				calls = append(calls, c)
			}
		}
		var callsCol any
		if len(calls) > 0 {
			raw, _ := json.Marshal(calls)
			callsCol = string(raw)
		}
		var reasonCol any
		if reason != "" {
			reasonCol = reason
		}
		return "assistant", text, "", "", callsCol, reasonCol
	default:
		return "user", hermesContent(e.Content), "", "", nil, nil
	}
}

// hermesContent is the content column for text and images: plain text when
// there is no image, else the JSON list of text and image_url parts hermes
// stores multimodal content as (hermes_state_messages.py _encode_content).
// hermes keeps no other kind of file in a message, so a file is dropped.
func hermesContent(blocks []transcript.Block) string {
	type imageURL struct {
		URL string `json:"url"`
	}
	type part struct {
		Type     string    `json:"type"`
		Text     *string   `json:"text,omitempty"`
		ImageURL *imageURL `json:"image_url,omitempty"`
	}
	var text string
	var parts []part
	images := false
	for _, b := range blocks {
		switch b.Kind {
		case transcript.BlockText:
			text += b.Text
			parts = append(parts, part{Type: "text", Text: &b.Text})
		case transcript.BlockImage:
			if url := mediaURL(b); url != "" {
				parts = append(parts, part{Type: "image_url", ImageURL: &imageURL{URL: url}})
				images = true
			}
		}
	}
	if !images {
		return text
	}
	raw, _ := json.Marshal(parts)
	return hermesJSONPrefix + string(raw)
}

func hermesSchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY, source TEXT NOT NULL, model TEXT, model_config TEXT, system_prompt TEXT,
  started_at REAL NOT NULL, ended_at REAL, message_count INTEGER DEFAULT 0, title TEXT, cwd TEXT);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT,
  timestamp REAL NOT NULL, token_count INTEGER, finish_reason TEXT, reasoning TEXT);`
	_, err := db.ExecContext(ctx, ddl)
	return err
}
