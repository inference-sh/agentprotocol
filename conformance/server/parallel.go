package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ParallelCall is one of several calls of the prepared tool the mock answers a
// single request with.
type ParallelCall struct {
	ID   string // tool call id; Gemini has none and pairs results by order
	Args string // JSON arguments
}

// PrepareParallelToolCalls makes the next request that offers the tool get
// all of calls in one answer, the way a model calls tools in parallel.
//
// Streamed, the arguments arrive the way OpenAI sends them: every call opened
// first, then each call's arguments in pieces, the pieces of different calls
// interleaved. A client that appends deltas to "the current call" instead of
// to the call its index names builds two broken argument strings. Anthropic
// streams one block after another, so its calls are sequential, each split.
//
// The ids are whatever the caller chooses. Some OpenAI-compatible providers
// number calls per turn (call_0, call_1) and repeat those ids every turn, so a
// client that keys tool state by id across turns mixes up results.
func (s *MockServer) PrepareParallelToolCalls(name string, calls []ParallelCall, path string) {
	s.PrepareToolCallIn("", name, calls[0].Args, path)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parallel = append([]ParallelCall(nil), calls...)
}

// ParallelServed is how many requests were answered with parallel calls since
// the last ClearLog.
func (s *MockServer) ParallelServed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parallelServed
}

// takeParallel returns the prepared parallel calls, once.
func (s *MockServer) takeParallel() []ParallelCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.parallel
	s.parallel = nil
	if len(calls) > 0 {
		s.parallelServed++
	}
	return calls
}

// halves splits arguments into two pieces, neither of them valid JSON.
func halves(args string) (string, string) {
	n := len(args) / 2
	return args[:n], args[n:]
}

func (s *MockServer) chatParallel(w http.ResponseWriter, model, name string, calls []ParallelCall, stream, usage bool) {
	if !stream {
		var tcs []ToolCall
		for i, c := range calls {
			tcs = append(tcs, ToolCall{Index: i, ID: c.ID, Type: "function", Function: FunctionCall{Name: name, Arguments: c.Args}})
		}
		writeJSON(w, newChat("mock-ptc", "chat.completion", model,
			[]ChatChoice{{Message: &ChatMessage{Role: "assistant", ToolCalls: tcs}, FinishReason: "tool_calls"}},
			&Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}))
		return
	}
	f := beginSSE(w)
	// Raw chunks, because the API leaves fields out of a delta rather than
	// sending them empty: only a call's first chunk has its id and name, and a
	// client that overwrote the name with every delta's would lose it.
	chunk := func(tc map[string]any) {
		streamData(w, f, map[string]any{
			"id": "mock-ptc", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{tc}}}},
		})
	}
	for i, c := range calls {
		chunk(map[string]any{"index": i, "id": c.ID, "type": "function", "function": map[string]any{"name": name, "arguments": ""}})
	}
	for half := 0; half < 2; half++ {
		for i, c := range calls {
			a, b := halves(c.Args)
			piece := a
			if half == 1 {
				piece = b
			}
			chunk(map[string]any{"index": i, "function": map[string]any{"arguments": piece}})
		}
	}
	streamData(w, f, newChat("mock-ptc", "chat.completion.chunk", model,
		[]ChatChoice{{Delta: &ChatMessage{}, FinishReason: "tool_calls"}}, nil))
	streamUsage(w, f, "mock-ptc", model, usage)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f != nil {
		f.Flush()
	}
}

func (s *MockServer) responsesParallel(w http.ResponseWriter, namespace, name string, calls []ParallelCall) {
	const rid = "mock-resp-ptc"
	empty := ""
	events := []sseEvent{typed("response.created", map[string]any{"response": newResponse(rid, "in_progress", nil, nil)})}
	var done []ResponseItem
	for i, c := range calls {
		args := c.Args
		events = append(events, typed("response.output_item.added", map[string]any{
			"response_id": rid, "output_index": i, "item": ResponseItem{
				Type: "function_call", ID: fmt.Sprintf("fc_mock_p%d", i), CallID: c.ID,
				Name: name, Namespace: namespace, Arguments: &empty, Status: "in_progress",
			},
		}))
		done = append(done, ResponseItem{
			Type: "function_call", ID: fmt.Sprintf("fc_mock_p%d", i), CallID: c.ID,
			Name: name, Namespace: namespace, Arguments: &args, Status: "completed",
		})
	}
	for half := 0; half < 2; half++ {
		for i, c := range calls {
			a, b := halves(c.Args)
			piece := a
			if half == 1 {
				piece = b
			}
			events = append(events, typed("response.function_call_arguments.delta", map[string]any{
				"response_id": rid, "item_id": fmt.Sprintf("fc_mock_p%d", i), "output_index": i, "delta": piece,
			}))
		}
	}
	for i, c := range calls {
		events = append(events,
			typed("response.function_call_arguments.done", map[string]any{
				"response_id": rid, "item_id": fmt.Sprintf("fc_mock_p%d", i), "output_index": i, "arguments": c.Args,
			}),
			typed("response.output_item.done", map[string]any{
				"response_id": rid, "output_index": i, "item": done[i],
			}))
	}
	events = append(events, typed("response.completed", map[string]any{
		"response": newResponse(rid, "completed", done, &Usage{InputTokens: 10, OutputTokens: 12, TotalTokens: 22}),
	}))
	streamSSEEvents(w, events)
}

func (s *MockServer) anthropicParallel(w http.ResponseWriter, model, name string, calls []ParallelCall, stream bool) {
	if !stream {
		var blocks []ContentBlock
		for _, c := range calls {
			var input any
			json.Unmarshal([]byte(c.Args), &input)
			blocks = append(blocks, ContentBlock{Type: "tool_use", ID: c.ID, Name: name, Input: input})
		}
		writeJSON(w, AnthropicMessage{
			ID: "mock-ptc", Type: "message", Role: "assistant", Model: model,
			Content: blocks, StopReason: "tool_use", Usage: &AntUsage{InputTokens: 10, OutputTokens: 5},
		})
		return
	}
	events := []sseEvent{{"message_start", map[string]any{
		"type":    "message_start",
		"message": AnthropicMessage{ID: "mock-ptc", Type: "message", Role: "assistant", Model: model, Usage: streamStartUsage()},
	}}}
	for i, c := range calls {
		a, b := halves(c.Args)
		events = append(events,
			sseEvent{"content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": ContentBlock{Type: "tool_use", ID: c.ID, Name: name},
			}},
			sseEvent{"content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]string{"type": "input_json_delta", "partial_json": a},
			}},
			sseEvent{"content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]string{"type": "input_json_delta", "partial_json": b},
			}},
			sseEvent{"content_block_stop", map[string]any{"type": "content_block_stop", "index": i}},
		)
	}
	events = append(events,
		sseEvent{"message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}, "usage": AntUsage{OutputTokens: 5},
		}},
		sseEvent{"message_stop", map[string]any{"type": "message_stop"}},
	)
	streamSSEEvents(w, events)
}

func (s *MockServer) geminiParallel(w http.ResponseWriter, model, name string, calls []ParallelCall) {
	var parts []geminiPart
	for _, c := range calls {
		var args map[string]any
		json.Unmarshal([]byte(c.Args), &args)
		parts = append(parts, geminiPart{FunctionCall: &geminiFuncCall{Name: name, Args: args}})
	}
	streamDataOnly(w, []any{
		geminiResponse{
			Candidates:   []geminiCandidate{{Content: geminiContent{Role: "model", Parts: parts}}},
			ModelVersion: model,
		},
		geminiResponse{
			Candidates:    []geminiCandidate{{Content: geminiContent{Role: "model", Parts: []geminiPart{{Text: ""}}}, FinishReason: "STOP"}},
			UsageMetadata: &geminiUsage{PromptTokenCount: 10, CandidatesTokenCount: 12, TotalTokenCount: 22},
			ModelVersion:  model,
		},
	})
}
