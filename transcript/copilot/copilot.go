// Package copilot reads and writes GitHub Copilot CLI sessions.
//
// Copilot keeps a directory per session under ~/.copilot/session-state/<id>
// with events.jsonl, the conversation as an event log, and workspace.yaml,
// the session's identity. Events link to their parent through id and
// parentId. user.message and assistant.message rows are the conversation;
// hook, model change and turn boundary rows stay opaque in the chain. A
// sub-agent's events go into the same log and chain, marked with its
// agentId, and a session.compaction_complete row replaces the history
// before it; see finish.
//
// Copilot finds sessions through an index, ~/.copilot/session-store.db, so a
// session it will load needs a row there too. Codec here is read-only;
// Writer writes the files, and the sqlite module adds the index.
package copilot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Codec reads Copilot CLI sessions. It is read-only: Copilot finds sessions
// through its index, ~/.copilot/session-store.db, and a session with no row
// there is "not found or could not be loaded" however well-formed its files
// are. Writing the index needs a database driver, so the codec that writes
// lives in the sqlite module, built on Writer.
var Codec = func() transcript.JSONL {
	c := Writer
	c.Encode = nil
	c.WriteHeader = nil
	c.After = nil
	return c
}()

// Writer reads Copilot sessions and writes their files, events.jsonl and
// workspace.yaml, but not the index. Alone it produces sessions Copilot will
// not load; the sqlite module's Copilot codec wraps it and adds the index.
var Writer = transcript.JSONL{
	Agent: "copilot",
	Layout: transcript.Layout{
		Files:       files,
		PathFor:     pathFor,
		Peek:        peek,
		SessionRoot: filepath.Dir,
		Holders:     holders,
		Serving:     serving,
	},
	Header:      header,
	Decode:      decode,
	Finish:      finish,
	Encode:      encode,
	WriteHeader: writeHeader,
	Prepare:     prepareStart,
	After:       writeWorkspace,
	Tree:        true,
}

// Vendor is what a Copilot session carries in Session.Vendor: the
// session.start event's data, so a write reproduces the producer and
// version fields this codec does not model.
type Vendor struct {
	Start map[string]json.RawMessage
}

const root = ".copilot/session-state"

// event is Copilot's event envelope. Copilot rejects an envelope that lacks
// any of its fields ("Session file is corrupted: invalid session event
// envelope"), so every field is always written; the root event's parentId is
// null.
type event struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	ParentID  *string         `json:"parentId"`
	// AgentID names the sub-agent an event belongs to. It is absent on the
	// main agent's events, and on every event this codec writes.
	AgentID string `json:"agentId,omitempty"`
}

// owner is the part of an event's data that also ties it to a sub-agent.
type owner struct {
	AgentID          string `json:"agentId"`
	ParentToolCallID string `json:"parentToolCallId"`
}

// subagent reports whether an event is a sub-agent's, by the rule Copilot's
// own readers use (a non-empty agentId on the event or its data, or a
// parentToolCallId on its data).
func (ev event) subagent() bool {
	if ev.AgentID != "" {
		return true
	}
	var o owner
	_ = json.Unmarshal(ev.Data, &o)
	return o.AgentID != "" || o.ParentToolCallID != ""
}

func parent(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

type sessionStart struct {
	SessionID string   `json:"sessionId"`
	Version   int      `json:"version,omitempty"`
	Producer  string   `json:"producer,omitempty"`
	StartTime string   `json:"startTime"`
	Context   context_ `json:"context"`
}

// startIdentity is the part of session.start that names this session. The
// rest of the event's data is the same for every session a given Copilot
// writes, and comes from Vendor.
type startIdentity struct {
	SessionID string   `json:"sessionId"`
	StartTime string   `json:"startTime"`
	Context   context_ `json:"context"`
}

// defaultStart is session.start's fixed data as Copilot CLI 1.0.88 writes it,
// used when the store has no session of Copilot's own to copy from. Copilot
// rejects a session.start without copilotVersion ("Session file is corrupted
// (line 1: missing field copilotVersion)").
var defaultStart = map[string]json.RawMessage{
	"version":        json.RawMessage(`1`),
	"producer":       json.RawMessage(`"copilot-agent"`),
	"copilotVersion": json.RawMessage(`"1.0.88"`),
	"contextTier":    json.RawMessage(`null`),
	"alreadyInUse":   json.RawMessage(`false`),
}

// prepareStart gives a session with no session.start of its own the fixed
// fields of the newest session Copilot wrote in the same store, so the
// version stamp matches the Copilot that will load it.
func prepareStart(home string, s *transcript.Session) error {
	if v, ok := s.Vendor.(*Vendor); ok && len(v.Start) > 0 {
		return nil
	}
	start := map[string]json.RawMessage{}
	for k, val := range defaultStart {
		start[k] = val
	}
	paths, err := files(home, "")
	if err != nil {
		return err
	}
	var newest string
	var newestTime time.Time
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(newestTime) {
			newest, newestTime = p, fi.ModTime()
		}
	}
	if newest != "" {
		_, _ = transcript.PeekFirstLine(newest, func(row json.RawMessage) (transcript.Info, error) {
			var ev event
			if json.Unmarshal(row, &ev) != nil || ev.Type != "session.start" {
				return transcript.Info{}, nil
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(ev.Data, &fields) == nil {
				for k, val := range fields {
					switch k {
					case "sessionId", "startTime", "context":
					default:
						start[k] = val
					}
				}
			}
			return transcript.Info{}, nil
		})
	}
	s.Vendor = &Vendor{Start: start}
	return nil
}

type context_ struct {
	CWD string `json:"cwd"`
}

type userMessage struct {
	Content string `json:"content"`
	// TransformedContent is the prompt as the model is given it: the
	// person's text behind the context Copilot adds, such as the time.
	TransformedContent string       `json:"transformedContent,omitempty"`
	Attachments        []attachment `json:"attachments,omitempty"`
	MessageID          string       `json:"messageId"`
}

// attachment is a file or blob attached to a prompt (Attachment in
// Copilot's session-events schema). Copilot persists an image's bytes in a
// session.binary_asset event and names it by AssetID; an attachment it does
// not send natively is listed in the prompt's <tagged_files> block, whose
// line it keeps in TaggedFilesEntry. Other attachment types (a selection, a
// GitHub reference) hold no file and are not read.
type attachment struct {
	Type             string `json:"type"`
	Path             string `json:"path,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
	AssetID          string `json:"assetId,omitempty"`
	MimeType         string `json:"mimeType,omitempty"`
	Data             string `json:"data,omitempty"`
	TaggedFilesEntry string `json:"taggedFilesEntry,omitempty"`
}

// binary is an image or other binary a tool returned for the model
// (PersistedBinaryResult), inline or by asset, and a session.binary_asset
// event's data (BinaryAssetData).
type binary struct {
	Type        string `json:"type"`
	AssetID     string `json:"assetId,omitempty"`
	MimeType    string `json:"mimeType"`
	Data        string `json:"data,omitempty"`
	Description string `json:"description,omitempty"`
}

// compactionComplete is the data of a session.compaction_complete event.
type compactionComplete struct {
	Success        bool   `json:"success"`
	SummaryContent string `json:"summaryContent"`
}

type assistantMessage struct {
	MessageID    string        `json:"messageId"`
	Content      string        `json:"content"`
	Model        string        `json:"model,omitempty"`
	ToolRequests []toolRequest `json:"toolRequests"`
}

type toolRequest struct {
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

type toolResult struct {
	ToolCallID string `json:"toolCallId"`
	Success    bool   `json:"success"`
	Result     struct {
		Content             string   `json:"content"`
		BinaryResultsForLlm []binary `json:"binaryResultsForLlm,omitempty"`
	} `json:"result"`
}

func files(home, cwd string) ([]string, error) {
	return transcript.Glob(filepath.Join(home, root, "*", "events.jsonl"))
}

// holders reads the in-use markers Copilot keeps in a session's directory
// while a process has the session open: inuse.<pid>.hold, one per process.
// Measured on Copilot CLI 1.0.88, which holds the file open for as long as
// it serves the session.
func holders(path string) []int {
	marks, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "inuse.*.hold"))
	var pids []int
	for _, m := range marks {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "inuse."), ".hold")
		if pid, err := strconv.Atoi(name); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// serving reads every session's in-use markers as pid to session id.
func serving(home string) map[int]string {
	out := map[int]string{}
	marks, _ := filepath.Glob(filepath.Join(home, root, "*", "inuse.*.hold"))
	for _, m := range marks {
		for _, pid := range holders(filepath.Join(filepath.Dir(m), "events.jsonl")) {
			out[pid] = filepath.Base(filepath.Dir(m))
		}
	}
	return out
}

func pathFor(home string, s *transcript.Session) string {
	return filepath.Join(home, root, s.ID, "events.jsonl")
}

func peek(path string) (transcript.Info, error) {
	return transcript.PeekFirstLine(path, func(raw json.RawMessage) (transcript.Info, error) {
		var ev event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return transcript.Info{}, err
		}
		if ev.Type != "session.start" {
			return transcript.Info{}, fmt.Errorf("%s: first event is %q", path, ev.Type)
		}
		var st sessionStart
		if err := json.Unmarshal(ev.Data, &st); err != nil {
			return transcript.Info{}, err
		}
		in := transcript.Info{ID: st.SessionID, CWD: st.Context.CWD}
		if t, err := time.Parse(time.RFC3339Nano, st.StartTime); err == nil {
			in.Updated = t
		}
		return in, nil
	})
}

func header(raw json.RawMessage, s *transcript.Session) (bool, error) {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return false, err
	}
	if ev.Type != "session.start" {
		return false, nil
	}
	var st sessionStart
	if err := json.Unmarshal(ev.Data, &st); err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &fields); err != nil {
		return false, err
	}
	s.ID = st.SessionID
	s.CWD = st.Context.CWD
	if t, err := time.Parse(time.RFC3339Nano, st.StartTime); err == nil {
		s.Created = t
	}
	s.Vendor = &Vendor{Start: fields}
	// The header is a node in the chain too, so it is returned through
	// decode rather than swallowed here.
	return false, nil
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return transcript.Entry{}, false, err
	}
	if ev.ID == "" {
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: ev.ID, Role: transcript.RoleOpaque}
	if ev.ParentID != nil {
		e.ParentID = *ev.ParentID
	}
	if ev.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, ev.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: timestamp: %w", ev.ID, err)
		}
		e.Time = t
	}
	switch ev.Type {
	case "user.message":
		var m userMessage
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: %w", ev.ID, err)
		}
		e.Role = transcript.RoleUser
		e.Content = []transcript.Block{{Kind: transcript.BlockText, Text: m.Content}}
	case "assistant.message":
		var m assistantMessage
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: %w", ev.ID, err)
		}
		e.Role = transcript.RoleAssistant
		if m.Content != "" {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: m.Content})
		}
		for _, tr := range m.ToolRequests {
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: tr.ToolCallID, Name: tr.Name, Input: tr.Arguments})
		}
	case "tool.execution_complete":
		var tr toolResult
		if err := json.Unmarshal(ev.Data, &tr); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("event %s: %w", ev.ID, err)
		}
		st := transcript.StatusOK
		if !tr.Success {
			st = transcript.StatusError
		}
		e.Role = transcript.RoleTool
		e.Content = []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: tr.ToolCallID, Text: tr.Result.Content, Status: st}}
	}
	if e.Role != transcript.RoleOpaque && ev.subagent() {
		// A sub-agent's turn runs in a context of its own, and the main
		// agent's model gets only the task tool's result. session/load
		// replays the sub-agent's answers and tool calls but skips its
		// prompt (mapEventForReplay drops a user.message with an agentId).
		e.Audience = transcript.AudienceUser
		if e.Role == transcript.RoleUser {
			e.Audience = transcript.AudienceNone
		}
	}
	return e, true, nil
}

// finish applies each successful compaction on the active branch. Copilot
// resumes a compacted session with one user message in place of everything
// before the compaction: the summary the compaction recorded, after the
// prompts the person gave the main agent until then. What follows the
// compaction is kept. Copilot builds the message from a template in its
// native runtime; the wording here is what it sent the model on resume
// (Copilot CLI 1.0.88). A session a rewind cut short needs nothing: the
// rewind removes the events from the log.
func finish(s *transcript.Session) error {
	if err := attach(s); err != nil {
		return err
	}
	var prompts []string
	for _, i := range s.Branch() {
		e := &s.Entries[i]
		var ev event
		if json.Unmarshal(e.Raw, &ev) != nil || ev.subagent() {
			continue
		}
		switch ev.Type {
		case "user.message":
			prompts = append(prompts, e.Text())
		case "session.compaction_complete":
			var c compactionComplete
			if err := json.Unmarshal(ev.Data, &c); err != nil {
				return fmt.Errorf("event %s: %w", ev.ID, err)
			}
			if !c.Success {
				continue
			}
			e.Compaction = &transcript.Compaction{Summary: []transcript.Entry{{
				ID: e.ID, Role: transcript.RoleUser, Time: e.Time,
				Content: []transcript.Block{{Kind: transcript.BlockText, Text: resumeSummary(c.SummaryContent, prompts)}},
			}}}
		}
	}
	return nil
}

// attach adds the images and files of prompts and tool results to their
// entries. Their bytes are in session.binary_asset events, which may be
// anywhere in the log, so this runs once every row is read. A prompt's
// entry also gets what the model is given for it: the transformed text, the
// <tagged_files> block naming the files not sent natively, and each image
// behind a line naming its path (or "Attached image" for one pasted as
// bytes), as Copilot CLI 1.0.88 sent them on resume.
// An image a tool returned is kept after the tool's result; Copilot sent the
// model only the result's text for it, over an OpenAI-compatible provider.
func attach(s *transcript.Session) error {
	assets := map[string]binary{}
	for _, e := range s.Entries {
		var ev event
		if json.Unmarshal(e.Raw, &ev) != nil || ev.Type != "session.binary_asset" {
			continue
		}
		var b binary
		if json.Unmarshal(ev.Data, &b) == nil && b.AssetID != "" {
			assets[b.AssetID] = b
		}
	}
	for i := range s.Entries {
		e := &s.Entries[i]
		var ev event
		if json.Unmarshal(e.Raw, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "user.message":
			var m userMessage
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				return fmt.Errorf("event %s: %w", ev.ID, err)
			}
			if m.TransformedContent == "" && len(m.Attachments) == 0 {
				continue
			}
			text := m.TransformedContent
			if text == "" {
				text = m.Content
			}
			var tagged []string
			var native []transcript.Block
			for _, a := range m.Attachments {
				b, ok, err := a.block(assets)
				if err != nil {
					return fmt.Errorf("event %s: %w", ev.ID, err)
				}
				if !ok {
					continue
				}
				e.Content = append(e.Content, b)
				if a.TaggedFilesEntry != "" {
					tagged = append(tagged, a.TaggedFilesEntry)
					continue
				}
				if b.Kind == transcript.BlockImage {
					label := "Attached image"
					if a.Path != "" {
						label = "Image file at path " + a.Path
					}
					native = append(native, transcript.Block{Kind: transcript.BlockText, Text: label})
				}
				native = append(native, b)
			}
			if len(tagged) > 0 {
				text += "\n\n\n\n<tagged_files>\n" + strings.Join(tagged, "\n") + "\n</tagged_files>"
			}
			e.ModelContent = append([]transcript.Block{{Kind: transcript.BlockText, Text: text}}, native...)
		case "tool.execution_complete":
			var tr toolResult
			if err := json.Unmarshal(ev.Data, &tr); err != nil {
				return fmt.Errorf("event %s: %w", ev.ID, err)
			}
			for _, r := range tr.Result.BinaryResultsForLlm {
				if a, ok := assets[r.AssetID]; ok && r.Data == "" {
					r.Data = a.Data
				}
				if r.Data == "" {
					continue // omitted: too large, or its asset is gone
				}
				b, err := binaryBlock(r.Type == "image", r.MimeType, r.Data)
				if err != nil {
					return fmt.Errorf("event %s: %w", ev.ID, err)
				}
				b.ToolID = tr.ToolCallID
				e.Content = append(e.Content, b)
			}
		}
	}
	return nil
}

// block is an attachment as an image or file block: its bytes when Copilot
// kept them, else its path. A file keeps its display name.
func (a attachment) block(assets map[string]binary) (transcript.Block, bool, error) {
	if a.Type != "file" && a.Type != "blob" {
		return transcript.Block{}, false, nil
	}
	data, mime := a.Data, a.MimeType
	if asset, ok := assets[a.AssetID]; ok && data == "" {
		data, mime = asset.Data, asset.MimeType
	}
	image := strings.HasPrefix(mime, "image/")
	var b transcript.Block
	switch {
	case data != "":
		var err error
		if b, err = binaryBlock(image, mime, data); err != nil {
			return transcript.Block{}, false, err
		}
	case a.Path != "":
		b = transcript.Block{Kind: transcript.BlockFile, MediaType: mime, URI: a.Path}
		if image {
			b.Kind = transcript.BlockImage
		}
	default:
		return transcript.Block{}, false, nil
	}
	if !image {
		b.Name = a.DisplayName
	}
	return b, true, nil
}

// binaryBlock decodes base64 bytes into an image or file block.
func binaryBlock(image bool, mime, data string) (transcript.Block, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return transcript.Block{}, fmt.Errorf("binary data: %w", err)
	}
	kind := transcript.BlockFile
	if image {
		kind = transcript.BlockImage
	}
	return transcript.Block{Kind: kind, MediaType: mime, Data: raw}, nil
}

// resumeSummary is the message that stands in for compacted history. The
// list of prompts is left out when there are none; no capture has shown
// what Copilot sends then.
func resumeSummary(summary string, prompts []string) string {
	var b strings.Builder
	b.WriteString("Some of the conversation history has been summarized to free up context.\n\n")
	if len(prompts) > 0 {
		b.WriteString("You were originally given instructions from a user over one or more turns. Here were the user messages:\n")
		for _, p := range prompts {
			b.WriteString("<user_message>\n" + p + "\n</user_message>\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Here is a summary of the prior context:\n<summary>\n" + summary + "\n</summary>\n")
	return b.String()
}

func writeHeader(s *transcript.Session) ([]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if v, ok := s.Vendor.(*Vendor); ok {
		for k, val := range v.Start {
			fields[k] = val
		}
	}
	identity, err := json.Marshal(startIdentity{SessionID: s.ID, StartTime: stamp(s.Created), Context: context_{CWD: s.CWD}})
	if err != nil {
		return nil, err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(identity, &known); err != nil {
		return nil, err
	}
	for k, val := range known {
		fields[k] = val
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	// The header is the root of the chain: the first message's parent.
	headID := transcript.NewUUID()
	for i := range s.Entries {
		if s.Entries[i].Role != transcript.RoleOpaque {
			if s.Entries[i].ParentID == "" {
				s.Entries[i].ParentID = headID
			}
			break
		}
	}
	h, err := json.Marshal(event{Type: "session.start", Data: data, ID: headID, Timestamp: stamp(s.Created)})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{h}, nil
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	ev := event{ID: e.ID, ParentID: parent(e.ParentID), Timestamp: stamp(t)}
	var data any
	switch e.Role {
	case transcript.RoleUser, transcript.RoleSystem:
		ev.Type = "user.message"
		data = userMessage{Content: e.Text(), Attachments: attachments(e.Content), MessageID: e.ID}
	case transcript.RoleAssistant:
		// Reasoning is not written. Copilot keeps a model's reasoning on the
		// message (reasoningText, with the provider's reasoningOpaque or
		// encryptedContent beside it) and strips it when it loads a session
		// (hydrate_from_events_strip_reasoning in the 1.0.88 runtime;
		// session-events.schema.json marks the opaque parts "stripped on
		// resume"). A reasoningText written here was never sent to the model
		// on session/load.
		ev.Type = "assistant.message"
		m := assistantMessage{MessageID: e.ID, Content: e.Text(), ToolRequests: []toolRequest{}}
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolUse {
				continue
			}
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			m.ToolRequests = append(m.ToolRequests, toolRequest{ToolCallID: b.ToolID, Name: b.Name, Arguments: args})
		}
		data = m
	case transcript.RoleTool:
		ev.Type = "tool.execution_complete"
		var tr toolResult
		for _, b := range e.Content {
			if b.Kind != transcript.BlockToolResult {
				continue
			}
			tr.ToolCallID = b.ToolID
			tr.Success = b.Status != transcript.StatusError
			tr.Result.Content = b.Text
		}
		// A tool's image or file goes to the model as a binary result held
		// inline (PersistedBinaryImage); one known only by reference has no
		// such form and is left out.
		for _, b := range e.Content {
			if (b.Kind == transcript.BlockImage || b.Kind == transcript.BlockFile) && len(b.Data) > 0 {
				typ := "resource"
				if b.Kind == transcript.BlockImage {
					typ = "image"
				}
				tr.Result.BinaryResultsForLlm = append(tr.Result.BinaryResultsForLlm, binary{Type: typ, MimeType: b.MediaType, Data: base64.StdEncoding.EncodeToString(b.Data)})
			}
		}
		data = tr
	default:
		return nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	ev.Data = raw
	return json.Marshal(ev)
}

// attachments encodes a prompt's images and files the way Copilot takes
// them: bytes as a blob attachment, whose data Copilot interns into an
// asset when it next writes the session, and a local file as a file
// attachment by path. A remote URL has no attachment form and is left out;
// so is an image in an assistant message, which Copilot's format has no
// place for.
func attachments(content []transcript.Block) []attachment {
	var out []attachment
	for _, b := range content {
		if b.Kind != transcript.BlockImage && b.Kind != transcript.BlockFile {
			continue
		}
		switch {
		case len(b.Data) > 0:
			out = append(out, attachment{Type: "blob", Data: base64.StdEncoding.EncodeToString(b.Data), MimeType: b.MediaType, DisplayName: b.Name})
		case strings.HasPrefix(b.URI, "/") || strings.HasPrefix(b.URI, "file://"):
			path := strings.TrimPrefix(b.URI, "file://")
			name := b.Name
			if name == "" {
				name = filepath.Base(path)
			}
			out = append(out, attachment{Type: "file", Path: path, DisplayName: name})
		}
	}
	return out
}

// writeWorkspace writes workspace.yaml beside events.jsonl. The file is a
// flat key: value document, so it is emitted directly rather than through
// a YAML dependency.
func writeWorkspace(ctx context.Context, path string, s *transcript.Session) error {
	name := s.Title
	if name == "" {
		if msgs := s.Messages(); len(msgs) > 0 {
			name = msgs[0].Text()
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\n", s.ID)
	fmt.Fprintf(&b, "cwd: %s\n", s.CWD)
	fmt.Fprintf(&b, "client_name: agentprotocol\n")
	fmt.Fprintf(&b, "name: %s\n", yamlString(name))
	fmt.Fprintf(&b, "user_named: false\n")
	fmt.Fprintf(&b, "summary_count: 0\n")
	fmt.Fprintf(&b, "fork_count: 0\n")
	fmt.Fprintf(&b, "created_at: %s\n", stamp(s.Created))
	fmt.Fprintf(&b, "updated_at: %s\n", stamp(s.Updated))
	return os.WriteFile(filepath.Join(filepath.Dir(path), "workspace.yaml"), []byte(b.String()), 0o644)
}

// yamlString quotes a scalar when YAML would otherwise misread it.
func yamlString(s string) string {
	if s == "" || strings.ContainsAny(s, ":#\n\"'{}[]&*!|>%@`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		q, _ := json.Marshal(s)
		return string(q)
	}
	return s
}

func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
