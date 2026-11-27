package runner

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/conformance/server"
)

// probeParallel asks whether the agent survives what a real model stream does
// to tool calls and this mock otherwise never does.
//
// Turn one answers with two calls of the agent's file tool at once, one for
// README.md and one for NOTES.md, their argument deltas interleaved. Turn two
// does it again with the same ids, call_0 and call_1, and the files swapped,
// the way providers that number calls per turn reuse ids. Each file holds a
// token of its own, so the request that returns the results says, from the
// files on disk, whether each call ran with its own arguments and whether its
// result went back under its own id.
func (r *TestRunner) probeParallel() {
	if r.harness.DriverKind() == "" || r.server == nil {
		return
	}
	fmt.Println("[probe] parallel (two interleaved tool calls, then the same ids again)")
	name, args := r.harness.ToolCallName, r.harness.ToolCallArgs
	if o, ok := r.harness.ToolCallByMode[ModeACP]; ok {
		name, args = o.Name, o.Args
	}
	if name == "" {
		// The same default armToolCall falls back to (claude relies on it).
		// An agent that does not declare it is never served, and the skip
		// says so with the tools it did declare.
		name, args = server.DefaultToolName, server.DefaultToolArgs
	}
	args = r.expand(args)
	if !strings.Contains(args, "README.md") {
		r.skip("parallel:no-file-tool", fmt.Sprintf("parallel: %s's tool call (%s %s) reads no file to swap", r.harness.Name, name, args))
		return
	}
	other := strings.ReplaceAll(args, "README.md", "NOTES.md")

	dir := r.workDir()
	readme, notes := filepath.Join(dir, "README.md"), filepath.Join(dir, "NOTES.md")
	old, oldErr := os.ReadFile(readme)
	defer func() {
		os.Remove(notes)
		if oldErr == nil {
			os.WriteFile(readme, old, 0o644)
		}
	}()
	// Each turn writes new tokens: an agent that remembers what it read
	// answers a repeat read of an unchanged file from memory (qwen: "Wasted
	// call — file NOTES.md unchanged"), which says nothing about the ids.
	plant := func() (tokReadme, tokNotes string) {
		tokReadme, tokNotes = "README-"+randHex(), "NOTES-"+randHex()
		os.WriteFile(readme, []byte(tokReadme+"\n"), 0o644)
		os.WriteFile(notes, []byte(tokNotes+"\n"), 0o644)
		return
	}

	ra, na := plant()
	first := []server.ParallelCall{{ID: "call_0", Args: args}, {ID: "call_1", Args: other}}
	r.server.PrepareParallelToolCalls(name, first, r.harness.ToolCallPath)
	d, ok := r.startProbeTurn("parallel")
	if !ok {
		return
	}
	defer d.Close()
	mark := r.entryCount()
	r.reportParallelTurn("parallel.interleaved", r.entries()[:mark], first, []string{ra, na}, ra, na)

	rb, nb := plant()
	second := []server.ParallelCall{{ID: "call_0", Args: other}, {ID: "call_1", Args: args}}
	r.server.PrepareParallelToolCalls(name, second, r.harness.ToolCallPath)
	answered := r.server.AnswersServed()
	if err := d.SendPrompt("Read both files again."); err != nil {
		r.skip("parallel.reused-ids:prompt", "parallel: second prompt: "+err.Error())
		return
	}
	d.WaitAnswered(r.server.AnswersServed, answered, 60*time.Second)
	d.WaitIdle(2 * time.Second)
	entries := r.entries()
	r.reportParallelTurn("parallel.reused-ids", entries[min(mark, len(entries)):], second, []string{nb, rb}, rb, nb)
}

// reportParallelTurn reads one turn's requests: the one answered with the
// parallel calls, then the first after it that carries their results.
func (r *TestRunner) reportParallelTurn(label string, entries []server.LogEntry, calls []server.ParallelCall, want []string, tokA, tokB string) {
	name := r.harness.Name
	asked := -1
	for i, e := range entries {
		if e.ToolCall {
			asked = i
			break
		}
	}
	if asked < 0 {
		r.skip(label+":not-served", fmt.Sprintf("parallel: no request in this turn was answered with the calls (%d request(s) to %s; tools declared: %s)",
			len(entries), strings.Join(distinctPaths(entries), " "), strings.Join(declaredIn(entries), " ")))
		return
	}
	// The history before the calls already holds results, the earlier turn's
	// under the same ids, so only results past the asking request's count.
	prev := len(toolResults(entries[asked].Body))
	var got []toolResult
	for _, e := range entries[asked+1:] {
		if e.Path != entries[asked].Path {
			continue
		}
		if res := toolResults(e.Body); len(res) > prev {
			got = res[prev:]
			break
		}
	}
	if got == nil {
		r.finding(label+":no-result", fmt.Sprintf("parallel: %s sent no new tool results after the two calls, so it did not run them", name))
		return
	}
	// A tool that failed for both calls says nothing about pairing: codex's
	// sandbox cannot start in CI, and both results are the same bwrap error.
	anyToken := false
	for _, g := range got {
		anyToken = anyToken || strings.Contains(g.text, tokA) || strings.Contains(g.text, tokB)
	}
	if !anyToken {
		r.skip(label+":tool-failed", fmt.Sprintf("parallel: %s ran the calls and neither result holds a file's contents (%s)", name, truncate(strings.Join(strings.Fields(got[0].text), " "), 300)))
		return
	}
	res := latestByID(got, calls)
	// An agent may give the calls ids of its own: droid rewrites every id,
	// kimi and qwen rename an id the conversation already used. The results
	// are then paired with the calls by position, which is the order the
	// agent received them in.
	renamed := false
	if _, ok := res[calls[0].ID]; !ok && len(got) == len(calls) {
		renamed = true
		for i, c := range calls {
			res[c.ID] = got[i].text
		}
	}
	var wrong []string
	swapped := true
	for i, c := range calls {
		text := res[c.ID]
		other := tokB
		if want[i] == tokB {
			other = tokA
		}
		if !strings.Contains(text, want[i]) {
			wrong = append(wrong, fmt.Sprintf("%s: %s", c.ID, truncate(strings.Join(strings.Fields(text), " "), 300)))
		}
		swapped = swapped && strings.Contains(text, other) && !strings.Contains(text, want[i])
	}
	switch {
	case len(wrong) == 0 && renamed:
		r.pass(label+":renamed-ids", fmt.Sprintf("parallel: %s gave the calls ids of its own (%q, %q) and returned each call's result in order", name, got[0].id, got[1].id))
	case len(wrong) == 0:
		r.pass(label, fmt.Sprintf("parallel: %s returned each call's result under its own id", name))
	case swapped:
		r.finding(label+":swapped", fmt.Sprintf("parallel: %s returned each call's result under the other's id (%s)", name, strings.Join(wrong, "; ")))
	default:
		var ids []string
		for _, g := range got {
			ids = append(ids, fmt.Sprintf("%q", g.id))
		}
		r.finding(label+":wrong", fmt.Sprintf("parallel: %s did not return each call's own result (%s; result ids sent: %s)", name, strings.Join(wrong, "; "), strings.Join(ids, " ")))
	}
}

func distinctPaths(entries []server.LogEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if !seen[e.Path] {
			seen[e.Path] = true
			out = append(out, e.Path)
		}
	}
	return out
}

func declaredIn(entries []server.LogEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		for _, n := range server.ToolNamesIn(e.Body) {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// toolResult is one tool result found in a request, in request order.
type toolResult struct {
	id, text string
}

// toolResults finds every tool result in a request body, whatever the API:
// chat's tool messages, Responses' function_call_output items, Anthropic's
// tool_result blocks, and Gemini's functionResponse parts, which carry no id.
func toolResults(body []byte) []toolResult {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	var out []toolResult
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			typ, _ := t["type"].(string)
			switch {
			case t["tool_call_id"] != nil:
				out = append(out, toolResult{str(t["tool_call_id"]), compact(t["content"])})
			case typ == "function_call_output":
				out = append(out, toolResult{str(t["call_id"]), compact(t["output"])})
			case typ == "tool_result":
				out = append(out, toolResult{str(t["tool_use_id"]), compact(t["content"])})
			case t["functionResponse"] != nil:
				out = append(out, toolResult{"", compact(t["functionResponse"])})
			default:
				for _, k := range sortedKeys(t) {
					walk(t[k])
				}
			}
		case []any:
			for _, y := range t {
				walk(y)
			}
		}
	}
	walk(v)
	return out
}

// latestByID maps each call to the last new result sent under its id. Gemini
// results have no id and are matched by position, in call order.
func latestByID(got []toolResult, calls []server.ParallelCall) map[string]string {
	m := map[string]string{}
	n := 0
	for _, g := range got {
		if g.id == "" {
			if n < len(calls) {
				m[calls[n].ID] = g.text
			}
			n++
			continue
		}
		m[g.id] = g.text
	}
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// compact is a result's content as text: a string as is, anything else as JSON.
func compact(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(bytes.TrimSpace(b))
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Request order is what matters and JSON objects have none; arrays keep
	// theirs, which is where conversations live.
	sort.Strings(keys)
	return keys
}

func randHex() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
