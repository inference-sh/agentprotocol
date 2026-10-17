package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/driver"
	"github.com/inference-sh/agentprotocol/harness"
	"github.com/inference-sh/agentprotocol/llm"
	"github.com/inference-sh/agentprotocol/transcript"
)

// The direct probe's planted values: what the model must be given, and
// nothing that an agent would write on its own.
const (
	directSystem   = "You are the direct-mode probe. Follow the user's instructions exactly. Probe marker: KESTREL-58."
	directTool     = "probe_echo"
	directArg      = "ping-7"
	directResult   = "ECHO-RESULT-93"
	directAnswer   = "pong-done"
	directCodename = "OSPREY"
)

var directTools = []llm.Tool{{
	Name:        directTool,
	Description: "Echo the given text back.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to echo"}},"required":["text"]}`),
}}

// probeDirect checks direct mode (package llm): the agent launched to serve
// as a model, with the probe's system prompt and tool in place of its own.
//
// It reads what the model was sent, through the mock:
//
//   - direct.system: the system prompt is the probe's alone, or what the
//     agent adds to it;
//   - direct.tools: the tools are the probe's alone, or which of the
//     agent's own come with them, and the name the probe's tool is given;
//   - direct.call: a call to the tool comes back as the response, held;
//   - direct.result: the next call's tool result reaches the model as it
//     was given, on the same process;
//   - direct.restart: a conversation the agent never had is written as its
//     session and resumed, with the probe's tools.
//
// A harness with no Direct row (and no native direct model) is skipped:
// the probe measures a row, and a row is written from what it measured.
func (r *TestRunner) probeDirect() {
	h := r.harness
	fmt.Println("[probe] direct (the agent as a model: our system prompt and tools only)")
	model, visible, ok := r.directModel()
	if !ok {
		r.skip("direct.none", fmt.Sprintf("direct: %s has no direct row", h.Name))
		return
	}
	defer model.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// 1: the tool call.
	r.server.ClearLog()
	r.server.PrepareToolCall(visible, fmt.Sprintf(`{"text":%q}`, directArg), h.ToolCallPath)
	first := userEntry(fmt.Sprintf("Remember the project codename %s. Then call %s with the text %s.", directCodename, directTool, directArg))
	req := llm.Request{Key: "direct", System: directSystem, Tools: directTools, Messages: []transcript.Entry{first}}
	// As newSession: a session driver other than ACP is given the model,
	// pi's --provider needing a --model.
	if k := h.DriverKind(); k != harness.DriverACP && k != harness.DriverClaudeCode {
		req.Model = h.DefaultModel
	}
	r1, err := model.Generate(ctx, req, nil)
	r.directSent(visible)
	r.directMessages(first.Content[0].Text)
	if err != nil {
		r.fail("direct.call", fmt.Sprintf("direct: %s: %v", h.Name, err))
		return
	}
	calls := r1.ToolCalls()
	if len(calls) != 1 || calls[0].Name != directTool || !strings.Contains(string(calls[0].Input), directArg) {
		r.fail("direct.call", fmt.Sprintf("direct: %s answered %s with %+v, want one %s call with %s", h.Name, r1.StopReason, r1.Content, directTool, directArg))
		return
	}
	r.pass("direct.call", fmt.Sprintf("direct: %s returned the model's %s call, held (%s)", h.Name, directTool, calls[0].Input))

	// 2: its result, on the same process.
	r.server.ClearLog()
	r.server.SetResponse(directAnswer)
	req.Messages = append(req.Messages,
		transcript.Entry{Role: transcript.RoleAssistant, Content: r1.Content},
		transcript.Entry{Role: transcript.RoleTool, Content: []transcript.Block{{Kind: transcript.BlockToolResult, ToolID: calls[0].ToolID, Name: directTool, Text: directResult, Status: transcript.StatusOK}}})
	r2, err := model.Generate(ctx, req, nil)
	switch {
	case err != nil:
		r.fail("direct.result", fmt.Sprintf("direct: %s after the result: %v", h.Name, err))
		return
	case r2.Restarted:
		r.fail("direct.result", fmt.Sprintf("direct: %s restarted to answer the tool result; the held call was lost", h.Name))
	case !r.directCarried(directResult):
		r.fail("direct.result", fmt.Sprintf("direct: the model was not given the tool result %s", directResult))
	case !strings.Contains(r2.Text(), directAnswer):
		r.fail("direct.result", fmt.Sprintf("direct: %s answered %q after the result, want %q", h.Name, r2.Text(), directAnswer))
	default:
		r.pass("direct.result", fmt.Sprintf("direct: %s gave the model the tool result on the same process and returned %q", h.Name, r2.Text()))
	}

	// 3: the whole conversation on a new key: written, resumed.
	r.server.ClearLog()
	r.server.SetResponse(directCodename)
	req.Key = "direct-restart"
	req.Messages = append(req.Messages,
		transcript.Entry{Role: transcript.RoleAssistant, Content: r2.Content},
		userEntry("What was the project codename? Reply with it only."))
	r3, err := model.Generate(ctx, req, nil)
	switch {
	case err != nil:
		r.fail("direct.restart", fmt.Sprintf("direct: %s restart: %v", h.Name, err))
	case !r3.Restarted:
		r.fail("direct.restart", fmt.Sprintf("direct: %s continued a conversation it never had", h.Name))
	case !r.directCarried(directCodename) || !r.directCarried(directResult):
		r.fail("direct.restart", fmt.Sprintf("direct: %s resumed without the written history", h.Name))
	case !r.directOffered(visible):
		r.fail("direct.restart", fmt.Sprintf("direct: %s resumed without the probe's tool", h.Name))
	default:
		r.pass("direct.restart", fmt.Sprintf("direct: %s resumed the written conversation with the probe's tool", h.Name))
	}
}

// directModel builds the model under test, pointed at the mock, and the
// name its model is given for the probe's tool.
func (r *TestRunner) directModel() (llm.Model, string, bool) {
	h := r.harness
	diagnose := func(msg string) { fmt.Printf("  … %s\n", msg) }
	if h.DriverKind() == harness.DriverClaudeCode {
		return &llm.Claude{Command: h.Binary, Env: r.envIn(r.workDir()), Home: r.agentHome(), Stderr: os.Stderr}, directTool, true
	}
	// HARNESS_TEST_DIRECT names a JSON file holding a candidate Direct row,
	// tried in place of the registry's: a row is measured before it is
	// written.
	if path := os.Getenv("HARNESS_TEST_DIRECT"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("  … HARNESS_TEST_DIRECT: %v\n", err)
			return nil, "", false
		}
		h.Direct = &harness.Direct{}
		if err := json.Unmarshal(b, h.Direct); err != nil {
			fmt.Printf("  … HARNESS_TEST_DIRECT: %v\n", err)
			return nil, "", false
		}
		fmt.Printf("  … direct row from %s: %s\n", path, b)
	}
	if h.Direct == nil {
		return nil, "", false
	}
	backend := r.sessionBackend(false, false)
	m := &llm.Harness{
		Harness: h,
		Backend: func() (driver.Backend, error) { return backend(diagnose), nil },
		Home:    r.agentHome(),
		Settle:  2 * time.Second,
	}
	visible := directTool
	if h.Direct.ToolName != "" {
		visible = strings.NewReplacer("{{.Server}}", "tools", "{{.Tool}}", directTool).Replace(h.Direct.ToolName)
	}
	return m, visible, true
}

// directSent checks the system prompt and tools of the requests that
// carried the probe's prompt.
func (r *TestRunner) directSent(visible string) {
	name := r.harness.Name
	var main, side int
	var system string
	tools := map[string]bool{}
	for _, e := range r.server.Log() {
		if !strings.Contains(string(e.Body), "KESTREL-58") {
			side++
			continue
		}
		main++
		if s := systemTextIn(e.Body); len(s) > len(system) {
			system = s
		}
		for _, d := range r.server.ToolDeclarationsIn(e.Body) {
			tools[d] = true
		}
	}
	if main == 0 {
		r.fail("direct.system", fmt.Sprintf("direct: no request to the model carried the probe's system prompt (%d other requests)", side))
		return
	}
	if side > 0 {
		r.finding("direct.side-requests", fmt.Sprintf("direct: %s made %d model requests of its own beside the probe's", name, side))
	}
	switch extra := len(strings.TrimSpace(system)) - len(directSystem); {
	case !strings.Contains(system, directSystem):
		r.fail("direct.system", fmt.Sprintf("direct: the probe's system prompt is not in the system prompt %s sent", name))
	case extra <= 0:
		r.pass("direct.system:exact", fmt.Sprintf("direct: %s sent the probe's system prompt and nothing else", name))
	default:
		r.finding("direct.system:added", fmt.Sprintf("direct: %s added %d characters to the probe's system prompt: %.300q", name, extra, strings.Replace(system, directSystem, "…", 1)))
	}
	names := make([]string, 0, len(tools))
	for t := range tools {
		names = append(names, t)
	}
	sort.Strings(names)
	switch {
	case !tools[visible]:
		r.fail("direct.tools", fmt.Sprintf("direct: %s did not offer the probe's tool as %s; it offered %v", name, visible, names))
	case len(names) == 1:
		r.pass("direct.tools:only", fmt.Sprintf("direct: %s offered the probe's tool alone, as %s", name, visible))
	default:
		others := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == visible })
		r.finding("direct.tools:more", fmt.Sprintf("direct: %s offered %d tools of its own with the probe's: %v", name, len(others), others))
	}
}

// directCarried reports whether a request to the model since the last
// ClearLog holds s.
func (r *TestRunner) directCarried(s string) bool {
	for _, e := range r.server.Log() {
		if strings.Contains(string(e.Body), "KESTREL-58") && strings.Contains(string(e.Body), s) {
			return true
		}
	}
	return false
}

// directOffered reports whether a request since the last ClearLog declared
// the tool.
func (r *TestRunner) directOffered(tool string) bool {
	for _, e := range r.server.Log() {
		if slices.Contains(r.server.ToolDeclarationsIn(e.Body), tool) {
			return true
		}
	}
	return false
}

// systemTextIn is the system prompt a request carries, in any of the wire
// formats: Anthropic system, OpenAI system and developer messages,
// Responses instructions, Gemini systemInstruction.
func systemTextIn(body []byte) string {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	var parts []string
	var text func(any)
	text = func(v any) {
		switch t := v.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, x := range t {
				text(x)
			}
		case map[string]any:
			for _, k := range []string{"text", "content", "parts"} {
				if x, ok := t[k]; ok {
					text(x)
				}
			}
		}
	}
	text(doc["system"])
	text(doc["instructions"])
	text(doc["systemInstruction"])
	text(doc["system_instruction"])
	for _, key := range []string{"messages", "input"} {
		list, _ := doc[key].([]any)
		for _, m := range list {
			mm, _ := m.(map[string]any)
			if role, _ := mm["role"].(string); role == "system" || role == "developer" {
				text(mm["content"])
			}
		}
	}
	return strings.Join(parts, "\n")
}

func userEntry(text string) transcript.Entry {
	return transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: text}}}
}

// directMessages checks that the conversation the model was sent holds the
// probe's prompt and nothing the agent wrote into it: context files, session
// or environment blocks, reminders added to the user's turn.
func (r *TestRunner) directMessages(prompt string) {
	name := r.harness.Name
	var added string
	for _, e := range r.server.Log() {
		if !strings.Contains(string(e.Body), "KESTREL-58") {
			continue
		}
		rest := strings.TrimSpace(strings.Replace(conversationTextIn(e.Body), prompt, "", 1))
		if len(rest) > len(added) {
			added = rest
		}
	}
	if added == "" {
		r.pass("direct.messages:exact", fmt.Sprintf("direct: %s sent the probe's prompt and nothing else in the conversation", name))
		return
	}
	r.finding("direct.messages:added", fmt.Sprintf("direct: %s added %d characters to the conversation: %.300q", name, len(added), added))
}

// conversationTextIn is the text of a request's non-system messages, in
// any of the wire formats: what a user or tool turn says, not ids or names.
func conversationTextIn(body []byte) string {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	var parts []string
	var text func(any)
	text = func(v any) {
		switch t := v.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, x := range t {
				text(x)
			}
		case map[string]any:
			switch t["type"] {
			case "tool_use", "function_call", "image", "input_image", "thinking", "reasoning":
				return
			}
			for _, k := range []string{"text", "content", "parts", "output"} {
				if x, ok := t[k]; ok {
					text(x)
				}
			}
		}
	}
	for _, key := range []string{"messages", "input", "contents"} {
		list, _ := doc[key].([]any)
		for _, m := range list {
			mm, _ := m.(map[string]any)
			if role, _ := mm["role"].(string); role == "system" || role == "developer" || role == "assistant" || role == "model" {
				continue
			}
			text(m)
		}
	}
	return strings.Join(parts, "\n")
}
