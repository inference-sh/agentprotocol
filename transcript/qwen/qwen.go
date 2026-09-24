// Package qwen reads and writes Qwen Code sessions.
//
// Qwen keeps one JSONL per session under
// ~/.qwen/projects/<sanitized cwd>/chats/<session id>.jsonl, where the
// project directory is the working directory with every character other
// than a letter or digit turned into a dash. Rows form a tree through uuid
// and parentUuid. A row's type is user, assistant, tool_result or system;
// the first three carry a message whose parts are Gemini-shaped (text,
// functionCall, functionResponse). System rows (telemetry, checkpoints,
// slash command results) stay opaque in the chain, except that a
// chat_compression row carries the history the model is given from then on.
//
// Qwen forked Gemini CLI but not its session store: nothing here matches
// ~/.gemini/tmp.
package qwen

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Version is the Qwen Code version whose row shape this codec writes for a
// session that did not come from Qwen.
const Version = "0.24.4"

const (
	agent = "qwen"
	root  = ".qwen/projects"
)

// sessionFile is the file name Qwen lists a session under
// (SESSION_FILE_PATTERN in its services/sessionService.ts). A file that does
// not match is not a session to Qwen.
var sessionFile = regexp.MustCompile(`^[0-9a-fA-F-]{32,36}\.jsonl$`)

// Codec is the Qwen Code session store.
var Codec = transcript.JSONL{
	Agent: agent,
	Layout: transcript.Layout{
		Ext: ".jsonl",
		Files: func(home, cwd string) ([]string, error) {
			project := "*"
			if cwd != "" {
				project = ProjectDir(cwd)
			}
			paths, err := transcript.Glob(filepath.Join(home, root, project, "chats", "*.jsonl"))
			if err != nil {
				return nil, err
			}
			out := paths[:0]
			for _, p := range paths {
				if sessionFile.MatchString(filepath.Base(p)) {
					out = append(out, p)
				}
			}
			return out, nil
		},
		PathFor: func(home string, s *transcript.Session) string {
			return filepath.Join(home, root, ProjectDir(s.CWD), "chats", s.ID+".jsonl")
		},
		Peek: peek,
	},
	Decode:  decode,
	Finish:  finish,
	Encode:  encode,
	Prepare: prepare,
	Tree:    true,
}

// Vendor is what a Qwen session carries in Session.Vendor: the stamps Qwen
// puts on every row.
type Vendor struct {
	Version   string
	GitBranch string
}

type row struct {
	UUID           string          `json:"uuid"`
	ParentUUID     *string         `json:"parentUuid"`
	SessionID      string          `json:"sessionId"`
	Timestamp      string          `json:"timestamp"`
	Type           string          `json:"type"`
	Subtype        subtype         `json:"subtype,omitempty"`
	Provenance     string          `json:"provenance,omitempty"`
	CWD            string          `json:"cwd"`
	Version        string          `json:"version,omitempty"`
	GitBranch      string          `json:"gitBranch,omitempty"`
	Message        *message        `json:"message,omitempty"`
	Model          string          `json:"model,omitempty"`
	ToolCallResult *toolCallResult `json:"toolCallResult,omitempty"`
	SystemPayload  json.RawMessage `json:"systemPayload,omitempty"`
}

// subtype refines a row's type. Only the subtypes that change what the
// person sees or the model is given are named.
type subtype string

const (
	subtypeCompression  subtype = "chat_compression"
	subtypeSlashCommand subtype = "slash_command"
	subtypeRealtime     subtype = "realtime_message"
	subtypeGoalRuntime  subtype = "goal_runtime"
	subtypeNotification subtype = "notification"
)

// sideRecords are the system rows Qwen's loader keeps out of the
// conversation: they are never the leaf and no row finds its parent among
// them (isTranscriptConversationRecord in utils/transcript-records.ts).
var sideRecords = map[subtype]bool{
	"session_artifact_event":    true,
	"session_artifact_snapshot": true,
	"session_sources_snapshot":  true,
	"managed_session_header_v1": true,
	"managed_session_event_v1":  true,
	"managed_session_commit_v1": true,
}

type message struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type functionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type functionResponse struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type toolCallResult struct {
	CallID string `json:"callId"`
	Status string `json:"status"`
}

// compressionPayload is a chat_compression row's systemPayload. When it
// carries compressedHistory, Qwen's resume replaces the whole model history
// with it (SessionApiHistoryAccumulator in services/session-api-history.ts);
// the history the person sees is untouched.
type compressionPayload struct {
	CompressedHistory []message `json:"compressedHistory"`
}

// slashCommandPayload is a slash_command row's systemPayload.
type slashCommandPayload struct {
	Phase              string `json:"phase"`
	RawCommand         string `json:"rawCommand"`
	SentToModel        *bool  `json:"sentToModel"`
	OutputHistoryItems []struct {
		Type string `json:"type"`
	} `json:"outputHistoryItems"`
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.UUID == "" {
		return transcript.Entry{}, false, nil
	}
	if r.Type == "system" && sideRecords[r.Subtype] {
		// Without an id the row stays out of the chain, as it does for Qwen,
		// and a row appended after it links to the conversation instead.
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.UUID, Role: transcript.RoleOpaque}
	if r.ParentUUID != nil {
		e.ParentID = *r.ParentUUID
	}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.UUID, err)
		}
		e.Time = t
	}
	if s.ID == "" {
		s.ID = r.SessionID
	}
	if s.CWD == "" {
		s.CWD = r.CWD
	}
	if r.Version != "" || r.GitBranch != "" {
		v, _ := s.Vendor.(*Vendor)
		if v == nil {
			v = &Vendor{}
			s.Vendor = v
		}
		if r.Version != "" {
			v.Version = r.Version
		}
		if r.GitBranch != "" {
			v.GitBranch = r.GitBranch
		}
	}
	if r.Type == "system" && r.Subtype == subtypeCompression {
		var p compressionPayload
		if err := json.Unmarshal(r.SystemPayload, &p); err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: compression: %w", r.UUID, err)
		}
		if p.CompressedHistory != nil {
			// composePostCompactHistory lays it out as the summary, a
			// synthetic acknowledgement, the files and images Qwen embeds
			// again, and a function call still waiting for its response.
			// The summary and that call are conversation; the rest is
			// Qwen's own context.
			c := &transcript.Compaction{}
			for i, m := range p.CompressedHistory {
				e := content(m, transcript.StatusOK)
				if i == 0 || hasCall(e) {
					e.Audience = transcript.AudienceAll
				}
				c.Summary = append(c.Summary, e)
			}
			e.Compaction = c
		}
		return e, true, nil
	}
	if r.Message == nil {
		return e, true, nil
	}
	switch r.Type {
	case "user":
		e.Role = transcript.RoleUser
	case "assistant":
		e.Role = transcript.RoleAssistant
		if r.Model != "" {
			s.Model = r.Model
		}
	case "tool_result":
		e.Role = transcript.RoleTool
	default:
		return e, true, nil
	}
	switch r.Subtype {
	case subtypeRealtime:
		// A voice exchange: shown, never replayed to the model.
		e.Audience = transcript.AudienceUser
	case subtypeGoalRuntime:
		// The goal runtime's own prompt: the model is given it, the TUI's
		// resume leaves it out.
		e.Role, e.Audience = transcript.RoleSystem, transcript.AudienceModel
	case subtypeNotification:
		// A background task reporting back. Qwen shows it as a notice, not a
		// turn; only the recorder's system provenance marks it as injected.
		if r.Provenance == "system" {
			e.Role = transcript.RoleSystem
		}
	}
	status := transcript.StatusOK
	if r.ToolCallResult != nil && r.ToolCallResult.Status != "" && r.ToolCallResult.Status != "success" {
		status = transcript.StatusError
	}
	e.Content = content(*r.Message, status).Content
	return e, true, nil
}

// content maps a Gemini-shaped message to an entry, for the history a
// compression keeps as much as for a row's own message. A user message that
// only answers function calls is a tool entry.
func content(m message, status transcript.Status) transcript.Entry {
	e := transcript.Entry{Role: transcript.RoleUser, Audience: transcript.AudienceModel}
	if m.Role == "model" {
		e.Role = transcript.RoleAssistant
	}
	results := 0
	for _, p := range m.Parts {
		switch {
		case p.FunctionCall != nil:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Input: p.FunctionCall.Args})
		case p.FunctionResponse != nil:
			results++
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name, Text: responseText(p.FunctionResponse.Response), Status: status})
		case p.Thought:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
		default:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
	}
	if e.Role == transcript.RoleUser && results > 0 && results == len(m.Parts) {
		e.Role = transcript.RoleTool
	}
	return e
}

// finish reads the rows the way Qwen's loader and resume do
// (prepareTranscriptRecords in utils/transcript-records.ts, then
// SessionApiHistoryAccumulator). A mid_turn_user_message stays its own
// entry: the accumulator joins its parts onto the user-role content before
// it, which gives the model the same text in one Gemini message.
func finish(s *transcript.Session) error {
	// Rows repeated under one uuid are fragments of one record: the first
	// carries the links, and the parts of every copy are joined onto it. The
	// leaf is the last row's uuid, which may name an earlier copy.
	first := map[string]int{}
	for i := range s.Entries {
		e := &s.Entries[i]
		if e.ID == "" {
			continue
		}
		s.Leaf = e.ID
		j, ok := first[e.ID]
		if !ok {
			first[e.ID] = i
			continue
		}
		base := &s.Entries[j]
		base.Content = append(base.Content, e.Content...)
		if e.Time.After(base.Time) {
			base.Time = e.Time
		}
		e.ID, e.ParentID = "", ""
	}

	// An ACP slash command records the typed command as a user row and its
	// output as a slash_command result. The accumulator drops the command
	// from the model's history when the result follows it directly and is
	// only local output; the person still sees it.
	last, lastUser := -1, false // the last row the accumulator took into history
	for _, i := range s.Branch() {
		e := &s.Entries[i]
		var r row
		if json.Unmarshal(e.Raw, &r) != nil {
			continue
		}
		switch {
		case e.Role != transcript.RoleOpaque:
			if r.Subtype != subtypeRealtime {
				last, lastUser = i, r.Type == "user"
			}
		case e.Compaction != nil:
			last = -1
		case r.Type == "system" && r.Subtype == subtypeSlashCommand:
			if last >= 0 && localCommand(r.SystemPayload, s.Entries[last].Raw) {
				s.Entries[last].Audience = transcript.AudienceUser
			}
			// A command fences off the input before it from the next one.
			if lastUser {
				last = -1
			}
		}
	}
	return nil
}

// prepare gives a session from another agent a UUID when its id is not one
// Qwen lists: Qwen only lists a file whose name matches sessionFile and whose
// first row's sessionId is that name.
func prepare(home string, s *transcript.Session) error {
	if s.Agent != agent && !sessionFile.MatchString(s.ID+".jsonl") {
		s.ID = transcript.NewUUID()
	}
	return nil
}

// localCommand reports whether a slash_command payload is the local result
// of the command typed in the user row before it: phase result, not sent to
// the model, only assistant output, and a user row holding nothing but the
// command's text.
func localCommand(payload, prev json.RawMessage) bool {
	var p slashCommandPayload
	if json.Unmarshal(payload, &p) != nil || p.Phase != "result" || (p.SentToModel != nil && *p.SentToModel) || len(p.OutputHistoryItems) == 0 {
		return false
	}
	for _, it := range p.OutputHistoryItems {
		if it.Type != "assistant" {
			return false
		}
	}
	var u struct {
		Type    string  `json:"type"`
		Subtype *string `json:"subtype"`
		Message *struct {
			Role  string                       `json:"role"`
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"message"`
	}
	if json.Unmarshal(prev, &u) != nil || u.Type != "user" || u.Subtype != nil || u.Message == nil || u.Message.Role != "user" || len(u.Message.Parts) != 1 || len(u.Message.Parts[0]) != 1 {
		return false
	}
	var text string
	return json.Unmarshal(u.Message.Parts[0]["text"], &text) == nil && text == p.RawCommand
}

// responseText reads a functionResponse.response, whose output field holds
// the tool's text for Qwen's own tools.
func responseText(raw json.RawMessage) string {
	var out struct {
		Output *string `json:"output"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Output != nil {
		return *out.Output
	}
	return string(raw)
}

func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	v, _ := s.Vendor.(*Vendor)
	if v == nil {
		v = &Vendor{}
	}
	version := v.Version
	if version == "" {
		version = Version
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	r := row{
		UUID:      e.ID,
		SessionID: s.ID,
		Timestamp: t.UTC().Format("2006-01-02T15:04:05.000Z"),
		CWD:       s.CWD,
		Version:   version,
		GitBranch: v.GitBranch,
		Message:   &message{},
	}
	if e.ParentID != "" {
		r.ParentUUID = &e.ParentID
	}
	switch e.Role {
	case transcript.RoleUser, transcript.RoleSystem:
		r.Type, r.Provenance, r.Message.Role = "user", "real_user", "user"
	case transcript.RoleAssistant:
		r.Type, r.Provenance, r.Message.Role, r.Model = "assistant", "assistant_output", "model", s.Model
	case transcript.RoleTool:
		r.Type, r.Provenance, r.Message.Role = "tool_result", "tool_result", "user"
	default:
		return nil, nil
	}
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			r.Message.Parts = append(r.Message.Parts, part{Text: b.Text})
		case transcript.BlockReasoning:
			r.Message.Parts = append(r.Message.Parts, part{Text: b.Text, Thought: true})
		case transcript.BlockToolUse:
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			r.Message.Parts = append(r.Message.Parts, part{FunctionCall: &functionCall{ID: b.ToolID, Name: b.Name, Args: args}})
		case transcript.BlockToolResult:
			resp, err := json.Marshal(struct {
				Output string `json:"output"`
			}{b.Text})
			if err != nil {
				return nil, err
			}
			r.Message.Parts = append(r.Message.Parts, part{FunctionResponse: &functionResponse{ID: b.ToolID, Name: b.Name, Response: resp}})
			status := "success"
			if b.Status == transcript.StatusError {
				status = "error"
			}
			r.ToolCallResult = &toolCallResult{CallID: b.ToolID, Status: status}
		}
	}
	if len(r.Message.Parts) == 0 {
		return nil, nil
	}
	return json.Marshal(r)
}

// ProjectDir is the directory Qwen keeps a working directory's sessions in,
// under ~/.qwen/projects.
func ProjectDir(cwd string) string { return transcript.SanitizedCwd.Name(cwd) }

// peek names the session's working directory, which the directory name
// holds only in a form that cannot be reversed, from the cwd its rows carry.
func peek(path string) (transcript.Info, error) {
	return transcript.Info{CWD: transcript.PeekField(path, "cwd", 64)}, nil
}

// hasCall reports whether an entry makes a tool call.
func hasCall(e transcript.Entry) bool {
	for _, b := range e.Content {
		if b.Kind == transcript.BlockToolUse {
			return true
		}
	}
	return false
}
