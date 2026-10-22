package kiro

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/protocol/acp"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// This test binary doubles as kiro-cli: with fakeEnv set, TestMain answers
// whoami and speaks the slice of ACP a probe uses instead of running tests.
const (
	fakeEnv = "KIRO_FAKE"
	logEnv  = "KIRO_FAKE_LOG"
)

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fakeEnv); scenario != "" {
		os.Exit(fakeKiro(scenario, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// usageResult is /usage over ACP as kiro-cli 2.33.1 answers it.
const usageResult = `{"success":true,"message":"Plan: KIRO PRO+","data":{"planName":"KIRO PRO+","billingCycleReset":"2026-11-02","overagesEnabled":true,"isEnterprise":false,
	"usageBreakdowns":[{"resourceType":"CREDIT","displayName":"Credits","used":123.45,"limit":1000.0,"percentage":12.345,"currentOverages":0.0,"overageRate":0.04,"overageCharges":0.0,"currency":"USD","hasLimit":true},
		{"resourceType":"SPEC","displayName":"Specs","used":3,"limit":0,"percentage":0,"hasLimit":false}],
	"bonusCredits":[{"name":"Welcome bonus","used":7.5,"total":50.0,"daysUntilExpiry":3}],"addOnCredits":[],"overageCapable":true}}`

// fakeKiro is kiro-cli in the named scenario. Every ACP method it receives is
// appended to the file logEnv names.
func fakeKiro(scenario string, args []string) int {
	logf := func(s string) {
		if p := os.Getenv(logEnv); p != "" {
			f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			fmt.Fprintln(f, s)
			_ = f.Close()
		}
	}
	if len(args) > 0 && args[0] == "whoami" {
		switch scenario {
		case "logged-out":
			fmt.Println(`{"account":null}`)
			return 1
		case "api-key":
			fmt.Println(`{"accountType":"ApiKey","email":null}`)
		case "whoami-broken":
			fmt.Fprintln(os.Stderr, "error: database is locked")
			return 2
		default:
			fmt.Println(`{"accountType":"BuilderId","email":"dev@example.com"}`)
		}
		return 0
	}
	if len(args) == 0 || args[0] != "acp" {
		fmt.Fprintln(os.Stderr, "unexpected arguments", args)
		return 2
	}
	logf("args " + strings.Join(args, " "))
	if scenario == "acp-dies" {
		fmt.Fprintln(os.Stderr, "error: You are not logged in, please log in with kiro-cli login")
		return 1
	}
	in := bufio.NewScanner(os.Stdin)
	reply := func(id json.RawMessage, result string) {
		result = strings.Join(strings.Fields(result), " ")
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
	}
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		logf(msg.Method + " " + string(msg.Params))
		switch msg.Method {
		case acp.MethodInitialize:
			reply(msg.ID, `{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"authMethods":[]}`)
		case acp.MethodSessionNew:
			reply(msg.ID, `{"sessionId":"sess-1"}`)
		case MethodCommandsExecute:
			var p executeParams
			_ = json.Unmarshal(msg.Params, &p)
			switch {
			case p.SessionID != "sess-1" || p.Command.Command != "usage":
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"bad params"}}`+"\n", msg.ID)
			case scenario == "usage-fails":
				reply(msg.ID, `{"success":false,"message":"Failed to fetch usage: throttled"}`)
			case scenario == "no-limits":
				reply(msg.ID, `{"success":true,"message":"","data":{"planName":"KIRO ENTERPRISE","usageBreakdowns":[{"resourceType":"CREDIT","displayName":"Credits","used":5,"hasLimit":false}],"bonusCredits":[]}}`)
			default:
				reply(msg.ID, usageResult)
			}
		default:
			if len(msg.ID) > 0 {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`+"\n", msg.ID)
			}
		}
	}
	return 0
}

// probe runs Prober against this binary as kiro-cli in scenario, and returns
// what reached it over ACP.
func probe(t *testing.T, scenario string) (ap.UsageSnapshot, error, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "acp.log")
	snap, err := Prober{}.Probe(context.Background(), usage.Env{
		Binary: exe, Args: []string{"--agent", "belt"}, Dir: t.TempDir(),
		Env: append(os.Environ(), fakeEnv+"="+scenario, logEnv+"="+log),
	})
	got, _ := os.ReadFile(log)
	return snap, err, string(got)
}

func TestProbe_subscription(t *testing.T) {
	snap, err, log := probe(t, "pro")
	if err != nil {
		t.Fatal(err)
	}
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Plan: "pro_plus", PlanLabel: "KIRO PRO+", Email: "dev@example.com"}
	if *snap.Account != want {
		t.Errorf("account = %+v, want %+v", *snap.Account, want)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 2 {
		t.Fatalf("limits = %+v", l)
	}
	c, b := l.Windows[0], l.Windows[1]
	if c.ID != "credit" || c.Kind != ap.UsageWindowMonthly || c.Label != "Credits" || c.UsedPercent != 12.345 ||
		!c.ResetsAt.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("credits = %+v", c)
	}
	if b.ID != "bonus:welcome_bonus" || b.Kind != ap.UsageWindowOther || b.Label != "Welcome bonus" || b.UsedPercent != 15 {
		t.Errorf("bonus = %+v", b)
	}
	if !strings.HasPrefix(log, "args acp --agent belt\n") {
		t.Errorf("acp started as %q, want the session's args after acp", strings.SplitN(log, "\n", 2)[0])
	}
	if strings.Contains(log, acp.MethodSessionPrompt) {
		t.Errorf("the probe prompted:\n%s", log)
	}
	if !strings.Contains(log, `"command":{"command":"usage","args":{}}`) {
		t.Errorf("no usage command:\n%s", log)
	}
}

func TestProbe_apiKeyReadsThePlanToo(t *testing.T) {
	snap, err, _ := probe(t, "api-key")
	if err != nil {
		t.Fatal(err)
	}
	if a := *snap.Account; a.Status != ap.AccountAuthenticated || a.Type != ap.AccountAPIKey || a.Email != "" || a.PlanLabel != "KIRO PRO+" {
		t.Errorf("account = %+v", a)
	}
	if snap.Limits == nil || len(snap.Limits.Windows) != 2 {
		t.Errorf("limits = %+v", snap.Limits)
	}
}

func TestProbe_loggedOutStartsNoACP(t *testing.T) {
	snap, err, log := probe(t, "logged-out")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Status != ap.AccountUnauthenticated || snap.Limits != nil {
		t.Errorf("snapshot = %+v %+v", snap.Account, snap.Limits)
	}
	if log != "" {
		t.Errorf("acp started while logged out:\n%s", log)
	}
}

func TestProbe_limitsFailKeepTheAccount(t *testing.T) {
	for scenario, msg := range map[string]string{
		"usage-fails": "throttled",
		"acp-dies":    "stream ended",
	} {
		snap, err, _ := probe(t, scenario)
		if err != nil {
			t.Fatalf("%s: %v", scenario, err)
		}
		if snap.Account == nil || snap.Account.Email != "dev@example.com" {
			t.Errorf("%s: account = %+v", scenario, snap.Account)
		}
		l := snap.Limits
		if l == nil || l.Unavailable == nil || l.Unavailable.Reason != ap.UsageProbeFailed || !strings.Contains(l.Unavailable.Message, msg) {
			t.Errorf("%s: limits = %+v %+v", scenario, l, l.Unavailable)
		}
	}
}

func TestProbe_noMeteredLimits(t *testing.T) {
	snap, err, _ := probe(t, "no-limits")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account.Plan != "enterprise" || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageUnsupported {
		t.Errorf("snapshot = %+v %+v", snap.Account, snap.Limits)
	}
}

func TestProbe_errorWhenNothingIsRead(t *testing.T) {
	if _, err, _ := probe(t, "whoami-broken"); err == nil || strings.Contains(err.Error(), "database is locked") {
		t.Errorf("err = %v", err)
	}
	if _, err := (Prober{}).Probe(context.Background(), usage.Env{Binary: filepath.Join(t.TempDir(), "kiro-cli")}); err == nil {
		t.Error("a missing binary probed without error")
	}
}

func TestPlan(t *testing.T) {
	for name, want := range map[string]string{"KIRO PRO+": "pro_plus", "KIRO FREE": "free", "Kiro Power": "power", "KIRO PRO": "pro"} {
		if id, label := Plan(name); id != want || label != name {
			t.Errorf("Plan(%q) = %q, %q", name, id, label)
		}
	}
}

func TestWindowsFromUsedAndLimit(t *testing.T) {
	ws := Windows(UsageData{UsageBreakdowns: []UsageBreakdown{{ResourceType: "AGENTIC_REQUEST", Used: 25, Limit: 50, HasLimit: true}}})
	if len(ws) != 1 || ws[0].ID != "agentic_request" || ws[0].UsedPercent != 50 || ws[0].Label != "AGENTIC_REQUEST" || !ws[0].ResetsAt.IsZero() {
		t.Errorf("windows = %+v", ws)
	}
}

func TestRemoveSessionFindsItByDirectory(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".kiro", "sessions", "cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := "/tmp/kiro-usage-123"
	for name, body := range map[string]string{
		"probe.json":  `{"cwd":"/tmp/kiro-usage-123","messages":[]}`,
		"probe.jsonl": ``,
		"user.json":   `{"cwd":"/home/dev/project"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removeSession(usage.Env{Env: []string{"HOME=" + home}}, "", cwd)
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "user.json" {
		t.Fatalf("left = %v", left)
	}
}
