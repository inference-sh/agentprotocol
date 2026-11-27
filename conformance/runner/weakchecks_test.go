package runner

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/conformance/server"
	"github.com/inference-sh/agentprotocol/harness"
)

// An agent that exits before the mock answers is a crash, not an answer.
func TestWaitAnsweredExitIsNotAnAnswer(t *testing.T) {
	d := NewSessionDriver(nil, t.TempDir())
	close(d.done)
	if err := d.WaitAnswered(func() int { return 0 }, 0, time.Second); !errors.Is(err, errExitedUnanswered) {
		t.Fatalf("exit with no answer: got %v, want errExitedUnanswered", err)
	}
	if err := d.WaitAnswered(func() int { return 1 }, 0, time.Second); err != nil {
		t.Fatalf("exit after an answer: %v", err)
	}
}

// A one-shot turn that prints and exits without the prompt reaching the
// model fails, however it exits.
func TestOneShotTurnNeedsThePrompt(t *testing.T) {
	for _, c := range []struct{ name, script, want string }{
		{"crash with a trace", "echo 'panic: boom'; exit 2", "fail"},
		{"clean exit, prompt never sent", "echo hello", "fail:prompt-not-sent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := harness.Harness{Name: "x", HeadlessCmd: []string{"sh", "-c", c.script, "sh"}}
			r := &TestRunner{harness: h, home: t.TempDir(), testEntries: []server.LogEntry{}}
			r.runOneShot("headless", h.HeadlessCmd, nil)
			if len(r.result.Checks) != 1 || r.result.Checks[0].Key() != c.want {
				t.Fatalf("checks = %+v, want one %s", r.result.Checks, c.want)
			}
		})
	}
}

func TestPromptSentSince(t *testing.T) {
	r := &TestRunner{testEntries: []server.LogEntry{
		{Body: []byte(`{"messages":[{"content":"` + promptText + `"}]}`)},
		{Body: []byte(`{"title":"side"}`)},
	}}
	if !r.promptSentSince(0, promptText) {
		t.Error("prompt in the first request not found")
	}
	if r.promptSentSince(1, promptText) {
		t.Error("a request before the mark counted")
	}
	if !strings.Contains(compactionSummary, compactionSummaryMark) || strings.Contains(promptText, compactionSummaryMark) {
		t.Error("compactionSummaryMark must be in the summary and not in the prompt")
	}
}
