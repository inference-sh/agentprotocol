package acp_test

import (
	"context"
	"testing"
	"time"

	"github.com/inference-sh/agentprotocol/acp"
)

// initialize completes only the handshake, leaving the session unopened so a
// test can choose between session/new and session/load.
func initialize(t *testing.T, c *acp.Client, a *fakeAgent, result map[string]any) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.Initialize(context.Background()); err != nil {
			t.Errorf("initialize: %v", err)
		}
	}()
	init := a.next()
	a.reply(*init.ID, result)
	<-done
}

func TestInitializeExposesWhatTheAgentSaidItCanDo(t *testing.T) {
	c, a := newPair(t, acp.Handler{})

	initialize(t, c, a, map[string]any{
		"protocolVersion":   1,
		"agentInfo":         map[string]any{"name": "kimi", "version": "3.2"},
		"agentCapabilities": map[string]any{"loadSession": true},
		"authMethods":       []any{map[string]any{"id": "oauth", "name": "Sign in"}},
	})

	if got := c.AgentInfo().Name; got != "kimi" {
		t.Errorf("agent name = %q, want kimi", got)
	}
	if !c.CanLoadSession() {
		t.Error("agent advertised loadSession and CanLoadSession is false")
	}
	methods := c.AuthMethods()
	if len(methods) != 1 || methods[0].ID != "oauth" {
		t.Errorf("auth methods = %+v, want one with id oauth", methods)
	}
	// Spawn performs the handshake itself, so a caller that used it can only
	// reach the result through the client.
	if c.InitializeRaw() == nil {
		t.Error("initialize result was not kept")
	}
}

// An agent that declares nothing reads as a zero capability set, and nothing
// in the library may turn that into a refusal. None of the twelve agents
// measured is silent — all twelve declare loadSession and one still fails
// every load — so the claim predicts nothing in either direction.
func TestSilentAgentIsNotTreatedAsRefusing(t *testing.T) {
	c, a := newPair(t, acp.Handler{})
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	if c.CanLoadSession() {
		t.Error("agent said nothing; CanLoadSession should be false")
	}
	if c.AgentCapabilities() != (acp.AgentCapabilities{}) {
		t.Error("capabilities should be zero when the agent declares none")
	}
}

func TestLoadSessionReplaysHistoryAndAnswers(t *testing.T) {
	var updates []acp.UpdateNotification
	c, a := newPair(t, acp.Handler{
		OnUpdate: func(n acp.UpdateNotification) { updates = append(updates, n) },
	})
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	type outcome struct {
		res acp.LoadResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := c.LoadSession(context.Background(), "ses_f647af", "/repo", nil)
		done <- outcome{res, err}
	}()

	load := a.next()
	if load.Method != acp.MethodSessionLoad {
		t.Fatalf("method = %q, want %q", load.Method, acp.MethodSessionLoad)
	}

	// History first, then the answer: the order every agent that answers uses.
	a.notify(acp.MethodSessionUpdate, map[string]any{
		"sessionId": "ses_f647af",
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "earlier"}},
	})
	a.notify(acp.MethodSessionUpdate, map[string]any{
		"sessionId": "ses_f647af",
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "reply"}},
	})
	a.reply(*load.ID, map[string]any{})

	got := <-done
	if got.err != nil {
		t.Fatalf("load: %v", got.err)
	}
	if !got.res.Answered {
		t.Error("agent answered the call; Answered should be true")
	}
	if got.res.Replayed != 2 {
		t.Errorf("replayed = %d, want 2", got.res.Replayed)
	}
	if c.SessionID() != "ses_f647af" {
		t.Errorf("session id = %q, want it remembered after a load", c.SessionID())
	}
	for i, n := range updates {
		if !n.Replay {
			t.Errorf("update %d arrived during a load and is not marked Replay", i)
		}
	}
}

// The behaviour that makes this worth writing: several agents attach and
// replay without ever answering session/load. A load that only waited for the
// reply would time out on a session that is in fact open.
func TestLoadSucceedsWhenTheAgentReplaysAndNeverAnswers(t *testing.T) {
	c, a := newPair(t, acp.Handler{})
	c.ReplayIdleGap = 150 * time.Millisecond
	c.LoadTimeout = 5 * time.Second
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	done := make(chan acp.LoadResult, 1)
	go func() {
		res, err := c.LoadSession(context.Background(), "20260913_1", "/repo", nil)
		if err != nil {
			t.Errorf("load: %v", err)
		}
		done <- res
	}()

	a.next() // the load call, deliberately never answered
	a.notify(acp.MethodSessionUpdate, map[string]any{
		"sessionId": "20260913_1",
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "history"}},
	})

	select {
	case res := <-done:
		if res.Answered {
			t.Error("the agent never answered; Answered should be false")
		}
		if res.Replayed != 1 {
			t.Errorf("replayed = %d, want 1", res.Replayed)
		}
		if c.SessionID() != "20260913_1" {
			t.Errorf("session id = %q, want it remembered", c.SessionID())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("load never concluded from replay going quiet")
	}
}

// Silence is not success. gemini rejects session/load outright, and an agent
// that neither answers nor replays must fail rather than report a session that
// was never opened.
func TestLoadWithNeitherAnswerNorReplayFails(t *testing.T) {
	c, a := newPair(t, acp.Handler{})
	c.LoadTimeout = 300 * time.Millisecond
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	done := make(chan error, 1)
	go func() {
		_, err := c.LoadSession(context.Background(), "ghost", "/repo", nil)
		done <- err
	}()
	a.next()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("load reported success with no answer and no replay")
		}
		if c.SessionID() != "" {
			t.Errorf("session id = %q, want empty after a failed load", c.SessionID())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("load did not give up")
	}
}

func TestLoadSurfacesAnOutrightRefusal(t *testing.T) {
	c, a := newPair(t, acp.Handler{})
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	done := make(chan error, 1)
	go func() {
		_, err := c.LoadSession(context.Background(), "nope", "/repo", nil)
		done <- err
	}()

	load := a.next()
	a.replyError(*load.ID, -32603, "Internal error")

	err := <-done
	if err == nil {
		t.Fatal("agent refused the load; want an error")
	}
	if c.SessionID() != "" {
		t.Errorf("session id = %q, want empty after a refusal", c.SessionID())
	}
}

// Updates outside a load are live, and must not be mistaken for history.
func TestUpdatesOutsideALoadAreNotMarkedReplay(t *testing.T) {
	seen := make(chan acp.UpdateNotification, 1)
	c, a := newPair(t, acp.Handler{
		OnUpdate: func(n acp.UpdateNotification) { seen <- n },
	})
	handshake(t, c, a, "sid")

	a.notify(acp.MethodSessionUpdate, map[string]any{
		"sessionId": "sid",
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "live"}},
	})

	select {
	case n := <-seen:
		if n.Replay {
			t.Error("a live update was marked as replay")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("update never arrived")
	}
}

func TestLoadWithoutASessionIDIsRefused(t *testing.T) {
	c, a := newPair(t, acp.Handler{})
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	if _, err := c.LoadSession(context.Background(), "", "/repo", nil); err == nil {
		t.Fatal("want an error for an empty session id")
	}
}

func TestAuthenticateSendsTheChosenMethod(t *testing.T) {
	c, a := newPair(t, acp.Handler{})
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	done := make(chan error, 1)
	go func() { done <- c.Authenticate(context.Background(), "oauth") }()

	call := a.next()
	if call.Method != acp.MethodAuthenticate {
		t.Fatalf("method = %q, want %q", call.Method, acp.MethodAuthenticate)
	}
	a.reply(*call.ID, map[string]any{})

	if err := <-done; err != nil {
		t.Fatalf("authenticate: %v", err)
	}
}

// A permission request that arrives during a load is marked but still
// delivered. The agent blocks on the reply, so dropping it would hang the
// session that was just resumed.
func TestPermissionDuringLoadIsMarkedAndStillAnswered(t *testing.T) {
	seen := make(chan acp.PermissionRequest, 1)
	c, a := newPair(t, acp.Handler{
		OnPermission: func(_ context.Context, r acp.PermissionRequest) (acp.PermissionResponse, error) {
			seen <- r
			return acp.Cancelled(), nil
		},
	})
	c.ReplayIdleGap = 150 * time.Millisecond
	initialize(t, c, a, map[string]any{"protocolVersion": 1})

	go func() {
		if _, err := c.LoadSession(context.Background(), "sid", "/repo", nil); err != nil {
			t.Errorf("load: %v", err)
		}
	}()

	a.next() // the load call, never answered
	a.notify(acp.MethodSessionUpdate, map[string]any{
		"sessionId": "sid",
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "history"}},
	})
	a.ask(7001, acp.MethodRequestPermission, map[string]any{
		"sessionId": "sid",
		"toolCall":  map[string]any{"toolCallId": "call_1", "title": "write"},
		"options":   []any{map[string]any{"optionId": "no", "kind": "reject_once"}},
	})

	select {
	case req := <-seen:
		if !req.DuringLoad {
			t.Error("a permission request arriving during a load was not marked DuringLoad")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("permission request was swallowed during the load; the agent would block forever")
	}
}

func TestPermissionOutsideALoadIsNotMarked(t *testing.T) {
	seen := make(chan acp.PermissionRequest, 1)
	c, a := newPair(t, acp.Handler{
		OnPermission: func(_ context.Context, r acp.PermissionRequest) (acp.PermissionResponse, error) {
			seen <- r
			return acp.Cancelled(), nil
		},
	})
	handshake(t, c, a, "sid")

	a.ask(7002, acp.MethodRequestPermission, map[string]any{
		"sessionId": "sid",
		"toolCall":  map[string]any{"toolCallId": "call_1", "title": "write"},
		"options":   []any{map[string]any{"optionId": "no", "kind": "reject_once"}},
	})

	select {
	case req := <-seen:
		if req.DuringLoad {
			t.Error("a live permission request was marked DuringLoad")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permission request never arrived")
	}
}

// The check that separates a real resume from a blank session with good
// manners: a fresh session cannot replay something the user said.
func TestLoadDistinguishesRestoredHistoryFromSetupChatter(t *testing.T) {
	cases := []struct {
		name     string
		updates  []map[string]any
		restored bool
		convo    int
	}{
		{
			name: "setup chatter only",
			updates: []map[string]any{
				{"sessionUpdate": "current_mode_update", "currentModeId": "default"},
				{"sessionUpdate": "available_commands_update", "availableCommands": []any{}},
			},
			restored: false,
			convo:    0,
		},
		{
			name: "a real conversation",
			updates: []map[string]any{
				{"sessionUpdate": "current_mode_update", "currentModeId": "default"},
				{"sessionUpdate": "user_message_chunk", "content": map[string]any{"text": "what did I ask?"}},
				{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "you asked this"}},
			},
			restored: true,
			convo:    2,
		},
		{
			// An agent replying without the user's turn is not proof: it could
			// be an opening banner. Counted as conversation, not as restored.
			name: "agent speech with no user turn",
			updates: []map[string]any{
				{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "ready"}},
			},
			restored: false,
			convo:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, a := newPair(t, acp.Handler{})
			initialize(t, c, a, map[string]any{"protocolVersion": 1})

			done := make(chan acp.LoadResult, 1)
			go func() {
				res, err := c.LoadSession(context.Background(), "sid", "/repo", nil)
				if err != nil {
					t.Errorf("load: %v", err)
				}
				done <- res
			}()

			load := a.next()
			for _, u := range tc.updates {
				a.notify(acp.MethodSessionUpdate, map[string]any{"sessionId": "sid", "update": u})
			}
			a.reply(*load.ID, map[string]any{})

			res := <-done
			if res.Replayed != len(tc.updates) {
				t.Errorf("replayed = %d, want %d", res.Replayed, len(tc.updates))
			}
			if res.Conversation != tc.convo {
				t.Errorf("conversation = %d, want %d", res.Conversation, tc.convo)
			}
			if res.RestoredConversation != tc.restored {
				t.Errorf("RestoredConversation = %v, want %v; %d updates is not by itself evidence",
					res.RestoredConversation, tc.restored, res.Replayed)
			}
		})
	}
}
