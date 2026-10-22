// Package kiro reads kiro-cli's account and plan limits. The account is what
// `kiro-cli whoami --format json` prints; the limits are what the CLI's own
// /usage shows, which kiro serves over ACP as the _kiro.dev/commands/execute
// extension: a short-lived `kiro-cli acp` opens a session, runs the usage
// command in it and exits, with no prompt and so no model call.
package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// DefaultBinary is the CLI a probe runs when usage.Env names none.
const DefaultBinary = "kiro-cli"

// Account types, as whoami's accountType names them (kiro-cli 2.33.1). The
// social logins (Google, GitHub) report names of their own.
const (
	AccountAPIKey      = "ApiKey"
	AccountBuilderID   = "BuilderId"
	AccountIdentityCtr = "IamIdentityCenter"
	AccountExternalIdP = "ExternalIdp"
)

// MethodCommandsExecute runs one of kiro's slash commands in a session.
const MethodCommandsExecute = "_kiro.dev/commands/execute"

// ProbeTimeout bounds a probe: whoami, then the ACP handshake, a session and
// the usage command, which asks kiro's backend. kiro-cli 2.33.1 took 15 to
// 18 seconds to answer initialize and 3 more for session/new in Docker
// (with an API key, network up).
const ProbeTimeout = 45 * time.Second

// Prober reads the account from whoami and the limits from /usage run over
// ACP. It sends no prompt, so no model is called, and it never touches the
// login kiro saved; the CLI reads and refreshes it as it does for a session.
type Prober struct {
	// ClientInfo names the caller to kiro. Empty uses "agentprotocol".
	ClientInfo acp.ClientInfo
}

func (p Prober) Probe(ctx context.Context, env usage.Env) (ap.UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	bin := env.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	w, err := whoami(ctx, bin, env)
	if err != nil {
		return ap.UsageSnapshot{}, err
	}
	acct := Account(w)
	snap := ap.UsageSnapshot{Account: &acct}
	if acct.Status != ap.AccountAuthenticated {
		return snap, nil
	}
	now := time.Now()
	res, err := p.usage(ctx, bin, env)
	if err != nil {
		snap.Limits = usage.Unavailable(ap.UsageProbeFailed, err.Error(), now)
		return snap, nil
	}
	if res.PlanName != "" {
		acct.Plan, acct.PlanLabel = Plan(res.PlanName)
	}
	ws := Windows(res)
	if len(ws) == 0 {
		snap.Limits = usage.Unavailable(ap.UsageUnsupported, "the plan reports no metered limits", now)
		return snap, nil
	}
	snap.Limits = usage.Limits(ws, now)
	return snap, nil
}

// Whoami is `kiro-cli whoami --format json`. Logged out kiro prints
// {"account":null} and exits 1.
type Whoami struct {
	AccountType string `json:"accountType"`
	Email       string `json:"email"`
}

func whoami(ctx context.Context, bin string, env usage.Env) (Whoami, error) {
	cmd := exec.CommandContext(ctx, bin, "whoami", "--format", "json")
	// A child that keeps the output open does not hold the probe past ctx.
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = env.Dir
	cmd.Env = env.Env
	out, err := cmd.Output()
	var w struct {
		Whoami
		Account json.RawMessage `json:"account"`
	}
	if json.Unmarshal(out, &w) == nil {
		switch {
		case err == nil && w.AccountType != "":
			return w.Whoami, nil
		case w.AccountType == "" && string(w.Account) == "null":
			// Logged out, whatever the exit status.
			return Whoami{}, nil
		}
	}
	if err != nil {
		return Whoami{}, fmt.Errorf("kiro: whoami: %w", err)
	}
	return Whoami{}, fmt.Errorf("kiro: whoami: unexpected output %q", strings.TrimSpace(string(out)))
}

// Account is the account whoami reports. An API key is a Pro+ subscriber's
// key, billed against that plan, but it is still a key, not a login.
func Account(w Whoami) ap.AgentAccount {
	switch w.AccountType {
	case "":
		return ap.AgentAccount{Status: ap.AccountUnauthenticated}
	case AccountAPIKey:
		return ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey, Email: w.Email}
	}
	return ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: w.Email}
}

// Plan is the plan's id and display name from /usage's planName ("KIRO
// PRO+"): the id is the name without the brand, lowercased, + spelled
// "_plus" ("pro_plus"); the label is the name as kiro shows it.
func Plan(name string) (id, label string) {
	label = strings.TrimSpace(name)
	id = strings.ToLower(label)
	id = strings.TrimPrefix(id, "kiro ")
	id = strings.ReplaceAll(id, "+", " plus")
	return slug(id), label
}

// Windows maps /usage: one monthly window per resource with a limit, reset
// with the billing cycle, and one per bonus credit grant, which lasts until
// it expires rather than a cycle.
func Windows(r UsageData) []ap.UsageWindow {
	var reset time.Time
	for _, layout := range []string{time.DateOnly, time.RFC3339} {
		if t, err := time.Parse(layout, r.BillingCycleReset); err == nil {
			reset = t.UTC()
			break
		}
	}
	var ws []ap.UsageWindow
	for _, b := range r.UsageBreakdowns {
		if !b.HasLimit || b.ResourceType == "" {
			continue
		}
		pct := b.Percentage
		if pct == 0 && b.Limit > 0 {
			pct = b.Used / b.Limit * 100
		}
		label := b.DisplayName
		if label == "" {
			label = b.ResourceType
		}
		ws = append(ws, ap.UsageWindow{
			ID: strings.ToLower(b.ResourceType), Kind: ap.UsageWindowMonthly, Label: label,
			UsedPercent: pct, ResetsAt: reset,
		})
	}
	for _, b := range r.BonusCredits {
		if b.Name == "" || b.Total <= 0 {
			continue
		}
		ws = append(ws, ap.UsageWindow{
			ID: "bonus:" + slug(b.Name), Kind: ap.UsageWindowOther, Label: b.Name,
			UsedPercent: b.Used / b.Total * 100,
		})
	}
	return ws
}

// The usage command's result over ACP (kiro-cli 2.33.1), the fields a probe
// reads.
type (
	CommandResult struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	UsageData struct {
		PlanName          string           `json:"planName"`
		BillingCycleReset string           `json:"billingCycleReset"`
		OveragesEnabled   bool             `json:"overagesEnabled"`
		IsEnterprise      bool             `json:"isEnterprise"`
		UsageBreakdowns   []UsageBreakdown `json:"usageBreakdowns"`
		BonusCredits      []BonusCredit    `json:"bonusCredits"`
	}
	UsageBreakdown struct {
		ResourceType string  `json:"resourceType"`
		DisplayName  string  `json:"displayName"`
		Used         float64 `json:"used"`
		Limit        float64 `json:"limit"`
		Percentage   float64 `json:"percentage"`
		HasLimit     bool    `json:"hasLimit"`
	}
	BonusCredit struct {
		Name            string  `json:"name"`
		Used            float64 `json:"used"`
		Total           float64 `json:"total"`
		DaysUntilExpiry int     `json:"daysUntilExpiry"`
	}
)

type executeParams struct {
	SessionID string  `json:"sessionId"`
	Command   command `json:"command"`
}

type command struct {
	Command string         `json:"command"`
	Args    map[string]any `json:"args"`
}

// usage runs /usage in a new session of a short-lived `kiro-cli acp`. kiro
// saves every new session, empty ones too (~/.kiro/sessions/cli/<id>.json,
// listed by `kiro-cli chat --list-sessions`), so once kiro has exited the
// probe removes the session it made. The session is rooted in an empty
// directory removed afterwards, so one left behind is never listed with the
// user's projects.
func (p Prober) usage(ctx context.Context, bin string, env usage.Env) (UsageData, error) {
	info := p.ClientInfo
	if info.Name == "" {
		info = acp.ClientInfo{Name: "agentprotocol", Version: "1"}
	}
	cwd, err := os.MkdirTemp("", "kiro-usage-")
	if err != nil {
		return UsageData{}, err
	}
	defer func() { _ = os.RemoveAll(cwd) }()
	dir := env.Dir
	if dir == "" {
		dir = cwd
	}
	proc, err := acp.Spawn(ctx, acp.ProcessConfig{
		Command: bin, Args: append([]string{"acp"}, env.Args...), Dir: dir, Env: env.Env,
	}, info, acp.Handler{})
	if err != nil {
		return UsageData{}, fmt.Errorf("kiro: %w", err)
	}
	// kiro keeps running after session/close and exits on the end of its
	// input, removing the session's .lock as it does; a kill leaves the lock.
	proc.ShutdownGrace = -1
	var sid string
	defer func() {
		_ = proc.Wait()
		removeSession(env, sid, cwd)
	}()
	sid, err = proc.NewSession(ctx, cwd, nil)
	if err != nil {
		return UsageData{}, fmt.Errorf("kiro: %w", err)
	}
	raw, err := proc.Call(ctx, MethodCommandsExecute, executeParams{SessionID: sid, Command: command{Command: "usage", Args: map[string]any{}}})
	if err != nil {
		return UsageData{}, fmt.Errorf("kiro: %w", err)
	}
	var res CommandResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return UsageData{}, fmt.Errorf("kiro: /usage: %w", err)
	}
	if !res.Success {
		msg := res.Message
		if msg == "" {
			msg = "failed"
		}
		return UsageData{}, fmt.Errorf("kiro: /usage: %s", msg)
	}
	var data UsageData
	if len(res.Data) == 0 || string(res.Data) == "null" {
		return UsageData{}, errors.New("kiro: /usage returned no data")
	}
	if err := json.Unmarshal(res.Data, &data); err != nil {
		return UsageData{}, fmt.Errorf("kiro: /usage: %w", err)
	}
	return data, nil
}

// removeSession deletes the files kiro saved for the probe's session. When
// the probe ran out of time before session/new answered, sid is unknown, and
// the session is found by the directory it was rooted in, which only the
// probe used.
func removeSession(env usage.Env, sid, cwd string) {
	dir := filepath.Join(env.Home(), ".kiro", "sessions", "cli")
	if sid == "" {
		sid = sessionIn(dir, cwd)
	}
	if sid == "" || strings.ContainsAny(sid, `/\`) || strings.Contains(sid, "..") {
		return
	}
	for _, ext := range []string{".json", ".jsonl", ".lock"} {
		_ = os.Remove(filepath.Join(dir, sid+ext))
	}
}

// sessionIn is the session saved in dir whose metadata names cwd, among those
// written in the last ProbeTimeout.
func sessionIn(dir, cwd string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	quoted, _ := json.Marshal(cwd)
	since := time.Now().Add(-2 * ProbeTimeout)
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(since) || info.Size() > 1<<20 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err == nil && bytes.Contains(raw, quoted) {
			return name
		}
	}
	return ""
}

// slug is s lowercased, with each run of other characters one "_".
func slug(s string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if sep && b.Len() > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			sep = false
			continue
		}
		sep = true
	}
	return b.String()
}
