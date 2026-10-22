// Package acp maps what an ACP agent reports about usage onto the
// agentprotocol types. ACP's stable report is usage_update: the tokens in the
// session's context and the window's size. Per-turn token counts are a draft
// (the PromptResponse usage field, RFD end-turn-token-usage) whose meaning,
// per turn or per session and with or without cache, is still open, so they
// are not mapped. ACP has no account or rate-limit report; an ACP agent's
// login is the registry's status check.
package acp

import (
	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/protocol/acp"
)

// Context is the window's fill a usage_update reports.
func Context(u acp.UsageUpdate) ap.ContextUpdatedPayload {
	return ap.ContextUpdatedPayload{UsedTokens: int(u.Used), MaxTokens: int(u.Size)}
}
