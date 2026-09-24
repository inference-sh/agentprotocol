// Package qwen reads and writes Qwen Code sessions.
//
// Qwen keeps one JSONL per session under
// ~/.qwen/projects/<mangled cwd>/chats/<session id>.jsonl, where the project
// directory is the working directory with every separator turned into a
// dash, as claude does. Rows form a tree through uuid and parentUuid. A row's
// type is user, assistant, tool_result or system; the first three carry a
// message whose parts are Gemini-shaped (text, functionCall,
// functionResponse). System rows (telemetry, snapshots, compression markers)
// stay opaque in the chain.
//
// Qwen forked Gemini CLI but not its session store: nothing here matches
// ~/.gemini/tmp.
package qwen

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Version is the Qwen Code version whose row shape this codec writes for a
// session that did not come from Qwen.
const Version = "0.24.4"

const root = ".qwen/projects"

// Codec is the Qwen Code session store.
var Codec = transcript.JSONL{
	Layout: transcript.Layout{
		Ext: ".jsonl",
		Files: func(home, cwd string) ([]string, error) {
			project := "*"
			if cwd != "" {
				project = transcript.MangledCwd.Name(cwd)
			}
			return transcript.Glob(filepath.Join(home, root, project, "chats", "*.jsonl"))
		},
		PathFor: func(home string, s *transcript.Session) string {
			return filepath.Join(home, root, transcript.MangledCwd.Name(s.CWD), "chats", s.ID+".jsonl")
		},
		Peek: peek,
	},
	Decode: decode,
	Encode: encode,
	Tree:   true,
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
	Provenance     string          `json:"provenance,omitempty"`
	CWD            string          `json:"cwd"`
	Version        string          `json:"version,omitempty"`
	GitBranch      string          `json:"gitBranch,omitempty"`
	Message        *message        `json:"message,omitempty"`
	Model          string          `json:"model,omitempty"`
	ToolCallResult *toolCallResult `json:"toolCallResult,omitempty"`
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
	status := transcript.StatusOK
	if r.ToolCallResult != nil && r.ToolCallResult.Status != "" && r.ToolCallResult.Status != "success" {
		status = transcript.StatusError
	}
	for _, p := range r.Message.Parts {
		switch {
		case p.FunctionCall != nil:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolUse, ToolID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Input: p.FunctionCall.Args})
		case p.FunctionResponse != nil:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockToolResult, ToolID: p.FunctionResponse.ID, Name: p.FunctionResponse.Name, Text: responseText(p.FunctionResponse.Response), Status: status})
		case p.Thought:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockReasoning, Text: p.Text})
		default:
			e.Content = append(e.Content, transcript.Block{Kind: transcript.BlockText, Text: p.Text})
		}
	}
	return e, true, nil
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
func ProjectDir(cwd string) string { return strings.TrimSpace(transcript.MangledCwd.Name(cwd)) }

// peek names the session's working directory, which the directory name
// holds only in a form that cannot be reversed, from the cwd its rows carry.
func peek(path string) (transcript.Info, error) {
	return transcript.Info{CWD: transcript.PeekField(path, "cwd", 64)}, nil
}
