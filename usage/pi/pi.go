// Package pi maps what pi (and Oh My Pi in its rpc mode) reports about usage
// onto the agentprotocol types: the context window's fill from
// get_session_stats. A turn's tokens come from the assistant messages'
// usage, which the pi driver sums. pi reports no account or rate limits; its
// login is the registry's status check.
package pi

import (
	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/pirpc"
)

// Context is the window's fill get_session_stats reports; ok is false when pi
// does not know it, as right after a compaction.
func Context(s pirpc.SessionStats) (ap.ContextUpdatedPayload, bool) {
	c := s.ContextUsage
	if c == nil || c.Tokens == nil {
		return ap.ContextUpdatedPayload{}, false
	}
	return ap.ContextUpdatedPayload{UsedTokens: *c.Tokens, MaxTokens: c.ContextWindow}, true
}

// Turn is a turn's usage from the assistant messages' usage summed over the
// turn. pi counts input without the cache, so the prompt adds the cache
// reads and writes back.
func Turn(u pirpc.Usage, status ap.TurnUsageStatus) ap.UsageUpdatedPayload {
	prompt := u.Input + u.CacheRead + u.CacheWrite
	return ap.UsageUpdatedPayload{
		PromptTokens:        prompt,
		CompletionTokens:    u.Output,
		TotalTokens:         prompt + u.Output,
		CachedInputTokens:   u.CacheRead,
		CacheCreationTokens: u.CacheWrite,
		ReasoningTokens:     min(u.Reasoning, u.Output),
		CostUSD:             u.Cost.Total,
		Status:              status,
	}
}
