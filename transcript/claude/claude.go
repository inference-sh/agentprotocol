// Package claude reads and writes Claude Code sessions.
//
// Claude keeps one JSONL per session under
// ~/.claude/projects/<mangled cwd>/<id>.jsonl. Rows form a tree through
// uuid and parentUuid, and the file's last row is normally the leaf. Message
// rows have type user or assistant and carry an Anthropic-shaped message.
// Every other row (system, attachment, mode, last-prompt, summary) is opaque
// but keeps its place in the chain.
package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Version is the Claude Code version whose row shape this codec writes for
// a session that did not come from Claude.
const Version = "2.1.278"

// Codec is the Claude Code session store.
var Codec = transcript.JSONL{
	Layout: transcript.Layout{
		Root:    ".claude/projects",
		Project: transcript.MangledCwd,
		Ext:     ".jsonl",
	},
	Decode: decode,
	Encode: encode,
	Tree:   true,
}

// Vendor is what a Claude session carries in Session.Vendor: the stamps
// Claude puts on every row, so a write reproduces them.
type Vendor struct {
	Version   string
	GitBranch string
	Model     string
}

// row is a Claude transcript line. Only the fields the codec reads or
// writes are named; a same-agent write emits the original bytes.
type row struct {
	ParentUUID  *string  `json:"parentUuid"`
	IsSidechain bool     `json:"isSidechain"`
	UserType    string   `json:"userType,omitempty"`
	CWD         string   `json:"cwd,omitempty"`
	SessionID   string   `json:"sessionId,omitempty"`
	Version     string   `json:"version,omitempty"`
	GitBranch   string   `json:"gitBranch,omitempty"`
	Type        string   `json:"type"`
	Message     *message `json:"message,omitempty"`
	UUID        string   `json:"uuid,omitempty"`
	Timestamp   string   `json:"timestamp,omitempty"`
}

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ID           string          `json:"id,omitempty"`
	Type         string          `json:"type,omitempty"`
	Model        string          `json:"model,omitempty"`
	StopReason   *string         `json:"stop_reason,omitempty"`
	StopSequence *string         `json:"stop_sequence,omitempty"`
	Usage        *usage          `json:"usage,omitempty"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// block is an Anthropic content block. Content is a string or an array of
// these; tool_result content is the same again, one level down.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func decode(raw json.RawMessage, s *transcript.Session) (transcript.Entry, bool, error) {
	var r row
	if err := json.Unmarshal(raw, &r); err != nil {
		return transcript.Entry{}, false, err
	}
	if r.UUID == "" {
		return transcript.Entry{}, false, nil
	}
	e := transcript.Entry{ID: r.UUID, Role: transcript.RoleOpaque}
	if r.ParentUUID != nil {
		e.ParentID = *r.ParentUUID
	}
	if r.Type != "user" && r.Type != "assistant" || r.IsSidechain || r.Message == nil {
		// Attachments and system rows sit in the parent chain between
		// messages, so they keep their links and stay opaque.
		return e, true, nil
	}
	if s.CWD == "" {
		s.CWD = r.CWD
	}
	if s.ID == "" {
		s.ID = r.SessionID
	}
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
	if r.Message.Model != "" {
		v.Model = r.Message.Model
	}
	if r.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			return transcript.Entry{}, false, fmt.Errorf("row %s: timestamp: %w", r.UUID, err)
		}
		e.Time = t
	}
	content, err := blocks(r.Message.Content)
	if err != nil {
		return transcript.Entry{}, false, fmt.Errorf("row %s: %w", r.UUID, err)
	}
	e.Content = content
	e.Role = transcript.Role(r.Message.Role)
	if e.Role == transcript.RoleUser && onlyToolResults(content) {
		e.Role = transcript.RoleTool
	}
	return e, true, nil
}

func onlyToolResults(content []transcript.Block) bool {
	if len(content) == 0 {
		return false
	}
	for _, b := range content {
		if b.Kind != transcript.BlockToolResult {
			return false
		}
	}
	return true
}

// blocks decodes an Anthropic content field.
func blocks(raw json.RawMessage) ([]transcript.Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []transcript.Block{{Kind: transcript.BlockText, Text: text}}, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	out := make([]transcript.Block, 0, len(bs))
	for _, b := range bs {
		switch b.Type {
		case "text":
			out = append(out, transcript.Block{Kind: transcript.BlockText, Text: b.Text})
		case "thinking":
			out = append(out, transcript.Block{Kind: transcript.BlockReasoning, Text: b.Thinking})
		case "tool_use":
			out = append(out, transcript.Block{Kind: transcript.BlockToolUse, ToolID: b.ID, Name: b.Name, Input: b.Input})
		case "tool_result":
			text, err := resultText(b.Content)
			if err != nil {
				return nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			st := transcript.StatusOK
			if b.IsError {
				st = transcript.StatusError
			}
			out = append(out, transcript.Block{Kind: transcript.BlockToolResult, ToolID: b.ToolUseID, Text: text, Status: st})
		}
	}
	return out, nil
}

// resultText flattens tool_result content, which is a string or an array
// of text blocks.
func resultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return "", err
	}
	for _, b := range bs {
		text += b.Text
	}
	return text, nil
}

// encode builds a Claude row for an entry from another agent, with the
// fields Claude writes itself on the version named above.
func encode(e transcript.Entry, s *transcript.Session) (json.RawMessage, error) {
	role := e.Role
	if role == transcript.RoleTool {
		role = transcript.RoleUser
	}
	if role != transcript.RoleUser && role != transcript.RoleAssistant {
		return nil, nil
	}
	v, _ := s.Vendor.(*Vendor)
	if v == nil {
		v = &Vendor{}
	}
	content := make([]block, 0, len(e.Content))
	for _, b := range e.Content {
		switch b.Kind {
		case transcript.BlockText:
			content = append(content, block{Type: "text", Text: b.Text})
		case transcript.BlockReasoning:
			// A thinking block carries a signature Claude will not accept
			// from anyone else, so foreign reasoning is left out rather than
			// forged.
		case transcript.BlockToolUse:
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage(`{}`)
			}
			content = append(content, block{Type: "tool_use", ID: b.ToolID, Name: b.Name, Input: in})
		case transcript.BlockToolResult:
			text, err := json.Marshal(b.Text)
			if err != nil {
				return nil, err
			}
			content = append(content, block{Type: "tool_result", ToolUseID: b.ToolID, Content: text, IsError: b.Status == transcript.StatusError})
		}
	}
	if len(content) == 0 {
		return nil, nil
	}
	cj, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	m := &message{Role: string(role), Content: cj}
	if role == transcript.RoleAssistant {
		m.ID = "msg_" + e.ID
		m.Type = "message"
		m.Model = v.Model
		m.Usage = &usage{}
	}
	t := e.Time
	if t.IsZero() {
		t = s.Updated
	}
	version := v.Version
	if version == "" {
		version = Version
	}
	r := row{
		IsSidechain: false,
		UserType:    "external",
		CWD:         s.CWD,
		SessionID:   s.ID,
		Version:     version,
		GitBranch:   v.GitBranch,
		Type:        string(role),
		Message:     m,
		UUID:        e.ID,
		Timestamp:   t.UTC().Format(time.RFC3339Nano),
	}
	if e.ParentID != "" {
		r.ParentUUID = &e.ParentID
	}
	return json.Marshal(r)
}
