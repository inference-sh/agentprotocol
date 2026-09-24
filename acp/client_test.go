package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/acp"
)

// fakeAgent is the other end of an ACP conversation, driven by a script. It
// exists so the client can be tested against real protocol traffic without
// installing a coding agent.
type fakeAgent struct {
	t   *testing.T
	in  *bufio.Scanner // what the client sent us
	out io.Writer      // what we send back

	mu       sync.Mutex
	received []acp.Message
}

func newPair(t *testing.T, h acp.Handler) (*acp.Client, *fakeAgent) {
	t.Helper()

	clientReads, agentWrites := io.Pipe()
	agentReads, clientWrites := io.Pipe()

	agent := &fakeAgent{t: t, in: bufio.NewScanner(agentReads), out: agentWrites}
	client := acp.NewClient(clientReads, clientWrites, acp.ClientInfo{Name: "test", Version: "1"}, h)
	client.CallTimeout = 2 * time.Second
	client.Start()

	t.Cleanup(func() { _ = client.Close() })
	return client, agent
}

// next reads the next message the client sent.
func (a *fakeAgent) next() acp.Message {
	a.t.Helper()
	if !a.in.Scan() {
		a.t.Fatal("client sent nothing; expected a message")
	}
	var msg acp.Message
	if err := json.Unmarshal(a.in.Bytes(), &msg); err != nil {
		a.t.Fatalf("client sent malformed JSON: %v", err)
	}
	a.mu.Lock()
	a.received = append(a.received, msg)
	a.mu.Unlock()
	return msg
}

// reply answers a call the client made.
func (a *fakeAgent) reply(id int, result any) {
	a.t.Helper()
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	a.write(data)
}

// replyError answers a call with a JSON-RPC error.
func (a *fakeAgent) replyError(id int, code int, message string) {
	a.t.Helper()
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
	a.write(data)
}

// notify sends a notification the client is not waiting on.
func (a *fakeAgent) notify(method string, params any) {
	a.t.Helper()
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	a.write(data)
}

// ask sends an agent-initiated request that expects a reply.
func (a *fakeAgent) ask(id int, method string, params any) {
	a.t.Helper()
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	a.write(data)
}

func (a *fakeAgent) write(data []byte) {
	a.t.Helper()
	if _, err := a.out.Write(append(data, '\n')); err != nil {
		a.t.Fatalf("write to client: %v", err)
	}
}

// handshake completes initialize and session/new, returning the session ID.
func handshake(t *testing.T, c *acp.Client, a *fakeAgent, sessionID string) string {
	t.Helper()

	var sid string
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.Background()
		if _, err := c.Initialize(ctx); err != nil {
			t.Errorf("initialize: %v", err)
			return
		}
		var err error
		sid, err = c.NewSession(ctx, "/tmp", nil)
		if err != nil {
			t.Errorf("session/new: %v", err)
		}
	}()

	init := a.next()
	a.reply(*init.ID, map[string]any{"protocolVersion": 1})
	sess := a.next()
	a.reply(*sess.ID, map[string]any{"sessionId": sessionID})

	<-done
	return sid
}

func TestHandshakeOpensASession(t *testing.T) {
	client, agent := newPair(t, acp.Handler{})
	sid := handshake(t, client, agent, "sess_abc")

	if sid != "sess_abc" {
		t.Errorf("session ID = %q, want sess_abc", sid)
	}
	if client.SessionID() != "sess_abc" {
		t.Errorf("client forgot the session ID")
	}
}

func TestInitializeAdvertisesOnlyWhatTheHandlerCanAnswer(t *testing.T) {
	// An agent told it may ask permission, when nobody is listening, will
	// block on a question that never gets answered.
	client, agent := newPair(t, acp.Handler{})

	go func() { _, _ = client.Initialize(context.Background()) }()
	msg := agent.next()

	var params acp.InitializeParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	if params.Capabilities.PermissionRequests {
		t.Error("advertised permission requests with no handler to answer them")
	}
	if params.Capabilities.FileSystem {
		t.Error("advertised filesystem access with no handler to serve it")
	}
	agent.reply(*msg.ID, map[string]any{})
}

func TestInitializeAdvertisesCapabilitiesTheHandlerProvides(t *testing.T) {
	client, agent := newPair(t, acp.Handler{
		OnPermission: func(context.Context, acp.PermissionRequest) (acp.PermissionResponse, error) {
			return acp.Cancelled(), nil
		},
		OnReadTextFile: func(context.Context, acp.ReadTextFileParams) (acp.ReadTextFileResult, error) {
			return acp.ReadTextFileResult{}, nil
		},
	})

	go func() { _, _ = client.Initialize(context.Background()) }()
	msg := agent.next()

	var params acp.InitializeParams
	_ = json.Unmarshal(msg.Params, &params)
	if !params.Capabilities.PermissionRequests {
		t.Error("did not advertise permission requests despite having a handler")
	}
	if !params.Capabilities.FileSystem {
		t.Error("did not advertise filesystem despite having a handler")
	}
	agent.reply(*msg.ID, map[string]any{})
}

func TestPromptDeliversUpdatesAndCompletes(t *testing.T) {
	var updates []acp.SessionUpdate
	var mu sync.Mutex

	client, agent := newPair(t, acp.Handler{
		OnUpdate: func(n acp.UpdateNotification) {
			mu.Lock()
			updates = append(updates, n.Update)
			mu.Unlock()
		},
	})
	handshake(t, client, agent, "sess_1")

	done := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), "hello")
		done <- err
	}()

	prompt := agent.next()
	agent.notify(acp.MethodSessionUpdate, acp.UpdateNotification{
		SessionID: "sess_1",
		Update: acp.SessionUpdate{
			Kind:    acp.UpdateKindAgentMessageChunk,
			Content: json.RawMessage(`{"type":"text","text":"hi there"}`),
		},
	})
	agent.reply(*prompt.ID, map[string]any{"stopReason": "end_turn"})

	if err := <-done; err != nil {
		t.Fatalf("prompt: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 1 {
		t.Fatalf("got %d updates, want 1", len(updates))
	}
	if got := updates[0].Text(); got != "hi there" {
		t.Errorf("update text = %q, want %q", got, "hi there")
	}
}

func TestPromptBeforeSessionIsRefused(t *testing.T) {
	client, _ := newPair(t, acp.Handler{})
	if _, err := client.Prompt(context.Background(), "hello"); err == nil {
		t.Error("prompt without a session succeeded; it should refuse")
	}
}

func TestPermissionRequestReachesTheHandlerAndIsAnswered(t *testing.T) {
	var seen acp.PermissionRequest
	client, agent := newPair(t, acp.Handler{
		OnPermission: func(_ context.Context, r acp.PermissionRequest) (acp.PermissionResponse, error) {
			seen = r
			id, ok := r.PickOption(acp.OptionKindAllowOnce)
			if !ok {
				return acp.Cancelled(), nil
			}
			return acp.Selected(id), nil
		},
	})
	handshake(t, client, agent, "sess_1")

	agent.ask(99, acp.MethodRequestPermission, acp.PermissionRequest{
		SessionID: "sess_1",
		ToolCall:  &acp.PermissionToolRef{ToolCallID: "call_1", Title: "run tests"},
		Options: []acp.PermissionOption{
			{OptionID: "o_reject", Kind: acp.OptionKindRejectOnce},
			{OptionID: "o_allow", Kind: acp.OptionKindAllowOnce},
		},
	})

	reply := agent.next()
	if reply.ID == nil || *reply.ID != 99 {
		t.Fatalf("reply addressed to %v, want request 99", reply.ID)
	}

	var res acp.PermissionResponse
	if err := json.Unmarshal(reply.Result, &res); err != nil {
		t.Fatalf("decode permission response: %v", err)
	}
	// The nested shape matters: schema-validating agents reject a flat one
	// and fail the tool call before it runs.
	if res.Outcome.Outcome != acp.OutcomeSelected {
		t.Errorf("outcome = %q, want selected", res.Outcome.Outcome)
	}
	if res.Outcome.OptionID != "o_allow" {
		t.Errorf("chose option %q, want o_allow", res.Outcome.OptionID)
	}
	if seen.ToolCall == nil || seen.ToolCall.Title != "run tests" {
		t.Error("handler did not receive the tool being approved")
	}
}

func TestPermissionWithNoHandlerCancels(t *testing.T) {
	// Never allow by default. An action nobody approved must not run.
	client, agent := newPair(t, acp.Handler{})
	handshake(t, client, agent, "sess_1")

	agent.ask(7, acp.MethodRequestPermission, acp.PermissionRequest{
		Options: []acp.PermissionOption{{OptionID: "o_allow", Kind: acp.OptionKindAllowOnce}},
	})

	reply := agent.next()
	var res acp.PermissionResponse
	_ = json.Unmarshal(reply.Result, &res)
	if res.Outcome.Outcome != acp.OutcomeCancelled {
		t.Errorf("outcome = %q, want cancelled when no handler decides", res.Outcome.Outcome)
	}
}

func TestUnknownRequestIsAnsweredNotDropped(t *testing.T) {
	// A dropped request leaves the agent waiting on its own timeout, which
	// presents as a hang far from the cause.
	client, agent := newPair(t, acp.Handler{})
	handshake(t, client, agent, "sess_1")

	agent.ask(42, "x.ai/hooks/run", map[string]any{"anything": true})

	reply := agent.next()
	if reply.ID == nil || *reply.ID != 42 {
		t.Fatalf("reply addressed to %v, want request 42", reply.ID)
	}
	if reply.Error == nil {
		t.Fatal("unknown method got a success reply")
	}
	if reply.Error.Code != acp.ErrCodeMethodNotFound {
		t.Errorf("error code = %d, want method-not-found", reply.Error.Code)
	}
}

func TestFileHandlersServeAndRefuse(t *testing.T) {
	client, agent := newPair(t, acp.Handler{
		OnReadTextFile: func(_ context.Context, p acp.ReadTextFileParams) (acp.ReadTextFileResult, error) {
			return acp.ReadTextFileResult{Content: "contents of " + p.Path}, nil
		},
	})
	handshake(t, client, agent, "sess_1")

	agent.ask(1, acp.MethodFsReadTextFile, acp.ReadTextFileParams{Path: "/a.txt"})
	read := agent.next()
	var res acp.ReadTextFileResult
	if err := json.Unmarshal(read.Result, &res); err != nil {
		t.Fatalf("decode read result: %v", err)
	}
	if res.Content != "contents of /a.txt" {
		t.Errorf("content = %q", res.Content)
	}

	// Writing was never offered, so it must be refused rather than silently
	// succeeding.
	agent.ask(2, acp.MethodFsWriteTextFile, acp.WriteTextFileParams{Path: "/b.txt", Content: "x"})
	write := agent.next()
	if write.Error == nil {
		t.Error("write succeeded despite no write handler")
	}
}

func TestCallSurfacesAgentErrors(t *testing.T) {
	client, agent := newPair(t, acp.Handler{})

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "initialize", nil)
		done <- err
	}()

	msg := agent.next()
	agent.replyError(*msg.ID, -32000, "agent is unhappy")

	err := <-done
	if err == nil {
		t.Fatal("agent error was swallowed")
	}
	if got := err.Error(); got == "" {
		t.Error("error carries no message")
	}
}

func TestMalformedLinesAreIgnored(t *testing.T) {
	// Agents print diagnostics to stdout alongside protocol traffic. One bad
	// line must not end the conversation.
	client, agent := newPair(t, acp.Handler{})

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "initialize", nil)
		done <- err
	}()

	msg := agent.next()
	agent.write([]byte("warning: something happened"))
	agent.write([]byte("{not json"))
	agent.reply(*msg.ID, map[string]any{"ok": true})

	if err := <-done; err != nil {
		t.Fatalf("noise on the stream broke the call: %v", err)
	}
}

func TestCallTimesOutRatherThanHanging(t *testing.T) {
	client, agent := newPair(t, acp.Handler{})
	client.CallTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "initialize", nil)
		done <- err
	}()
	agent.next() // received, deliberately never answered

	select {
	case err := <-done:
		if err == nil {
			t.Error("unanswered call returned success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call did not time out")
	}
}

func TestPickOptionPrefersInOrderThenFallsBack(t *testing.T) {
	r := acp.PermissionRequest{Options: []acp.PermissionOption{
		{OptionID: "a", Kind: acp.OptionKindAllowAlways},
		{OptionID: "b", Kind: acp.OptionKindAllowOnce},
	}}

	if id, ok := r.PickOption(acp.OptionKindAllowOnce, acp.OptionKindAllowAlways); !ok || id != "b" {
		t.Errorf("picked %q, want the first preference b", id)
	}

	// An agent using its own spelling should still match on the verb.
	odd := acp.PermissionRequest{Options: []acp.PermissionOption{{OptionID: "z", Kind: "allow_for_this_folder"}}}
	if id, ok := odd.PickOption(acp.OptionKindAllowOnce); !ok || id != "z" {
		t.Errorf("picked %q, want the allow-prefixed option z", id)
	}

	// Nothing matching must report false, never guess.
	none := acp.PermissionRequest{Options: []acp.PermissionOption{{OptionID: "r", Kind: acp.OptionKindRejectOnce}}}
	if id, ok := none.PickOption(acp.OptionKindAllowOnce); ok {
		t.Errorf("picked %q from reject-only options; should report false", id)
	}
}

func TestUpdateTextAcceptsBothContentShapes(t *testing.T) {
	single := acp.SessionUpdate{Content: json.RawMessage(`{"type":"text","text":"one"}`)}
	if got := single.Text(); got != "one" {
		t.Errorf("single block text = %q", got)
	}

	many := acp.SessionUpdate{Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)}
	if got := many.Text(); got != "ab" {
		t.Errorf("block array text = %q, want concatenated", got)
	}

	if got := (acp.SessionUpdate{}).Text(); got != "" {
		t.Errorf("empty content text = %q, want empty", got)
	}
}

func TestEventForUpdateMapsContentAndTools(t *testing.T) {
	chunk := acp.SessionUpdate{
		Kind:    acp.UpdateKindAgentMessageChunk,
		Content: json.RawMessage(`{"type":"text","text":"thinking"}`),
	}
	ev, ok := acp.EventForUpdate(chunk, "run_1", "chat_1")
	if !ok {
		t.Fatal("content chunk produced no event")
	}
	if ev.Type != ap.AgentEventContentDelta {
		t.Errorf("event type = %s, want content delta", ev.Type)
	}
	payload, ok := ap.PayloadAs[ap.ContentDeltaPayload](ev, ap.AgentEventContentDelta)
	if !ok {
		t.Fatal("could not decode the delta payload")
	}
	if payload.Delta != "thinking" || payload.Kind != ap.ContentDeltaText {
		t.Errorf("payload = %+v", payload)
	}

	start := acp.SessionUpdate{Kind: acp.UpdateKindToolCall, ToolCallID: "c1", Title: "bash"}
	ev, ok = acp.EventForUpdate(start, "run_1", "chat_1")
	if !ok || ev.Type != ap.AgentEventToolStarted {
		t.Fatalf("tool call mapped to %s, want tool started", ev.Type)
	}

	// A tool still running is not a completion; emitting one would close an
	// invocation that is still going.
	running := acp.SessionUpdate{Kind: acp.UpdateKindToolCallUpdate, ToolCallID: "c1", Status: "in_progress"}
	if _, ok := acp.EventForUpdate(running, "run_1", "chat_1"); ok {
		t.Error("in-progress tool update produced a completion event")
	}

	finished := acp.SessionUpdate{Kind: acp.UpdateKindToolCallUpdate, ToolCallID: "c1", Status: "completed"}
	ev, ok = acp.EventForUpdate(finished, "run_1", "chat_1")
	if !ok || ev.Type != ap.AgentEventToolCompleted {
		t.Fatalf("completed tool mapped to %s", ev.Type)
	}
}

func TestUnknownToolStatusCompletesAsFailed(t *testing.T) {
	// A status we do not model must still terminate the invocation, or it
	// hangs in progress forever.
	u := acp.SessionUpdate{Kind: acp.UpdateKindToolCallUpdate, ToolCallID: "c1", Status: "exploded"}
	ev, ok := acp.EventForUpdate(u, "run_1", "chat_1")
	if !ok {
		t.Fatal("unknown status produced no event")
	}
	payload, _ := ap.PayloadAs[ap.ToolCompletedPayload](ev, ap.AgentEventToolCompleted)
	if payload.Status != ap.ToolInvocationStatusFailed {
		t.Errorf("status = %s, want failed", payload.Status)
	}
}

func TestResponseForResolutionNeverGuesses(t *testing.T) {
	allowOnly := acp.PermissionRequest{Options: []acp.PermissionOption{
		{OptionID: "a", Kind: acp.OptionKindAllowOnce},
	}}

	if res := acp.ResponseForResolution(allowOnly, ap.InterruptResolutionAllow); res.Outcome.OptionID != "a" {
		t.Errorf("approve chose %q, want a", res.Outcome.OptionID)
	}

	// Denying when the agent offers no reject option must cancel, not pick
	// the allow option that happens to be there.
	res := acp.ResponseForResolution(allowOnly, ap.InterruptResolutionDeny)
	if res.Outcome.Outcome != acp.OutcomeCancelled {
		t.Errorf("deny with no reject option gave %q/%q, want cancelled",
			res.Outcome.Outcome, res.Outcome.OptionID)
	}
}

func TestApprovalForPermissionCarriesTheToolIdentity(t *testing.T) {
	req := acp.PermissionRequest{
		ToolCall: &acp.PermissionToolRef{
			ToolCallID: "call_9",
			Title:      "rm -rf /",
			RawInput:   json.RawMessage(`{"path":"/"}`),
		},
	}
	p := acp.ApprovalForPermission(req)

	if p.ToolInvocationID != "call_9" {
		t.Errorf("invocation ID = %q; without it a decision cannot be routed back", p.ToolInvocationID)
	}
	if p.ToolName != "rm -rf /" {
		t.Errorf("tool name = %q", p.ToolName)
	}
	if p.Reason != ap.InterruptReasonToolApproval {
		t.Errorf("reason = %q, want tool approval", p.Reason)
	}
	if got, _ := p.Arguments.GetString("path"); got != "/" {
		t.Errorf("arguments lost the path the human needs to see: %+v", p.Arguments)
	}
}

func TestIsTurnDoneAcceptsBothSignalFields(t *testing.T) {
	if !acp.IsTurnDone(acp.SessionUpdate{Kind: "turn_complete"}) {
		t.Error("turn_complete in kind not recognised")
	}
	if !acp.IsTurnDone(acp.SessionUpdate{Status: "idle"}) {
		t.Error("idle in status not recognised")
	}
	if acp.IsTurnDone(acp.SessionUpdate{Kind: acp.UpdateKindContentChunk}) {
		t.Error("a content chunk was treated as the end of a turn")
	}
}

func TestIdlessErrorDoesNotCrash(t *testing.T) {
	// kiro-cli answers a session/close notification with an error carrying no
	// id. That is legal JSON-RPC: a peer sends a null or absent id when it
	// cannot attribute the failure to a request. Such a message has neither an
	// ID nor a method, so it must not be mistaken for a request to answer.
	client, agent := newPair(t, acp.Handler{})
	handshake(t, client, agent, "sess_1")

	agent.write([]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found","data":"session/close"}}`))

	// The read loop must still be alive afterwards.
	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "initialize", nil)
		done <- err
	}()
	msg := agent.next()
	agent.reply(*msg.ID, map[string]any{"ok": true})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("client died after an id-less error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read loop stopped after an id-less error")
	}
}

func TestIdlessErrorReachesOnPeerError(t *testing.T) {
	// Nothing can be replied to and no caller is waiting, so without a handler
	// the failure is invisible. This is how it stays diagnosable.
	got := make(chan *acp.Error, 1)
	client, agent := newPair(t, acp.Handler{
		OnPeerError: func(e *acp.Error) { got <- e },
	})
	handshake(t, client, agent, "sess_1")

	agent.write([]byte(`{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found","data":"session/close"}}`))

	select {
	case e := <-got:
		if e.Code != -32601 || e.Message != "Method not found" {
			t.Errorf("got %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unattributable error never surfaced")
	}
}

func TestPromptGetsALongerBudgetThanProtocolCalls(t *testing.T) {
	// A prompt's duration belongs to the model. Timing it out on a protocol
	// budget kills work that was still progressing.
	client, agent := newPair(t, acp.Handler{})
	handshake(t, client, agent, "sess_1")

	client.CallTimeout = 50 * time.Millisecond
	client.PromptTimeout = 3 * time.Second

	done := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), "slow work")
		done <- err
	}()

	prompt := agent.next()
	// Longer than CallTimeout, well inside PromptTimeout.
	time.Sleep(300 * time.Millisecond)
	agent.reply(*prompt.ID, map[string]any{"stopReason": "end_turn"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt timed out on the protocol budget: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt never returned")
	}
}

func TestProtocolCallStillUsesTheShortBudget(t *testing.T) {
	client, agent := newPair(t, acp.Handler{})
	client.CallTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), acp.MethodInitialize, nil)
		done <- err
	}()
	agent.next() // never answered

	select {
	case err := <-done:
		if err == nil {
			t.Error("protocol call ignored CallTimeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("protocol call did not time out")
	}
}

func TestPromptHasNoTotalBoundByDefault(t *testing.T) {
	// A long turn that is still working must not be cut off. Dead agents are
	// caught by the stream ending, silent ones by the driver's first-event
	// bound; the prompt itself waits as long as the turn takes.
	client, agent := newPair(t, acp.Handler{})
	handshake(t, client, agent, "sess_1")
	if client.PromptTimeout != 0 {
		t.Fatalf("default PromptTimeout = %s, want none", client.PromptTimeout)
	}
	client.CallTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), "long work")
		done <- err
	}()
	prompt := agent.next()
	time.Sleep(400 * time.Millisecond)
	agent.reply(*prompt.ID, map[string]any{"stopReason": "end_turn"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt never returned")
	}
}
