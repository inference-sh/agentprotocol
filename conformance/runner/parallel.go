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
	tokA, tokB := "README-"+randHex(), "NOTES-"+randHex()
	os.WriteFile(readme, []byte(tokA+"\n"), 0o644)
	os.WriteFile(notes, []byte(tokB+"\n"), 0o644)

	turns := []struct {
		id    string
		calls []server.ParallelCall
		want  []string // token each call's result must hold, by call
	}{
		{"parallel.interleaved", []server.ParallelCall{{ID: "call_0", Args: args}, {ID: "call_1", Args: other}}, []string{tokA, tokB}},
		{"parallel.reused-ids", []server.ParallelCall{{ID: "call_0", Args: other}, {ID: "call_1", Args: args}}, []string{tokB, tokA}},
	}

	r.server.PrepareParallelToolCalls(name, turns[0].calls, r.harness.ToolCallPath)
	d, ok := r.startProbeTurn("parallel")
	if !ok {
		return
	}
	defer d.Close()
	marks := []int{r.entryCount()}

	r.server.PrepareParallelToolCalls(name, turns[1].calls, r.harness.ToolCallPath)
	answered := r.server.AnswersServed()
	if err := d.SendPrompt("Read both files again."); err != nil {
		r.skip("parallel.reused-ids:prompt", "parallel: second prompt: "+err.Error())
		turns = turns[:1]
	} else {
		d.WaitAnswered(r.server.AnswersServed, answered, 60*time.Second)
		d.WaitIdle(2 * time.Second)
	}
	marks = append(marks, r.entryCount())

	entries := r.entries()
	start := 0
	for i, t := range turns {
		end := min(marks[i], len(entries))
		r.reportParallelTurn(t.id, entries[start:end], t.calls, t.want, tokA, tokB)
		start = end
	}
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
	res := latestByID(got, calls)
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
