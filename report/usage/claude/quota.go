package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
)

// The quota check. A claude setup-token (inference scope only) cannot read
// /api/oauth/usage, but every Messages response to a Claude login carries
// the plan's windows in anthropic-ratelimit-unified-* headers. Claude Code
// reads them the same way: a one-token request it calls quota_check, whose
// headers it parses for the 5-hour and weekly windows. The check costs that
// one token against the plan.

// QuotaModel is the model the quota check asks: the smallest every Claude
// plan includes.
const QuotaModel = "claude-haiku-4-5"

// quotaWindows are the windows the headers report, by header infix:
// anthropic-ratelimit-unified-<infix>-utilization (a fraction, 0 to 1) and
// -reset (Unix seconds).
var quotaWindows = []struct {
	infix, id, label string
	kind             ap.UsageWindowKind
	minutes          int
}{
	{"5h", WindowFiveHour, "5-hour", ap.UsageWindowSession, 5 * 60},
	{"7d", WindowSevenDay, "Weekly", ap.UsageWindowWeekly, 7 * 24 * 60},
}

// QuotaCheck reads the plan's windows from the headers of a one-token
// Messages request to model ("" for QuotaModel). A refused request (a plan
// at its limit answers 429) still reports its windows; one whose headers
// report none is an error.
func QuotaCheck(ctx context.Context, client *http.Client, base, accessToken, model string) ([]ap.UsageWindow, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if base == "" {
		base = DefaultOAuthBase
	}
	if model == "" {
		model = QuotaModel
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "quota"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude: quota check: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	ws := QuotaWindows(resp.Header)
	if len(ws) == 0 {
		// The status alone: the body can name the account (see
		// webjson.StatusError).
		return nil, fmt.Errorf("claude: quota check: HTTP %d reported no windows", resp.StatusCode)
	}
	return ws, nil
}

// QuotaWindows maps a Messages response's anthropic-ratelimit-unified-*
// headers onto get_usage's windows.
func QuotaWindows(h http.Header) []ap.UsageWindow {
	var ws []ap.UsageWindow
	for _, q := range quotaWindows {
		util, err := strconv.ParseFloat(h.Get("anthropic-ratelimit-unified-"+q.infix+"-utilization"), 64)
		if err != nil {
			continue
		}
		w := ap.UsageWindow{ID: q.id, Kind: q.kind, Label: q.label, UsedPercent: util * 100, DurationMinutes: q.minutes}
		if secs, err := strconv.ParseInt(h.Get("anthropic-ratelimit-unified-"+q.infix+"-reset"), 10, 64); err == nil {
			w.ResetsAt = time.Unix(secs, 0).UTC()
		}
		ws = append(ws, w)
	}
	return ws
}
