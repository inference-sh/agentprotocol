package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/inference-sh/agentprotocol/conformance/server"
)

// checkCachePrefix asks whether the agent keeps its prompt cacheable across a
// tool call. Providers cache a prompt by its prefix: the request that carries
// the tool's result has to start with everything the request that asked for
// the tool sent, unchanged, or the cache misses from the first change on and
// the whole conversation is billed again. An agent that rewrites its system
// prompt, reorders its tools, or edits an earlier message every step pays
// that on every step, and nothing else in this suite would notice.
//
// The comparison is on parsed JSON. Providers render the prompt from the
// parsed request, so key order and whitespace do not reach the cache; what
// does is any change to a value. Anthropic's cache_control markers are left
// out: they mark where to cache, and claude moves the last one every turn by
// design.
func (r *TestRunner) checkCachePrefix(phase string, entries []server.LogEntry) {
	asked := -1
	for i, e := range entries {
		if e.ToolCall {
			asked = i
			break
		}
	}
	if asked < 0 {
		return
	}
	fmt.Printf("[check] cache prefix (%s)\n", phase)
	first := entries[asked]
	shape, ok := promptShapeFor(first.Path)
	if !ok {
		r.skip("cache-prefix:unknown-api", fmt.Sprintf("%s: no prompt layout known for %s", phase, first.Path))
		return
	}
	answered := -1
	for i := asked + 1; i < len(entries); i++ {
		if entries[i].Path == first.Path && bytes.Contains(entries[i].Body, []byte(shape.resultMarker)) {
			answered = i
			break
		}
	}
	if answered < 0 {
		r.skip("cache-prefix:no-result", fmt.Sprintf("%s: no request carried the tool's result back, nothing to compare", phase))
		return
	}
	next := entries[answered]
	if shape.incremental != "" && jsonField(next.Body, shape.incremental) != "" {
		r.pass("cache-prefix:incremental", fmt.Sprintf("%s: %s sent only the new items (%s), the earlier ones stay on the server", phase, r.harness.Name, shape.incremental))
		return
	}
	if where, before, after, ok := shape.prefixBreak(first.Body, next.Body); !ok {
		r.finding("cache-prefix", fmt.Sprintf("%s: %s changed %s between the tool call and its result, so the prompt cache misses from there (before: %s; after: %s)",
			phase, r.harness.Name, where, truncate(before, 160), truncate(after, 160)))
		return
	}
	r.pass("cache-prefix", fmt.Sprintf("%s: the request with the tool's result starts with the request that asked for it, unchanged", phase))
}

// promptShape is where one API keeps the parts of a prompt a cache keys on,
// in the order the provider renders them.
type promptShape struct {
	fixed        []string // parts sent whole each time: system prompt, tools
	list         string   // the conversation, which may only grow at the end
	resultMarker string   // text only the request carrying the mock's tool result contains
	incremental  string   // a field that, when set, means only new items were sent
	dropCache    bool     // leave out cache_control markers
}

func promptShapeFor(path string) (promptShape, bool) {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return promptShape{fixed: []string{"tools"}, list: "messages", resultMarker: "call_mock_1"}, true
	case strings.HasSuffix(path, "/messages"):
		return promptShape{fixed: []string{"system", "tools"}, list: "messages", resultMarker: "toolu_mock_1", dropCache: true}, true
	case strings.HasSuffix(path, "/responses"):
		return promptShape{fixed: []string{"instructions", "tools"}, list: "input", resultMarker: "call_mock_1", incremental: "previous_response_id"}, true
	case strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent"):
		return promptShape{fixed: []string{"systemInstruction", "tools"}, list: "contents", resultMarker: `"functionResponse"`}, true
	}
	return promptShape{}, false
}

// prefixBreak reports the first part of before that after does not repeat,
// with both values, or ok when after starts with all of before.
func (p promptShape) prefixBreak(beforeBody, afterBody []byte) (where, before, after string, ok bool) {
	var b, a map[string]json.RawMessage
	if json.Unmarshal(beforeBody, &b) != nil || json.Unmarshal(afterBody, &a) != nil {
		return "the request body (not JSON)", "", "", false
	}
	for _, k := range p.fixed {
		x, y := p.canonical(b[k]), p.canonical(a[k])
		if x != y {
			return k, x, y, false
		}
	}
	var bl, al []json.RawMessage
	json.Unmarshal(b[p.list], &bl)
	json.Unmarshal(a[p.list], &al)
	for i := range bl {
		if i >= len(al) {
			return fmt.Sprintf("%s (%d items, then %d)", p.list, len(bl), len(al)), "", "", false
		}
		x, y := p.canonical(bl[i]), p.canonical(al[i])
		if x != y {
			return fmt.Sprintf("%s[%d]", p.list, i), x, y, false
		}
	}
	return "", "", "", true
}

// canonical re-encodes a JSON value with sorted keys, so two values compare
// equal when they mean the same thing.
func (p promptShape) canonical(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	if p.dropCache {
		v = dropKey(v, "cache_control")
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func dropKey(v any, key string) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, key)
		for k, x := range t {
			t[k] = dropKey(x, key)
		}
	case []any:
		for i, x := range t {
			t[i] = dropKey(x, key)
		}
	}
	return v
}

// jsonField is a top-level string field of a JSON object, or "".
func jsonField(body []byte, field string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var s string
	json.Unmarshal(m[field], &s)
	return s
}
