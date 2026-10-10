package server

import (
	"fmt"
	"net/http"
	"time"
)

func newChat(id, object, model string, choices []ChatChoice, usage *Usage) ChatCompletion {
	return ChatCompletion{
		ID: id, Object: object, Created: time.Now().Unix(),
		Model: model, Choices: choices, Usage: usage,
	}
}

func (s *MockServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	req, body, idx := s.parseRequest(r)
	model := req.modelOrDefault()

	if s.shouldToolCall(req.hasTools() && s.bodyOffersTool(body), r.URL.Path) {
		if req.Stream {
			s.markStreamed(idx)
		}
		s.chatToolCall(w, model, req.Stream, req.wantsStreamUsage())
		return
	}

	text := s.getResponse()
	if req.Stream {
		s.markStreamed(idx)
		s.chatStream(w, model, text, req.wantsStreamUsage())
		return
	}

	writeJSON(w, newChat("mock-1", "chat.completion", model,
		[]ChatChoice{{
			Message:      &ChatMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
		&Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	))
}

func (s *MockServer) chatStream(w http.ResponseWriter, model, text string, usage bool) {
	f := beginSSE(w)
	streamData(w, f, newChat("mock-1", "chat.completion.chunk", model,
		[]ChatChoice{{Delta: &ChatMessage{Role: "assistant", Content: text}}}, nil))
	streamData(w, f, newChat("mock-1", "chat.completion.chunk", model,
		[]ChatChoice{{Delta: &ChatMessage{}, FinishReason: "stop"}}, nil))
	streamUsage(w, f, "mock-1", model, usage)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f != nil {
		f.Flush()
	}
}

func (s *MockServer) chatToolCall(w http.ResponseWriter, model string, stream, usage bool) {
	name, args := s.getToolCall()
	tc := ToolCall{
		Index:    0,
		ID:       "call_mock_1",
		Type:     "function",
		Function: FunctionCall{Name: name, Arguments: args},
	}

	if stream {
		f := beginSSE(w)
		streamData(w, f, newChat("mock-tc", "chat.completion.chunk", model,
			[]ChatChoice{{Delta: &ChatMessage{Role: "assistant", ToolCalls: []ToolCall{tc}}}}, nil))
		streamData(w, f, newChat("mock-tc", "chat.completion.chunk", model,
			[]ChatChoice{{Delta: &ChatMessage{}, FinishReason: "tool_calls"}}, nil))
		streamUsage(w, f, "mock-tc", model, usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f != nil {
			f.Flush()
		}
		return
	}

	writeJSON(w, newChat("mock-tc", "chat.completion", model,
		[]ChatChoice{{
			Message:      &ChatMessage{Role: "assistant", ToolCalls: []ToolCall{tc}},
			FinishReason: "tool_calls",
		}},
		&Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	))
}

// streamUsage sends the usage chunk a streamed request asked for with
// stream_options.include_usage: no choices, and the request's usage, before
// [DONE]. Without the option the API sends none.
func streamUsage(w http.ResponseWriter, f http.Flusher, id, model string, asked bool) {
	if !asked {
		return
	}
	streamData(w, f, newChat(id, "chat.completion.chunk", model, []ChatChoice{},
		&Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}))
}
