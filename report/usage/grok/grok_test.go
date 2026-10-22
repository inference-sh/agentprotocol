package grok

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

// This test binary doubles as grok: with fakeEnv set, TestMain speaks the
// slice of ACP a probe uses instead of running tests.
const (
	fakeEnv = "GROK_FAKE"
	logEnv  = "GROK_FAKE_LOG"
)

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fakeEnv); scenario != "" {
		os.Exit(fakeGrok(scenario, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// Answers as grok 1.0.67 gives them. Logged out (and with only XAI_API_KEY)
// check_subscription and billing answer as measured; the API-key and OIDC
// answers were measured with a cached token of that mode. The grok.com
// answers are built from the shapes in the binary and xAI's billing API.
const (
	loggedOut       = `{"authenticated":false,"meta":null}`
	apiKeyMeta      = `{"authenticated":true,"meta":{"email":"dev@example.com","auth_mode":"ApiKey","team_id":null,"is_team_principal":false,"is_organization_principal":false,"team_name":null,"is_zdr":false,"team_role":null,"coding_data_retention_opt_out":true,"can_administer_team":null,"show_resolved_model":null,"gate":null,"subscription_tier":"api_key","feedback_trace_offer":false,"backend_billed":false}}`
	oidcMeta        = `{"authenticated":true,"meta":{"email":"dev@corp.example","auth_mode":"Oidc","team_id":"team-9","team_name":null,"is_zdr":false,"subscription_tier":null,"backend_billed":false}}`
	superGrokMeta   = `{"authenticated":true,"meta":{"email":"dev@example.com","auth_mode":"Grok","team_id":null,"team_name":null,"is_zdr":false,"subscription_tier":"supergrok_heavy","backend_billed":false}}`
	billingNoLogin  = `{"code":-32000,"message":"Authentication required","data":"Authentication required to fetch billing data"}`
	billingNotGrok  = `{"code":-32000,"message":"Authentication required","data":"Billing data requires auth with grok.com. Run ` + "`grok login`" + ` to authenticate."}`
	billingUpstream = `{"code":-32000,"message":"billing: upstream error","data":"status 503"}`
	billing         = `{"config":{"creditUsagePercent":37.5,"currentPeriod":{"start":"2026-10-20T00:00:00Z","end":"2026-10-27T00:00:00Z"},
		"monthlyLimit":{"val":"0"},"onDemandCap":{"val":"1000"},"onDemandUsed":{"val":250},"prepaidBalance":{"val":5},
		"isUnifiedBillingUser":false,"billingPeriodStart":"2026-10-01T00:00:00Z","history":[]},
		"on_demand_enabled":true,"subscription_tier":"supergrok_heavy"}`
)

// fakeGrok is grok in the named scenario. Every ACP method it receives is
// appended to the file logEnv names.
func fakeGrok(scenario string, args []string) int {
	logf := func(s string) {
		if p := os.Getenv(logEnv); p != "" {
			f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			fmt.Fprintln(f, s)
			_ = f.Close()
		}
	}
	logf("args " + strings.Join(args, " "))
	if len(args) < 2 || args[0] != "agent" || args[1] != "stdio" {
		fmt.Fprintln(os.Stderr, "unexpected arguments", args)
		return 2
	}
	if scenario == "dies" {
		fmt.Fprintln(os.Stderr, "Error: failed to read ~/.grok/config.toml")
		return 1
	}
	in := bufio.NewScanner(os.Stdin)
	reply := func(id json.RawMessage, result string) {
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, strings.Join(strings.Fields(result), " "))
	}
	fail := func(id json.RawMessage, e string) {
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":%s}`+"\n", id, e)
	}
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		logf(msg.Method)
		switch msg.Method {
		case acp.MethodInitialize:
			reply(msg.ID, `{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"authMethods":[{"id":"grok.com","name":"Grok"}]}`)
			// grok notifies and asks things of its own right away.
			fmt.Println(`{"jsonrpc":"2.0","method":"_x.ai/mcp/servers_updated","params":{"mcpServers":[]}}`)
		case MethodCheckSubscription:
			switch scenario {
			case "logged-out":
				reply(msg.ID, loggedOut)
			case "api-key":
				reply(msg.ID, apiKeyMeta)
			case "oidc":
				reply(msg.ID, oidcMeta)
			default:
				reply(msg.ID, superGrokMeta)
			}
		case MethodBilling:
			switch scenario {
			case "logged-out":
				fail(msg.ID, billingNoLogin)
			case "oidc", "api-key":
				fail(msg.ID, billingNotGrok)
			case "billing-fails":
				fail(msg.ID, billingUpstream)
			default:
				reply(msg.ID, billing)
			}
		default:
			if len(msg.ID) > 0 {
				fail(msg.ID, `{"code":-32601,"message":"Method not found"}`)
			}
		}
	}
	return 0
}

// probe runs Prober against this binary as grok in scenario, and returns
// what reached it over ACP.
func probe(t *testing.T, scenario string, env ...string) (ap.UsageSnapshot, error, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "acp.log")
	base := []string{fakeEnv + "=" + scenario, logEnv + "=" + log, "HOME=" + t.TempDir()}
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Binary: exe, Env: append(base, env...)})
	got, _ := os.ReadFile(log)
	return snap, err, string(got)
}

func TestProbe_superGrok(t *testing.T) {
	snap, err, log := probe(t, "supergrok")
	if err != nil {
		t.Fatal(err)
	}
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: "dev@example.com", Plan: "supergrok_heavy", PlanLabel: "SuperGrok Heavy"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 2 {
		t.Fatalf("limits = %+v", l)
	}
	c, od := l.Windows[0], l.Windows[1]
	if c.ID != WindowCredits || c.Kind != ap.UsageWindowWeekly || c.UsedPercent != 37.5 || c.DurationMinutes != 7*24*60 ||
		!c.ResetsAt.Equal(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("credits = %+v", c)
	}
	if od.ID != WindowOnDemand || od.UsedPercent != 25 {
		t.Errorf("on-demand = %+v", od)
	}
	// No session, no prompt: initialize and the two extension methods only.
	if got := strings.Fields(log); strings.Join(got, " ") != "args agent stdio "+acp.MethodInitialize+" "+MethodCheckSubscription+" "+MethodBilling {
		t.Errorf("grok saw %q", log)
	}
}

func TestProbe_loggedOut(t *testing.T) {
	snap, err, log := probe(t, "logged-out")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || *snap.Account != (ap.AgentAccount{Status: ap.AccountUnauthenticated}) || snap.Limits != nil {
		t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
	}
	if strings.Contains(log, MethodBilling) {
		t.Errorf("billing asked while logged out: %q", log)
	}
}

func TestProbe_apiKey(t *testing.T) {
	for _, tc := range []struct {
		name, scenario string
		env            []string
		email          string
	}{
		{"cached key", "api-key", nil, "dev@example.com"},
		{"XAI_API_KEY without a login", "logged-out", []string{"XAI_API_KEY=xai-k"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap, err, log := probe(t, tc.scenario, tc.env...)
			if err != nil {
				t.Fatal(err)
			}
			want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey, Email: tc.email}
			if snap.Account == nil || *snap.Account != want {
				t.Fatalf("account = %+v", snap.Account)
			}
			if snap.Limits == nil || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageUnsupported {
				t.Fatalf("limits = %+v", snap.Limits)
			}
			if strings.Contains(log, MethodBilling) {
				t.Errorf("billing asked for an API key: %q", log)
			}
		})
	}
}

func TestProbe_enterpriseLoginHasNoBilling(t *testing.T) {
	snap, err, _ := probe(t, "oidc")
	if err != nil {
		t.Fatal(err)
	}
	if a := snap.Account; a == nil || a.Type != ap.AccountSubscription || a.Email != "dev@corp.example" || a.Workspace != "team-9" {
		t.Fatalf("account = %+v", a)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageUnsupported || !strings.Contains(u.Message, "grok.com login") {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_billingFailureKeepsTheAccount(t *testing.T) {
	snap, err, _ := probe(t, "billing-fails")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Plan != "supergrok_heavy" {
		t.Fatalf("account = %+v", snap.Account)
	}
	if u := snap.Limits.Unavailable; u == nil || u.Reason != ap.UsageProbeFailed || !strings.Contains(u.Message, "status 503") {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_errorWhenNothingIsRead(t *testing.T) {
	_, err, _ := probe(t, "dies")
	if err == nil || strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("err = %v", err)
	}
}

func TestWindowsOfAFreshPeriod(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	var r BillingConfigResponse
	if err := json.Unmarshal([]byte(`{"config":{"currentPeriod":{"start":"2026-10-01T00:00:00Z","end":"2026-11-01T00:00:00Z"},"onDemandCap":{"val":0}}}`), &r); err != nil {
		t.Fatal(err)
	}
	ws := Windows(r, now)
	if len(ws) != 1 || ws[0].UsedPercent != 0 || ws[0].Kind != ap.UsageWindowMonthly || ws[0].Label != "Monthly credits" {
		t.Errorf("fresh period = %+v", ws)
	}
	// An ended period without a percentage says nothing.
	if ws := Windows(r, now.AddDate(0, 1, 0)); len(ws) != 0 {
		t.Errorf("ended period = %+v", ws)
	}
}

func TestAmount(t *testing.T) {
	for raw, want := range map[string]Amount{`{"val":"12.5"}`: 12.5, `{"val":3}`: 3, `7`: 7, `null`: 0, `{}`: 0} {
		var a Amount
		if err := json.Unmarshal([]byte(raw), &a); err != nil || a != want {
			t.Errorf("%s = %v %v", raw, a, err)
		}
	}
}
