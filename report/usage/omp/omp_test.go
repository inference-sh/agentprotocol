package omp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol/model"
	"github.com/inference-sh/agentprotocol/report/usage"
)

// fakeOMP writes an omp that records its arguments to args.txt and prints
// out, as `omp usage --json` would.
func fakeOMP(t *testing.T, out string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "omp")
	argsFile = filepath.Join(dir, "args.txt")
	if err := os.WriteFile(filepath.Join(dir, "out.json"), []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$@\" > '" + argsFile + "'\ncat '" + filepath.Join(dir, "out.json") + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

// Two providers signed in, shaped as omp 18.12 prints them.
const twoProviders = `{
  "generatedAt": 1792682719311,
  "reports": [
    {
      "provider": "openai-codex",
      "fetchedAt": 1792682719000,
      "limits": [
        {"id": "primary", "label": "5 Hour", "scope": {"provider": "openai-codex", "accountId": "acct", "windowId": "5h", "tier": "plus"},
         "window": {"id": "5h", "label": "5 Hour", "durationMs": 18000000, "resetsAt": 1792700000000},
         "amount": {"used": 30, "limit": 100, "usedFraction": 0.3, "unit": "percent"}, "status": "ok"},
        {"id": "secondary", "label": "7 Day", "scope": {"provider": "openai-codex", "windowId": "7d"},
         "window": {"id": "7d", "label": "7 Day", "durationMs": 604800000},
         "amount": {"remainingFraction": 0.25, "unit": "percent"}, "status": "warning"},
        {"id": "credits", "label": "Credits", "scope": {"provider": "openai-codex"}, "amount": {"unit": "credits"}}
      ],
      "metadata": {"planType": "plus", "email": "dev@example.com", "accountId": "acct"}
    },
    {
      "provider": "kimi-code",
      "fetchedAt": 1792682719000,
      "limits": [
        {"id": "kimi-code:0", "label": "Monthly total", "scope": {"provider": "kimi-code", "windowId": "limit_month_total", "shared": true},
         "window": {"id": "limit_month_total", "label": "Monthly total"},
         "amount": {"used": 10, "limit": 100, "unit": "percent"}}
      ],
      "metadata": {"accountId": "kimi-acct"}
    }
  ],
  "accountsWithoutUsage": [{"provider": "anthropic", "type": "api_key"}],
  "disabledCredentials": [],
  "capacity": {}
}`

func TestProbe_firstReport(t *testing.T) {
	bin, argsFile := fakeOMP(t, twoProviders)
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Binary: bin, Args: []string{"--model", "gpt-5", "--profile", "work"}})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if got := strings.TrimSpace(string(args)); got != "--profile work usage --json" {
		t.Fatalf("args = %q", got)
	}
	want := ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountSubscription, Email: "dev@example.com", Plan: "plus"}
	if snap.Account == nil || *snap.Account != want {
		t.Fatalf("account = %+v", snap.Account)
	}
	l := snap.Limits
	if l == nil || l.Unavailable != nil || len(l.Windows) != 2 {
		t.Fatalf("limits = %+v", l)
	}
	five, week := l.Windows[0], l.Windows[1]
	if five.ID != "openai-codex:primary" || five.Kind != ap.UsageWindowSession || five.UsedPercent != 30 || five.DurationMinutes != 300 ||
		!five.ResetsAt.Equal(time.UnixMilli(1792700000000)) || five.Label != "5 Hour" {
		t.Fatalf("5h = %+v", five)
	}
	if week.ID != "openai-codex:secondary" || week.Kind != ap.UsageWindowWeekly || week.UsedPercent != 75 {
		t.Fatalf("7d = %+v", week)
	}
}

func TestProbe_sessionProvider(t *testing.T) {
	bin, argsFile := fakeOMP(t, twoProviders)
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Binary: bin, Args: []string{"--model", "kimi-code/kimi-for-coding"}})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	if got := strings.TrimSpace(string(args)); got != "usage --json --provider kimi-code" {
		t.Fatalf("args = %q", got)
	}
	if snap.Account == nil || snap.Account.Email != "" || len(snap.Limits.Windows) != 1 {
		t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
	}
	if w := snap.Limits.Windows[0]; w.ID != "kimi-code:kimi-code:0" || w.Kind != ap.UsageWindowMonthly || w.UsedPercent != 10 {
		t.Fatalf("window = %+v", w)
	}

	// The fake ignores --provider; Snapshot does the filtering too.
	snap, err = Prober{}.Probe(context.Background(), usage.Env{Binary: bin, Args: []string{"--provider", "anthropic"}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Type != ap.AccountAPIKey || snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageUnsupported {
		t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
	}
}

func TestProbe_loginWithoutUsage(t *testing.T) {
	// A login whose usage read failed (measured with a revoked Kimi token).
	bin, _ := fakeOMP(t, `{"generatedAt":1,"reports":[],"accountsWithoutUsage":[{"provider":"kimi-code","type":"oauth","email":"dev@example.com","accountId":"acct-1"}],"disabledCredentials":[],"capacity":{}}`)
	snap, err := Prober{}.Probe(context.Background(), usage.Env{Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Account == nil || snap.Account.Email != "dev@example.com" || snap.Account.Type != ap.AccountSubscription {
		t.Fatalf("account = %+v", snap.Account)
	}
	if snap.Limits.Unavailable == nil || snap.Limits.Unavailable.Reason != ap.UsageProbeFailed {
		t.Fatalf("limits = %+v", snap.Limits)
	}
}

func TestProbe_loggedOut(t *testing.T) {
	// omp 18.12.6 with no logins (measured).
	bin, _ := fakeOMP(t, `{"generatedAt":1792682637642,"reports":[],"accountsWithoutUsage":[],"disabledCredentials":[],"capacity":{}}`)
	for _, tc := range []struct {
		env  []string
		want ap.AgentAccount
	}{
		// Nothing saved could still be a key in omp's config: unknown.
		{[]string{"HOME=/nonexistent"}, ap.AgentAccount{Status: ap.AccountUnknown}},
		{[]string{"HOME=/nonexistent", "OPENAI_API_KEY=sk-x"}, ap.AgentAccount{Status: ap.AccountAuthenticated, Type: ap.AccountAPIKey}},
	} {
		snap, err := Prober{}.Probe(context.Background(), usage.Env{Binary: bin, Env: tc.env})
		if err != nil {
			t.Fatal(err)
		}
		if snap.Account == nil || *snap.Account != tc.want || snap.Limits == nil || snap.Limits.Unavailable.Reason != ap.UsageUnsupported {
			t.Fatalf("snap = %+v %+v", snap.Account, snap.Limits)
		}
	}
}

func TestProbe_failure(t *testing.T) {
	bin, _ := fakeOMP(t, "not json")
	if _, err := (Prober{}).Probe(context.Background(), usage.Env{Binary: bin}); err == nil {
		t.Fatal("no error for unreadable output")
	}
	if _, err := (Prober{}).Probe(context.Background(), usage.Env{Binary: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("no error for a missing binary")
	}
}
