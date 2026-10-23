package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"

	"github.com/inference-sh/agentprotocol/transcript"
)

// Hosted sessions. A host that drives Claude Code as a model (the agent
// SDK, agentprotocol's llm.Claude) gives it every tool through its own MCP
// server and resumes conversations it wrote itself. For the resumed request
// to start the way the live one did, which is what Anthropic's prompt cache
// keys on, the session has to hold what Claude Code would have recorded:
// tool results in the shape it records an MCP tool's, and the context
// attachments it records after the first prompt and renders into the
// system turn that follows it. Without them it renders that turn afresh at
// the end of the request, and nothing after the first prompt is cached.

// ContextTypes are the attachments Claude Code records after a session's
// first prompt and renders, together, into the system turn after it: where
// it runs (environment), as what (model), and the day. session_context
// carries nothing yet, but without it the environment is rendered at the
// end of the request instead (claude 2.1.309).
var ContextTypes = []string{"environment", "model", "session_context", "date"}

// WriteHosted writes s for a host whose tools are all MCP tools and returns
// the session id. attachments are recorded after the first user entry, in
// order, each the attachment object Claude Code writes ({"type":
// "environment", ...}); see ContextAttachments.
func WriteHosted(ctx context.Context, home string, s *transcript.Session, attachments []json.RawMessage) (string, error) {
	st, err := Codec.Open(home)
	if err != nil {
		return "", err
	}
	out := s.Portable().Lower(Codec.Caps)
	out.Agent = Codec.Agent
	v := &Vendor{hosted: true, attachments: map[string]json.RawMessage{}}
	out.Vendor = v
	if len(attachments) > 0 {
		if at := slices.IndexFunc(out.Entries, func(e transcript.Entry) bool { return e.Role == transcript.RoleUser }); at >= 0 {
			rows := make([]transcript.Entry, len(attachments))
			for i, a := range attachments {
				// A user entry to the writer, so it is linked into the
				// chain; encode writes it as the attachment.
				rows[i] = transcript.Entry{ID: transcript.NewUUID(), Role: transcript.RoleUser, Time: out.Entries[at].Time}
				v.attachments[rows[i].ID] = a
			}
			out.Entries = slices.Insert(out.Entries, at+1, rows...)
		}
	}
	return st.Write(ctx, out)
}

// textPart is a text block that keeps its text when it is empty, as an MCP
// tool's empty result does.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// attachmentRow is an attachment row as Claude Code writes one.
type attachmentRow struct {
	row
	Attachment json.RawMessage `json:"attachment"`
}

func encodeAttachment(e transcript.Entry, s *transcript.Session, v *Vendor, a json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(attachmentRow{row: stamped(e, s, v, "attachment"), Attachment: a})
}

// ContextAttachments reads the context attachments (ContextTypes) Claude
// Code recorded after the first prompt of the session at path, in order.
func ContextAttachments(path string) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	var out []json.RawMessage
	prompted := false
	for sc.Scan() {
		var r struct {
			Type       string          `json:"type"`
			Attachment json.RawMessage `json:"attachment"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Type {
		case "user":
			if prompted {
				return out, nil
			}
			prompted = true
		case "assistant":
			if prompted {
				return out, nil
			}
		case "attachment":
			var a struct {
				Type string `json:"type"`
			}
			if prompted && json.Unmarshal(r.Attachment, &a) == nil && slices.Contains(ContextTypes, a.Type) {
				out = append(out, bytes.Clone(r.Attachment))
			}
		}
	}
	return out, sc.Err()
}
